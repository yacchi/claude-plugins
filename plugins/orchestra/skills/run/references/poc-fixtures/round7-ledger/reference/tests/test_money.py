from decimal import Decimal

import pytest

from ledger import CurrencyMismatchError, Money, ValidationError, format_money, round_amount, sum_money


def test_round_amount():
    cases = [("1.234", "USD", "1.23"), ("1.236", "USD", "1.24"), ("1.2344", "KWD", "1.234"), ("1234.4", "JPY", "1234")]
    for amount, currency, expected in cases:
        assert round_amount(Decimal(amount), currency) == Decimal(expected), (amount, currency)


def test_format_money():
    cases = [
        ("-1234.5", "USD", "-1,234.50 USD"),
        ("1234567.891", "USD", "1,234,567.89 USD"),
        ("1234.6", "JPY", "1,235 JPY"),
        ("-0.001", "USD", "0.00 USD"),
        ("12.3456", "KWD", "12.346 KWD"),
    ]
    assert [format_money(Money.of(a, c)) for a, c, _ in cases] == [t for _, _, t in cases]


def test_equality_is_numeric():
    assert Money.of("1.5", "USD") == Money.of("1.50", "USD")
    assert Money.of("1.5", "USD") != Money.of("1.5", "EUR")


def test_arithmetic_requires_same_currency():
    assert Money.of("1.10", "USD") + Money.of("2.205", "USD") == Money.of("3.305", "USD")
    with pytest.raises(CurrencyMismatchError):
        Money.of("1", "USD") + Money.of("1", "EUR")


def test_rejects_float():
    with pytest.raises(ValidationError):
        Money.of(1.5, "USD")


def test_sum_money_empty():
    assert sum_money([], "JPY") == Money.zero("JPY")
