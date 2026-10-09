from datetime import date
from decimal import Decimal

import pytest

from ledger import Entry, Money, ParseError, parse_amount, read_entries

HEADER = "date,customer,region,description,amount,currency,kind\n"


def test_reads_rows_in_file_order():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-01-03,bolt,SOUTH,Setup fee,250,EUR,charge\n"
    )
    assert entries == [
        Entry(date(2025, 1, 5), "acme", "NORTH", "Hosting", Money.of("100.00", "USD"), "charge"),
        Entry(date(2025, 1, 3), "bolt", "SOUTH", "Setup fee", Money.of("250", "EUR"), "charge"),
    ]


def test_quoted_amount_with_thousands_separator():
    (entry,) = read_entries(HEADER + '2025-01-05,acme,NORTH,Licence,"12,500.00",USD,charge\n')
    assert entry.amount == Money.of("12500.00", "USD")


def test_minus_sign_amount():
    (entry,) = read_entries(HEADER + "2025-01-05,acme,NORTH,Refund,-12.50,USD,credit\n")
    assert entry.amount == Money.of("-12.50", "USD")
    assert entry.kind == "credit"


def test_parse_amount_valid():
    cases = {"42": "42", "  42  ": "42", "1,234,567.89": "1234567.89", "-0.75": "-0.75"}
    assert {text: parse_amount(text) for text in cases} == {
        text: Decimal(value) for text, value in cases.items()
    }


def test_parse_amount_invalid():
    for text in ["1,23.00", "12,34", "+5", ".5", ""]:
        with pytest.raises(ParseError):
            parse_amount(text)


def test_error_reports_line_number():
    text = (
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-13-05,acme,NORTH,Hosting,100.00,USD,charge\n"
    )
    with pytest.raises(ParseError) as info:
        read_entries(text)
    assert str(info.value).startswith("line 3: ")


def test_rejects_unexpected_header():
    with pytest.raises(ParseError):
        read_entries("date,customer,amount\n2025-01-05,acme,1.00\n")


def test_rejects_unknown_region_and_currency():
    with pytest.raises(ParseError):
        read_entries(HEADER + "2025-01-05,acme,MOON,Hosting,1.00,USD,charge\n")
    with pytest.raises(ParseError):
        read_entries(HEADER + "2025-01-05,acme,NORTH,Hosting,1.00,CHF,charge\n")


def test_normalises_currency_and_default_kind():
    (entry,) = read_entries(HEADER + "2025-01-05,acme,NORTH,Hosting,1.00,usd,\n")
    assert entry.amount == Money.of("1.00", "USD")
    assert entry.kind == "charge"


def test_skips_blank_lines():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,1.00,USD,charge\n"
        + "\n"
        + "   \n"
        + "2025-01-06,acme,NORTH,Hosting,2.00,USD,charge\n"
    )
    assert [e.amount.amount for e in entries] == [Decimal("1.00"), Decimal("2.00")]
