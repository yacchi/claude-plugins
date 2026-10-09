"""Hidden grader for round7-ledger. Each test is one check; ids are <family><nn>.

Families: R = shared rounding rule, P = calendar/proration, C = amount parsing,
K = tax-rate memoization, G = regression of behaviour that already works.
"""

from datetime import date
from decimal import Decimal

import pytest

import ledger
from ledger import (
    Entry,
    Money,
    ParseError,
    Subscription,
    TaxSummaryRow,
    ValidationError,
    aging,
    apply_discount,
    build_invoice,
    customer_balances,
    days_in_month,
    format_money,
    line_amount,
    parse_amount,
    prorate,
    rate_for,
    read_entries,
    revenue_by_month,
    round_amount,
    run_checks,
    statement,
    tax_for,
    tax_summary,
    to_usd,
    write_entries,
)

HEADER = "date,customer,region,description,amount,currency,kind\n"


def usd(value):
    return Money.of(value, "USD")


def entry(day, amount, customer="acme", region="NORTH", currency="USD", kind="charge", description="x"):
    return Entry(day, customer, region, description, Money.of(amount, currency), kind)


# --- R: one rounding rule (half away from zero, currency minor units) everywhere ---------


def test_R01_round_amount_ties():
    assert round_amount(Decimal("0.145"), "USD") == Decimal("0.15")
    assert round_amount(Decimal("-0.145"), "USD") == Decimal("-0.15")
    assert round_amount(Decimal("4.5"), "JPY") == Decimal("5")
    assert round_amount(Decimal("2.0025"), "KWD") == Decimal("2.003")


def test_R02_tax_tie_in_yen():
    assert tax_for(Money.of("45", "JPY"), "NORTH", date(2025, 1, 15)) == Money.of("5", "JPY")


def test_R03_tax_tie_in_dinar():
    assert tax_for(Money.of("0.125", "KWD"), "NORTH", date(2025, 1, 15)) == Money.of("0.013", "KWD")


def test_R04_discount_tie():
    result = apply_discount(usd("2.50"), 5)
    assert (result.discount, result.net) == (usd("0.13"), usd("2.37"))


def test_R05_line_amount_tie():
    assert line_amount(usd("0.25"), "0.5") == usd("0.13")


def test_R06_proration_tie():
    assert prorate(usd("20.25"), date(2025, 4, 16), date(2025, 4, 30)) == usd("10.13")


def test_R07_fx_tie():
    assert to_usd(Money.of("2.15", "EUR")) == usd("2.37")
    assert to_usd(Money.of("50", "JPY")) == usd("0.33")


def test_R08_format_tie():
    assert format_money(usd("2.345")) == "2.35 USD"
    assert format_money(Money.of("-6.5", "JPY")) == "-7 JPY"


def test_R09_revenue_rounds_each_conversion():
    entries = [
        entry(date(2025, 1, 5), "1.15", currency="EUR"),
        entry(date(2025, 1, 6), "1.15", currency="EUR", customer="bolt"),
    ]
    assert revenue_by_month(entries) == {"2025-01": usd("2.54")}


# --- P: Gregorian month lengths drive proration ------------------------------------------


def test_P01_month_lengths_second_half_of_year():
    assert [days_in_month(2025, m) for m in range(7, 13)] == [31, 31, 30, 31, 30, 31]


def test_P02_century_leap_rule():
    assert (days_in_month(2100, 2), days_in_month(1900, 2), days_in_month(2000, 2)) == (28, 28, 29)


def test_P03_prorate_september():
    assert prorate(usd("30.00"), date(2025, 9, 11), date(2025, 9, 30)) == usd("20.00")


def test_P04_prorate_october():
    assert prorate(usd("31.00"), date(2025, 10, 1), date(2025, 10, 10)) == usd("10.00")


def test_P05_prorate_november():
    assert prorate(usd("60.00"), date(2025, 11, 21), date(2025, 11, 30)) == usd("20.00")


def test_P06_prorate_across_year_boundary():
    assert prorate(usd("31.00"), date(2025, 12, 22), date(2026, 1, 10)) == usd("20.00")


def test_P07_prorate_february_2100():
    assert prorate(usd("28.00"), date(2100, 2, 15), date(2100, 2, 28)) == usd("14.00")


def test_P08_full_august_is_monthly_price():
    assert prorate(usd("49.99"), date(2025, 8, 1), date(2025, 8, 31)) == usd("49.99")


# --- C: parenthesised amounts are negative, at the parser ---------------------------------


def test_C01_parse_parenthesised():
    assert parse_amount("(150.00)") == Decimal("-150.00")


def test_C02_parse_parenthesised_grouped():
    assert parse_amount(" (1,234.50) ") == Decimal("-1234.50")


def test_C03_read_entries_credit_amount():
    (row,) = read_entries(HEADER + "2025-03-04,acme,NORTH,Refund,(150.00),USD,credit\n")
    assert row.amount == usd("-150.00")


def test_C04_revenue_with_parenthesised_adjustment():
    entries = read_entries(
        HEADER
        + "2025-03-01,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-03-05,acme,NORTH,Fix,(20.00),USD,adjustment\n"
    )
    assert revenue_by_month(entries) == {"2025-03": usd("80.00")}


def test_C05_balances_with_parenthesised_adjustment():
    entries = read_entries(
        HEADER
        + "2025-03-01,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-03-05,acme,NORTH,Fix,(120.00),USD,adjustment\n"
        + "2025-03-06,bolt,NORTH,Hosting,10.00,USD,charge\n"
    )
    assert customer_balances(entries) == [("bolt", usd("10.00")), ("acme", usd("-20.00"))]


def test_C06_tax_summary_with_parenthesised_credit():
    entries = read_entries(
        HEADER
        + "2025-03-01,acme,NORTH,Hosting,200.00,USD,charge\n"
        + "2025-03-04,acme,NORTH,Refund,(150.00),USD,credit\n"
    )
    assert tax_summary(entries) == [TaxSummaryRow("NORTH", "USD", usd("50.00"), usd("5.00"))]


def test_C07_invoice_from_csv_with_credit():
    entries = read_entries(
        HEADER
        + "2025-03-01,acme,NORTH,Hosting,\"1,000.00\",USD,charge\n"
        + "2025-03-04,acme,NORTH,Refund,(100.00),USD,credit\n"
    )
    invoice = build_invoice("acme", entries)
    assert (invoice.subtotal, invoice.tax, invoice.total) == (usd("900.00"), usd("90.00"), usd("990.00"))


def test_C08_checks_accept_parenthesised_credit():
    entries = read_entries(HEADER + "2025-03-04,acme,NORTH,Refund,(150.00),USD,credit\n")
    assert run_checks(entries, today=date(2025, 4, 1)) == []


# --- K: rate_for depends only on its arguments --------------------------------------------


def test_K01_rate_before_then_after_revision():
    assert rate_for("SOUTH", date(2025, 6, 30)) == Decimal("0.06")
    assert rate_for("SOUTH", date(2025, 7, 1)) == Decimal("0.075")


def test_K02_rate_after_then_before_revision():
    assert rate_for("SOUTH", date(2025, 7, 1)) == Decimal("0.075")
    assert rate_for("SOUTH", date(2025, 6, 30)) == Decimal("0.06")


def test_K03_east_revision_same_year():
    assert rate_for("EAST", date(2026, 3, 31)) == Decimal("0.05")
    assert rate_for("EAST", date(2026, 4, 1)) == Decimal("0.09")


def test_K04_invoice_spanning_revision():
    invoice = build_invoice(
        "acme",
        [entry(date(2025, 6, 15), "100.00", region="SOUTH"), entry(date(2025, 7, 15), "100.00", region="SOUTH")],
    )
    assert [line.tax for line in invoice.lines] == [usd("6.00"), usd("7.50")]
    assert invoice.tax == usd("13.50")


def test_K05_tax_summary_spanning_revision():
    entries = [entry(date(2025, 6, 10), "200.00", region="SOUTH"), entry(date(2025, 8, 10), "200.00", region="SOUTH")]
    assert tax_summary(entries) == [TaxSummaryRow("SOUTH", "USD", usd("400.00"), usd("27.00"))]


def test_K06_tax_after_unrelated_lookup():
    rate_for("SOUTH", date(2025, 1, 1))
    assert tax_for(usd("200.00"), "SOUTH", date(2025, 12, 31)) == usd("15.00")


# --- G: regression checks for behaviour that already works --------------------------------


def test_G01_parse_amount_rejects_malformed():
    for text in ["(-5)", "-(5)", "(5", "1,2345", "1 2", "--5", "5.", "(1,23.00)"]:
        with pytest.raises(ParseError):
            parse_amount(text)


def test_G02_parse_minus_grouped():
    assert parse_amount("-1,234.50") == Decimal("-1234.50")
    assert parse_amount("12,345,678") == Decimal("12345678")


def test_G03_format_money_non_ties():
    assert format_money(Money.of("-98765.432", "USD")) == "-98,765.43 USD"
    assert format_money(Money.of("-0.4", "JPY")) == "0 JPY"
    assert format_money(Money.of("7", "KWD")) == "7.000 KWD"


def test_G04_tax_non_tie():
    assert tax_for(usd("33.33"), "NORTH", date(2025, 2, 1)) == usd("3.33")
    assert tax_for(Money.of("1234", "JPY"), "NORTH", date(2025, 2, 1)) == Money.of("123", "JPY")


def test_G05_invoice_order_and_filtering():
    entries = [
        entry(date(2025, 2, 20), "10.00", description="b"),
        entry(date(2025, 2, 1), "20.00", customer="bolt"),
        entry(date(2025, 2, 10), "30.00", description="a"),
        entry(date(2025, 2, 20), "40.00", description="c"),
    ]
    invoice = build_invoice("acme", entries)
    assert [line.description for line in invoice.lines] == ["a", "b", "c"]
    assert invoice.total == usd("88.00")


def test_G06_revenue_months_chronological():
    entries = [
        entry(date(2025, 1, 3), "1.00"),
        entry(date(2024, 12, 31), "2.00"),
        entry(date(2024, 2, 1), "4.00"),
    ]
    assert list(revenue_by_month(entries)) == ["2024-02", "2024-12", "2025-01"]


def test_G07_balances_tie_order():
    entries = [entry(date(2025, 1, 1), "5.00", customer=c) for c in ("delta", "Bravo", "alpha")]
    assert [c for c, _ in customer_balances(entries)] == ["Bravo", "alpha", "delta"]


def test_G08_to_usd_non_ties():
    assert to_usd(Money.of("3.333", "KWD")) == usd("10.83")
    assert to_usd(Money.of("19.99", "GBP")) == usd("24.99")


def test_G09_rate_errors():
    with pytest.raises(ValidationError):
        rate_for("NORTH", date(1999, 12, 31))
    with pytest.raises(ValidationError):
        rate_for("WEST", date(2025, 1, 1))


def test_G10_kind_never_changes_sign():
    entries = [
        entry(date(2025, 1, 5), "25.00", kind="credit"),
        entry(date(2025, 1, 6), "-5.00", kind="charge", customer="bolt"),
    ]
    assert revenue_by_month(entries) == {"2025-01": usd("20.00")}
    assert customer_balances(entries) == [("acme", usd("25.00")), ("bolt", usd("-5.00"))]


def test_G11_proration_known_months():
    assert prorate(usd("30.00"), date(2025, 4, 1), date(2025, 4, 30)) == usd("30.00")
    assert prorate(usd("58.00"), date(2024, 2, 1), date(2024, 2, 10)) == usd("20.00")
    assert prorate(usd("31.00"), date(2025, 1, 31), date(2025, 2, 14)) == usd("16.50")


def test_G12_csv_line_numbers_and_round_trip():
    with pytest.raises(ParseError) as info:
        read_entries(HEADER + "\n" + "2025-01-05,acme,NORTH,x,abc,USD,charge\n")
    assert str(info.value).startswith("line 3: ")
    entries = [entry(date(2025, 1, 5), "-1.250", currency="KWD"), entry(date(2025, 1, 6), "7", currency="JPY")]
    text = write_entries(entries)
    assert text.splitlines()[1:] == [
        "2025-01-05,acme,NORTH,x,-1.250,KWD,charge",
        "2025-01-06,acme,NORTH,x,7,JPY,charge",
    ]
    assert read_entries(text) == entries
