"""Structured logging and metrics, matching docs/CONTRACTS.md §9."""

from __future__ import annotations

import logging
import sys

from prometheus_client import Counter, Gauge, Histogram
from pythonjsonlogger import jsonlogger


def configure_logging(service: str, version: str, env: str) -> None:
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(
        jsonlogger.JsonFormatter(
            "%(asctime)s %(levelname)s %(name)s %(message)s",
            rename_fields={"asctime": "timestamp", "levelname": "level"},
            static_fields={"service": service, "version": version, "env": env},
            datefmt="%Y-%m-%dT%H:%M:%S.%fZ",
        )
    )
    root = logging.getLogger()
    root.handlers = [handler]
    root.setLevel(logging.DEBUG if env == "local" else logging.INFO)

    # These two are extremely chatty at INFO and drown everything else.
    logging.getLogger("elastic_transport").setLevel(logging.WARNING)
    logging.getLogger("aiokafka").setLevel(logging.WARNING)


HTTP_REQUESTS = Counter(
    "http_server_requests_total",
    "HTTP requests by route, method and status class.",
    ["route", "method", "status"],
)

HTTP_DURATION = Histogram(
    "http_server_requests_seconds",
    "HTTP request duration.",
    ["route", "method"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5),
)

SEARCHES = Counter(
    "souq_searches_total",
    "Searches by outcome.",
    ["outcome"],  # ok | degraded | empty | error
)

SEARCH_LATENCY = Histogram(
    "souq_search_latency_seconds",
    "Elasticsearch round-trip latency.",
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.0),
)

#: Searches served from the Postgres fallback. Not an outage — results are
#: still returned — but relevance collapses, so a sustained rise is a
#: customer-visible problem long before it is an availability one.
DEGRADED_SEARCHES = Counter(
    "souq_search_degraded_total",
    "Searches served from the Postgres fallback because OpenSearch was unavailable.",
)

#: Queries that returned nothing. A rising rate is usually a broken indexer,
#: not a change in what customers want.
ZERO_RESULT_QUERIES = Counter(
    "souq_search_zero_results_total",
    "Searches that returned no hits.",
)

INDEX_LAG = Gauge(
    "souq_search_index_lag_seconds",
    "Age of the newest document in the index. Growing means the indexer is behind.",
)

DOCUMENTS_INDEXED = Counter(
    "souq_search_documents_indexed_total",
    "Catalogue documents indexed by outcome.",
    ["outcome"],  # upserted | deleted | stale | failed
)
