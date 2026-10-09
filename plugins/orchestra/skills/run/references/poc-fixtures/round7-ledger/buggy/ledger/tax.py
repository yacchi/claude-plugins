"""Sales-tax computation."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date

from .money import Money
from .rates import rate_for


@dataclass(frozen=True)
class TaxedAmount:
    net: Money
    tax: Money

    @property
    def gross(self) -> Money:
        return self.net + self.tax


def tax_for(amount: Money, region: str, on: date) -> Money:
    """Tax due on ``amount`` booked in ``region`` on ``on``, rounded to minor units."""
    rate = rate_for(region, on)
    return amount.times(rate).rounded()


def apply_tax(amount: Money, region: str, on: date) -> TaxedAmount:
    return TaxedAmount(net=amount, tax=tax_for(amount, region, on))
