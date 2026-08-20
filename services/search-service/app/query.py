"""Query construction and relevance.

The scoring model here is the product decision that most affects revenue, so
the weights are documented rather than left as magic numbers. The shape is:

    (text relevance) x (business signals) x (availability)

multiplicative, not additive, because an out-of-stock product should be pushed
down regardless of how well it matches, and a perfect title match on something
nobody buys should still beat a weak match on a bestseller.
"""

from __future__ import annotations

from typing import Any

# Field weights for the main text query.
#
# `title.raw` is enormous on purpose: an exact title match is almost always
# what the customer meant, and without a large boost a long description
# mentioning the phrase repeatedly can outrank it through term frequency.
FIELD_WEIGHTS = [
    "title.raw^20",
    "title^8",
    "title.en^6",
    "title.ar^6",
    "brand.raw^5",
    "brand^3",
    "categoryPath^2",
    # Descriptions are noisy: keyword-stuffed marketing copy would dominate
    # at any meaningful weight.
    "description^0.5",
    "description.en^0.4",
    "description.ar^0.4",
]

SORTS: dict[str, list[Any]] = {
    "relevance": ["_score", {"popularity30d": "desc"}],
    "price_asc": [{"price": "asc"}, "_score"],
    "price_desc": [{"price": "desc"}, "_score"],
    "newest": [{"createdAt": "desc"}, "_score"],
    # Bayesian, not raw average: one 5-star review must not outrank two
    # hundred 4.8-star ones. Handled by the script sort below.
    "rating": [{"_script": {
        "type": "number",
        "script": {
            "source": (
                "double r = doc['rating'].size() == 0 ? 0 : doc['rating'].value;"
                "double n = doc['ratingCount'].value;"
                "double prior = 3.8; double weight = 20;"
                "return (n * r + weight * prior) / (n + weight);"
            ),
        },
        "order": "desc",
    }}, "_score"],
}


def build_search(
    q: str,
    filters: dict[str, list[str]],
    *,
    page: int = 1,
    size: int = 24,
    sort: str = "relevance",
    price_min: int | None = None,
    price_max: int | None = None,
    in_stock_only: bool = False,
) -> dict[str, Any]:
    """Compose the full search request."""
    must: list[dict[str, Any]] = []
    filter_clauses: list[dict[str, Any]] = [{"term": {"status": "ACTIVE"}}]

    if q.strip():
        must.append({
            "bool": {
                "should": [
                    # cross_fields treats title/brand/category as one field,
                    # which is what makes "sony headphones" match a Sony-brand
                    # product titled only "WH-1000XM5". best_fields would score
                    # it near zero because neither field contains both words.
                    {
                        "multi_match": {
                            "query": q,
                            "fields": FIELD_WEIGHTS,
                            "type": "cross_fields",
                            "operator": "and",
                            "tie_breaker": 0.3,
                        }
                    },
                    # Fallback for typos. Lower boost so a fuzzy match never
                    # beats an exact one. prefix_length=2 stops "iPhone" from
                    # fuzzily matching "phone" — a real complaint.
                    {
                        "multi_match": {
                            "query": q,
                            "fields": FIELD_WEIGHTS,
                            "type": "best_fields",
                            "fuzziness": "AUTO",
                            "prefix_length": 2,
                            "max_expansions": 20,
                            "boost": 0.3,
                        }
                    },
                    # Phrase match, heavily boosted: "red running shoes" as a
                    # phrase is a much stronger signal than the three words
                    # scattered through a description.
                    {
                        "multi_match": {
                            "query": q,
                            "fields": ["title^10", "title.en^8", "title.ar^8"],
                            "type": "phrase",
                            "slop": 2,
                            "boost": 3,
                        }
                    },
                ],
                "minimum_should_match": 1,
            }
        })
    else:
        must.append({"match_all": {}})

    for field, values in filters.items():
        if not values:
            continue
        # Attribute facets live under the flattened field.
        target = f"attributes.{field}" if field not in {"brand", "categoryHierarchy"} else (
            "brand.raw" if field == "brand" else "categoryHierarchy"
        )
        # `terms`, not several `term` clauses: selecting two brands means OR
        # within the facet and AND across facets, which is what every
        # e-commerce customer expects without being told.
        filter_clauses.append({"terms": {target: values}})

    if price_min is not None or price_max is not None:
        rng: dict[str, int] = {}
        if price_min is not None:
            rng["gte"] = price_min
        if price_max is not None:
            rng["lte"] = price_max
        filter_clauses.append({"range": {"price": rng}})

    if in_stock_only:
        filter_clauses.append({"term": {"inStock": True}})

    query: dict[str, Any] = {
        "function_score": {
            "query": {"bool": {"must": must, "filter": filter_clauses}},
            "functions": [
                # Popularity. log1p so a product with 10,000 views does not
                # simply bury everything else — the curve flattens.
                {
                    "field_value_factor": {
                        "field": "popularity30d",
                        "modifier": "log1p",
                        "factor": 1.0,
                        "missing": 0,
                    },
                    "weight": 1.5,
                },
                # Conversion rate is a stronger signal than views: it means
                # people who saw it bought it.
                {
                    "field_value_factor": {
                        "field": "conversionRate",
                        "modifier": "sqrt",
                        "factor": 4.0,
                        "missing": 0,
                    },
                    "weight": 2.0,
                },
                # Freshness. New products have no behavioural signal at all
                # and would otherwise never surface — a cold-start trap that
                # quietly kills new listings.
                {
                    "gauss": {
                        "createdAt": {"origin": "now", "scale": "30d", "decay": 0.5}
                    },
                    "weight": 0.5,
                },
                # Out of stock: heavily demoted but NOT filtered out. A
                # customer searching for something specific still wants to
                # know we normally carry it, and hiding it looks like we do
                # not sell it at all.
                {
                    "filter": {"term": {"inStock": False}},
                    "weight": 0.15,
                },
            ],
            # Sum the boosts, then MULTIPLY into the text score. Additive
            # boost_mode would let a popular but irrelevant product outrank an
            # exact match, which is the classic way relevance is ruined.
            "score_mode": "sum",
            "boost_mode": "multiply",
        }
    }

    body: dict[str, Any] = {
        "query": query,
        "from": (page - 1) * size,
        "size": size,
        "sort": SORTS.get(sort, SORTS["relevance"]),
        "aggs": _facets(),
        "highlight": {
            "fields": {"title": {}, "description": {"fragment_size": 150, "number_of_fragments": 1}},
            # Escaped by the service before it reaches the browser; the
            # storefront renders these as HTML, so an unescaped product title
            # would be stored XSS.
            "pre_tags": ["<mark>"],
            "post_tags": ["</mark>"],
        },
        # Only what the result card renders. Fetching `description` for 24
        # hits moves megabytes per search for text nobody displays.
        "_source": [
            "productId", "sku", "slug", "title", "brand", "image",
            "price", "listPrice", "currency", "rating", "ratingCount", "inStock",
        ],
        # Deep pagination is capped by max_result_window, so past ~10k hits
        # the count is a floor rather than an exact number. Saying so is
        # cheaper than computing it.
        "track_total_hits": 10_000,
    }

    if q.strip():
        body["suggest"] = {
            "did_you_mean": {
                "text": q,
                "term": {"field": "title", "size": 1, "suggest_mode": "popular"},
            }
        }

    return body


def _facets() -> dict[str, Any]:
    return {
        "brand": {"terms": {"field": "brand.raw", "size": 30, "order": {"_count": "desc"}}},
        "category": {"terms": {"field": "categoryHierarchy", "size": 50}},
        # Buckets in minor units. Fixed rather than dynamic so the UI can
        # label them consistently and a customer's filter does not move
        # between one search and the next.
        "price_ranges": {
            "range": {
                "field": "price",
                "ranges": [
                    {"key": "under_500", "to": 50_000},
                    {"key": "500_2000", "from": 50_000, "to": 200_000},
                    {"key": "2000_10000", "from": 200_000, "to": 1_000_000},
                    {"key": "over_10000", "from": 1_000_000},
                ],
            }
        },
        "rating": {
            "range": {
                "field": "rating",
                "ranges": [
                    {"key": "4_plus", "from": 4.0},
                    {"key": "3_plus", "from": 3.0},
                ],
            }
        },
        "in_stock": {"terms": {"field": "inStock"}},
        "discounted": {"filter": {"range": {"discountBasisPoints": {"gt": 0}}}},
    }


def build_suggest(prefix: str, size: int = 8) -> dict[str, Any]:
    """Autocomplete.

    Searches ``title.autocomplete``, which is edge-n-grammed at index time and
    NOT at search time. That asymmetry is the whole trick: analysing the query
    with the same n-gram filter would make "ipho" match every product
    containing "i".
    """
    return {
        "query": {
            "bool": {
                "must": [{"match": {"title.autocomplete": {"query": prefix, "operator": "and"}}}],
                "filter": [{"term": {"status": "ACTIVE"}}],
                # Bias suggestions towards things people actually buy.
                "should": [{"term": {"inStock": True}}],
            }
        },
        "size": size,
        "_source": ["productId", "slug", "title", "image", "price", "currency"],
        "sort": ["_score", {"popularity30d": "desc"}],
    }


def parse_response(raw: dict[str, Any], page: int, size: int) -> dict[str, Any]:
    """Normalise an Elasticsearch response into the shape SearchResponse expects."""
    hits = raw.get("hits", {})
    total = hits.get("total", {})

    items = []
    for h in hits.get("hits", []):
        src = h["_source"]
        items.append({
            "productId": src["productId"],
            "sku": src.get("sku", ""),
            "title": src.get("title", ""),
            "slug": src.get("slug", ""),
            "brand": src.get("brand"),
            "image": src.get("image"),
            "price": {"amount": src.get("price", 0), "currency": src.get("currency", "EGP")},
            "listPrice": (
                {"amount": src["listPrice"], "currency": src.get("currency", "EGP")}
                if src.get("listPrice") else None
            ),
            "rating": src.get("rating"),
            "ratingCount": src.get("ratingCount", 0),
            "inStock": src.get("inStock", True),
            "score": h.get("_score") or 0.0,
            "highlights": h.get("highlight"),
        })

    suggestion = None
    for s in raw.get("suggest", {}).get("did_you_mean", []):
        options = s.get("options", [])
        if options:
            suggestion = options[0]["text"]
            break

    return {
        "hits": items,
        "total": total.get("value", 0),
        # "gte" means Elasticsearch stopped counting at track_total_hits.
        "totalIsLowerBound": total.get("relation") == "gte",
        "page": page,
        "size": size,
        "facets": _parse_facets(raw.get("aggregations", {})),
        "tookMs": raw.get("took", 0),
        "didYouMean": suggestion,
        "degraded": False,
    }


def _parse_facets(aggs: dict[str, Any]) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []

    for field, label in (("brand", "Brand"), ("category", "Category")):
        buckets = aggs.get(field, {}).get("buckets", [])
        if buckets:
            out.append({
                "field": field, "label": label, "type": "terms",
                "values": [
                    {"value": str(b["key"]), "count": b["doc_count"], "selected": False}
                    for b in buckets
                ],
            })

    for field, label in (("price_ranges", "Price"), ("rating", "Rating")):
        buckets = aggs.get(field, {}).get("buckets", [])
        # Range aggs return every bucket including empty ones; showing a
        # filter that yields nothing is worse than not showing it.
        values = [
            {"value": str(b["key"]), "count": b["doc_count"], "selected": False}
            for b in buckets if b["doc_count"] > 0
        ]
        if values:
            out.append({"field": field, "label": label, "type": "range", "values": values})

    return out
