"""Ledger consistency checks run before month-end close."""

from __future__ import annotations

from collections import Counter
from dataclasses import dataclass
from datetime import date
from typing import Iterable

from .models import Entry


@dataclass(frozen=True)
class Finding:
    code: str
    customer: str
    message: str


def _duplicates(entries: list[Entry]) -> list[Finding]:
    keys = Counter(
        (e.date, e.customer, e.region, e.description, e.amount.amount, e.currency, e.kind)
        for e in entries
    )
    findings = []
    for key, count in sorted(keys.items(), key=lambda item: (item[0][1], item[0][0], item[0][3])):
        if count > 1:
            when, customer, _, description, _, _, _ = key
            findings.append(
                Finding(
                    code="duplicate",
                    customer=customer,
                    message=f"{count} identical entries on {when.isoformat()}: {description}",
                )
            )
    return findings


def _mixed_currencies(entries: list[Entry]) -> list[Finding]:
    seen: dict[str, set[str]] = {}
    for entry in entries:
        seen.setdefault(entry.customer, set()).add(entry.currency)
    return [
        Finding(
            code="mixed-currency",
            customer=customer,
            message=f"entries in {', '.join(sorted(codes))}",
        )
        for customer, codes in sorted(seen.items())
        if len(codes) > 1
    ]


def _future_dated(entries: list[Entry], today: date) -> list[Finding]:
    return [
        Finding(
            code="future-dated",
            customer=entry.customer,
            message=f"{entry.description} is dated {entry.date.isoformat()}",
        )
        for entry in sorted(entries, key=lambda e: (e.customer, e.date))
        if entry.date > today
    ]


def _credit_signs(entries: list[Entry]) -> list[Finding]:
    return [
        Finding(
            code="positive-credit",
            customer=entry.customer,
            message=f"credit {entry.description!r} on {entry.date.isoformat()} is not negative",
        )
        for entry in entries
        if entry.kind == "credit" and entry.amount.amount > 0
    ]


def run_checks(entries: Iterable[Entry], today: date) -> list[Finding]:
    """All findings, grouped by check in a fixed order."""
    items = list(entries)
    return (
        _duplicates(items)
        + _mixed_currencies(items)
        + _future_dated(items, today)
        + _credit_signs(items)
    )
