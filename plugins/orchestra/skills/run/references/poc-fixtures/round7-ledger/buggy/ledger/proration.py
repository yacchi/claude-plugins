"""Proration of monthly subscription prices over partial periods."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date
from decimal import Decimal, localcontext
from fractions import Fraction

from .dates import days_in_month, inclusive_days, iter_month_segments
from .models import Entry, Subscription
from .money import Money


@dataclass(frozen=True)
class Segment:
    start: date
    end: date
    active_days: int
    month_days: int

    @property
    def fraction(self) -> Fraction:
        return Fraction(self.active_days, self.month_days)


def segments(start: date, end: date) -> list[Segment]:
    """Break ``start..end`` (inclusive) into calendar-month segments."""
    result = []
    for seg_start, seg_end in iter_month_segments(start, end):
        result.append(
            Segment(
                start=seg_start,
                end=seg_end,
                active_days=inclusive_days(seg_start, seg_end),
                month_days=days_in_month(seg_start.year, seg_start.month),
            )
        )
    return result


def _to_decimal(value: Fraction) -> Decimal:
    with localcontext() as ctx:
        ctx.prec = 60
        return Decimal(value.numerator) / Decimal(value.denominator)


def prorate(monthly_price: Money, start: date, end: date) -> Money:
    """Charge for ``start..end`` inclusive at ``monthly_price`` per calendar month.

    Each calendar month contributes ``price * active_days / days_in_month``;
    the contributions are added exactly and the sum is rounded once.
    """
    exact = Fraction(0)
    price = Fraction(monthly_price.amount)
    for segment in segments(start, end):
        exact += price * segment.fraction
    return Money(_to_decimal(exact), monthly_price.currency).rounded()


def subscription_entry(subscription: Subscription, start: date, end: date) -> Entry:
    """Ledger entry charging ``subscription`` for ``start..end`` inclusive."""
    amount = prorate(subscription.monthly_price, start, end)
    return Entry(
        date=start,
        customer=subscription.customer,
        region=subscription.region,
        description=f"{subscription.plan} {start.isoformat()}..{end.isoformat()}",
        amount=amount,
        kind="charge",
    )
