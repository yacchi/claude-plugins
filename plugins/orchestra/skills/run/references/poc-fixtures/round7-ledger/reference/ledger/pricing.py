"""Line pricing and discounts."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal

from .errors import ValidationError
from .money import Money, to_decimal

_HUNDRED = Decimal(100)


@dataclass(frozen=True)
class Discounted:
    gross: Money
    discount: Money

    @property
    def net(self) -> Money:
        return self.gross - self.discount


def line_amount(unit_price: Money, quantity: Decimal | int | str) -> Money:
    """Price of ``quantity`` units at ``unit_price``, rounded to minor units."""
    qty = to_decimal(quantity)
    if qty < 0:
        raise ValidationError(f"quantity must not be negative: {qty}")
    return unit_price.times(qty).rounded()


def apply_discount(amount: Money, percent: Decimal | int | str) -> Discounted:
    """Apply a percentage discount; the discount amount is rounded to minor units."""
    pct = to_decimal(percent)
    if not Decimal(0) <= pct <= _HUNDRED:
        raise ValidationError(f"discount percent must be between 0 and 100: {pct}")
    discount = amount.times(pct / _HUNDRED).rounded()
    return Discounted(gross=amount, discount=discount)
