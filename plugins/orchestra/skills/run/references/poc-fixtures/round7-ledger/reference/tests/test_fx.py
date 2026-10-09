import pytest

from ledger import Money, ValidationError, to_usd


def test_to_usd():
    cases = [
        ("100.00", "EUR", "110.00"),
        ("10000", "JPY", "65.00"),
        ("2.000", "KWD", "6.50"),
        ("10.00", "GBP", "12.50"),
        ("1.234", "USD", "1.23"),
    ]
    for amount, currency, usd in cases:
        assert to_usd(Money.of(amount, currency)) == Money.of(usd, "USD"), (amount, currency)


def test_unsupported_currency():
    with pytest.raises(ValidationError):
        Money.of("1", "CHF")
