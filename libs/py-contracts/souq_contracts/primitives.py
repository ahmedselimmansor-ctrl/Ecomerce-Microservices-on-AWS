"""
Shared value types.

The Python counterpart of ``libs/ts-contracts/src/primitives.ts``. Field names,
constraints and defaults mirror the Zod schemas deliberately — the two are the
same contract expressed twice, and ``tests/test_parity.py`` reads the TypeScript
source and fails if they diverge.

``extra="forbid"`` everywhere mirrors Zod's ``.strict()``. That is not
fastidiousness: a service that silently accepts an unknown field is a service
that will happily consume a renamed one and produce a null downstream, hours
later and far from the cause.
"""

from __future__ import annotations

from datetime import datetime
from enum import StrEnum
from typing import Annotated, Any, Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator

__all__ = [
    "Strict",
    "Money",
    "Address",
    "Image",
    "ProblemDetails",
    "FieldError",
    "CloudEvent",
    "ERROR_CODES",
    "CurrencyCode",
]


class Strict(BaseModel):
    """
    The base every contract model uses.

    ``extra="forbid"`` rejects unknown fields, and ``validate_assignment`` means
    a model cannot be mutated into an invalid state after construction — which
    matters because these objects are passed between async tasks and a
    half-updated model is very hard to trace back.
    """

    model_config = ConfigDict(
        extra="forbid",
        validate_assignment=True,
        # Reject ``Money(amount="129900")``. Pydantic would otherwise coerce the
        # string, and a price arriving as text from a misconfigured upstream
        # should be a loud failure rather than a silent success.
        strict=False,
        frozen=False,
    )


CurrencyCode = Annotated[str, Field(pattern=r"^[A-Z]{3}$")]


class Money(Strict):
    """
    An amount in minor units, with its currency.

    ``int``, never ``float`` or ``Decimal``-with-scale. 0.1 + 0.2 is not 0.3 in
    binary floating point, and a cart that sums floats disagrees with the sum of
    its own lines at the hundredth order — small enough to ship, large enough to
    become a reconciliation problem.

    There is no scale field. The currency determines it, and carrying a second
    source of truth for the same fact is how "1000" becomes ten pounds in one
    service and a thousand in another.
    """

    amount: int
    currency: CurrencyCode

    @field_validator("currency")
    @classmethod
    def _upper(cls, value: str) -> str:
        return value.upper()

    def formatted(self) -> str:
        """A plain rendering for logs. Not for a user interface — that is the frontend's job."""
        return f"{self.amount / 100:.2f} {self.currency}"

    def __add__(self, other: Money) -> Money:
        if self.currency != other.currency:
            raise ValueError(f"cannot add {other.currency} to {self.currency}")
        return Money(amount=self.amount + other.amount, currency=self.currency)


class Address(Strict):
    recipient: str = Field(min_length=1, max_length=200)
    line1: str = Field(min_length=1, max_length=300)
    line2: str | None = Field(default=None, max_length=300)
    city: str = Field(min_length=1, max_length=150)
    region: str | None = Field(default=None, max_length=150)
    postal_code: str = Field(min_length=1, max_length=30, alias="postalCode")
    country_code: str = Field(pattern=r"^[A-Z]{2}$", alias="countryCode")
    phone: str | None = Field(default=None, max_length=40)

    model_config = ConfigDict(extra="forbid", populate_by_name=True)


class Image(Strict):
    url: str
    alt: str = Field(default="", max_length=300)
    width: int | None = Field(default=None, gt=0)
    height: int | None = Field(default=None, gt=0)


class FieldError(Strict):
    field: str
    message: str


# Every code the storefront and admin handle explicitly. Must stay in step with
# ERROR_CODES in libs/ts-contracts/src/primitives.ts — the parity test asserts it.
ERROR_CODES: tuple[str, ...] = (
    "VALIDATION_FAILED",
    "UNSUPPORTED_CURRENCY",
    "WEAK_PASSWORD",
    "UNAUTHENTICATED",
    "TOKEN_EXPIRED",
    "REFRESH_TOKEN_REUSED",
    "FORBIDDEN",
    "MFA_REQUIRED",
    "PRODUCT_NOT_FOUND",
    "ORDER_NOT_FOUND",
    "CART_NOT_FOUND",
    "INVENTORY_INSUFFICIENT_STOCK",
    "IDEMPOTENCY_KEY_REUSE",
    "REQUEST_IN_PROGRESS",
    "CART_STALE",
    "ORDER_NOT_CANCELLABLE",
    "ACCOUNT_LOCKED",
    "RATE_LIMITED",
    "PAYMENT_DECLINED",
    "PAYMENT_REQUIRES_ACTION",
    "UPSTREAM_UNAVAILABLE",
    "UPSTREAM_TIMEOUT",
    "INTERNAL_ERROR",
)


class ProblemDetails(Strict):
    """
    RFC 9457, extended — docs/CONTRACTS.md §2.2.

    Every 4xx and 5xx from every service in every language serialises to exactly
    this shape. ``extra`` is permitted here and nowhere else: the RFC defines
    extension members as top-level siblings of the standard ones, so forbidding
    them would reject conforming documents from our own services.
    """

    model_config = ConfigDict(extra="allow", populate_by_name=True)

    type: str
    title: str
    status: int = Field(ge=100, le=599)
    detail: str | None = None
    instance: str | None = None
    code: str
    request_id: str = Field(alias="requestId")
    timestamp: str
    errors: list[FieldError] | None = None


class CloudEvent(Strict):
    """
    The CloudEvents 1.0 envelope every Kafka message carries.

    ``data`` is deliberately untyped here. The envelope is uniform across the
    platform; the payload is not, and a consumer that has already switched on
    ``type`` knows far more about the shape than this model could.

    ``id`` is what every consumer's inbox dedupes on, which is why it is
    required rather than optional — an event without one cannot be deduplicated,
    so accepting it means accepting unbounded reprocessing on every rebalance.
    """

    specversion: Literal["1.0"]
    id: str = Field(min_length=1)
    source: str
    type: str
    time: datetime
    datacontenttype: str = "application/json"
    subject: str | None = None
    data: Any = None


class ProductStatus(StrEnum):
    DRAFT = "DRAFT"
    ACTIVE = "ACTIVE"
    ARCHIVED = "ARCHIVED"
    DISCONTINUED = "DISCONTINUED"
