"""Money value type and the package-wide rounding helper."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import ROUND_HALF_UP, Decimal, InvalidOperation
from typing import Iterable

from .currency import get_currency
from .errors import CurrencyMismatchError, ValidationError


def to_decimal(value: Decimal | int | str) -> Decimal:
    """Convert ``value`` to Decimal without going through float."""
    if isinstance(value, Decimal):
        return value
    if isinstance(value, bool) or isinstance(value, float):
        raise ValidationError(f"refusing to build a Decimal from {type(value).__name__}")
    try:
        return Decimal(value)
    except (InvalidOperation, ValueError):
        raise ValidationError(f"not a decimal number: {value!r}") from None


def round_amount(amount: Decimal, currency: str) -> Decimal:
    """Round ``amount`` to the minor units of ``currency``."""
    quantum = get_currency(currency).quantum
    return amount.quantize(quantum, rounding=ROUND_HALF_UP)


@dataclass(frozen=True)
class Money:
    amount: Decimal
    currency: str

    def __post_init__(self) -> None:
        object.__setattr__(self, "amount", to_decimal(self.amount))
        get_currency(self.currency)

    @classmethod
    def of(cls, amount: Decimal | int | str, currency: str) -> "Money":
        return cls(to_decimal(amount), currency)

    @classmethod
    def zero(cls, currency: str) -> "Money":
        return cls(Decimal(0), currency)

    def _check(self, other: "Money") -> None:
        if not isinstance(other, Money):
            raise TypeError(f"expected Money, got {type(other).__name__}")
        if other.currency != self.currency:
            raise CurrencyMismatchError(self.currency, other.currency)

    def __add__(self, other: "Money") -> "Money":
        self._check(other)
        return Money(self.amount + other.amount, self.currency)

    def __sub__(self, other: "Money") -> "Money":
        self._check(other)
        return Money(self.amount - other.amount, self.currency)

    def __neg__(self) -> "Money":
        return Money(-self.amount, self.currency)

    def times(self, factor: Decimal | int | str) -> "Money":
        """Multiply without rounding."""
        return Money(self.amount * to_decimal(factor), self.currency)

    def rounded(self) -> "Money":
        return Money(round_amount(self.amount, self.currency), self.currency)

    def is_negative(self) -> bool:
        return self.amount < 0

    def __str__(self) -> str:
        return format_money(self)


def sum_money(items: Iterable[Money], currency: str) -> Money:
    """Sum ``items``; an empty iterable gives zero in ``currency``."""
    total = Money.zero(currency)
    for item in items:
        total = total + item
    return total


def format_money(money: Money) -> str:
    """Render ``money`` as e.g. ``-1,234.50 USD``."""
    value = round_amount(money.amount, money.currency)
    places = get_currency(money.currency).minor_units
    if value == 0:
        value = abs(value)
    sign = "-" if value < 0 else ""
    text = f"{abs(value):,.{places}f}"
    return f"{sign}{text} {money.currency}"
