"""Calendar helpers (proleptic Gregorian calendar)."""

from __future__ import annotations

import re
from datetime import date, timedelta
from typing import Iterator

from .errors import ParseError, ValidationError

_ISO_DATE = re.compile(r"(\d{4})-(\d{2})-(\d{2})")
_MONTH_LENGTHS = (31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31)


def parse_date(text: str, line: int | None = None) -> date:
    """Parse a strict ``YYYY-MM-DD`` date."""
    match = _ISO_DATE.fullmatch(text.strip())
    if match is None:
        raise ParseError(f"invalid date {text!r} (expected YYYY-MM-DD)", line)
    year, month, day = (int(part) for part in match.groups())
    try:
        return date(year, month, day)
    except ValueError as exc:
        raise ParseError(f"invalid date {text!r}: {exc}", line) from None


def is_leap_year(year: int) -> bool:
    return year % 4 == 0 and (year % 100 != 0 or year % 400 == 0)


def days_in_month(year: int, month: int) -> int:
    if not 1 <= month <= 12:
        raise ValueError(f"month out of range: {month}")
    if month == 2 and is_leap_year(year):
        return 29
    return _MONTH_LENGTHS[month - 1]


def month_start(d: date) -> date:
    return d.replace(day=1)


def next_month_start(d: date) -> date:
    if d.month == 12:
        return date(d.year + 1, 1, 1)
    return date(d.year, d.month + 1, 1)


def month_end(d: date) -> date:
    return next_month_start(d) - timedelta(days=1)


def month_key(d: date) -> str:
    """Return the ``YYYY-MM`` key used to group by calendar month."""
    return f"{d.year:04d}-{d.month:02d}"


def inclusive_days(start: date, end: date) -> int:
    """Number of days from ``start`` to ``end``, both included."""
    return (end - start).days + 1


def iter_month_segments(start: date, end: date) -> Iterator[tuple[date, date]]:
    """Split ``start..end`` (inclusive) into per-calendar-month segments."""
    if end < start:
        raise ValidationError(f"end {end} is before start {start}")
    cursor = start
    while cursor <= end:
        segment_end = min(end, month_end(cursor))
        yield cursor, segment_end
        cursor = segment_end + timedelta(days=1)
