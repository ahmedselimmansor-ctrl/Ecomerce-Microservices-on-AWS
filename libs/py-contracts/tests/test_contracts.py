"""
Behaviour these models must have, beyond matching field names.

``scripts/contract-parity.py`` compares names against the TypeScript side and
says so explicitly: it does not check types, constraints or optionality. This
file covers what that check cannot, and it needs pydantic installed — so it runs
in CI and not in the development container.
"""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from souq_contracts import (
    ERROR_CODES,
    CloudEvent,
    Money,
    ProblemDetails,
    RecommendationRequest,
    RecommendationResponse,
    SearchHit,
    SearchRequest,
    SearchResponse,
)


# --------------------------------------------------------------------- Money

def test_money_is_integer_minor_units() -> None:
    """
    The reason amounts are int and not float.

    Summing 0.1 three times in binary floating point gives 0.30000000000000004.
    Over a cart of twenty lines the error is invisible on screen and real in the
    total — which is how a checkout ends up disagreeing with the sum of its own
    lines.
    """
    total = Money(amount=0, currency="EGP")
    for _ in range(1_000):
        total = total + Money(amount=10, currency="EGP")

    assert total.amount == 10_000
    assert isinstance(total.amount, int)


def test_money_rejects_a_fractional_amount() -> None:
    with pytest.raises(ValidationError):
        Money(amount=129.99, currency="EGP")


def test_money_refuses_to_mix_currencies() -> None:
    with pytest.raises(ValueError, match="cannot add"):
        Money(amount=100, currency="EGP") + Money(amount=100, currency="USD")


def test_money_normalises_the_currency_code() -> None:
    assert Money(amount=1, currency="egp").currency == "EGP"


@pytest.mark.parametrize("code", ["EGPX", "EG", "", "12A"])
def test_money_rejects_a_bad_currency_code(code: str) -> None:
    with pytest.raises(ValidationError):
        Money(amount=1, currency=code)


# --------------------------------------------------------------- strictness

def test_unknown_fields_are_rejected() -> None:
    """
    ``extra='forbid'`` mirrors Zod's ``.strict()``.

    A service that silently accepts an unknown field will happily accept a
    *renamed* one, produce a null downstream, and surface hours later far from
    the cause.
    """
    with pytest.raises(ValidationError):
        Money.model_validate({"amount": 1, "currency": "EGP", "scale": 2})


def test_aliases_are_the_wire_names() -> None:
    hit = SearchHit.model_validate({
        "productId": "prd_1",
        "sku": "sku_1",
        "title": "Headphones",
        "slug": "headphones",
        "price": {"amount": 129900, "currency": "EGP"},
        "inStock": True,
        "score": 1.0,
    })

    assert hit.product_id == "prd_1"
    assert hit.in_stock is True
    # Round-tripping must produce camelCase, or the BFF's Zod parse rejects it.
    assert "productId" in hit.model_dump(by_alias=True)
    assert "product_id" not in hit.model_dump(by_alias=True)


def test_snake_case_is_also_accepted_on_input() -> None:
    """``populate_by_name`` so a Python caller can construct one naturally."""
    hit = SearchHit(
        product_id="prd_1", sku="s", title="t", slug="t",
        price=Money(amount=1, currency="EGP"), in_stock=True, score=0.0,
    )
    assert hit.model_dump(by_alias=True)["productId"] == "prd_1"


# ------------------------------------------------------------------- search

def test_search_request_defaults_match_the_contract() -> None:
    request = SearchRequest()

    assert request.page == 1
    assert request.size == 24
    assert request.sort == "relevance"
    assert request.in_stock_only is False
    assert request.filters == {}


@pytest.mark.parametrize("size", [0, 101, -1])
def test_search_request_bounds_the_page_size(size: int) -> None:
    """
    A page bigger than 100 is almost always a scraper, and it is also where the
    response stops fitting comfortably in one CDN object.
    """
    with pytest.raises(ValidationError):
        SearchRequest(size=size)


def test_degraded_defaults_to_false_and_survives_a_round_trip() -> None:
    """
    ``degraded`` is a contract field, not an implementation detail. When
    OpenSearch is unavailable the service falls back to a Postgres LIKE, and the
    UI has to say so — relevance ordering and facets are gone, and the user will
    otherwise conclude the catalogue is broken.
    """
    response = SearchResponse(
        hits=[], total=0, page=1, size=24, facets=[], took_ms=3, did_you_mean=None,
    )
    assert response.degraded is False

    parsed = SearchResponse.model_validate({**response.model_dump(by_alias=True), "degraded": True})
    assert parsed.degraded is True


def test_total_is_lower_bound_is_carried_not_flattened() -> None:
    """
    Elasticsearch caps how deep it will count. Rendering "10,000 results" when
    the truth is "at least 10,000" makes the last page of pagination behave
    inexplicably.
    """
    response = SearchResponse(
        hits=[], total=10_000, page=1, size=24, facets=[], took_ms=9,
        did_you_mean=None, total_is_lower_bound=True,
    )
    assert response.model_dump(by_alias=True)["totalIsLowerBound"] is True


# ---------------------------------------------------------- recommendations

def test_recommendation_items_carry_no_product_data() -> None:
    """
    The split that keeps prices correct.

    Personalize ranks; catalog-service owns the data. A recommender that cached
    titles and prices would serve stale ones the moment either changed, so the
    response carries ids and callers hydrate them.
    """
    response = RecommendationResponse.model_validate({
        "recommendationId": "rec_1",
        "placement": "home_for_you",
        "items": [{"productId": "prd_1", "score": 0.9, "reason": None}],
    })

    assert response.items[0].product_id == "prd_1"
    assert not hasattr(response.items[0], "price")
    assert not hasattr(response.items[0], "title")


def test_recommendation_rejects_an_unknown_placement() -> None:
    """
    A placement is a campaign selector, not an ARN. Accepting an arbitrary
    string would let a caller reach any campaign in the account.
    """
    with pytest.raises(ValidationError):
        RecommendationRequest(placement="arn:aws:personalize:::campaign/anything")


def test_fallback_defaults_to_false() -> None:
    response = RecommendationResponse(
        recommendation_id="rec_1", placement="home_for_you", items=[],
    )
    assert response.fallback is False


# ------------------------------------------------------------------ envelope

def test_cloud_event_requires_an_id() -> None:
    """
    Every consumer's inbox dedupes on ``id``. An event without one cannot be
    deduplicated, so accepting it means accepting unbounded reprocessing on
    every consumer-group rebalance.
    """
    with pytest.raises(ValidationError):
        CloudEvent.model_validate({
            "specversion": "1.0",
            "source": "souq/catalog-service",
            "type": "souq.catalog.product_upserted.v1",
            "time": "2026-08-18T10:00:00Z",
        })


def test_cloud_event_rejects_a_different_spec_version() -> None:
    with pytest.raises(ValidationError):
        CloudEvent.model_validate({
            "specversion": "0.3",
            "id": "evt_1",
            "source": "s",
            "type": "t",
            "time": "2026-08-18T10:00:00Z",
        })


# ------------------------------------------------------------------ problems

def test_problem_details_allows_extension_members() -> None:
    """
    The one model where ``extra`` is permitted. RFC 9457 puts extension members
    alongside the standard ones, so forbidding them would reject conforming
    documents from our own services — ``retryAfterSeconds`` on a 429, for one.
    """
    problem = ProblemDetails.model_validate({
        "type": "https://errors.souq.dev/identity/account-locked",
        "title": "Too many attempts",
        "status": 429,
        "code": "ACCOUNT_LOCKED",
        "requestId": "req_1",
        "timestamp": "2026-08-18T10:00:00Z",
        "retryAfterSeconds": 900,
    })

    assert problem.code == "ACCOUNT_LOCKED"
    assert problem.model_extra["retryAfterSeconds"] == 900


def test_every_error_code_is_screaming_snake_case() -> None:
    for code in ERROR_CODES:
        assert code.isupper(), code
        assert " " not in code, code
        assert code.replace("_", "").isalnum(), code
