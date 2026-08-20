"""
search-service request and response shapes.

Mirrors the ``search-service`` section of ``libs/ts-contracts/src/api.ts``. The
BFF parses every response with the Zod equivalents, so a field renamed on one
side and not the other produces one clean 502 with a request id rather than an
``undefined`` three renders away from the cause. These models are the other half
of that guarantee: they make the mismatch fail *here*, at the boundary that
produced it.
"""

from __future__ import annotations

from enum import StrEnum

from pydantic import ConfigDict, Field

from .primitives import Money, Strict

__all__ = [
    "SortOrder",
    "SearchRequest",
    "SearchFacetValue",
    "SearchFacet",
    "SearchHit",
    "SearchResponse",
    "Suggestion",
    "SuggestResponse",
]


class SortOrder(StrEnum):
    RELEVANCE = "relevance"
    PRICE_ASC = "price_asc"
    PRICE_DESC = "price_desc"
    NEWEST = "newest"
    RATING = "rating"


class SearchRequest(Strict):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    q: str = Field(default="", max_length=200)
    page: int = Field(default=1, ge=1, le=100)
    size: int = Field(default=24, ge=1, le=100)
    sort: SortOrder = SortOrder.RELEVANCE
    filters: dict[str, list[str]] = Field(default_factory=dict)
    price_min: int | None = Field(default=None, alias="priceMin", ge=0)
    price_max: int | None = Field(default=None, alias="priceMax", ge=0)
    in_stock_only: bool = Field(default=False, alias="inStockOnly")


class SearchFacetValue(Strict):
    value: str
    count: int = Field(ge=0)
    selected: bool = False


class SearchFacet(Strict):
    field: str
    label: str
    type: str = Field(pattern=r"^(terms|range|boolean)$")
    values: list[SearchFacetValue]


class SearchHit(Strict):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    product_id: str = Field(alias="productId")
    sku: str
    title: str
    slug: str
    brand: str | None = None
    image: str | None = None
    price: Money
    list_price: Money | None = Field(default=None, alias="listPrice")
    rating: float | None = Field(default=None, ge=0, le=5)
    rating_count: int = Field(default=0, alias="ratingCount", ge=0)
    in_stock: bool = Field(alias="inStock")
    score: float
    # Already HTML-escaped by the service. The frontend renders these as text
    # regardless — a highlight fragment is derived from a product title, and a
    # product title is admin-supplied.
    highlights: dict[str, list[str]] | None = None


class SearchResponse(Strict):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    hits: list[SearchHit]
    total: int = Field(ge=0)
    # Elasticsearch caps how deep it will count. Saying "10,000 results" when
    # the truth is "at least 10,000" makes the last page of pagination behave
    # inexplicably, so the distinction is carried rather than flattened.
    total_is_lower_bound: bool = Field(default=False, alias="totalIsLowerBound")
    page: int
    size: int
    facets: list[SearchFacet]
    took_ms: int = Field(alias="tookMs")
    did_you_mean: str | None = Field(default=None, alias="didYouMean")
    # True when OpenSearch was unavailable and we fell back to a Postgres LIKE.
    # Explicit in the contract because the UI must say so: relevance ordering
    # and facets are gone, and the user will otherwise conclude the catalogue
    # is broken.
    degraded: bool = False


class Suggestion(Strict):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    text: str
    type: str = Field(pattern=r"^(query|product|category|brand)$")
    product_id: str | None = Field(default=None, alias="productId")
    image: str | None = None


class SuggestResponse(Strict):
    suggestions: list[Suggestion]
