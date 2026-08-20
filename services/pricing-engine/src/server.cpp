// gRPC server for the pricing rule engine.
//
// Thin by design: decode the request, call `RuleSet::calculate`, encode the
// answer. All the interesting logic — and all the tests — live in rules.cpp,
// which has no dependency on gRPC and compiles with a single g++ invocation.
//
// Two operational properties matter more here than anything about the protocol:
//
//   1. THE RULE SET IS SWAPPED ATOMICALLY. A reload builds a whole new RuleSet
//      and swaps a shared_ptr. A request that started under the old rules
//      finishes under the old rules; nothing ever sees a half-loaded set.
//
//   2. A BAD RULES FILE NEVER TAKES PRICING DOWN. Reload validates first and
//      keeps the current set on any error. The alternative — a typo in a
//      promotion emptying the rule set at 9am — is the failure this service
//      exists to avoid.

#include "rules.hpp"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <csignal>
#include <cstdlib>
#include <fstream>
#include <iostream>
#include <memory>
#include <mutex>
#include <set>
#include <sstream>
#include <string>
#include <thread>

#include <grpcpp/grpcpp.h>
#include <grpcpp/health_check_service_interface.h>
#include <grpcpp/ext/proto_server_reflection_plugin.h>

#include "souq/pricing/v1/pricing.grpc.pb.h"

namespace pricing = souq::pricing::v1;
namespace common = souq::common::v1;
using souq::pricing::RuleSet;

namespace {

// ---------------------------------------------------------------------------
// Structured logging.
//
// JSON to stdout, matching docs/CONTRACTS.md §9. Hand-rolled because pulling a
// logging library into a service whose entire job is arithmetic is not worth
// the dependency.

void logJson(const char* level, const std::string& msg,
             const std::string& extra = "") {
  const auto now = std::chrono::system_clock::now();
  const auto t = std::chrono::system_clock::to_time_t(now);
  const auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(
                      now.time_since_epoch()) % 1000;

  char stamp[32];
  std::tm tm{};
  gmtime_r(&t, &tm);
  std::strftime(stamp, sizeof(stamp), "%Y-%m-%dT%H:%M:%S", &tm);

  std::ostringstream out;
  out << R"({"timestamp":")" << stamp << '.'
      << (ms.count() < 100 ? (ms.count() < 10 ? "00" : "0") : "") << ms.count()
      << R"(Z","level":")" << level
      << R"(","service":"pricing-engine","msg":")" << msg << '"';
  if (!extra.empty()) out << ',' << extra;
  out << "}\n";

  std::cout << out.str() << std::flush;
}

// ---------------------------------------------------------------------------
// The live rule set.
//
// shared_ptr rather than a mutex around the RuleSet itself: readers take a
// copy of the pointer under a very short lock and then work without one, so a
// reload never blocks a request and a long request never blocks a reload.

class RuleSetHolder {
 public:
  std::shared_ptr<const RuleSet> current() const {
    std::lock_guard<std::mutex> lock(mu_);
    return set_;
  }

  // Returns false and leaves the current set untouched on any error.
  bool reload(const std::string& path, std::string& error) {
    std::ifstream file(path);
    if (!file) {
      error = "cannot open " + path;
      return false;
    }
    std::stringstream buffer;
    buffer << file.rdbuf();

    RuleSet candidate;
    if (!RuleSet::from_json(buffer.str(), candidate, error)) {
      return false;
    }

    auto next = std::make_shared<const RuleSet>(std::move(candidate));
    {
      std::lock_guard<std::mutex> lock(mu_);
      set_ = next;
    }
    logJson("info", "rule set loaded",
            R"("version":")" + next->version() + R"(","rules":)" +
                std::to_string(next->active_rule_count()));
    return true;
  }

 private:
  mutable std::mutex mu_;
  std::shared_ptr<const RuleSet> set_ = std::make_shared<const RuleSet>();
};

RuleSetHolder g_rules;
std::atomic<bool> g_reload_requested{false};
std::atomic<bool> g_shutdown_requested{false};

// Signal handlers do the minimum that is async-signal-safe: set a flag. The
// work happens on a normal thread.
void handleSignal(int sig) {
  if (sig == SIGHUP) g_reload_requested.store(true);
  else g_shutdown_requested.store(true);
}

// ---------------------------------------------------------------------------
// Conversions

// `Minor`, not `Money` — the engine has no Money type, deliberately. A money
// value on the wire is {amount, currency}; inside the engine the currency is
// carried once on the context and every amount is a bare integer, so a
// per-value currency cannot silently disagree with the cart's.
//
// This said `souq::pricing::Money` and therefore never compiled. Nothing
// caught it because the local test target builds rules.cpp and the two test
// files; server.cpp was only ever compiled by the Docker build, which was
// failing for an unrelated reason and so never got this far.
souq::pricing::Minor toMinor(const common::Money& m) { return m.amount(); }

void setMoney(common::Money* out, souq::pricing::Minor amount,
              const std::string& currency) {
  out->set_amount(amount);
  out->set_currency_code(currency);
}

souq::pricing::Context toContext(const pricing::PricingContext& in) {
  souq::pricing::Context ctx;
  ctx.user_id = in.user_id();
  ctx.currency = in.currency_code().empty() ? "EGP" : in.currency_code();
  ctx.country_code = in.country_code().empty() ? "EG" : in.country_code();
  ctx.channel = in.channel().empty() ? "web" : in.channel();
  for (const auto& s : in.customer_segments()) ctx.segments.push_back(s);
  for (const auto& c : in.coupon_codes()) ctx.coupons.push_back(c);
  return ctx;
}

std::vector<souq::pricing::CartLine> toLines(
    const google::protobuf::RepeatedPtrField<pricing::CartLine>& in) {
  std::vector<souq::pricing::CartLine> out;
  out.reserve(static_cast<std::size_t>(in.size()));

  for (const auto& l : in) {
    souq::pricing::CartLine line;
    line.sku = l.sku();
    line.product_id = l.product_id();
    line.quantity = l.quantity();
    line.unit_list_price = toMinor(l.unit_list_price());
    line.brand = l.brand();
    for (const auto& c : l.category_path()) line.category_path.push_back(c);
    for (const auto& [k, v] : l.attributes()) line.attributes[k] = v;
    out.push_back(std::move(line));
  }
  return out;
}

pricing::PromotionType toProto(souq::pricing::PromotionType t) {
  using P = souq::pricing::PromotionType;
  switch (t) {
    case P::PercentOff:     return pricing::PROMOTION_TYPE_PERCENT_OFF;
    case P::AmountOff:      return pricing::PROMOTION_TYPE_AMOUNT_OFF;
    case P::BuyXGetY:       return pricing::PROMOTION_TYPE_BUY_X_GET_Y;
    case P::TieredQuantity: return pricing::PROMOTION_TYPE_TIERED_QUANTITY;
    case P::Bundle:         return pricing::PROMOTION_TYPE_BUNDLE;
    case P::FreeShipping:   return pricing::PROMOTION_TYPE_FREE_SHIPPING;
    case P::CartThreshold:  return pricing::PROMOTION_TYPE_CART_THRESHOLD;
  }
  return pricing::PROMOTION_TYPE_UNSPECIFIED;
}

// ---------------------------------------------------------------------------

class PricingServiceImpl final : public pricing::PricingService::Service {
 public:
  grpc::Status CalculateCart(grpc::ServerContext* context,
                             const pricing::CalculateCartRequest* request,
                             pricing::CalculateCartResponse* response) override {
    const auto started = std::chrono::steady_clock::now();

    // The caller's deadline is 250ms (docs/CONTRACTS.md §5.4). If it has
    // already passed there is no point doing the work — the client has moved
    // on to its list-price fallback.
    if (context->IsCancelled()) {
      return grpc::Status(grpc::StatusCode::DEADLINE_EXCEEDED,
                          "deadline exceeded before evaluation started");
    }

    auto rules = g_rules.current();
    const auto lines = toLines(request->lines());
    const auto ctx = toContext(request->context());
    const auto shipping = toMinor(request->shipping_quote());

    // The clock is read ONCE and passed in, so the evaluation is deterministic
    // and an order can be re-priced identically at capture time.
    const auto now = std::chrono::duration_cast<std::chrono::seconds>(
                         std::chrono::system_clock::now().time_since_epoch())
                         .count();

    const auto quote = rules->calculate(lines, ctx, shipping, now);

    for (const auto& lp : quote.lines) {
      auto* out = response->add_lines();
      out->set_sku(lp.sku);
      out->set_quantity(lp.quantity);
      setMoney(out->mutable_unit_list_price(), lp.unit_list_price, ctx.currency);
      setMoney(out->mutable_unit_effective_price(), lp.unit_effective_price, ctx.currency);
      setMoney(out->mutable_line_subtotal(), lp.line_subtotal, ctx.currency);
      setMoney(out->mutable_line_discount(), lp.line_discount, ctx.currency);
      setMoney(out->mutable_line_total(), lp.line_total, ctx.currency);
      for (const auto& id : lp.applied_promotion_ids) out->add_applied_promotion_ids(id);
    }

    setMoney(response->mutable_subtotal(), quote.subtotal, ctx.currency);
    setMoney(response->mutable_discount_total(), quote.discount_total, ctx.currency);
    setMoney(response->mutable_shipping_total(), quote.shipping_total, ctx.currency);
    setMoney(response->mutable_tax_total(), quote.tax_total, ctx.currency);
    setMoney(response->mutable_grand_total(), quote.grand_total, ctx.currency);

    for (const auto& a : quote.applied) {
      auto* out = response->add_applied_promotions();
      out->set_promotion_id(a.promotion_id);
      out->set_name(a.name);
      out->set_type(toProto(a.type));
      out->set_coupon_code(a.coupon_code);
      out->set_priority(a.priority);
      out->set_exclusive(a.exclusive);
      setMoney(out->mutable_discount(), a.discount, ctx.currency);
      for (const auto& s : a.applies_to_skus) out->add_applies_to_skus(s);
    }

    // Rejections are returned, not swallowed. A customer who typed a coupon
    // deserves to know why it did not apply, and "reasonCode" is what the
    // storefront turns into a localised message.
    for (const auto& r : quote.rejected) {
      auto* out = response->add_rejected_promotions();
      out->set_promotion_id(r.promotion_id);
      out->set_coupon_code(r.coupon_code);
      out->set_reason_code(r.reason_code);
      out->set_detail(r.detail);
    }

    for (const auto& t : quote.tax_lines) {
      auto* out = response->add_tax_lines();
      out->set_name(t.name);
      out->set_jurisdiction(t.jurisdiction);
      out->mutable_rate()->set_value(t.rate);
      out->set_inclusive(t.inclusive);
      setMoney(out->mutable_amount(), t.amount, ctx.currency);
    }

    response->set_rules_version(quote.rules_version);
    response->set_degraded(quote.degraded);
    for (const auto& r : quote.degraded_reasons) response->add_degraded_reasons(r);

    const auto micros = std::chrono::duration_cast<std::chrono::microseconds>(
                            std::chrono::steady_clock::now() - started).count();
    // Logged at debug: this runs on every cart view and an info line per
    // request would drown every other log the service emits.
    logJson("debug", "cart priced",
            R"("lines":)" + std::to_string(quote.lines.size()) +
            R"(,"grandTotal":)" + std::to_string(quote.grand_total) +
            R"(,"micros":)" + std::to_string(micros));

    return grpc::Status::OK;
  }

  grpc::Status GetPrices(grpc::ServerContext*,
                         const pricing::GetPricesRequest* request,
                         pricing::GetPricesResponse* response) override {
    // Listing pages: effective unit price, no tax, no shipping, no coupons.
    //
    // The caller supplies the list prices, the same way CartLine does — see the
    // comment on SkuListPrice in the proto. Giving the engine a catalogue would
    // put a synchronous dependency inside the one service on the checkout hot
    // path with a sub-millisecond budget, and that is the property worth more
    // than the convenience.
    auto rules = g_rules.current();
    response->set_rules_version(rules->version());

    const auto currency = request->currency_code().empty()
                              ? std::string{"EGP"} : request->currency_code();

    souq::pricing::Context ctx;
    ctx.currency = currency;
    ctx.country_code = request->country_code().empty()
                           ? std::string{"EG"} : request->country_code();
    ctx.segments.assign(request->customer_segments().begin(),
                        request->customer_segments().end());
    // Deliberately no coupons and no user_id. A listing price must be the same
    // for everyone in a segment; a per-user price shown on a grid and then
    // recalculated at checkout is the complaint that becomes a chargeback.

    const auto now = std::chrono::duration_cast<std::chrono::seconds>(
                         std::chrono::system_clock::now().time_since_epoch()).count();

    // ONE calculate() call for the whole batch, not one per SKU.
    //
    // A listing page asks for a hundred SKUs. A hundred single-item carts would
    // be a hundred passes over the rule set, and — worse — cart-level rules
    // (spend thresholds, bundles) would each see a cart of one and behave
    // differently from how they will at checkout.
    //
    // Quantity 1 per line, so the result is a genuine unit price. Cart-scoped
    // promotions are then excluded below rather than being allowed to leak a
    // basket discount onto a grid tile.
    std::vector<souq::pricing::CartLine> lines;
    lines.reserve(static_cast<std::size_t>(request->items_size()));

    for (const auto& item : request->items()) {
      if (item.sku().empty()) {
        continue;   // nothing to key a response on
      }

      // A price in the wrong currency cannot be compared with the rest of the
      // batch, and silently pricing it would produce a grid mixing currencies.
      if (!item.list_price().currency_code().empty()
          && item.list_price().currency_code() != currency) {
        response->add_unknown_skus(item.sku());
        continue;
      }

      souq::pricing::CartLine line;
      line.sku = item.sku();
      line.product_id = item.product_id();
      line.quantity = 1;
      line.unit_list_price = item.list_price().amount();
      lines.push_back(std::move(line));
    }

    if (lines.empty()) {
      return grpc::Status::OK;
    }

    const auto quote = rules->calculate(lines, ctx, 0, now);

    // Promotions that only make sense across a basket. Applying them to a
    // single-item view would advertise a price the customer cannot actually
    // get, which is the kind of thing a consumer authority calls a misleading
    // price indication.
    std::set<std::string> cart_scoped;
    for (const auto& applied : quote.applied) {
      if (applied.type == souq::pricing::PromotionType::CartThreshold
          || applied.type == souq::pricing::PromotionType::FreeShipping
          || applied.type == souq::pricing::PromotionType::BuyXGetY
          || applied.type == souq::pricing::PromotionType::Bundle) {
        cart_scoped.insert(applied.promotion_id);
      }
    }

    for (const auto& priced : quote.lines) {
      const bool only_cart_scoped =
          !priced.applied_promotion_ids.empty()
          && std::all_of(priced.applied_promotion_ids.begin(),
                         priced.applied_promotion_ids.end(),
                         [&](const std::string& id) { return cart_scoped.count(id) > 0; });

      const souq::pricing::Minor effective =
          only_cart_scoped ? priced.unit_list_price : priced.unit_effective_price;

      auto* out = response->add_prices();
      out->set_sku(priced.sku);
      setMoney(out->mutable_list_price(), priced.unit_list_price, currency);
      setMoney(out->mutable_effective_price(), effective, currency);
      out->set_on_promotion(effective < priced.unit_list_price);

      // Basis points, computed from the two integers rather than carried from
      // the rule. A tiered or stacked discount has no single rate, and this is
      // the one the customer can verify against the two numbers on screen.
      if (priced.unit_list_price > 0 && effective < priced.unit_list_price) {
        const auto saved = priced.unit_list_price - effective;
        out->mutable_discount_rate()->set_value(static_cast<std::int32_t>(
            (saved * 10000) / priced.unit_list_price));
      }
    }

    return grpc::Status::OK;
  }

  grpc::Status ExplainPromotions(grpc::ServerContext*,
                                 const pricing::ExplainPromotionsRequest* request,
                                 pricing::ExplainPromotionsResponse* response) override {
    // Support and the admin dashboard only. Never on the hot path — it
    // evaluates every rule and records every predicate.
    auto rules = g_rules.current();
    response->set_rules_version(rules->version());

    const auto lines = toLines(request->lines());
    const auto ctx = toContext(request->context());
    const auto now = std::chrono::duration_cast<std::chrono::seconds>(
                         std::chrono::system_clock::now().time_since_epoch()).count();

    const auto quote = rules->calculate(lines, ctx, 0, now);

    for (const auto& a : quote.applied) {
      auto* trace = response->add_traces();
      trace->set_promotion_id(a.promotion_id);
      trace->set_matched(true);
      setMoney(trace->mutable_would_discount(), a.discount, ctx.currency);
    }
    for (const auto& r : quote.rejected) {
      auto* trace = response->add_traces();
      trace->set_promotion_id(r.promotion_id);
      trace->set_matched(false);
      auto* cond = trace->add_conditions();
      cond->set_expression(r.reason_code);
      cond->set_satisfied(false);
      cond->set_actual_value(r.detail);
    }
    return grpc::Status::OK;
  }

  grpc::Status GetRulesVersion(grpc::ServerContext*,
                               const pricing::GetRulesVersionRequest*,
                               pricing::GetRulesVersionResponse* response) override {
    auto rules = g_rules.current();
    response->set_rules_version(rules->version());
    response->set_active_rule_count(static_cast<int32_t>(rules->active_rule_count()));
    return grpc::Status::OK;
  }
};

std::string env(const char* key, const char* fallback) {
  const char* v = std::getenv(key);
  return (v && *v) ? std::string(v) : std::string(fallback);
}

}  // namespace

int main() {
  const std::string grpcAddr = env("SOUQ_GRPC_ADDR", "0.0.0.0:9089");
  const std::string rulesPath = env("SOUQ_RULES_PATH", "/etc/souq/rules.json");

  std::signal(SIGHUP, handleSignal);
  std::signal(SIGTERM, handleSignal);
  std::signal(SIGINT, handleSignal);

  std::string error;
  if (!g_rules.reload(rulesPath, error)) {
    // Refusing to start is correct. Serving with an empty rule set means every
    // customer silently loses every promotion, which is worse than an outage
    // because nobody notices for hours.
    logJson("error", "cannot load the rule set; refusing to start",
            R"("path":")" + rulesPath + R"(","error":")" + error + R"(")");
    return 1;
  }

  grpc::EnableDefaultHealthCheckService(true);
  grpc::reflection::InitProtoReflectionServerBuilderPlugin();

  PricingServiceImpl service;
  grpc::ServerBuilder builder;

  builder.AddListeningPort(grpcAddr, grpc::InsecureServerCredentials());
  builder.RegisterService(&service);

  // Bounded so a struggling caller cannot make this service queue unboundedly
  // and run out of memory. The bulkhead from docs/CONTRACTS.md §5.4.
  builder.SetMaxReceiveMessageSize(4 * 1024 * 1024);
  builder.AddChannelArgument(GRPC_ARG_MAX_CONCURRENT_STREAMS, 200);
  builder.AddChannelArgument(GRPC_ARG_KEEPALIVE_TIME_MS, 30000);
  builder.AddChannelArgument(GRPC_ARG_KEEPALIVE_TIMEOUT_MS, 5000);

  auto server = builder.BuildAndStart();
  if (!server) {
    logJson("error", "failed to bind", R"("addr":")" + grpcAddr + R"(")");
    return 1;
  }

  logJson("info", "listening", R"("grpc":")" + grpcAddr + R"(")");

  // The main thread watches the flags rather than blocking in server->Wait(),
  // so SIGHUP can swap the rule set without a restart.
  while (!g_shutdown_requested.load()) {
    if (g_reload_requested.exchange(false)) {
      std::string reloadError;
      if (!g_rules.reload(rulesPath, reloadError)) {
        // Keep serving the rules we already have. A bad file must never take
        // pricing down.
        logJson("error", "rule set reload failed; keeping the current set",
                R"("error":")" + reloadError + R"(")");
      }
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(200));
  }

  logJson("info", "shutdown signal received; draining");
  // Deadline rather than an unbounded drain: Kubernetes will SIGKILL at the
  // grace period anyway, and finishing cleanly beforehand is better than
  // being killed mid-response.
  server->Shutdown(std::chrono::system_clock::now() + std::chrono::seconds(10));
  server->Wait();
  logJson("info", "stopped");
  return 0;
}
