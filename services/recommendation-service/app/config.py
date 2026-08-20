"""Configuration."""

from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="SOUQ_", extra="ignore")

    env: str = "local"
    version: str = "dev"
    http_addr: str = "0.0.0.0:8088"

    redis_url: str
    catalog_url: str = "http://catalog-service:8082"
    kafka_brokers: str = ""

    #: Empty locally. With no campaign ARN the service serves its fallback
    #: rankers — which is deliberate: the fallback is what runs during a
    #: Personalize outage, so it should be exercised every day rather than
    #: only during an incident.
    personalize_campaign_home: str = ""
    personalize_campaign_similar: str = ""
    personalize_campaign_ranking: str = ""

    #: Forces the fallback even when campaigns are configured. Used to test the
    #: degraded path against production data.
    fallback_only: bool = False

    aws_region: str = "eu-west-1"

    @property
    def brokers(self) -> list[str]:
        return [b.strip() for b in self.kafka_brokers.split(",") if b.strip()]


settings = Settings()  # type: ignore[call-arg]
