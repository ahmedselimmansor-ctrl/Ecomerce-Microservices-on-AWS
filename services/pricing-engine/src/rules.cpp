#include "rules.hpp"

#include <algorithm>
#include <numeric>

namespace souq::pricing {

RuleSet::RuleSet(std::vector<Rule> rules, std::vector<TaxRule> taxes, std::string version)
    : rules_(std::move(rules)), taxes_(std::move(taxes)), version_(std::move(version)) {
    // Sorted once at load, not on every request. Rule order is load-bearing
    // (see the comment on Rule::priority) and re-sorting per cart would put an
    // O(n log n) on the hot path for a set that changes daily.
    std::stable_sort(rules_.begin(), rules_.end(),
                     [](const Rule& a, const Rule& b) { return a.priority < b.priority; });
}

std::string_view to_string(PromotionType t) noexcept {
    switch (t) {
        case PromotionType::PercentOff:     return "PERCENT_OFF";
        case PromotionType::AmountOff:      return "AMOUNT_OFF";
        case PromotionType::BuyXGetY:       return "BUY_X_GET_Y";
        case PromotionType::TieredQuantity: return "TIERED_QUANTITY";
        case PromotionType::Bundle:         return "BUNDLE";
        case PromotionType::FreeShipping:   return "FREE_SHIPPING";
        case PromotionType::CartThreshold:  return "CART_THRESHOLD";
    }
    return "UNKNOWN";
}

namespace {

bool contains(const std::vector<std::string>& haystack, std::string_view needle) {
    return std::find(haystack.begin(), haystack.end(), needle) != haystack.end();
}

bool any_line_matches(const std::vector<CartLine>& lines,
                      const std::vector<std::string>& values,
                      Condition::Field field) {
    for (const auto& l : lines) {
        switch (field) {
            case Condition::Field::Brand:
                if (contains(values, l.brand)) return true;
                break;
            case Condition::Field::Sku:
                if (contains(values, l.sku)) return true;
                break;
            case Condition::Field::CategoryPath:
                for (const auto& c : l.category_path) {
                    if (contains(values, c)) return true;
                }
                break;
            default:
                break;
        }
    }
    return false;
}

}  // namespace

bool RuleSet::condition_holds(const Condition& c,
                              const std::vector<CartLine>& lines,
                              const Context& ctx,
                              Minor subtotal) {
    using F = Condition::Field;
    using O = Condition::Op;

    switch (c.field) {
        case F::CartSubtotal:
            return c.op == O::Gte ? subtotal >= c.numeric
                 : c.op == O::Lte ? subtotal <= c.numeric
                 : subtotal == c.numeric;

        case F::CartQuantity: {
            const auto total = std::accumulate(
                lines.begin(), lines.end(), std::int64_t{0},
                [](std::int64_t acc, const CartLine& l) { return acc + l.quantity; });
            return c.op == O::Gte ? total >= c.numeric
                 : c.op == O::Lte ? total <= c.numeric
                 : total == c.numeric;
        }

        case F::LineQuantity:
            return std::any_of(lines.begin(), lines.end(), [&](const CartLine& l) {
                return c.op == O::Gte ? l.quantity >= c.numeric
                     : c.op == O::Lte ? l.quantity <= c.numeric
                     : l.quantity == c.numeric;
            });

        case F::Brand:
        case F::Sku:
        case F::CategoryPath: {
            const bool found = any_line_matches(lines, c.values, c.field);
            return c.op == O::NotIn ? !found : found;
        }

        case F::Segment: {
            const bool found = std::any_of(c.values.begin(), c.values.end(),
                                           [&](const std::string& v) { return contains(ctx.segments, v); });
            return c.op == O::NotIn ? !found : found;
        }

        case F::Channel: {
            const bool found = contains(c.values, ctx.channel);
            return c.op == O::NotIn ? !found : found;
        }

        case F::CountryCode: {
            const bool found = contains(c.values, ctx.country_code);
            return c.op == O::NotIn ? !found : found;
        }
    }
    return false;
}

bool RuleSet::matches(const Rule& r,
                      const std::vector<CartLine>& lines,
                      const Context& ctx,
                      Minor subtotal,
                      std::int64_t now,
                      std::string& reject_reason) const {
    if (!r.active) {
        reject_reason = "PROMOTION_INACTIVE";
        return false;
    }
    if (r.valid_from != 0 && now < r.valid_from) {
        reject_reason = "PROMOTION_NOT_STARTED";
        return false;
    }
    if (r.valid_until != 0 && now > r.valid_until) {
        reject_reason = "COUPON_EXPIRED";
        return false;
    }
    if (!r.coupon_code.empty() && !contains(ctx.coupons, r.coupon_code)) {
        // Not an error — the customer simply did not enter this code. Reported
        // only when they DID enter something that did not match.
        reject_reason = "COUPON_NOT_PRESENTED";
        return false;
    }

    for (const auto& c : r.conditions) {
        if (!condition_holds(c, lines, ctx, subtotal)) {
            switch (c.field) {
                case Condition::Field::CartSubtotal: reject_reason = "MIN_SPEND_NOT_MET"; break;
                case Condition::Field::Segment:      reject_reason = "SEGMENT_MISMATCH"; break;
                case Condition::Field::Channel:      reject_reason = "CHANNEL_MISMATCH"; break;
                case Condition::Field::CountryCode:  reject_reason = "COUNTRY_NOT_ELIGIBLE"; break;
                default:                             reject_reason = "CONDITIONS_NOT_MET"; break;
            }
            return false;
        }
    }
    return true;
}

Quote RuleSet::calculate(const std::vector<CartLine>& lines,
                         const Context& ctx,
                         Minor shipping_quote,
                         std::int64_t now) const {
    Quote q;
    q.rules_version = version_;
    q.shipping_total = shipping_quote;
    q.lines.reserve(lines.size());

    // ---- 1. list prices ----
    for (const auto& l : lines) {
        LinePrice lp;
        lp.sku = l.sku;
        lp.quantity = l.quantity;
        lp.unit_list_price = l.unit_list_price;
        lp.unit_effective_price = l.unit_list_price;
        lp.line_subtotal = l.unit_list_price * l.quantity;
        lp.line_total = lp.line_subtotal;
        q.subtotal += lp.line_subtotal;
        q.lines.push_back(std::move(lp));
    }

    // ---- 2. promotions, in priority order ----
    //
    // Discounts accumulate against the ORIGINAL subtotal, not against the
    // running total. Compounding percentages ("20% off, then 20% off the
    // result") is almost never what a merchant means, and it is very hard to
    // explain to a customer afterwards.
    const Minor original_subtotal = q.subtotal;
    bool exclusive_applied = false;

    for (const auto& r : rules_) {
        if (exclusive_applied) {
            q.rejected.push_back({r.id, r.coupon_code, "EXCLUDED_BY_EXCLUSIVE_PROMOTION", {}});
            continue;
        }

        std::string reason;
        if (!matches(r, lines, ctx, original_subtotal, now, reason)) {
            // Only surface rules the customer actually asked for. Reporting
            // every automatic rule they failed to qualify for would be a wall
            // of noise on every cart.
            if (!r.coupon_code.empty() && contains(ctx.coupons, r.coupon_code)) {
                q.rejected.push_back({r.id, r.coupon_code, reason, {}});
            }
            continue;
        }

        AppliedPromotion applied;
        applied.promotion_id = r.id;
        applied.name = r.name;
        applied.type = r.type;
        applied.coupon_code = r.coupon_code;
        applied.priority = r.priority;
        applied.exclusive = r.exclusive;
        Minor discount = 0;

        switch (r.type) {
            case PromotionType::PercentOff: {
                for (auto& lp : q.lines) {
                    const Minor off = apply_bp(lp.line_subtotal, r.percent_off);
                    // Never below zero per line. An intermediate negative
                    // would poison every percentage rule after this one.
                    const Minor applied_off = std::min(off, lp.line_total);
                    lp.line_discount -= applied_off;
                    lp.line_total = non_negative(lp.line_total - applied_off);
                    lp.applied_promotion_ids.push_back(r.id);
                    applied.applies_to_skus.push_back(lp.sku);
                    discount += applied_off;
                }
                break;
            }

            case PromotionType::AmountOff:
            case PromotionType::CartThreshold: {
                // Spread proportionally across lines so a partial refund later
                // can attribute the discount correctly. Distributing it all to
                // line 1 makes returns arithmetic wrong.
                Minor remaining = std::min(r.amount_off, q.subtotal);
                discount = remaining;

                for (std::size_t i = 0; i < q.lines.size() && remaining > 0; ++i) {
                    auto& lp = q.lines[i];
                    const bool last = (i + 1 == q.lines.size());
                    // The last line absorbs the rounding remainder, so the
                    // parts always sum exactly to the whole.
                    Minor share = last
                        ? remaining
                        : std::min(remaining, (r.amount_off * lp.line_subtotal) / std::max<Minor>(q.subtotal, 1));
                    share = std::min(share, lp.line_total);

                    lp.line_discount -= share;
                    lp.line_total = non_negative(lp.line_total - share);
                    if (share > 0) {
                        lp.applied_promotion_ids.push_back(r.id);
                        applied.applies_to_skus.push_back(lp.sku);
                    }
                    remaining -= share;
                }
                discount -= remaining;   // whatever could not be placed
                break;
            }

            case PromotionType::TieredQuantity: {
                for (auto& lp : q.lines) {
                    // Highest qualifying tier wins. Tiers are not additive.
                    BasisPoints best = 0;
                    for (const auto& t : r.tiers) {
                        if (lp.quantity >= t.min_quantity && t.discount_bp > best) {
                            best = t.discount_bp;
                        }
                    }
                    if (best == 0) continue;

                    const Minor off = std::min(apply_bp(lp.line_subtotal, best), lp.line_total);
                    lp.line_discount -= off;
                    lp.line_total = non_negative(lp.line_total - off);
                    lp.applied_promotion_ids.push_back(r.id);
                    applied.applies_to_skus.push_back(lp.sku);
                    discount += off;
                }
                break;
            }

            case PromotionType::BuyXGetY: {
                for (auto& lp : q.lines) {
                    if (r.buy_quantity <= 0 || lp.quantity < r.buy_quantity + r.get_quantity) continue;

                    // How many complete "buy X get Y" groups fit. Integer
                    // division on purpose: 7 items under buy-2-get-1 is two
                    // complete groups, not two and a third.
                    const std::int32_t groups = lp.quantity / (r.buy_quantity + r.get_quantity);
                    const std::int32_t free_units = groups * r.get_quantity;

                    const Minor off = std::min(
                        apply_bp(lp.unit_list_price * free_units, r.get_discount_bp),
                        lp.line_total);

                    lp.line_discount -= off;
                    lp.line_total = non_negative(lp.line_total - off);
                    lp.applied_promotion_ids.push_back(r.id);
                    applied.applies_to_skus.push_back(lp.sku);
                    discount += off;
                }
                break;
            }

            case PromotionType::FreeShipping: {
                discount = q.shipping_total;
                q.shipping_total = 0;
                break;
            }

            case PromotionType::Bundle:
                // Requires cross-line SKU matching; not implemented. Reported
                // rather than silently ignored, so a merchant who configures
                // one finds out immediately instead of wondering why it never
                // fires.
                q.rejected.push_back({r.id, r.coupon_code, "PROMOTION_TYPE_NOT_SUPPORTED",
                                      "bundle pricing is not implemented"});
                continue;
        }

        if (discount <= 0) continue;

        // The cap. A percentage rule with no cap on a high-value cart is how a
        // pricing bug becomes a five-figure loss before anyone notices.
        if (r.max_discount > 0 && discount > r.max_discount) {
            const Minor excess = discount - r.max_discount;
            discount = r.max_discount;
            // Give the excess back to the last line that received a discount,
            // so line totals still sum to the cart total.
            for (auto it = q.lines.rbegin(); it != q.lines.rend(); ++it) {
                if (it->line_discount < 0) {
                    const Minor giveback = std::min(excess, -it->line_discount);
                    it->line_discount += giveback;
                    it->line_total += giveback;
                    break;
                }
            }
        }

        applied.discount = -discount;
        q.discount_total -= discount;
        q.applied.push_back(std::move(applied));

        if (r.exclusive) exclusive_applied = true;
    }

    // ---- 3. recompute the subtotal from the lines ----
    // Derived, never carried forward, so a rounding slip inside a rule cannot
    // leave the parts disagreeing with the whole.
    Minor line_sum = 0;
    for (auto& lp : q.lines) {
        lp.line_total = non_negative(lp.line_total);
        lp.unit_effective_price = lp.quantity > 0 ? lp.line_total / lp.quantity : 0;
        line_sum += lp.line_total;
    }

    // ---- 4. tax ----
    const Minor taxable = line_sum + q.shipping_total;
    for (const auto& t : taxes_) {
        if (t.country_code != ctx.country_code) continue;

        TaxLine tl;
        tl.name = t.name;
        tl.jurisdiction = t.jurisdiction;
        tl.rate = t.rate;
        tl.inclusive = t.inclusive;

        if (t.inclusive) {
            // The tax is already inside the price. Extracting it:
            //   tax = gross * rate / (10000 + rate)
            // Reporting it as gross * rate would overstate the tax and make
            // the line items fail to sum to the total the customer pays.
            tl.amount = (taxable * t.rate) / (10000 + t.rate);
        } else {
            tl.amount = apply_bp(taxable, t.rate);
            q.tax_total += tl.amount;
        }
        q.tax_lines.push_back(std::move(tl));
    }

    // ---- 5. grand total ----
    q.subtotal = original_subtotal;
    q.grand_total = non_negative(line_sum + q.shipping_total + q.tax_total);

    return q;
}

}  // namespace souq::pricing
