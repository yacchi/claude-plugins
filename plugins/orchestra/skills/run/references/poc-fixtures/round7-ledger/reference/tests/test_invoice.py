from datetime import date

import pytest

from ledger import Entry, Money, ValidationError, build_invoice, render_invoice


def usd(value):
    return Money.of(value, "USD")


def entry(day, amount, customer="acme", region="NORTH", description="Hosting", currency="USD"):
    return Entry(day, customer, region, description, Money.of(amount, currency), "charge")


def test_invoice_totals():
    invoice = build_invoice(
        "acme",
        [entry(date(2025, 1, 10), "100.00"), entry(date(2025, 1, 20), "50.00")],
    )
    assert [line.tax for line in invoice.lines] == [usd("10.00"), usd("5.00")]
    assert invoice.subtotal == usd("150.00")
    assert invoice.tax == usd("15.00")
    assert invoice.total == usd("165.00")


def test_lines_sorted_by_date_stable():
    entries = [
        entry(date(2025, 1, 20), "1.00", description="b"),
        entry(date(2025, 1, 10), "2.00", description="a"),
        entry(date(2025, 1, 20), "3.00", description="c"),
    ]
    invoice = build_invoice("acme", entries)
    assert [line.description for line in invoice.lines] == ["a", "b", "c"]


def test_ignores_other_customers():
    invoice = build_invoice(
        "acme",
        [entry(date(2025, 1, 10), "100.00"), entry(date(2025, 1, 11), "999.00", customer="bolt")],
    )
    assert invoice.subtotal == usd("100.00")


def test_south_invoice():
    invoice = build_invoice("acme", [entry(date(2025, 3, 10), "200.00", region="SOUTH")])
    assert invoice.tax == usd("12.00")
    assert invoice.total == usd("212.00")


def test_rejects_mixed_currencies_and_empty():
    with pytest.raises(ValidationError):
        build_invoice(
            "acme",
            [entry(date(2025, 1, 10), "1.00"), entry(date(2025, 1, 11), "1.00", currency="EUR")],
        )
    with pytest.raises(ValidationError):
        build_invoice("nobody", [entry(date(2025, 1, 10), "1.00")])


def test_render_invoice():
    invoice = build_invoice("acme", [entry(date(2025, 1, 10), "1200.00")])
    lines = render_invoice(invoice).splitlines()
    assert lines[0] == "Invoice for acme (USD)"
    assert lines[-3:] == [
        "Subtotal: 1,200.00 USD",
        "Tax: 120.00 USD",
        "Total: 1,320.00 USD",
    ]
