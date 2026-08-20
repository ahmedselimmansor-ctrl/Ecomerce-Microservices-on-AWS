// SOUQ pricing rule engine.
//
// This is the only synchronous dependency on the checkout hot path with a
// sub-millisecond budget, which is why it is C++ and why the evaluator below
// is written the way it is: no allocation in the inner loop, no virtual
// dispatch per rule, and the whole rule set resident in memory.
//
// It is also the only service that handles money for every single cart view,
// so correctness matters more than speed. Three properties are enforced by
// construction rather than by discipline:
//
//   1. INTEGER ONLY. There is no double anywhere in this file. Money is
//      int64_t minor units and rates are basis points. A cart total that is
//      off by a cent is a support ticket, a failed reconciliation, and
//      eventually an audit finding.
//
//   2. DETERMINISTIC. The same cart and the same rules_version always produce
//      the same total. That is what lets order-service re-price an order at
//      capture time and get the same number, even if a promotion expired in
//      between (docs/CONTRACTS.md §4).
//
//   3. NEVER NEGATIVE. A stack of promotions cannot make a line, or a cart,
//      cost less than zero. Enforced at every step, not just at the end,
//      because an intermediate negative would poison the percentage rules
//      that come after it.

#pragma once

#include <cstdint>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

namespace souq::pricing {

// ---------------------------------------------------------------------------
// Money

using Minor = std::int64_t;      // minor units: 129900 == 1299.00
using BasisPoints = std::int32_t; // 1900 == 19.00%

/// Applies a basis-point rate with half-up rounding.
///
/// Rounding is specified rather than left to the compiler because it is
/// visible to the customer: 19% VAT on 999 is 189.81, and whether that becomes
/// 189 or 190 must be the same on every platform and in every re-price.
/// Half-up matches what accountants and every tax authority expect.
[[nodiscard]] constexpr Minor apply_bp(Minor amount, BasisPoints bp) noexcept {
    // Guard the multiplication before it happens. 2^63 / 10000 is about
    // 9.2e14, which is 9.2 trillion in major units — far beyond any real cart,
    // but a malformed request should be clamped rather than wrap around into a
    // negative total.
    constexpr Minor kMaxSafe = 922'337'203'685'477LL;
    if (amount > kMaxSafe || amount < -kMaxSafe) {
        return amount > 0 ? kMaxSafe : -kMaxSafe;
    }

    const Minor product = amount * static_cast<Minor>(bp);
    // Half-up, symmetric about zero so a discount rounds the same way as a
    // charge. Integer arithmetic throughout: no std::round, no double.
    return product >= 0 ? (product + 5000) / 10000 : (product - 5000) / 10000;
}

/// Clamps to zero. Used everywhere a discount is subtracted.
[[nodiscard]] constexpr Minor non_negative(Minor v) noexcept { return v < 0 ? 0 : v; }

// ---------------------------------------------------------------------------
// Cart

struct CartLine {
    std::string sku;
    std::string product_id;
    std::int32_t quantity{0};
    Minor unit_list_price{0};
    std::vector<std::string> category_path;
    std::string brand;
    std::unordered_map<std::string, std::string> attributes;
};

struct Context {
    std::string user_id;
    std::string currency{"EGP"};
    std::string country_code{"EG"};
    std::vector<std::string> segments;   // "vip", "b2b", "first_order"
    std::vector<std::string> coupons;
    std::string channel{"web"};
};

// ---------------------------------------------------------------------------
// Rules

enum class PromotionType : std::uint8_t {
    PercentOff,
    AmountOff,
    BuyXGetY,
    TieredQuantity,
    Bundle,
    FreeShipping,
    CartThreshold,
};

/// A predicate a rule must satisfy.
///
/// A small closed set rather than an expression language. An interpreter would
/// be more flexible and would also put arbitrary merchant-authored code on the
/// checkout hot path, where a pathological expression becomes an outage.
struct Condition {
    enum class Field : std::uint8_t {
        CartSubtotal, LineQuantity, CartQuantity,
        Brand, CategoryPath, Sku, Segment, Channel, CountryCode,
    };
    enum class Op : std::uint8_t { Gte, Lte, Eq, In, NotIn };

    Field field{Field::CartSubtotal};
    Op op{Op::Gte};
    Minor numeric{0};
    std::vector<std::string> values;
};

struct Tier {
    std::int32_t min_quantity{0};
    BasisPoints discount_bp{0};
};

struct Rule {
    std::string id;
    std::string name;
    PromotionType type{PromotionType::PercentOff};

    BasisPoints percent_off{0};
    Minor amount_off{0};

    std::int32_t buy_quantity{0};
    std::int32_t get_quantity{0};
    BasisPoints get_discount_bp{10000};   // 10000 == free

    std::vector<Tier> tiers;

    std::string coupon_code;      // empty means automatic
    std::vector<Condition> conditions;

    /// Lower runs first. Order is load-bearing: a percentage applied before an
    /// absolute discount gives a different total than the reverse, and the
    /// merchant needs to be able to say which.
    std::int32_t priority{100};

    /// Suppresses every lower-priority rule once it applies. Without this, a
    /// stack of "20% off" and "30% off" and "500 off" compounds into giving
    /// the product away.
    bool exclusive{false};

    /// Caps how much this one rule can take off, whatever the maths says.
    /// A percentage rule with no cap on a high-value cart is how a pricing bug
    /// becomes a five-figure loss before anyone notices.
    Minor max_discount{0};        // 0 == uncapped

    std::int64_t valid_from{0};   // unix seconds; 0 == always
    std::int64_t valid_until{0};
    bool active{true};
};

// ---------------------------------------------------------------------------
// Results

struct AppliedPromotion {
    std::string promotion_id;
    std::string name;
    PromotionType type{};
    Minor discount{0};            // always negative
    std::string coupon_code;
    std::vector<std::string> applies_to_skus;
    std::int32_t priority{0};
    bool exclusive{false};
};

struct RejectedPromotion {
    std::string promotion_id;
    std::string coupon_code;
    std::string reason_code;      // machine-readable; the UI localises it
    std::string detail;
};

struct LinePrice {
    std::string sku;
    std::int32_t quantity{0};
    Minor unit_list_price{0};
    Minor unit_effective_price{0};
    Minor line_subtotal{0};
    Minor line_discount{0};       // negative
    Minor line_total{0};
    std::vector<std::string> applied_promotion_ids;
};

struct TaxLine {
    std::string name;
    std::string jurisdiction;
    BasisPoints rate{0};
    Minor amount{0};
    bool inclusive{false};
};

struct Quote {
    std::vector<LinePrice> lines;
    Minor subtotal{0};
    Minor discount_total{0};      // negative
    Minor shipping_total{0};
    Minor tax_total{0};
    Minor grand_total{0};

    std::vector<AppliedPromotion> applied;
    std::vector<RejectedPromotion> rejected;
    std::vector<TaxLine> tax_lines;

    std::string rules_version;
    bool degraded{false};
    std::vector<std::string> degraded_reasons;
};

// ---------------------------------------------------------------------------

/// Tax rules, kept separate from promotions because they compose differently:
/// promotions choose the best outcome for the customer, tax is not optional.
struct TaxRule {
    std::string country_code;
    std::string name;
    std::string jurisdiction;
    BasisPoints rate{0};
    /// True in the EU and Egypt (VAT is in the shelf price), false in the US
    /// (sales tax is added at checkout). Getting it backwards changes every
    /// displayed price by the tax rate.
    bool inclusive{true};
};

class RuleSet {
public:
    RuleSet() = default;
    RuleSet(std::vector<Rule> rules, std::vector<TaxRule> taxes, std::string version);

    /// Prices a whole cart. Pure: no I/O, no clock beyond `now`, no globals.
    /// Passing `now` in rather than calling the clock is what makes the
    /// re-price at capture time reproducible.
    [[nodiscard]] Quote calculate(const std::vector<CartLine>& lines,
                                  const Context& ctx,
                                  Minor shipping_quote,
                                  std::int64_t now) const;

    [[nodiscard]] const std::string& version() const noexcept { return version_; }
    [[nodiscard]] std::size_t active_rule_count() const noexcept { return rules_.size(); }

    /// Loads a rule set from JSON. Returns false and leaves the current set
    /// untouched on any error — a bad rules file must never take pricing down.
    static bool from_json(std::string_view json, RuleSet& out, std::string& error);

private:
    std::vector<Rule> rules_;                 // pre-sorted by priority
    std::vector<TaxRule> taxes_;
    std::string version_;

    [[nodiscard]] bool matches(const Rule& r, const std::vector<CartLine>& lines,
                               const Context& ctx, Minor subtotal, std::int64_t now,
                               std::string& reject_reason) const;

    [[nodiscard]] static bool condition_holds(const Condition& c,
                                              const std::vector<CartLine>& lines,
                                              const Context& ctx, Minor subtotal);
};

[[nodiscard]] std::string_view to_string(PromotionType t) noexcept;

}  // namespace souq::pricing
