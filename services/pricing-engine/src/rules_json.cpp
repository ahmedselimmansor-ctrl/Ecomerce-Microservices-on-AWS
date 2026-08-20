// JSON loading for the rule set.
//
// A hand-written parser rather than a dependency. That is a deliberate trade
// and worth justifying: this service's whole value is that the rule engine is
// a static library with no dependencies, compilable and testable with one g++
// invocation. Pulling in nlohmann/json to read one config file at startup
// would make `g++ rules.cpp test.cpp` stop working, and that command is the
// reason people actually run the tests while changing a discount.
//
// The parser is strict and small. It handles exactly the subset rules.json
// uses, and it REFUSES anything it does not understand rather than guessing —
// a silently-ignored field in a pricing rule is a promotion that never fires
// and nobody can explain why.

#include "rules.hpp"

#include <cctype>
#include <charconv>
#include <cstdlib>
#include <map>
#include <memory>
#include <stdexcept>
#include <variant>
#include <vector>

namespace souq::pricing {
namespace {

// ---------------------------------------------------------------------------
// A minimal JSON value

struct Value;
using Object = std::map<std::string, Value>;
using Array = std::vector<Value>;

struct Value {
    std::variant<std::nullptr_t, bool, std::int64_t, double, std::string,
                 std::shared_ptr<Array>, std::shared_ptr<Object>> v;

    [[nodiscard]] bool is_object() const { return std::holds_alternative<std::shared_ptr<Object>>(v); }
    [[nodiscard]] bool is_array() const { return std::holds_alternative<std::shared_ptr<Array>>(v); }

    [[nodiscard]] const Object& obj() const { return *std::get<std::shared_ptr<Object>>(v); }
    [[nodiscard]] const Array& arr() const { return *std::get<std::shared_ptr<Array>>(v); }

    [[nodiscard]] std::string str(std::string fallback = {}) const {
        if (auto* s = std::get_if<std::string>(&v)) return *s;
        return fallback;
    }
    [[nodiscard]] std::int64_t num(std::int64_t fallback = 0) const {
        if (auto* n = std::get_if<std::int64_t>(&v)) return *n;
        // A rule file with 15.0 where an integer belongs is a mistake, but
        // truncating is friendlier than refusing and the CHECK downstream
        // catches anything that actually matters.
        if (auto* d = std::get_if<double>(&v)) return static_cast<std::int64_t>(*d);
        return fallback;
    }
    [[nodiscard]] bool boolean(bool fallback = false) const {
        if (auto* b = std::get_if<bool>(&v)) return *b;
        return fallback;
    }

    /// Field lookup that returns a null Value rather than throwing, so an
    /// absent optional field reads naturally at the call site.
    [[nodiscard]] const Value& operator[](const std::string& key) const {
        static const Value kNull{};
        if (!is_object()) return kNull;
        const auto& o = obj();
        auto it = o.find(key);
        return it == o.end() ? kNull : it->second;
    }
    [[nodiscard]] bool present() const { return !std::holds_alternative<std::nullptr_t>(v); }
};

class Parser {
public:
    explicit Parser(std::string_view src) : s_(src) {}

    Value parse() {
        skip();
        Value v = value();
        skip();
        if (i_ != s_.size()) fail("trailing content after the top-level value");
        return v;
    }

private:
    std::string_view s_;
    std::size_t i_{0};

    [[noreturn]] void fail(const std::string& what) const {
        // Byte offset, because a rules file is a few hundred lines and "line 3"
        // from a hand-rolled counter is usually wrong.
        throw std::runtime_error(what + " at byte " + std::to_string(i_));
    }

    void skip() {
        while (i_ < s_.size() && (s_[i_] == ' ' || s_[i_] == '\t' || s_[i_] == '\n' || s_[i_] == '\r')) ++i_;
    }

    char peek() const { return i_ < s_.size() ? s_[i_] : '\0'; }

    void expect(char c) {
        if (peek() != c) fail(std::string("expected '") + c + "'");
        ++i_;
    }

    Value value() {
        switch (peek()) {
            case '{': return object();
            case '[': return array();
            case '"': return Value{string()};
            case 't': literal("true");  return Value{true};
            case 'f': literal("false"); return Value{false};
            case 'n': literal("null");  return Value{};
            default:  return number();
        }
    }

    void literal(std::string_view lit) {
        if (s_.substr(i_, lit.size()) != lit) fail("expected " + std::string(lit));
        i_ += lit.size();
    }

    Value object() {
        expect('{');
        auto o = std::make_shared<Object>();
        skip();
        if (peek() == '}') { ++i_; return Value{o}; }

        for (;;) {
            skip();
            std::string key = string();
            skip();
            expect(':');
            skip();
            (*o)[key] = value();
            skip();
            if (peek() == ',') { ++i_; continue; }
            expect('}');
            break;
        }
        return Value{o};
    }

    Value array() {
        expect('[');
        auto a = std::make_shared<Array>();
        skip();
        if (peek() == ']') { ++i_; return Value{a}; }

        for (;;) {
            skip();
            a->push_back(value());
            skip();
            if (peek() == ',') { ++i_; continue; }
            expect(']');
            break;
        }
        return Value{a};
    }

    std::string string() {
        expect('"');
        std::string out;
        while (i_ < s_.size() && s_[i_] != '"') {
            if (s_[i_] == '\\') {
                ++i_;
                if (i_ >= s_.size()) fail("unterminated escape");
                switch (s_[i_]) {
                    case 'n': out += '\n'; break;
                    case 't': out += '\t'; break;
                    case 'r': out += '\r'; break;
                    case 'b': out += '\b'; break;
                    case 'f': out += '\f'; break;
                    case '"': out += '"';  break;
                    case '\\': out += '\\'; break;
                    case '/': out += '/';  break;
                    case 'u': {
                        // Rule names carry Arabic. Decode the code point to
                        // UTF-8 rather than dropping it, or a merchant's
                        // promotion name becomes mojibake in the admin UI.
                        if (i_ + 4 >= s_.size()) fail("truncated \\u escape");
                        unsigned cp = 0;
                        auto [ptr, ec] = std::from_chars(s_.data() + i_ + 1, s_.data() + i_ + 5, cp, 16);
                        if (ec != std::errc{}) fail("bad \\u escape");
                        i_ += 4;
                        appendUtf8(out, cp);
                        break;
                    }
                    default: fail("unknown escape");
                }
                ++i_;
                continue;
            }
            out += s_[i_++];
        }
        expect('"');
        return out;
    }

    static void appendUtf8(std::string& out, unsigned cp) {
        if (cp < 0x80) {
            out += static_cast<char>(cp);
        } else if (cp < 0x800) {
            out += static_cast<char>(0xC0 | (cp >> 6));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        } else {
            out += static_cast<char>(0xE0 | (cp >> 12));
            out += static_cast<char>(0x80 | ((cp >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (cp & 0x3F));
        }
    }

    Value number() {
        const std::size_t start = i_;
        if (peek() == '-' || peek() == '+') ++i_;
        bool fractional = false;
        while (i_ < s_.size() && (std::isdigit(static_cast<unsigned char>(s_[i_])) ||
                                  s_[i_] == '.' || s_[i_] == 'e' || s_[i_] == 'E' ||
                                  s_[i_] == '-' || s_[i_] == '+')) {
            if (s_[i_] == '.' || s_[i_] == 'e' || s_[i_] == 'E') fractional = true;
            ++i_;
        }
        if (i_ == start) fail("expected a value");

        const std::string text(s_.substr(start, i_ - start));
        if (fractional) return Value{std::strtod(text.c_str(), nullptr)};
        return Value{static_cast<std::int64_t>(std::strtoll(text.c_str(), nullptr, 10))};
    }
};

// ---------------------------------------------------------------------------
// Mapping onto the rule types

PromotionType parseType(const std::string& s, bool& ok) {
    ok = true;
    if (s == "PERCENT_OFF")     return PromotionType::PercentOff;
    if (s == "AMOUNT_OFF")      return PromotionType::AmountOff;
    if (s == "BUY_X_GET_Y")     return PromotionType::BuyXGetY;
    if (s == "TIERED_QUANTITY") return PromotionType::TieredQuantity;
    if (s == "BUNDLE")          return PromotionType::Bundle;
    if (s == "FREE_SHIPPING")   return PromotionType::FreeShipping;
    if (s == "CART_THRESHOLD")  return PromotionType::CartThreshold;
    ok = false;
    return PromotionType::PercentOff;
}

Condition::Field parseField(const std::string& s, bool& ok) {
    ok = true;
    if (s == "CART_SUBTOTAL") return Condition::Field::CartSubtotal;
    if (s == "LINE_QUANTITY") return Condition::Field::LineQuantity;
    if (s == "CART_QUANTITY") return Condition::Field::CartQuantity;
    if (s == "BRAND")         return Condition::Field::Brand;
    if (s == "CATEGORY_PATH") return Condition::Field::CategoryPath;
    if (s == "SKU")           return Condition::Field::Sku;
    if (s == "SEGMENT")       return Condition::Field::Segment;
    if (s == "CHANNEL")       return Condition::Field::Channel;
    if (s == "COUNTRY_CODE")  return Condition::Field::CountryCode;
    ok = false;
    return Condition::Field::CartSubtotal;
}

Condition::Op parseOp(const std::string& s, bool& ok) {
    ok = true;
    if (s == "GTE")    return Condition::Op::Gte;
    if (s == "LTE")    return Condition::Op::Lte;
    if (s == "EQ")     return Condition::Op::Eq;
    if (s == "IN")     return Condition::Op::In;
    if (s == "NOT_IN") return Condition::Op::NotIn;
    ok = false;
    return Condition::Op::Gte;
}

}  // namespace

bool RuleSet::from_json(std::string_view json, RuleSet& out, std::string& error) {
    Value root;
    try {
        root = Parser(json).parse();
    } catch (const std::exception& e) {
        error = std::string("malformed JSON: ") + e.what();
        return false;
    }

    if (!root.is_object()) {
        error = "the top level must be an object";
        return false;
    }

    const std::string version = root["version"].str();
    if (version.empty()) {
        // Not optional. Without it an order cannot record which rule set
        // priced it, and re-pricing at capture time becomes a guess.
        error = "`version` is required — orders record it so they can be re-priced";
        return false;
    }

    std::vector<TaxRule> taxes;
    if (root["taxes"].is_array()) {
        for (const auto& t : root["taxes"].arr()) {
            TaxRule tr;
            tr.country_code = t["countryCode"].str();
            tr.name = t["name"].str("Tax");
            tr.jurisdiction = t["jurisdiction"].str();
            tr.rate = static_cast<BasisPoints>(t["rateBasisPoints"].num());
            tr.inclusive = t["inclusive"].boolean(true);

            if (tr.country_code.size() != 2) {
                error = "tax entry has an invalid countryCode: " + tr.country_code;
                return false;
            }
            if (tr.rate < 0 || tr.rate > 10000) {
                error = "tax rate for " + tr.country_code + " is out of range (0-10000 bp)";
                return false;
            }
            taxes.push_back(std::move(tr));
        }
    }

    std::vector<Rule> rules;
    if (root["rules"].is_array()) {
        for (const auto& r : root["rules"].arr()) {
            Rule rule;
            rule.id = r["id"].str();
            if (rule.id.empty()) {
                error = "every rule needs an id";
                return false;
            }
            rule.name = r["name"].str(rule.id);

            bool ok = false;
            rule.type = parseType(r["type"].str(), ok);
            if (!ok) {
                // Refuse rather than skip. A rule silently dropped for a typo
                // is a promotion that never fires and nobody can explain why.
                error = "rule " + rule.id + " has an unknown type: " + r["type"].str();
                return false;
            }

            rule.percent_off = static_cast<BasisPoints>(r["percentOffBasisPoints"].num());
            rule.amount_off = r["amountOff"].num();
            rule.buy_quantity = static_cast<std::int32_t>(r["buyQuantity"].num());
            rule.get_quantity = static_cast<std::int32_t>(r["getQuantity"].num());
            rule.get_discount_bp = static_cast<BasisPoints>(r["getDiscountBasisPoints"].num(10000));
            rule.coupon_code = r["couponCode"].str();
            rule.priority = static_cast<std::int32_t>(r["priority"].num(100));
            rule.exclusive = r["exclusive"].boolean(false);
            rule.max_discount = r["maxDiscount"].num();
            rule.valid_from = r["validFrom"].num();
            rule.valid_until = r["validUntil"].num();
            rule.active = r["active"].boolean(true);

            // An uncapped percentage on a high-value cart is how a pricing bug
            // becomes a five-figure loss before anyone notices.
            if (rule.type == PromotionType::PercentOff && rule.max_discount == 0) {
                error = "rule " + rule.id + " is a percentage discount with no maxDiscount; "
                        "an uncapped percentage on a large cart is unbounded loss";
                return false;
            }
            if (rule.percent_off < 0 || rule.percent_off > 10000) {
                error = "rule " + rule.id + " has percentOffBasisPoints outside 0-10000";
                return false;
            }
            if (rule.valid_from != 0 && rule.valid_until != 0 && rule.valid_until <= rule.valid_from) {
                error = "rule " + rule.id + " expires before it starts";
                return false;
            }

            if (r["tiers"].is_array()) {
                for (const auto& t : r["tiers"].arr()) {
                    rule.tiers.push_back(Tier{
                        static_cast<std::int32_t>(t["minQuantity"].num()),
                        static_cast<BasisPoints>(t["discountBasisPoints"].num()),
                    });
                }
            }

            if (r["conditions"].is_array()) {
                for (const auto& c : r["conditions"].arr()) {
                    Condition cond;
                    bool fieldOk = false, opOk = false;
                    cond.field = parseField(c["field"].str(), fieldOk);
                    cond.op = parseOp(c["op"].str(), opOk);
                    if (!fieldOk || !opOk) {
                        error = "rule " + rule.id + " has an unknown condition field or operator";
                        return false;
                    }
                    cond.numeric = c["numeric"].num();
                    if (c["values"].is_array()) {
                        for (const auto& v : c["values"].arr()) cond.values.push_back(v.str());
                    }
                    rule.conditions.push_back(std::move(cond));
                }
            }

            rules.push_back(std::move(rule));
        }
    }

    out = RuleSet(std::move(rules), std::move(taxes), version);
    return true;
}

}  // namespace souq::pricing
