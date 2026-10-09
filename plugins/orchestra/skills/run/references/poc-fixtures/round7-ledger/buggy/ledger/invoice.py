"""Per-customer invoices built from ledger entries."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date
from typing import Iterable

from .errors import ValidationError
from .models import Entry
from .money import Money, sum_money
from .tax import tax_for


@dataclass(frozen=True)
class InvoiceLine:
    date: date
    description: str
    region: str
    kind: str
    amount: Money
    tax: Money

    @property
    def total(self) -> Money:
        return self.amount + self.tax


@dataclass(frozen=True)
class Invoice:
    customer: str
    currency: str
    lines: tuple[InvoiceLine, ...]

    @property
    def subtotal(self) -> Money:
        return sum_money((line.amount for line in self.lines), self.currency)

    @property
    def tax(self) -> Money:
        return sum_money((line.tax for line in self.lines), self.currency)

    @property
    def total(self) -> Money:
        return self.subtotal + self.tax


def _line(entry: Entry) -> InvoiceLine:
    return InvoiceLine(
        date=entry.date,
        description=entry.description,
        region=entry.region,
        kind=entry.kind,
        amount=entry.amount,
        tax=tax_for(entry.amount, entry.region, entry.date),
    )


def build_invoice(customer: str, entries: Iterable[Entry]) -> Invoice:
    """Invoice ``customer`` for their entries among ``entries``.

    Lines are ordered by date; entries on the same date keep their input order.
    """
    own = [entry for entry in entries if entry.customer == customer]
    if not own:
        raise ValidationError(f"no entries for customer {customer!r}")
    currencies = {entry.currency for entry in own}
    if len(currencies) > 1:
        raise ValidationError(
            f"customer {customer!r} has entries in several currencies: {', '.join(sorted(currencies))}"
        )
    ordered = sorted(own, key=lambda entry: entry.date)
    return Invoice(
        customer=customer,
        currency=own[0].currency,
        lines=tuple(_line(entry) for entry in ordered),
    )


def render_invoice(invoice: Invoice) -> str:
    """Plain-text rendering used for e-mail bodies."""
    rows = [f"Invoice for {invoice.customer} ({invoice.currency})"]
    for line in invoice.lines:
        rows.append(
            f"{line.date.isoformat()}  {line.description:<30}  {line.amount}  tax {line.tax}"
        )
    rows.append(f"Subtotal: {invoice.subtotal}")
    rows.append(f"Tax: {invoice.tax}")
    rows.append(f"Total: {invoice.total}")
    return "\n".join(rows)
