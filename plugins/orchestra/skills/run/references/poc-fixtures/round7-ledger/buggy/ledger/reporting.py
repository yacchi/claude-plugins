"""Aggregate reports over ledger entries."""

from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass
from typing import Iterable

from .currency import REPORTING_CURRENCY
from .dates import month_key
from .fx import to_usd
from .models import Entry
from .money import Money, format_money, sum_money
from .tax import tax_for


@dataclass(frozen=True)
class TaxSummaryRow:
    region: str
    currency: str
    taxable: Money
    tax: Money


def revenue_by_month(entries: Iterable[Entry]) -> dict[str, Money]:
    """USD revenue per ``YYYY-MM``, months in chronological order."""
    buckets: dict[str, list[Money]] = defaultdict(list)
    for entry in entries:
        buckets[month_key(entry.date)].append(to_usd(entry.amount))
    return {
        key: sum_money(buckets[key], REPORTING_CURRENCY)
        for key in sorted(buckets)
    }


def customer_balances(entries: Iterable[Entry]) -> list[tuple[str, Money]]:
    """USD balance per customer, largest balance first, ties by customer name."""
    totals: dict[str, Money] = {}
    for entry in entries:
        usd = to_usd(entry.amount)
        totals[entry.customer] = totals.get(entry.customer, Money.zero(REPORTING_CURRENCY)) + usd
    return sorted(totals.items(), key=lambda item: (-item[1].amount, item[0]))


def tax_summary(entries: Iterable[Entry]) -> list[TaxSummaryRow]:
    """Taxable amount and tax per (region, currency), sorted by region then currency."""
    taxable: dict[tuple[str, str], Money] = {}
    taxes: dict[tuple[str, str], Money] = {}
    for entry in entries:
        key = (entry.region, entry.currency)
        zero = Money.zero(entry.currency)
        taxable[key] = taxable.get(key, zero) + entry.amount
        taxes[key] = taxes.get(key, zero) + tax_for(entry.amount, entry.region, entry.date)
    return [
        TaxSummaryRow(region=region, currency=currency, taxable=taxable[(region, currency)], tax=taxes[(region, currency)])
        for region, currency in sorted(taxable)
    ]


def render_revenue_report(entries: Iterable[Entry]) -> str:
    """One ``YYYY-MM  <amount>`` line per month followed by a ``TOTAL`` line."""
    months = revenue_by_month(entries)
    lines = [f"{key}  {format_money(total)}" for key, total in months.items()]
    grand = sum_money(months.values(), REPORTING_CURRENCY)
    lines.append(f"TOTAL  {format_money(grand)}")
    return "\n".join(lines)
