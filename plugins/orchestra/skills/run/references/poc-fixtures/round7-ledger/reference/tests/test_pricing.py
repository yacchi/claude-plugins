import pytest

from ledger import Money, ValidationError, apply_discount, line_amount


def usd(value):
    return Money.of(value, "USD")


def test_line_amount():
    assert line_amount(usd("2.50"), 3) == usd("7.50")
    assert line_amount(usd("0.333"), 3) == usd("1.00")


def test_line_amount_rejects_negative_quantity():
    with pytest.raises(ValidationError):
        line_amount(usd("1.00"), -1)


def test_discount():
    result = apply_discount(usd("80.00"), 10)
    assert result.discount == usd("8.00")
    assert result.net == usd("72.00")


def test_discount_percent_range():
    with pytest.raises(ValidationError):
        apply_discount(usd("80.00"), 150)
