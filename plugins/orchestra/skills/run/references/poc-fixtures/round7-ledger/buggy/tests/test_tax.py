from datetime import date
from decimal import Decimal

import pytest

from ledger import Money, ValidationError, apply_tax, rate_for, tax_for


def usd(value):
    return Money.of(value, "USD")


def test_north_rate():
    assert tax_for(usd("100.00"), "NORTH", date(2025, 1, 15)) == usd("10.00")


def test_south_rate_before_revision():
    assert rate_for("SOUTH", date(2024, 11, 1)) == Decimal("0.06")
    assert tax_for(usd("50.00"), "SOUTH", date(2024, 11, 1)) == usd("3.00")


def test_exempt():
    assert tax_for(usd("123.45"), "EXEMPT", date(2025, 1, 15)) == usd("0.00")


def test_apply_tax_gross():
    taxed = apply_tax(usd("80.00"), "NORTH", date(2025, 1, 15))
    assert (taxed.net, taxed.tax, taxed.gross) == (usd("80.00"), usd("8.00"), usd("88.00"))


def test_half_cent_rounds_up():
    assert tax_for(usd("12.25"), "NORTH", date(2025, 1, 15)) == usd("1.23")


def test_negative_half_cent_rounds_away_from_zero():
    assert tax_for(usd("-12.25"), "NORTH", date(2025, 1, 15)) == usd("-1.23")


def test_south_rate_after_revision():
    assert tax_for(usd("200.00"), "SOUTH", date(2025, 9, 15)) == usd("15.00")


def test_unknown_region():
    with pytest.raises(ValidationError):
        rate_for("MOON", date(2025, 1, 1))
