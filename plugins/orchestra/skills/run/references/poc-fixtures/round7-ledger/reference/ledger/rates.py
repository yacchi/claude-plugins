"""Sales-tax rate tables per region, with effective dates."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date
from decimal import Decimal

from .errors import ValidationError


@dataclass(frozen=True)
class RateRevision:
    effective_from: date
    rate: Decimal


# Each region's revisions are listed oldest first.
_TABLE: dict[str, tuple[RateRevision, ...]] = {
    "NORTH": (RateRevision(date(2000, 1, 1), Decimal("0.10")),),
    "SOUTH": (
        RateRevision(date(2000, 1, 1), Decimal("0.06")),
        RateRevision(date(2025, 7, 1), Decimal("0.075")),
    ),
    "EAST": (
        RateRevision(date(2000, 1, 1), Decimal("0.08")),
        RateRevision(date(2024, 10, 1), Decimal("0.05")),
        RateRevision(date(2026, 4, 1), Decimal("0.09")),
    ),
    "EXEMPT": (RateRevision(date(2000, 1, 1), Decimal("0")),),
}

_cache: dict[tuple[str, date], Decimal] = {}


def known_regions() -> tuple[str, ...]:
    return tuple(sorted(_TABLE))


def revisions(region: str) -> tuple[RateRevision, ...]:
    try:
        return _TABLE[region]
    except KeyError:
        raise ValidationError(f"unknown tax region: {region!r}") from None


def _lookup(region: str, on: date) -> Decimal:
    current: RateRevision | None = None
    for revision in revisions(region):
        if revision.effective_from <= on:
            current = revision
        else:
            break
    if current is None:
        raise ValidationError(f"no tax rate for {region} on {on.isoformat()}")
    return current.rate


def rate_for(region: str, on: date) -> Decimal:
    """Return the tax rate in effect for ``region`` on ``on``."""
    key = (region, on)
    cached = _cache.get(key)
    if cached is not None:
        return cached
    rate = _lookup(region, on)
    _cache[key] = rate
    return rate


def clear_rate_cache() -> None:
    _cache.clear()
