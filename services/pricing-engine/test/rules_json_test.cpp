// Tests for rule-set loading.
//
// The property that matters most: a bad rules file must be REJECTED, not
// partially loaded. A rule silently dropped for a typo is a promotion that
// never fires and nobody can explain why, and an uncapped percentage is
// unbounded loss.

#include "../src/rules.hpp"

#include <cstdio>
#include <fstream>
#include <sstream>
#include <string>

using namespace souq::pricing;

namespace {
int g_failures = 0;
int g_checks = 0;

void check(bool ok, const std::string& what) {
    ++g_checks;
    if (ok) std::printf("  \033[0;32mok\033[0m   %s\n", what.c_str());
    else { ++g_failures; std::printf("  \033[0;31mFAIL\033[0m %s\n", what.c_str()); }
}

bool loads(const std::string& json, RuleSet& out, std::string& err) {
    return RuleSet::from_json(json, out, err);
}

void rejects(const std::string& json, const std::string& what) {
    RuleSet rs;
    std::string err;
    const bool ok = loads(json, rs, err);
    ++g_checks;
    if (!ok) std::printf("  \033[0;32mok\033[0m   rejects %s \033[2m(%s)\033[0m\n", what.c_str(), err.c_str());
    else { ++g_failures; std::printf("  \033[0;31mFAIL\033[0m ACCEPTED %s\n", what.c_str()); }
}
}  // namespace

int main() {
    std::printf("rule set loading\n\n");

    // --- the real file ------------------------------------------------------
    {
        std::ifstream f("rules/rules.json");
        if (f) {
            std::stringstream buf;
            buf << f.rdbuf();
            RuleSet rs;
            std::string err;
            const bool ok = loads(buf.str(), rs, err);
            check(ok, "the shipped rules/rules.json loads" + std::string(ok ? "" : ": " + err));
            if (ok) {
                check(rs.version() == "rules-2026-08-01", "version is read: " + rs.version());
                check(rs.active_rule_count() == 7, "7 rules loaded");

                // End to end: a VIP cart must get the exclusive 15% and
                // nothing else.
                Context ctx;
                ctx.segments = {"vip"};
                ctx.country_code = "EG";
                CartLine line;
                line.sku = "sku_1";
                line.quantity = 1;
                line.unit_list_price = 100000;
                line.category_path = {"audio"};

                const auto q = rs.calculate({line}, ctx, 5000, 1793000000);
                check(q.discount_total == -15000, "VIP gets 15% off (" + std::to_string(q.discount_total) + ")");
                check(q.applied.size() == 1, "the exclusive rule suppressed the rest");
                check(q.rules_version == "rules-2026-08-01", "the quote echoes the rule set version");
                check(!q.tax_lines.empty() && q.tax_lines[0].inclusive,
                      "Egyptian VAT is applied inclusively");
            }
        } else {
            std::printf("  \033[2mskip  rules/rules.json not found (run from the service directory)\033[0m\n");
        }
    }

    std::printf("\nvalidation\n\n");

    rejects(R"({"rules":[]})", "a file with no version");
    rejects(R"({"version":"v1","rules":[{"name":"x","type":"PERCENT_OFF"}]})", "a rule with no id");
    rejects(R"({"version":"v1","rules":[{"id":"r","type":"NONSENSE"}]})", "an unknown promotion type");
    rejects(R"({"version":"v1","rules":[{"id":"r","type":"PERCENT_OFF","percentOffBasisPoints":1000}]})",
            "an UNCAPPED percentage discount");
    rejects(R"({"version":"v1","rules":[{"id":"r","type":"PERCENT_OFF","percentOffBasisPoints":20000,"maxDiscount":1}]})",
            "a percentage above 100%");
    rejects(R"({"version":"v1","rules":[{"id":"r","type":"AMOUNT_OFF","validFrom":200,"validUntil":100}]})",
            "a rule that expires before it starts");
    rejects(R"({"version":"v1","rules":[{"id":"r","type":"AMOUNT_OFF","conditions":[{"field":"MOON_PHASE","op":"EQ"}]}]})",
            "an unknown condition field");
    rejects(R"({"version":"v1","taxes":[{"countryCode":"EGY","rateBasisPoints":1400}]})",
            "a three-letter country code");
    rejects(R"({"version":"v1","taxes":[{"countryCode":"EG","rateBasisPoints":99999}]})",
            "a tax rate above 100%");
    rejects("{ this is not json", "malformed JSON");
    rejects(R"(["not","an","object"])", "a top-level array");

    std::printf("\nparser\n\n");
    {
        // Rule names carry Arabic; a dropped code point becomes mojibake in
        // the admin UI.
        RuleSet rs;
        std::string err;
        const bool ok = loads(
            R"({"version":"v1","rules":[{"id":"r","name":"خصم","type":"AMOUNT_OFF","amountOff":100}]})",
            rs, err);
        check(ok, "a \\u escape sequence parses" + std::string(ok ? "" : ": " + err));
    }
    {
        RuleSet rs;
        std::string err;
        check(loads(R"({"version":"v1","rules":[],"taxes":[]})", rs, err), "empty arrays are fine");
        check(rs.active_rule_count() == 0, "an empty rule set loads with zero rules");
    }
    {
        RuleSet rs;
        std::string err;
        check(loads(R"({"version":"v1","_comment":["ignored"],"rules":[]})", rs, err),
              "unknown top-level keys are ignored (comments)");
    }

    std::printf("\n%d checks, %d failures\n", g_checks, g_failures);
    return g_failures == 0 ? 0 : 1;
}
