"""Configuration. Fails fast on anything missing.

A service that boots with half its configuration and discovers the gap on the
first real request is strictly worse than one that refuses to start.
"""

from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="SOUQ_", extra="ignore")

    env: str = "local"
    version: str = "dev"
    http_addr: str = "0.0.0.0:8087"

    elasticsearch_url: str
    kafka_brokers: str
    catalog_url: str = "http://catalog-service:8082"

    #: Fallback for when OpenSearch is unreachable. A Postgres LIKE is a much
    #: worse search, and it is enormously better than an error page.
    fallback_db_url: str | None = None

    consumer_group: str = "search-service.catalog-indexer"

    #: Deliberately tight. Search is on the browsing path and a slow search
    #: should degrade to the fallback rather than becoming a spinner.
    es_timeout_seconds: float = 2.0

    @property
    def brokers(self) -> list[str]:
        return [b.strip() for b in self.kafka_brokers.split(",") if b.strip()]


settings = Settings()  # type: ignore[call-arg]
