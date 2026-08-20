"""
recommendation-service request and response shapes.

Mirrors the ``recommendation-service`` section of
``libs/ts-contracts/src/api.ts``.

The shape worth noticing is :class:`RecommendationItem`: it carries a product id
and a score, and **no product data at all**. That split is deliberate — Personalize
ranks, catalog-service owns the data, and a recommender that cached titles and
prices would serve stale ones the moment either changed. Callers hydrate the ids
through catalog's batch endpoint.
"""

from __future__ import annotations

from enum import StrEnum

from pydantic import ConfigDict, Field

from .primitives import Strict

__all__ = [
    "Placement",
    "RecommendationRequest",
    "RecommendationItem",
    "RecommendationResponse",
]


class Placement(StrEnum):
    """
    A campaign selector, not a raw ARN.

    The ARN stays server-side. Accepting one from a caller would let anyone with
    the endpoint query any campaign in the account, including ones built on data
    they should not see.
    """

    HOME_FOR_YOU = "home_for_you"
    PDP_SIMILAR = "pdp_similar"
    PDP_FREQUENTLY_BOUGHT_TOGETHER = "pdp_frequently_bought_together"
    CART_UPSELL = "cart_upsell"
    SEARCH_RERANKED = "search_reranked"
    EMAIL_WINBACK = "email_winback"


class RecommendationRequest(Strict):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    placement: Placement
    user_id: str | None = Field(default=None, alias="userId")
    item_id: str | None = Field(default=None, alias="itemId")
    item_ids: list[str] | None = Field(default=None, alias="itemIds", max_length=50)
    count: int = Field(default=10, ge=1, le=50)
    context: dict[str, str] | None = None


class RecommendationItem(Strict):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    product_id: str = Field(alias="productId")
    score: float | None = None
    reason: str | None = None


class RecommendationResponse(Strict):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    # Echoed onto the resulting activity events. Without it Personalize cannot
    # attribute a purchase to a recommendation and the model never improves —
    # which is the whole reason for having a managed recommender.
    recommendation_id: str = Field(alias="recommendationId")
    placement: str
    items: list[RecommendationItem]
    # True when Personalize was unavailable or cold-start and the fallback
    # ranker served bestsellers. The UI drops the "Recommended for you" heading
    # rather than lying about personalisation.
    fallback: bool = False
    fallback_reason: str | None = Field(default=None, alias="fallbackReason")
