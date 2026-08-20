"""
souq-contracts — the platform contract, in Python.

The counterpart of ``@souq/contracts`` for search-service and
recommendation-service. Both previously built response dictionaries by hand,
which meant the only thing enforcing the contract on the Python side was
whichever consumer noticed first.

``tests/test_parity.py`` reads the TypeScript source directly and fails when the
two disagree, so this package cannot quietly drift from the schemas the BFF
validates against.
"""

from .primitives import (
    ERROR_CODES,
    Address,
    CloudEvent,
    CurrencyCode,
    FieldError,
    Image,
    Money,
    ProblemDetails,
    ProductStatus,
    Strict,
)
from .recommendations import (
    Placement,
    RecommendationItem,
    RecommendationRequest,
    RecommendationResponse,
)
from .search import (
    SearchFacet,
    SearchFacetValue,
    SearchHit,
    SearchRequest,
    SearchResponse,
    SortOrder,
    SuggestResponse,
    Suggestion,
)

__version__ = "1.0.0"

__all__ = [
    "ERROR_CODES",
    "Address",
    "CloudEvent",
    "CurrencyCode",
    "FieldError",
    "Image",
    "Money",
    "Placement",
    "ProblemDetails",
    "ProductStatus",
    "RecommendationItem",
    "RecommendationRequest",
    "RecommendationResponse",
    "SearchFacet",
    "SearchFacetValue",
    "SearchHit",
    "SearchRequest",
    "SearchResponse",
    "SortOrder",
    "Strict",
    "SuggestResponse",
    "Suggestion",
]
