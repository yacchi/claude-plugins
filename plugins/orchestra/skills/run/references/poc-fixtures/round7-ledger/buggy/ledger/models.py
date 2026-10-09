"""Domain records shared across the package."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import date

from .errors import ValidationError
from .money import Money

KINDS = ("charge", "credit", "adjustment")


@dataclass(frozen=True)
class Entry:
    """One ledger row: a signed amount booked for a customer on a date."""

    date: date
    customer: str
    region: str
    description: str
    amount: Money
    kind: str = "charge"

    def __post_init__(self) -> None:
        if not self.customer:
            raise ValidationError("customer must not be empty")
        if self.kind not in KINDS:
            raise ValidationError(f"unknown kind {self.kind!r}; expected one of {', '.join(KINDS)}")

    @property
    def currency(self) -> str:
        return self.amount.currency


@dataclass(frozen=True)
class Subscription:
    """A recurring plan billed per calendar month."""

    customer: str
    plan: str
    region: str
    monthly_price: Money
    tags: tuple[str, ...] = field(default_factory=tuple)

    def __post_init__(self) -> None:
        if not self.customer:
            raise ValidationError("customer must not be empty")
        if self.monthly_price.is_negative():
            raise ValidationError("monthly price must not be negative")
