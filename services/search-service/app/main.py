"""search-service.

FastAPI over OpenSearch, plus a Kafka consumer that keeps the index in step
with the catalogue.

The design decision that shapes this file: **search degrades, it does not
fail.** OpenSearch being unavailable takes relevance away, not the ability to
find a product. The Postgres fallback is a much worse search — no facets, no
typo tolerance, no ranking beyond a full-text score — and it is enormously
better than an error page on the browsing path.

`SearchResponse.degraded` is in the contract so the storefront can say so
rather than silently presenting bad results as good ones.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import time
from collections.abc import AsyncIterator
from typing import Any

import asyncpg
from aiokafka import AIOKafkaConsumer
from elasticsearch import AsyncElasticsearch, NotFoundError
from souq_contracts import SearchResponse, SuggestResponse, Suggestion

from fastapi import FastAPI, Query, Request, Response
from fastapi.responses import JSONResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from . import index as idx
from . import query as q
from .config import settings
from .telemetry import (
    DEGRADED_SEARCHES,
    DOCUMENTS_INDEXED,
    HTTP_DURATION,
    HTTP_REQUESTS,
    INDEX_LAG,
    SEARCH_LATENCY,
    SEARCHES,
    ZERO_RESULT_QUERIES,
    configure_logging,
)

log = logging.getLogger("search")

CONSUMER_GROUP = settings.consumer_group
CATALOG_TOPIC = "souq.catalog.events.v1"
INVENTORY_TOPIC = "souq.inventory.events.v1"


class State:
    """Process-wide handles. Attached to app.state, not module globals, so a
    test can construct an app with fakes."""

    es: AsyncElasticsearch
    pg: asyncpg.Pool | None = None
    consumer_task: asyncio.Task[None] | None = None
    ready: bool = False


state = State()


@contextlib.asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    configure_logging("search-service", settings.version, settings.env)

    state.es = AsyncElasticsearch(
        settings.elasticsearch_url,
        request_timeout=settings.es_timeout_seconds,
        # Retry only on connection errors, never on a timeout: a timed-out
        # search has already blown its latency budget and retrying it makes
        # the page slower, not more likely to succeed.
        retry_on_timeout=False,
        max_retries=2,
    )

    try:
        await idx.ensure_index(state.es)
    except Exception:
        # Not fatal. The service can still serve from the fallback, and
        # refusing to start would turn an OpenSearch outage into a search
        # outage — which is exactly what the fallback exists to prevent.
        log.exception("could not ensure the index at startup; serving degraded until it recovers")

    if settings.fallback_db_url:
        try:
            state.pg = await asyncpg.create_pool(
                settings.fallback_db_url, min_size=1, max_size=5, command_timeout=3
            )
            log.info("postgres fallback ready")
        except Exception:
            log.exception("postgres fallback unavailable; an OpenSearch outage will mean no search")

    state.consumer_task = asyncio.create_task(run_indexer())
    state.ready = True
    log.info("search-service started")

    yield

    # Readiness off first, so the load balancer drains this pod while it can
    # still finish the requests it has.
    state.ready = False
    await asyncio.sleep(3)

    if state.consumer_task:
        state.consumer_task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await state.consumer_task
    await state.es.close()
    if state.pg:
        await state.pg.close()
    log.info("search-service stopped")


app = FastAPI(title="souq search-service", lifespan=lifespan, docs_url="/v1/docs")


@app.middleware("http")
async def observe(request: Request, call_next: Any) -> Response:
    started = time.perf_counter()
    # The TEMPLATED path, never the concrete one — concrete paths make the
    # metric cardinality unbounded and eventually take Prometheus down.
    # Bound once. Calling .get twice means the guard and the access are
    # separate lookups with nothing tying them together — which is also why
    # mypy flagged it: it cannot narrow the second call from the first.
    matched = request.scope.get("route")
    route = getattr(matched, "path", None) or request.url.path

    response = await call_next(request)

    HTTP_DURATION.labels(route, request.method).observe(time.perf_counter() - started)
    HTTP_REQUESTS.labels(route, request.method, f"{response.status_code // 100}xx").inc()
    response.headers["X-Request-Id"] = request.headers.get("x-request-id", "")
    return response


# ---------------------------------------------------------------------------
# Probes


@app.get("/health/live", include_in_schema=False)
async def live() -> dict[str, str]:
    # Never touches a dependency. A liveness probe an OpenSearch blip can fail
    # restarts every pod at once and turns a brownout into an outage.
    return {"status": "UP"}


@app.get("/health/ready", include_in_schema=False)
async def ready() -> JSONResponse:
    if not state.ready:
        return JSONResponse({"status": "DOWN", "reason": "draining"}, status_code=503)

    # OpenSearch being down is NOT unreadiness: the fallback still serves.
    # Taking this pod out of rotation would turn a partial degradation into a
    # full outage.
    es_ok = False
    try:
        es_ok = bool(await state.es.ping())
    except Exception:
        pass

    return JSONResponse(
        {"status": "UP", "elasticsearch": "UP" if es_ok else "DOWN (serving degraded)"}
    )


@app.get("/metrics", include_in_schema=False)
async def metrics() -> Response:
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)


# ---------------------------------------------------------------------------
# Search


@app.get("/v1/search", response_model=SearchResponse, response_model_by_alias=True)
async def search(
    q_: str = Query("", alias="q", max_length=200),
    page: int = Query(1, ge=1, le=100),
    size: int = Query(24, ge=1, le=100),
    sort: str = Query("relevance"),
    price_min: int | None = Query(None, alias="priceMin"),
    price_max: int | None = Query(None, alias="priceMax"),
    in_stock_only: bool = Query(False, alias="inStockOnly"),
    brand: list[str] = Query(default=[]),
    category: list[str] = Query(default=[]),
) -> SearchResponse:
    filters: dict[str, list[str]] = {}
    if brand:
        filters["brand"] = brand
    if category:
        filters["categoryHierarchy"] = category

    body = q.build_search(
        q_, filters, page=page, size=size, sort=sort,
        price_min=price_min, price_max=price_max, in_stock_only=in_stock_only,
    )

    started = time.perf_counter()
    try:
        raw = await state.es.search(index=idx.ALIAS, body=body)
        SEARCH_LATENCY.observe(time.perf_counter() - started)
    except Exception as exc:
        log.warning("opensearch unavailable, falling back to postgres: %s", exc)
        DEGRADED_SEARCHES.inc()
        SEARCHES.labels("degraded").inc()
        return SearchResponse.model_validate(await fallback_search(q_, page, size))

    parsed = q.parse_response(raw.body if hasattr(raw, "body") else dict(raw), page, size)

    if parsed["total"] == 0:
        ZERO_RESULT_QUERIES.inc()
        SEARCHES.labels("empty").inc()
    else:
        SEARCHES.labels("ok").inc()

    # The contract boundary. parse_response builds this dictionary by hand from
    # an Elasticsearch document, so a mapping change or a missing field is
    # caught here — in the service that produced it — rather than as a 502 from
    # the BFF's Zod parse two hops away.
    return SearchResponse.model_validate(parsed)


@app.get("/v1/suggest", response_model=SuggestResponse, response_model_by_alias=True)
async def suggest(
    # `q`, matching /v1/search and matching what the BFF actually sends. This
    # was `prefix`, which meant every type-ahead request returned 422 and the
    # storefront silently showed no suggestions — the client swallows suggest
    # failures on purpose, so nothing anywhere reported it.
    prefix: str = Query(..., alias="q", min_length=2, max_length=100),
) -> SuggestResponse:
    try:
        raw = await state.es.search(index=idx.ALIAS, body=q.build_suggest(prefix))
    except Exception:
        # Autocomplete has no fallback and needs none: an empty dropdown is
        # invisible, and the customer can still press enter.
        return SuggestResponse(suggestions=[])

    hits = (raw.body if hasattr(raw, "body") else dict(raw)).get("hits", {}).get("hits", [])

    # Validated on the way out, not just typed. These dictionaries are built by
    # hand from an Elasticsearch document whose mapping lives in another file;
    # a renamed source field would otherwise reach the BFF and fail its Zod
    # parse there, one hop further from the cause.
    return SuggestResponse(
        suggestions=[
            Suggestion(
                text=h["_source"]["title"],
                type="product",
                product_id=h["_source"]["productId"],
                image=h["_source"].get("image"),
            )
            for h in hits
        ]
    )


async def fallback_search(text: str, page: int, size: int) -> dict[str, Any]:
    """Postgres full-text, used only when OpenSearch is unreachable.

    No facets, no typo tolerance, no behavioural ranking. `degraded: true` is
    set so the storefront can hide the facet rail rather than showing an empty
    one and looking broken.
    """
    if not state.pg or not text.strip():
        return _empty(page, size, degraded=True)

    try:
        rows = await state.pg.fetch(
            """
            SELECT p.id, p.sku, p.title, p.slug, p.brand, p.images,
                   v.price, v.currency,
                   ts_rank(to_tsvector('simple',
                       p.title || ' ' || coalesce(p.brand,'') || ' ' || p.description),
                       plainto_tsquery('simple', $1)) AS rank
              FROM products p
              LEFT JOIN LATERAL (
                  SELECT price, currency FROM variants
                   WHERE product_id = p.id AND active ORDER BY position LIMIT 1
              ) v ON TRUE
             WHERE p.status = 'ACTIVE'
               AND to_tsvector('simple',
                     p.title || ' ' || coalesce(p.brand,'') || ' ' || p.description)
                   @@ plainto_tsquery('simple', $1)
             ORDER BY rank DESC
             LIMIT $2 OFFSET $3
            """,
            text, size, (page - 1) * size,
        )
    except Exception:
        log.exception("the postgres fallback failed too")
        SEARCHES.labels("error").inc()
        return _empty(page, size, degraded=True)

    hits = []
    for r in rows:
        images = json.loads(r["images"]) if isinstance(r["images"], str) else (r["images"] or [])
        hits.append({
            "productId": r["id"], "sku": r["sku"], "title": r["title"], "slug": r["slug"],
            "brand": r["brand"],
            "image": images[0]["url"] if images else None,
            "price": {"amount": r["price"] or 0, "currency": r["currency"] or "EGP"},
            "listPrice": None, "rating": None, "ratingCount": 0,
            "inStock": True,  # unknown here; the reservation is authoritative anyway
            "score": float(r["rank"] or 0),
        })

    return {
        "hits": hits, "total": len(hits), "totalIsLowerBound": True,
        "page": page, "size": size,
        "facets": [],  # the fallback cannot compute these
        "tookMs": 0, "didYouMean": None,
        "degraded": True,
    }


def _empty(page: int, size: int, *, degraded: bool) -> dict[str, Any]:
    return {
        "hits": [], "total": 0, "totalIsLowerBound": False, "page": page, "size": size,
        "facets": [], "tookMs": 0, "didYouMean": None, "degraded": degraded,
    }


# ---------------------------------------------------------------------------
# Indexer


async def run_indexer() -> None:
    """Keeps the index in step with the catalogue and with stock levels.

    Two topics, deliberately handled differently:

    * ``catalog.events`` is log-compacted, so a full reindex is a replay from
      offset 0. That is how a mapping change ships without a migration.
    * ``inventory.events`` only ever updates ``inStock``/``availableQuantity``
      on an existing document. A partial update, not an upsert — an inventory
      event must never create a product document, or a stock change for a
      deleted SKU resurrects it.
    """
    consumer = AIOKafkaConsumer(
        CATALOG_TOPIC, INVENTORY_TOPIC,
        bootstrap_servers=settings.brokers,
        group_id=CONSUMER_GROUP,
        # Manual commit after the write lands. Auto-commit acknowledges
        # documents that were never indexed, and the gap is invisible.
        enable_auto_commit=False,
        auto_offset_reset="earliest",
        max_poll_records=200,
    )

    while True:
        try:
            await consumer.start()
            break
        except Exception as exc:
            log.warning("kafka not ready (%s); retrying in 5s", exc)
            await asyncio.sleep(5)

    log.info("indexer started on %s and %s", CATALOG_TOPIC, INVENTORY_TOPIC)

    try:
        async for msg in consumer:
            try:
                await handle_message(msg.topic, msg.value, msg.offset)
                await consumer.commit()
            except asyncio.CancelledError:
                raise
            except Exception:
                # Do NOT commit. The message is redelivered, and indexing is
                # idempotent so a repeat is harmless. Committing here would
                # silently drop a product from the catalogue.
                log.exception("indexing failed; offset not committed")
                DOCUMENTS_INDEXED.labels("failed").inc()
                await asyncio.sleep(1)
    except asyncio.CancelledError:
        pass
    finally:
        await consumer.stop()
        log.info("indexer stopped")


async def handle_message(topic: str, raw: bytes, offset: int) -> None:
    envelope = json.loads(raw)
    event_type = envelope.get("type", "")
    data = envelope.get("data") or {}

    if event_type == "souq.catalog.product_upserted.v1":
        doc = idx.build_document(data, offset)
        await state.es.index(index=idx.ALIAS, id=doc["productId"], document=doc)
        DOCUMENTS_INDEXED.labels("upserted").inc()
        INDEX_LAG.set(0)

    elif event_type == "souq.catalog.product_deleted.v1":
        try:
            await state.es.delete(index=idx.ALIAS, id=data["productId"])
            DOCUMENTS_INDEXED.labels("deleted").inc()
        except NotFoundError:
            # Already gone. A compaction tombstone can arrive after the delete
            # we already applied.
            pass

    elif event_type == "souq.inventory.stock_changed.v1":
        # Partial update keyed by SKU. `doc_as_upsert` is deliberately NOT set:
        # a stock event must never create a product document, or a change for
        # a deleted SKU resurrects it as a document with no title or price.
        try:
            await state.es.update_by_query(
                index=idx.ALIAS,
                body={
                    "query": {"term": {"sku": data["sku"]}},
                    "script": {
                        "source": (
                            "ctx._source.inStock = params.inStock;"
                            "ctx._source.availableQuantity = params.available"
                        ),
                        "params": {
                            "inStock": data.get("available", 0) > 0,
                            "available": data.get("available", 0),
                        },
                    },
                },
                conflicts="proceed",
                refresh=False,
            )
        except NotFoundError:
            pass

    else:
        DOCUMENTS_INDEXED.labels("stale").inc()
