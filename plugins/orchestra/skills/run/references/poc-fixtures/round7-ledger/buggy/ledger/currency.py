"""Supported currencies and their minor units."""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal

from .errors import ValidationError


@dataclass(frozen=True)
class Currency:
    code: str
    minor_units: int
    name: str

    @property
    def quantum(self) -> Decimal:
        """The smallest representable step, e.g. Decimal('0.01') for USD."""
        return Decimal(1).scaleb(-self.minor_units)


_CURRENCIES: dict[str, Currency] = {
    c.code: c
    for c in (
        Currency("USD", 2, "US dollar"),
        Currency("EUR", 2, "Euro"),
        Currency("GBP", 2, "Pound sterling"),
        Currency("JPY", 0, "Japanese yen"),
        Currency("KWD", 3, "Kuwaiti dinar"),
    )
}

REPORTING_CURRENCY = "USD"


def get_currency(code: str) -> Currency:
    """Return the currency for ``code`` or raise ValidationError."""
    try:
        return _CURRENCIES[code]
    except KeyError:
        raise ValidationError(f"unsupported currency: {code!r}") from None


def is_supported(code: str) -> bool:
    return code in _CURRENCIES


def supported_codes() -> tuple[str, ...]:
    return tuple(sorted(_CURRENCIES))


def minor_units(code: str) -> int:
    return get_currency(code).minor_units
