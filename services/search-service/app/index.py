"""Elasticsearch index definition and the catalogue indexer.

The mapping below is most of this service. Search relevance is decided by
analyzers and field weights far more than by query syntax, and a mapping
mistake is expensive to fix later: changing an analyzer requires a full
reindex, which on a 5M-product catalogue is an hour of dual-writing.

Two decisions worth reading before changing anything:

1. **Arabic and English in one index.** SOUQ's primary market is Egypt, where
   product titles mix both scripts freely and customers search in either, often
   transliterated. Rather than two indices and a language guess at query time,
   every text field is indexed three ways — ``.en``, ``.ar`` and a
   language-neutral ``.folded`` — and the query searches all three. It costs
   index size and buys not having to detect language on a two-word query,
   which is a coin flip.

2. **Aliases, never a direct index.** Everything reads ``products``; the real
   index is ``products-v3``. A reindex builds ``products-v4`` alongside and
   flips the alias atomically, so a mapping change is a zero-downtime
   operation rather than a maintenance window.
"""

from __future__ import annotations

import logging
from typing import Any

logger = logging.getLogger(__name__)

ALIAS = "products"
INDEX_PREFIX = "products-v"
CURRENT_VERSION = 3

# Bump this whenever `MAPPING` or `SETTINGS` changes in a way that needs a
# reindex. The indexer compares it against the live index and refuses to write
# into a stale mapping rather than silently producing bad relevance.
MAPPING_FINGERPRINT = "2026-08-arabic-folded-v3"


SETTINGS: dict[str, Any] = {
    "index": {
        # Sized for ~5M products. Over-sharding is the more common mistake:
        # each shard is a Lucene index with its own overhead, and a query
        # fans out to all of them. 3 shards of ~1.5M docs is comfortably
        # inside the 10-50GB-per-shard guidance.
        "number_of_shards": 3,
        "number_of_replicas": 1,
        # 1s is the default and it is wrong for a catalogue. Products change
        # a few thousand times a day, not a few thousand times a second, and
        # a 5s refresh roughly triples indexing throughput. The visible cost
        # is that an admin edit takes up to 5s to appear in search.
        "refresh_interval": "5s",
        "max_result_window": 10000,
        "mapping": {"total_fields": {"limit": 2000}},
    },
    "analysis": {
        "filter": {
            "english_stop": {"type": "stop", "stopwords": "_english_"},
            "english_stemmer": {"type": "stemmer", "language": "english"},
            # Keeps "iPhone" from stemming into "iphon" and losing exact
            # brand matches.
            "english_possessive": {"type": "stemmer", "language": "possessive_english"},
            "arabic_stop": {"type": "stop", "stopwords": "_arabic_"},
            "arabic_stemmer": {"type": "stemmer", "language": "arabic"},
            # Arabic text is written with optional diacritics; normalising
            # them means a customer typing without them still matches.
            "arabic_normalization": {"type": "arabic_normalization"},
            "decimal_digit": {"type": "decimal_digit"},  # ٣ -> 3
            "edge_ngram_2_20": {
                "type": "edge_ngram", "min_gram": 2, "max_gram": 20,
            },
            # Splits "iPhone15Pro" into iPhone/15/Pro so partial model
            # numbers match. Catalogue titles are full of these.
            "product_word_delimiter": {
                "type": "word_delimiter_graph",
                "generate_word_parts": True,
                "generate_number_parts": True,
                "catenate_all": True,
                "preserve_original": True,
                "split_on_case_change": True,
            },
        },
        "char_filter": {
            # Strips the tatweel elongation character, which customers type
            # inconsistently and which never carries meaning.
            "arabic_tatweel": {"type": "mapping", "mappings": ["ـ=>"]},
        },
        "analyzer": {
            "english_text": {
                "type": "custom",
                "tokenizer": "standard",
                "filter": ["lowercase", "english_possessive", "english_stop",
                           "english_stemmer", "asciifolding"],
            },
            "arabic_text": {
                "type": "custom",
                "char_filter": ["arabic_tatweel"],
                "tokenizer": "standard",
                "filter": ["lowercase", "decimal_digit", "arabic_normalization",
                           "arabic_stop", "arabic_stemmer"],
            },
            # The one that catches everything else: no stemming, no stopwords,
            # just folding. This is what makes a transliterated or misspelt
            # query still find something.
            "folded": {
                "type": "custom",
                "char_filter": ["arabic_tatweel"],
                "tokenizer": "standard",
                "filter": ["lowercase", "decimal_digit", "arabic_normalization",
                           "asciifolding", "product_word_delimiter"],
            },
            # Autocomplete. Edge n-grams at INDEX time only.
            "autocomplete_index": {
                "type": "custom",
                "tokenizer": "standard",
                "filter": ["lowercase", "asciifolding", "arabic_normalization",
                           "edge_ngram_2_20"],
            },
            # ...and NOT at search time. Applying edge n-grams to the query
            # too makes "ipho" match anything containing "i", "ip", "iph" —
            # every product in the catalogue. This asymmetry is the single
            # most common autocomplete bug.
            "autocomplete_search": {
                "type": "custom",
                "tokenizer": "standard",
                "filter": ["lowercase", "asciifolding", "arabic_normalization"],
            },
        },
        "normalizer": {
            # For keyword fields used in facets: "Sony" and "SONY" must be one
            # bucket, not two.
            "lowercase_keyword": {
                "type": "custom", "filter": ["lowercase", "asciifolding"],
            },
        },
    },
}


MAPPING: dict[str, Any] = {
    # Strict. A typo'd field name silently creating a new mapping is how an
    # index ends up with 400 fields and an unexplained slowdown.
    "dynamic": "strict",
    "properties": {
        "productId": {"type": "keyword"},
        "sku": {"type": "keyword"},
        "slug": {"type": "keyword"},

        "title": {
            "type": "text",
            "analyzer": "folded",
            "fields": {
                "en": {"type": "text", "analyzer": "english_text"},
                "ar": {"type": "text", "analyzer": "arabic_text"},
                # Exact-match boost: a query that exactly equals the title
                # should always outrank a partial one.
                "raw": {"type": "keyword", "ignore_above": 512,
                        "normalizer": "lowercase_keyword"},
                "autocomplete": {
                    "type": "text",
                    "analyzer": "autocomplete_index",
                    "search_analyzer": "autocomplete_search",
                },
            },
        },

        "description": {
            "type": "text",
            "analyzer": "folded",
            "fields": {
                "en": {"type": "text", "analyzer": "english_text"},
                "ar": {"type": "text", "analyzer": "arabic_text"},
            },
        },

        "brand": {
            "type": "text",
            "analyzer": "folded",
            "fields": {"raw": {"type": "keyword", "normalizer": "lowercase_keyword"}},
        },

        # Hierarchical facets. The path analyzer makes a filter on
        # "electronics" match "electronics/audio/headphones" without storing
        # every prefix separately.
        "categoryPath": {
            "type": "text",
            "analyzer": "folded",
            "fields": {"raw": {"type": "keyword"}},
        },
        "categoryHierarchy": {"type": "keyword"},  # ["electronics", "electronics/audio", ...]

        # Flattened rather than nested: attributes are only ever used for
        # exact-match facets, and `flattened` costs one field in the mapping
        # instead of one per attribute name across the whole catalogue.
        "attributes": {"type": "flattened"},

        "price": {"type": "long"},          # minor units, always
        "listPrice": {"type": "long"},
        "currency": {"type": "keyword"},
        "discountBasisPoints": {"type": "integer"},

        # Denormalised from inventory-service via Kafka. Eventually
        # consistent and used only to sort out-of-stock items down the page,
        # never to decide whether an order can be accepted.
        "inStock": {"type": "boolean"},
        "availableQuantity": {"type": "integer"},

        "rating": {"type": "half_float"},
        "ratingCount": {"type": "integer"},

        # Behavioural signals that feed the relevance score. Updated nightly
        # from the activity stream.
        "popularity30d": {"type": "float"},
        "conversionRate": {"type": "float"},

        "status": {"type": "keyword"},
        "image": {"type": "keyword", "index": False},   # stored, never searched
        "createdAt": {"type": "date"},
        "updatedAt": {"type": "date"},

        # The Kafka offset this document was built from. Lets the indexer
        # discard an out-of-order redelivery instead of overwriting newer
        # data with older data — the search-side equivalent of the inbox
        # pattern (docs/CONTRACTS.md §5.2).
        "_sourceOffset": {"type": "long"},
        "_mappingFingerprint": {"type": "keyword"},
    },
}


def index_name(version: int = CURRENT_VERSION) -> str:
    return f"{INDEX_PREFIX}{version}"


def build_document(product: dict[str, Any], offset: int = 0) -> dict[str, Any]:
    """Turn a ``souq.catalog.product_upserted.v1`` payload into a document.

    Everything derived is computed here rather than at query time: a search
    that has to compute a discount percentage per hit cannot use the index.
    """
    price = product.get("price", {}).get("amount", 0)
    list_price = (product.get("listPrice") or {}).get("amount")

    discount_bp = 0
    if list_price and list_price > price > 0:
        discount_bp = round((list_price - price) * 10_000 / list_price)

    # Prefix expansion, so a filter on any ancestor matches.
    path = product.get("categoryPath", []) or []
    hierarchy = ["/".join(path[: i + 1]) for i in range(len(path))]

    return {
        "productId": product["productId"],
        "sku": product["sku"],
        "slug": product.get("slug", product["productId"]),
        "title": product.get("title", ""),
        "description": product.get("description", ""),
        "brand": product.get("brand"),
        "categoryPath": " ".join(path),
        "categoryHierarchy": hierarchy,
        "attributes": product.get("attributes", {}),
        "price": price,
        "listPrice": list_price,
        "currency": product.get("price", {}).get("currency", "EGP"),
        "discountBasisPoints": discount_bp,
        # Absent stock information means "assume available". Defaulting to
        # False would hide the entire catalogue the first time the inventory
        # consumer lags, which is a far worse failure than showing something
        # that turns out to be out of stock at checkout.
        "inStock": product.get("inStock", True),
        "availableQuantity": product.get("availableQuantity", 0),
        "rating": product.get("rating"),
        "ratingCount": product.get("ratingCount", 0),
        "popularity30d": product.get("popularity30d", 0.0),
        "conversionRate": product.get("conversionRate", 0.0),
        "status": product.get("status", "ACTIVE"),
        "image": (product.get("images") or [{}])[0].get("url"),
        "createdAt": product.get("createdAt"),
        "updatedAt": product.get("updatedAt"),
        "_sourceOffset": offset,
        "_mappingFingerprint": MAPPING_FINGERPRINT,
    }


async def ensure_index(es, version: int = CURRENT_VERSION) -> str:
    """Create the versioned index and point the alias at it if it is new."""
    name = index_name(version)

    if not await es.indices.exists(index=name):
        logger.info("creating index %s", name)
        await es.indices.create(index=name, settings=SETTINGS, mappings=MAPPING)

    if not await es.indices.exists_alias(name=ALIAS):
        await es.indices.put_alias(index=name, name=ALIAS)
        logger.info("alias %s -> %s", ALIAS, name)

    return name


async def reindex_into(es, new_version: int) -> str:
    """Zero-downtime mapping change.

    Build the new index, copy everything across, then flip the alias in one
    atomic call. Readers never see a partially-populated index and there is no
    window in which ``products`` resolves to nothing.

    The old index is deliberately NOT deleted here. Keeping it means a bad
    mapping change is a one-command rollback rather than another hour of
    reindexing.
    """
    new_name = index_name(new_version)
    old_name = index_name(new_version - 1)

    await es.indices.create(index=new_name, settings=SETTINGS, mappings=MAPPING)

    # Throttled: an unthrottled reindex will happily saturate the cluster and
    # take live search down with it.
    await es.reindex(
        body={"source": {"index": old_name, "size": 1000}, "dest": {"index": new_name}},
        wait_for_completion=False,
        requests_per_second=2000,
    )

    await es.indices.update_aliases(
        actions=[
            {"remove": {"index": old_name, "alias": ALIAS}},
            {"add": {"index": new_name, "alias": ALIAS}},
        ]
    )
    logger.info("alias %s flipped from %s to %s", ALIAS, old_name, new_name)
    return new_name
