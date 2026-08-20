// Tests for the pricing rule engine.
//
// No test framework: a single header-only harness keeps the build to one g++
// invocation, which matters because this is the test people run while
// iterating on a promotion rule.
//
// The properties asserted here are the ones that cost money when wrong:
// rounding, never-negative, discount caps, and exclusivity.

#include "../src/rules.hpp"

#include <cstdio>
#include <string>
#include <vector>

using namespace souq::pricing;

namespace {

int g_failures = 0;
int g_checks = 0;

void check(bool ok, const std::string& what) {
    ++g_checks;
    if (ok) {
        std::printf("  \033[0;32mok\033[0m   %s\n", what.c_str());
    } else {
        ++g_failures;
        std::printf("  \033[0;31mFAIL\033[0m %s\n", what.c_str());
    }
}

void check_eq(Minor got, Minor want, const std::string& what) {
    ++g_checks;
    if (got == want) {
        std::printf("  \033[0;32mok\033[0m   %s (%lld)\n", what.c_str(), static_cast<long long>(got));
    } else {
        ++g_failures;
        std::printf("  \033[0;31mFAIL\033[0m %s: got %lld, want %lld\n",
                    what.c_str(), static_cast<long long>(got), static_cast<long long>(want));
    }
}

CartLine line(std::string sku, std::int32_t qty, Minor unit,
              std::string brand = "Acme",
              std::vector<std::string> cats = {"electronics"}) {
    CartLine l;
    l.sku = std::move(sku);
    l.product_id = "prd_" + l.sku;
    l.quantity = qty;
    l.unit_list_price = unit;
    l.brand = std::move(brand);
    l.category_path = std::move(cats);
    return l;
}

Rule percent(std::string id, BasisPoints bp, std::int32_t prio = 100) {
    Rule r;
    r.id = std::move(id);
    r.name = "percent";
    r.type = PromotionType::PercentOff;
    r.percent_off = bp;
    r.priority = prio;
    return r;
}

constexpr std::int64_t kNow = 1'800'000'000;   // fixed clock: deterministic

}  // namespace

// ---------------------------------------------------------------------------

void test_rounding() {
    std::printf("\nrounding (half-up, integer only)\n");

    // 19% VAT on 999 is 189.81 -> 190. Truncation would give 189 and every
    // invoice would be a piastre light.
    check_eq(apply_bp(999, 1900), 190, "19% of 999 rounds half-up");
    check_eq(apply_bp(1000, 1900), 190, "19% of 1000");
    check_eq(apply_bp(100, 1500), 15, "15% of 100 is exact");
    check_eq(apply_bp(101, 1500), 15, "15% of 101 rounds down (15.15)");
    check_eq(apply_bp(103, 1500), 15, "15% of 103 rounds down (15.45)");
    check_eq(apply_bp(104, 1500), 16, "15% of 104 rounds up (15.60)");

    // Symmetric about zero, so a discount rounds the same way as a charge.
    check_eq(apply_bp(-999, 1900), -190, "negative amounts round symmetrically");

    // Overflow is clamped, not wrapped. A wrapped multiplication would produce
    // a negative total and a free order.
    check(apply_bp(9'000'000'000'000'000LL, 10000) > 0, "an implausible amount clamps rather than wrapping");
}

void test_never_negative() {
    std::printf("\nnever negative\n");

    // Three stacked 50% discounts. Naive subtraction gives -50% of the price.
    std::vector<Rule> rules{percent("p1", 5000, 10), percent("p2", 5000, 20), percent("p3", 5000, 30)};
    RuleSet rs(rules, {}, "test");

    auto q = rs.calculate({line("sku_1", 1, 10000)}, Context{}, 0, kNow);

    check(q.grand_total >= 0, "three stacked 50% discounts cannot go below zero");
    check(q.lines[0].line_total >= 0, "no line total is negative");
    check_eq(q.grand_total, 0, "the cart bottoms out at zero, not below");
}

void test_percent_off() {
    std::printf("\npercentage discount\n");

    RuleSet rs({percent("p1", 2000)}, {}, "test");
    auto q = rs.calculate({line("sku_1", 2, 50000)}, Context{}, 0, kNow);

    check_eq(q.subtotal, 100000, "subtotal is 2 x 500.00");
    check_eq(q.discount_total, -20000, "20% off is 200.00");
    check_eq(q.grand_total, 80000, "total is 800.00");
    check_eq(q.lines[0].unit_effective_price, 40000, "unit effective price is 400.00");
    check(q.applied.size() == 1, "one promotion applied");
}

void test_amount_off_is_spread_proportionally() {
    std::printf("\nfixed-amount discount spreads across lines\n");

    Rule r;
    r.id = "amt";
    r.name = "100 off";
    r.type = PromotionType::AmountOff;
    r.amount_off = 10000;
    RuleSet rs({r}, {}, "test");

    // 300.00 + 100.00 = 400.00, discount 100.00 -> 75/25 split.
    auto q = rs.calculate({line("sku_1", 1, 30000), line("sku_2", 1, 10000)}, Context{}, 0, kNow);

    check_eq(q.discount_total, -10000, "the whole discount is applied");
    check_eq(q.lines[0].line_total + q.lines[1].line_total, 30000,
             "line totals sum to the discounted subtotal");
    // The parts must sum exactly to the whole, or a partial refund later
    // computes the wrong amount.
    check_eq(-(q.lines[0].line_discount + q.lines[1].line_discount), 10000,
             "per-line discounts sum exactly to the cart discount");
    check(q.lines[0].line_discount < q.lines[1].line_discount,
          "the more expensive line absorbs more of the discount");
}

void test_amount_off_cannot_exceed_cart() {
    std::printf("\na fixed discount larger than the cart\n");

    Rule r;
    r.id = "amt";
    r.type = PromotionType::AmountOff;
    r.amount_off = 100000;      // 1000.00 off a 100.00 cart
    RuleSet rs({r}, {}, "test");

    auto q = rs.calculate({line("sku_1", 1, 10000)}, Context{}, 0, kNow);
    check_eq(q.grand_total, 0, "the cart cannot go below zero");
    check_eq(q.discount_total, -10000, "the discount is capped at the cart value, not the coupon value");
}

void test_max_discount_cap() {
    std::printf("\nper-rule discount cap\n");

    Rule r = percent("capped", 5000);
    r.max_discount = 5000;      // never more than 50.00 off
    RuleSet rs({r}, {}, "test");

    // 50% of 1000.00 would be 500.00; the cap holds it to 50.00.
    auto q = rs.calculate({line("sku_1", 1, 100000)}, Context{}, 0, kNow);

    check_eq(q.discount_total, -5000, "the cap holds");
    check_eq(q.grand_total, 95000, "the customer pays 950.00, not 500.00");
    check_eq(q.lines[0].line_total, 95000, "the line total reflects the cap too");
}

void test_exclusive_suppresses_lower_priority() {
    std::printf("\nexclusivity\n");

    Rule big = percent("big", 3000, 10);
    big.exclusive = true;
    RuleSet rs({big, percent("small", 1000, 20), percent("tiny", 500, 30)}, {}, "test");

    auto q = rs.calculate({line("sku_1", 1, 10000)}, Context{}, 0, kNow);

    check_eq(q.discount_total, -3000, "only the exclusive rule applied (30%, not 30+10+5)");
    check(q.applied.size() == 1, "exactly one promotion applied");
    check(q.rejected.size() == 2, "the two suppressed rules are reported, not silently dropped");
    check(q.rejected[0].reason_code == "EXCLUDED_BY_EXCLUSIVE_PROMOTION",
          "the suppression reason is machine-readable");
}

void test_buy_x_get_y() {
    std::printf("\nbuy X get Y\n");

    Rule r;
    r.id = "b2g1";
    r.name = "buy 2 get 1 free";
    r.type = PromotionType::BuyXGetY;
    r.buy_quantity = 2;
    r.get_quantity = 1;
    r.get_discount_bp = 10000;   // free
    RuleSet rs({r}, {}, "test");

    // 7 items at 10.00. Groups of 3 -> 2 complete groups -> 2 free.
    auto q = rs.calculate({line("sku_1", 7, 1000)}, Context{}, 0, kNow);
    check_eq(q.discount_total, -2000, "2 of 7 are free, not 2.33");
    check_eq(q.grand_total, 5000, "the customer pays for 5");

    // Below the threshold nothing applies.
    auto q2 = rs.calculate({line("sku_1", 2, 1000)}, Context{}, 0, kNow);
    check_eq(q2.discount_total, 0, "2 items do not qualify for buy-2-get-1");
}

void test_tiered_quantity_takes_the_best_tier() {
    std::printf("\ntiered quantity\n");

    Rule r;
    r.id = "tier";
    r.type = PromotionType::TieredQuantity;
    r.tiers = {{3, 500}, {5, 1000}, {10, 2000}};
    RuleSet rs({r}, {}, "test");

    auto q = rs.calculate({line("sku_1", 6, 10000)}, Context{}, 0, kNow);
    // 6 qualifies for the 5+ tier at 10%, NOT 5% + 10% stacked.
    check_eq(q.discount_total, -6000, "the highest qualifying tier wins, tiers are not additive");

    auto q2 = rs.calculate({line("sku_1", 2, 10000)}, Context{}, 0, kNow);
    check_eq(q2.discount_total, 0, "below the lowest tier, nothing applies");
}

void test_conditions() {
    std::printf("\nconditions\n");

    Rule r = percent("vip", 1500);
    r.conditions.push_back({Condition::Field::CartSubtotal, Condition::Op::Gte, 50000, {}});
    r.conditions.push_back({Condition::Field::Segment, Condition::Op::In, 0, {"vip"}});
    RuleSet rs({r}, {}, "test");

    Context vip;
    vip.segments = {"vip"};

    auto qualifies = rs.calculate({line("sku_1", 1, 60000)}, vip, 0, kNow);
    check_eq(qualifies.discount_total, -9000, "a VIP over the minimum spend gets the discount");

    auto too_cheap = rs.calculate({line("sku_1", 1, 10000)}, vip, 0, kNow);
    check_eq(too_cheap.discount_total, 0, "under the minimum spend, no discount");

    auto not_vip = rs.calculate({line("sku_1", 1, 60000)}, Context{}, 0, kNow);
    check_eq(not_vip.discount_total, 0, "a non-VIP gets nothing");
}

void test_coupons() {
    std::printf("\ncoupons\n");

    Rule r = percent("save10", 1000);
    r.coupon_code = "SAVE10";
    r.valid_until = kNow - 1;    // expired yesterday
    RuleSet rs({r}, {}, "test");

    Context with_coupon;
    with_coupon.coupons = {"SAVE10"};

    auto q = rs.calculate({line("sku_1", 1, 10000)}, with_coupon, 0, kNow);
    check_eq(q.discount_total, 0, "an expired coupon does not apply");
    check(q.rejected.size() == 1, "the customer is told why their coupon failed");
    check(q.rejected[0].reason_code == "COUPON_EXPIRED", "the reason is COUPON_EXPIRED");

    // A coupon the customer did NOT enter must not clutter the response.
    auto q2 = rs.calculate({line("sku_1", 1, 10000)}, Context{}, 0, kNow);
    check(q2.rejected.empty(), "unpresented coupons are not reported");
}

void test_inclusive_vs_exclusive_tax() {
    std::printf("\ntax\n");

    TaxRule vat{"EG", "VAT", "Egypt", 1400, true};    // inclusive, as in Egypt
    RuleSet rs_inclusive({}, {vat}, "test");

    Context eg;
    eg.country_code = "EG";
    auto q = rs_inclusive.calculate({line("sku_1", 1, 11400)}, eg, 0, kNow);

    check_eq(q.grand_total, 11400, "inclusive VAT does not add to the price");
    // 11400 * 1400 / 11400 = 1400
    check_eq(q.tax_lines[0].amount, 1400, "inclusive tax is extracted, not added");

    TaxRule sales{"US", "Sales Tax", "California", 875, false};
    RuleSet rs_exclusive({}, {sales}, "test");

    Context us;
    us.country_code = "US";
    auto q2 = rs_exclusive.calculate({line("sku_1", 1, 10000)}, us, 0, kNow);

    check_eq(q2.tax_total, 875, "exclusive tax is 8.75% added");
    check_eq(q2.grand_total, 10875, "the customer pays price plus tax");
}

void test_free_shipping() {
    std::printf("\nfree shipping\n");

    Rule r;
    r.id = "freeship";
    r.type = PromotionType::FreeShipping;
    r.conditions.push_back({Condition::Field::CartSubtotal, Condition::Op::Gte, 50000, {}});
    RuleSet rs({r}, {}, "test");

    auto over = rs.calculate({line("sku_1", 1, 60000)}, Context{}, 5000, kNow);
    check_eq(over.shipping_total, 0, "shipping is free over the threshold");
    check_eq(over.grand_total, 60000, "the total excludes shipping");

    auto under = rs.calculate({line("sku_1", 1, 10000)}, Context{}, 5000, kNow);
    check_eq(under.shipping_total, 5000, "shipping is charged under the threshold");
    check_eq(under.grand_total, 15000, "the total includes shipping");
}

void test_determinism() {
    std::printf("\ndeterminism\n");

    // The same inputs must give the same output, or order-service cannot
    // re-price an order at capture time (docs/CONTRACTS.md §4).
    RuleSet rs({percent("p1", 1700, 10), percent("p2", 300, 20)}, {}, "v1");
    const std::vector<CartLine> cart{line("sku_1", 3, 3333), line("sku_2", 7, 999)};

    auto a = rs.calculate(cart, Context{}, 1500, kNow);
    for (int i = 0; i < 100; ++i) {
        auto b = rs.calculate(cart, Context{}, 1500, kNow);
        if (a.grand_total != b.grand_total || a.discount_total != b.discount_total) {
            check(false, "100 identical calls produced the same total");
            return;
        }
    }
    check(true, "100 identical calls produced the same total");
    check(a.rules_version == "v1", "the rules version is echoed so the order can pin it");
}

void test_lines_sum_to_total() {
    std::printf("\nthe parts sum to the whole\n");

    RuleSet rs({percent("p1", 1234, 10)}, {}, "test");
    auto q = rs.calculate(
        {line("sku_1", 3, 3333), line("sku_2", 7, 999), line("sku_3", 1, 12345)},
        Context{}, 0, kNow);

    Minor sum = 0;
    for (const auto& lp : q.lines) sum += lp.line_total;

    // If these disagree, the customer sees line items that do not add up to
    // what they are charged — which is the single most reported pricing bug.
    check_eq(sum, q.grand_total, "line totals sum exactly to the grand total");
}

void test_empty_cart() {
    std::printf("\nedge cases\n");

    RuleSet rs({percent("p1", 2000)}, {}, "test");
    auto q = rs.calculate({}, Context{}, 0, kNow);

    check_eq(q.grand_total, 0, "an empty cart totals zero");
    check(q.lines.empty(), "an empty cart has no lines");

    auto free_item = rs.calculate({line("sku_1", 1, 0)}, Context{}, 0, kNow);
    check_eq(free_item.grand_total, 0, "a zero-price item does not break the maths");
}

int main() {
    std::printf("pricing rule engine\n");

    test_rounding();
    test_never_negative();
    test_percent_off();
    test_amount_off_is_spread_proportionally();
    test_amount_off_cannot_exceed_cart();
    test_max_discount_cap();
    test_exclusive_suppresses_lower_priority();
    test_buy_x_get_y();
    test_tiered_quantity_takes_the_best_tier();
    test_conditions();
    test_coupons();
    test_inclusive_vs_exclusive_tax();
    test_free_shipping();
    test_determinism();
    test_lines_sum_to_total();
    test_empty_cart();

    std::printf("\n%d checks, %d failures\n", g_checks, g_failures);
    return g_failures == 0 ? 0 : 1;
}
