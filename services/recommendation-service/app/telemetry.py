"""Structured logging and metrics."""

from __future__ import annotations

import logging
import sys

from prometheus_client import Counter, Histogram
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
    logging.getLogger("botocore").setLevel(logging.WARNING)


HTTP_REQUESTS = Counter(
    "http_server_requests_total", "HTTP requests.", ["route", "method", "status"]
)
HTTP_DURATION = Histogram(
    "http_server_requests_seconds", "HTTP request duration.", ["route", "method"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0),
)

#: The metric the RecommendationsAllFallback alert fires on. Labelled by
#: placement AND by whether it was the fallback, because "half of home_for_you
#: is falling back" is a very different problem from "all of pdp_similar is".
RECOMMENDATIONS = Counter(
    "souq_recommendations_total",
    "Recommendation requests by placement and whether the fallback was used.",
    ["placement", "fallback"],
)

FALLBACK_REASONS = Counter(
    "souq_recommendation_fallback_total",
    "Why the fallback was used.",
    ["placement", "reason"],  # timeout | not_configured | cold_start | error
)

PERSONALIZE_LATENCY = Histogram(
    "souq_personalize_latency_seconds",
    "Amazon Personalize round-trip latency.",
    ["placement"],
    buckets=(0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1.0, 2.0),
)

CACHE = Counter(
    "souq_recommendation_cache_total", "Cache outcomes.", ["outcome"]  # hit | miss | error
)
