"""Conversion into the reporting currency (USD) using fixed book rates."""

from __future__ import annotations

from decimal import Decimal

from .currency import REPORTING_CURRENCY, get_currency
from .errors import ValidationError
from .money import Money

# Units of USD per one unit of the listed currency.
_BOOK_RATES: dict[str, Decimal] = {
    "USD": Decimal("1"),
    "EUR": Decimal("1.10"),
    "GBP": Decimal("1.25"),
    "JPY": Decimal("0.0065"),
    "KWD": Decimal("3.25"),
}


def book_rate(currency: str) -> Decimal:
    get_currency(currency)
    try:
        return _BOOK_RATES[currency]
    except KeyError:
        raise ValidationError(f"no book rate for {currency}") from None


def to_usd(money: Money) -> Money:
    """Convert ``money`` to USD at the book rate, rounded to cents."""
    if money.currency == REPORTING_CURRENCY:
        return money.rounded()
    converted = Money(money.amount * book_rate(money.currency), REPORTING_CURRENCY)
    return converted.rounded()
