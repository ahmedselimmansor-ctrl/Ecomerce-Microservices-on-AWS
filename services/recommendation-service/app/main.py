"""recommendation-service.

Wraps Amazon Personalize with per-placement deadlines, a short cache, and a
fallback that is not an afterthought.

The design decision that shapes this file is the same as search-service's:
**recommendations degrade, they do not fail.** A home page that 500s because a
carousel could not be personalised is a far worse outcome than a home page
showing bestsellers. `RecommendationResponse.fallback` is in the contract so
the storefront can drop the "Recommended for you" heading rather than claiming
personalisation that did not happen.
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import time
from collections.abc import AsyncIterator
from typing import Any

import redis.asyncio as aioredis
from souq_contracts import RecommendationItem, RecommendationResponse

from fastapi import FastAPI, HTTPException, Query, Request, Response
from fastapi.responses import JSONResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from .config import settings
from .recommender import CAMPAIGNS, Placement, Recommender
from .telemetry import (
    FALLBACK_REASONS,
    HTTP_DURATION,
    HTTP_REQUESTS,
    RECOMMENDATIONS,
    configure_logging,
)

log = logging.getLogger("recommendation")


class CatalogClient:
    """Minimal catalogue lookup, used only by the same-category fallback."""

    def __init__(self, base_url: str) -> None:
        self._base = base_url.rstrip("/")
        self._cache: dict[str, tuple[dict[str, Any], float]] = {}

    async def get_product(self, product_id: str) -> dict[str, Any] | None:
        hit = self._cache.get(product_id)
        if hit and time.monotonic() - hit[1] < 300:
            return hit[0]

        import httpx

        try:
            async with httpx.AsyncClient(timeout=2.0) as client:
                r = await client.get(f"{self._base}/v1/products/{product_id}")
                if r.status_code != 200:
                    return None
                data = r.json()
        except Exception:
            # The fallback's own fallback is bestsellers, so a catalogue blip
            # costs relevance, not availability.
            return None

        self._cache[product_id] = (data, time.monotonic())
        return data


class State:
    redis: aioredis.Redis
    recommender: Recommender
    ready: bool = False


state = State()


@contextlib.asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    configure_logging("recommendation-service", settings.version, settings.env)

    state.redis = aioredis.from_url(
        settings.redis_url,
        decode_responses=False,
        socket_timeout=1.0,
        socket_connect_timeout=1.0,
        # A cache failure must never become a request failure.
        retry_on_timeout=False,
    )

    personalize = None
    if not settings.fallback_only:
        campaigns = {
            Placement.HOME_FOR_YOU: settings.personalize_campaign_home,
            Placement.PDP_SIMILAR: settings.personalize_campaign_similar,
            Placement.SEARCH_RERANKED: settings.personalize_campaign_ranking,
        }
        for placement, arn in campaigns.items():
            if arn:
                CAMPAIGNS[placement] = CAMPAIGNS[placement].__class__(
                    arn=arn,
                    cache_ttl=CAMPAIGNS[placement].cache_ttl,
                    timeout_ms=CAMPAIGNS[placement].timeout_ms,
                    fallback=CAMPAIGNS[placement].fallback,
                )
        if any(c.arn for c in CAMPAIGNS.values()):
            import boto3

            personalize = boto3.client("personalize-runtime", region_name=settings.aws_region)
            log.info("amazon personalize configured")

    if personalize is None:
        log.warning(
            "no Personalize campaigns configured — serving the fallback rankers. "
            "This is the expected local behaviour."
        )

    state.recommender = Recommender(
        personalize, state.redis, CatalogClient(settings.catalog_url),
        fallback_only=settings.fallback_only or personalize is None,
    )
    state.ready = True
    log.info("recommendation-service started")

    yield

    state.ready = False
    await asyncio.sleep(3)
    await state.redis.aclose()
    log.info("recommendation-service stopped")


app = FastAPI(title="souq recommendation-service", lifespan=lifespan, docs_url="/v1/docs")


@app.middleware("http")
async def observe(request: Request, call_next: Any) -> Response:
    started = time.perf_counter()
    route = request.scope.get("route").path if request.scope.get("route") else request.url.path
    response = await call_next(request)
    HTTP_DURATION.labels(route, request.method).observe(time.perf_counter() - started)
    HTTP_REQUESTS.labels(route, request.method, f"{response.status_code // 100}xx").inc()
    return response


@app.get("/health/live", include_in_schema=False)
async def live() -> dict[str, str]:
    return {"status": "UP"}


@app.get("/health/ready", include_in_schema=False)
async def ready() -> JSONResponse:
    if not state.ready:
        return JSONResponse({"status": "DOWN", "reason": "draining"}, status_code=503)
    # Neither Redis nor Personalize being down is unreadiness — both have
    # fallbacks, and removing this pod would turn a degradation into an outage.
    try:
        await state.redis.ping()
        cache = "UP"
    except Exception:
        cache = "DOWN (serving uncached)"
    return JSONResponse({"status": "UP", "cache": cache})


@app.get("/metrics", include_in_schema=False)
async def metrics() -> Response:
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)


@app.get(
    "/v1/recommendations",
    response_model=RecommendationResponse,
    response_model_by_alias=True,
)
async def recommend(
    placement: str = Query(...),
    user_id: str | None = Query(None, alias="userId"),
    item_id: str | None = Query(None, alias="itemId"),
    count: int = Query(10, ge=1, le=50),
) -> RecommendationResponse:
    try:
        p = Placement(placement)
    except ValueError:
        # 422 with the valid set, rather than a 500 or a silent empty list. A
        # placement is a campaign selector and the caller has simply named one
        # that does not exist — telling them which do is not a disclosure,
        # because the list is in the published contract.
        raise HTTPException(
            status_code=422,
            detail=f"unknown placement; expected one of {[x.value for x in Placement]}",
        ) from None

    result = await state.recommender.recommend(
        p, user_id=user_id, item_id=item_id, count=count
    )

    RECOMMENDATIONS.labels(p.value, str(result.fallback).lower()).inc()
    if result.fallback and result.fallback_reason:
        FALLBACK_REASONS.labels(p.value, result.fallback_reason).inc()

    # Validated on the way out. `result` comes from the Personalize wrapper or
    # from one of three fallback rankers, and those four code paths previously
    # agreed on a shape only by convention.
    #
    # Note what is NOT here: no title, no price, no image. Personalize ranks and
    # catalog-service owns the data; a recommender that cached product copy
    # would serve a stale price the moment one changed. Callers hydrate the ids
    # through catalog's batch endpoint.
    return RecommendationResponse(
        # Echoed by the storefront onto every resulting activity event. Without
        # it Personalize cannot attribute a purchase to a recommendation, the
        # model never learns, and nobody can say whether any of this works.
        recommendation_id=result.recommendation_id,
        placement=result.placement,
        items=[
            RecommendationItem(product_id=i.product_id, score=i.score, reason=i.reason)
            for i in result.items
        ],
        fallback=result.fallback,
        fallback_reason=result.fallback_reason,
    )
