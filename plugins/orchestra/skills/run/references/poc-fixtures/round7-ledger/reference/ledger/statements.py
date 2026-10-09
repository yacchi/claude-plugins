"""Customer statements: running balances and receivables aging."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date
from typing import Iterable

from .currency import REPORTING_CURRENCY
from .errors import ValidationError
from .fx import to_usd
from .models import Entry
from .money import Money

AGING_BUCKETS = ("0-30", "31-60", "61-90", "90+")


@dataclass(frozen=True)
class StatementRow:
    date: date
    description: str
    kind: str
    amount: Money
    balance: Money


@dataclass(frozen=True)
class AgingRow:
    customer: str
    buckets: dict[str, Money]

    @property
    def total(self) -> Money:
        total = Money.zero(REPORTING_CURRENCY)
        for name in AGING_BUCKETS:
            total = total + self.buckets[name]
        return total


def statement(customer: str, entries: Iterable[Entry]) -> list[StatementRow]:
    """Chronological rows for ``customer`` with a running balance in the entry currency."""
    own = sorted((e for e in entries if e.customer == customer), key=lambda e: e.date)
    if not own:
        return []
    currency = own[0].currency
    balance = Money.zero(currency)
    rows = []
    for entry in own:
        if entry.currency != currency:
            raise ValidationError(f"statement for {customer!r} mixes {currency} and {entry.currency}")
        balance = balance + entry.amount
        rows.append(
            StatementRow(
                date=entry.date,
                description=entry.description,
                kind=entry.kind,
                amount=entry.amount,
                balance=balance,
            )
        )
    return rows


def _bucket_for(age_days: int) -> str:
    if age_days <= 30:
        return "0-30"
    if age_days <= 60:
        return "31-60"
    if age_days <= 90:
        return "61-90"
    return "90+"


def aging(entries: Iterable[Entry], as_of: date) -> list[AgingRow]:
    """USD amounts per customer bucketed by age in days relative to ``as_of``.

    Entries dated after ``as_of`` are ignored. Rows are sorted by customer name.
    """
    table: dict[str, dict[str, Money]] = {}
    for entry in entries:
        if entry.date > as_of:
            continue
        age = (as_of - entry.date).days
        buckets = table.setdefault(
            entry.customer, {name: Money.zero(REPORTING_CURRENCY) for name in AGING_BUCKETS}
        )
        name = _bucket_for(age)
        buckets[name] = buckets[name] + to_usd(entry.amount)
    return [AgingRow(customer=name, buckets=table[name]) for name in sorted(table)]
