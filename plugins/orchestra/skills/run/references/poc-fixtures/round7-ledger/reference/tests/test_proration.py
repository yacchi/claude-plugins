from datetime import date

import pytest

from ledger import Money, Subscription, ValidationError, prorate, segments, subscription_entry


def usd(value):
    return Money.of(value, "USD")


def test_partial_june():
    assert prorate(usd("30.00"), date(2025, 6, 11), date(2025, 6, 30)) == usd("20.00")


def test_full_month_is_monthly_price():
    assert prorate(usd("49.99"), date(2025, 1, 1), date(2025, 1, 31)) == usd("49.99")


def test_leap_february():
    assert prorate(usd("29.00"), date(2024, 2, 15), date(2024, 2, 29)) == usd("15.00")


def test_common_february():
    assert prorate(usd("28.00"), date(2025, 2, 1), date(2025, 2, 14)) == usd("14.00")


def test_span_across_months_rounds_once():
    assert prorate(usd("62.00"), date(2025, 3, 20), date(2025, 4, 10)) == usd("44.67")


def test_partial_august():
    assert prorate(usd("31.00"), date(2025, 8, 10), date(2025, 8, 31)) == usd("22.00")


def test_segments():
    result = segments(date(2025, 1, 25), date(2025, 3, 5))
    assert [(s.active_days, s.month_days) for s in result] == [(7, 31), (28, 28), (5, 31)]


def test_end_before_start():
    with pytest.raises(ValidationError):
        prorate(usd("10.00"), date(2025, 6, 2), date(2025, 6, 1))


def test_subscription_entry():
    sub = Subscription("acme", "pro", "NORTH", usd("30.00"))
    item = subscription_entry(sub, date(2025, 6, 21), date(2025, 6, 30))
    assert item.amount == usd("10.00")
    assert item.description == "pro 2025-06-21..2025-06-30"
    assert (item.customer, item.region, item.kind, item.date) == ("acme", "NORTH", "charge", date(2025, 6, 21))
