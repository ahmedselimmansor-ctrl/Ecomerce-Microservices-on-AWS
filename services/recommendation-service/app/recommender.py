"""Amazon Personalize, with a fallback that is not an afterthought.

Personalize is genuinely good once it has data. The problem is everything
before and around that:

* **Cold start.** A new user has no interaction history and a new product has
  no interactions at all. Personalize returns something, but for a brand-new
  user it is close to a popularity ranking with extra latency.
* **Latency.** A campaign call is 20-80ms p50 but has a long tail, and it sits
  on the home page render path.
* **Availability.** A campaign being updated, a throttle, or a bad ARN after a
  deploy all produce the same outcome: no recommendations.
* **Cost.** Campaigns bill per provisioned TPS, and the home page is the most
  requested page on the site.

So the design here is: **cache aggressively, fall back always, and be honest
about which one the customer got.** ``RecommendationResponse.fallback`` is in
the contract precisely so the storefront can drop the "Recommended for you"
heading rather than claiming personalisation that did not happen.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import time
from dataclasses import dataclass
from enum import Enum
from typing import Any

logger = logging.getLogger(__name__)


class Placement(str, Enum):
    """Each placement maps to one Personalize campaign and one fallback."""

    HOME_FOR_YOU = "home_for_you"
    PDP_SIMILAR = "pdp_similar"
    PDP_FBT = "pdp_frequently_bought_together"
    CART_UPSELL = "cart_upsell"
    SEARCH_RERANKED = "search_reranked"
    EMAIL_WINBACK = "email_winback"


@dataclass(frozen=True)
class CampaignConfig:
    arn: str
    #: Seconds. Home recommendations change slowly; cart upsell must reflect
    #: what is in the cart right now, so it is barely cached at all.
    cache_ttl: int
    #: Personalize deadline. Beyond this the fallback is strictly better than
    #: making the customer wait.
    timeout_ms: int
    #: What to serve when Personalize cannot answer.
    fallback: str


CAMPAIGNS: dict[Placement, CampaignConfig] = {
    Placement.HOME_FOR_YOU: CampaignConfig("", 900, 300, "bestsellers"),
    Placement.PDP_SIMILAR: CampaignConfig("", 3600, 250, "same_category"),
    Placement.PDP_FBT: CampaignConfig("", 3600, 250, "same_category"),
    # 60s, because a stale cart upsell recommends something already in the cart.
    Placement.CART_UPSELL: CampaignConfig("", 60, 200, "accessories"),
    Placement.SEARCH_RERANKED: CampaignConfig("", 300, 150, "identity"),
    # Off the request path entirely, so it can afford to wait.
    Placement.EMAIL_WINBACK: CampaignConfig("", 86400, 2000, "bestsellers"),
}


@dataclass
class Recommendation:
    product_id: str
    score: float | None
    reason: str | None


@dataclass
class Result:
    recommendation_id: str
    placement: str
    items: list[Recommendation]
    fallback: bool
    fallback_reason: str | None


class Recommender:
    def __init__(self, personalize, redis, catalog, *, fallback_only: bool = False):
        self._personalize = personalize
        self._redis = redis
        self._catalog = catalog
        # Set locally, where there is no Personalize. That is deliberate: the
        # fallback IS the local behaviour, so it is exercised every day rather
        # than only during an incident.
        self._fallback_only = fallback_only

    async def recommend(
        self,
        placement: Placement,
        *,
        user_id: str | None = None,
        item_id: str | None = None,
        item_ids: list[str] | None = None,
        count: int = 10,
        context: dict[str, str] | None = None,
    ) -> Result:
        config = CAMPAIGNS[placement]
        cache_key = self._cache_key(placement, user_id, item_id, item_ids, count, context)

        cached = await self._read_cache(cache_key)
        if cached is not None:
            return cached

        if self._fallback_only or not config.arn:
            result = await self._fallback(placement, config, user_id, item_id, count,
                                          reason="personalize_not_configured")
            await self._write_cache(cache_key, result, config.cache_ttl)
            return result

        try:
            items = await asyncio.wait_for(
                self._call_personalize(config, user_id, item_id, item_ids, count, context),
                timeout=config.timeout_ms / 1000,
            )
        except asyncio.TimeoutError:
            logger.warning("personalize timed out for %s after %dms", placement, config.timeout_ms)
            result = await self._fallback(placement, config, user_id, item_id, count, reason="timeout")
            # Cache the fallback too, briefly. Otherwise a Personalize outage
            # means every single request pays the full timeout before falling
            # back, and the home page becomes unusably slow rather than merely
            # unpersonalised.
            await self._write_cache(cache_key, result, min(config.cache_ttl, 60))
            return result
        except Exception as exc:
            logger.warning("personalize failed for %s: %s", placement, exc)
            result = await self._fallback(placement, config, user_id, item_id, count,
                                          reason=type(exc).__name__)
            await self._write_cache(cache_key, result, min(config.cache_ttl, 60))
            return result

        # Personalize answered, but a cold-start user gets back a handful of
        # low-confidence items. Serving three recommendations under a
        # "Recommended for you" heading looks broken; top up from the fallback
        # and say so.
        if len(items) < count // 2:
            logger.info("personalize returned %d/%d for %s; topping up from the fallback",
                        len(items), count, placement)
            topped = await self._fallback(placement, config, user_id, item_id, count,
                                          reason="insufficient_personalized_results")
            seen = {i.product_id for i in items}
            items.extend(i for i in topped.items if i.product_id not in seen)
            items = items[:count]

            result = Result(
                recommendation_id=self._new_recommendation_id(placement, user_id),
                placement=placement.value, items=items,
                fallback=True, fallback_reason="cold_start_topped_up",
            )
            await self._write_cache(cache_key, result, config.cache_ttl)
            return result

        result = Result(
            recommendation_id=self._new_recommendation_id(placement, user_id),
            placement=placement.value, items=items,
            fallback=False, fallback_reason=None,
        )
        await self._write_cache(cache_key, result, config.cache_ttl)
        return result

    # ------------------------------------------------------------------ AWS

    async def _call_personalize(
        self, config: CampaignConfig, user_id: str | None, item_id: str | None,
        item_ids: list[str] | None, count: int, context: dict[str, str] | None,
    ) -> list[Recommendation]:
        params: dict[str, Any] = {"campaignArn": config.arn, "numResults": count}
        if user_id:
            params["userId"] = user_id
        if item_id:
            params["itemId"] = item_id
        if item_ids:
            # Personalized ranking: Personalize reorders OUR candidate list
            # rather than choosing from the whole catalogue. Used for search
            # reranking, where the candidates come from Elasticsearch.
            params["inputList"] = item_ids
        if context:
            # Contextual metadata (device, time of day) measurably improves
            # recommendations, but only if the model was trained with the same
            # keys — an unknown key is silently ignored.
            params["context"] = context

        loop = asyncio.get_running_loop()
        # boto3 is synchronous. Running it on the default executor keeps the
        # event loop free; blocking it here would stall every other request.
        response = await loop.run_in_executor(
            None,
            lambda: (
                self._personalize.get_personalized_ranking(**params)
                if item_ids else self._personalize.get_recommendations(**params)
            ),
        )

        key = "personalizedRanking" if item_ids else "itemList"
        return [
            Recommendation(
                product_id=i["itemId"],
                score=i.get("score"),
                reason=None,
            )
            for i in response.get(key, [])
        ]

    # ------------------------------------------------------------- fallbacks

    async def _fallback(
        self, placement: Placement, config: CampaignConfig,
        user_id: str | None, item_id: str | None, count: int, *, reason: str,
    ) -> Result:
        """Serve something useful without Personalize.

        These are not placeholders. On a cold catalogue or during an outage
        they are what every customer sees, and a bestseller list is a
        genuinely decent recommendation — it is what the site would have shown
        before anyone built a model.
        """
        items: list[Recommendation] = []

        try:
            if config.fallback == "bestsellers":
                items = await self._bestsellers(count)
            elif config.fallback == "same_category" and item_id:
                items = await self._same_category(item_id, count)
            elif config.fallback == "accessories" and item_id:
                items = await self._accessories(item_id, count)
            elif config.fallback == "identity":
                items = []  # search reranking degrades to Elasticsearch's own order
        except Exception as exc:
            # Even the fallback can fail. Returning nothing is correct: the
            # storefront renders no carousel, which is far better than a 500
            # on the home page.
            logger.error("recommendation fallback failed for %s: %s", placement, exc)

        return Result(
            recommendation_id=self._new_recommendation_id(placement, user_id),
            placement=placement.value,
            items=items,
            fallback=True,
            fallback_reason=reason,
        )

    async def _bestsellers(self, count: int) -> list[Recommendation]:
        """Top sellers over 30 days, precomputed nightly into Redis.

        Precomputed rather than queried live: this is the fallback for when
        things are already going wrong, and it must not depend on a database
        that may be the thing that is broken.
        """
        raw = await self._redis.zrevrange("recs:bestsellers:30d", 0, count - 1, withscores=True)
        return [
            Recommendation(product_id=pid.decode() if isinstance(pid, bytes) else pid,
                           score=float(score), reason="bestseller")
            for pid, score in raw
        ]

    async def _same_category(self, item_id: str, count: int) -> list[Recommendation]:
        product = await self._catalog.get_product(item_id)
        if not product or not product.get("categoryPath"):
            return await self._bestsellers(count)

        leaf = product["categoryPath"][-1]
        # +1 then filter, because the product itself is in its own category
        # and recommending it back is the most obvious possible bug.
        raw = await self._redis.zrevrange(f"recs:category:{leaf}", 0, count, withscores=True)

        out = []
        for pid, score in raw:
            pid = pid.decode() if isinstance(pid, bytes) else pid
            if pid == item_id:
                continue
            out.append(Recommendation(product_id=pid, score=float(score), reason="same_category"))
            if len(out) >= count:
                break
        return out

    async def _accessories(self, item_id: str, count: int) -> list[Recommendation]:
        """Co-purchase pairs, precomputed from order history."""
        raw = await self._redis.zrevrange(f"recs:bought_with:{item_id}", 0, count - 1, withscores=True)
        if not raw:
            return await self._same_category(item_id, count)
        return [
            Recommendation(product_id=pid.decode() if isinstance(pid, bytes) else pid,
                           score=float(score), reason="frequently_bought_together")
            for pid, score in raw
        ]

    # ---------------------------------------------------------------- cache

    def _cache_key(self, placement, user_id, item_id, item_ids, count, context) -> str:
        # Hash the whole shape so a different context does not silently reuse
        # another context's answer.
        payload = json.dumps({
            "p": placement.value, "u": user_id, "i": item_id,
            "l": sorted(item_ids or []), "n": count, "c": sorted((context or {}).items()),
        }, sort_keys=True)
        return "recs:" + hashlib.sha256(payload.encode()).hexdigest()[:32]

    async def _read_cache(self, key: str) -> Result | None:
        try:
            raw = await self._redis.get(key)
        except Exception as exc:
            # A cache miss and a broken cache must behave identically. Letting
            # a Redis failure surface here would take the home page down over
            # an optimisation.
            logger.warning("recommendation cache read failed: %s", exc)
            return None
        if not raw:
            return None

        data = json.loads(raw)
        return Result(
            # A NEW id on every serve, even from cache. The id is what ties a
            # subsequent click or purchase back to this specific impression;
            # reusing it across serves makes attribution meaningless and the
            # campaign impossible to evaluate.
            recommendation_id=self._new_recommendation_id(
                Placement(data["placement"]), data.get("user_id")),
            placement=data["placement"],
            items=[Recommendation(**i) for i in data["items"]],
            fallback=data["fallback"],
            fallback_reason=data["fallback_reason"],
        )

    async def _write_cache(self, key: str, result: Result, ttl: int) -> None:
        try:
            await self._redis.setex(key, ttl, json.dumps({
                "placement": result.placement,
                "items": [i.__dict__ for i in result.items],
                "fallback": result.fallback,
                "fallback_reason": result.fallback_reason,
            }))
        except Exception as exc:
            logger.warning("recommendation cache write failed: %s", exc)

    @staticmethod
    def _new_recommendation_id(placement: Placement, user_id: str | None) -> str:
        """Impression id.

        Echoed by the storefront onto every resulting activity event
        (docs/CONTRACTS.md §3.3). Without it Personalize cannot attribute a
        purchase to a recommendation, the model never learns, and nobody can
        say whether any of this is working.
        """
        seed = f"{placement.value}:{user_id or 'anon'}:{time.time_ns()}"
        return hashlib.sha256(seed.encode()).hexdigest()[:24]
