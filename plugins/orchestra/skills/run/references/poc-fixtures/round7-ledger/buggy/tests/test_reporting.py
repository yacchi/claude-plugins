from datetime import date

from ledger import (
    Entry,
    Money,
    TaxSummaryRow,
    customer_balances,
    read_entries,
    render_revenue_report,
    revenue_by_month,
    tax_summary,
)

HEADER = "date,customer,region,description,amount,currency,kind\n"


def usd(value):
    return Money.of(value, "USD")


def test_revenue_by_month_converts_and_orders():
    entries = read_entries(
        HEADER
        + "2025-02-03,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-01-15,bolt,NORTH,Hosting,100.00,EUR,charge\n"
        + "2024-12-30,kaze,EXEMPT,Setup,10000,JPY,charge\n"
        + "2025-02-11,acme,NORTH,Support,-30.00,USD,adjustment\n"
    )
    result = revenue_by_month(entries)
    assert list(result) == ["2024-12", "2025-01", "2025-02"]
    assert result == {"2024-12": usd("65.00"), "2025-01": usd("110.00"), "2025-02": usd("70.00")}


def test_customer_balances_order():
    def e(customer, amount):
        return Entry(date(2025, 1, 1), customer, "NORTH", "x", usd(amount))

    rows = customer_balances([e("zed", "10.00"), e("amy", "10.00"), e("bob", "25.00"), e("cat", "-5.00")])
    assert rows == [("bob", usd("25.00")), ("amy", usd("10.00")), ("zed", usd("10.00")), ("cat", usd("-5.00"))]


def test_tax_summary():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-01-06,bolt,NORTH,Hosting,40.00,EUR,charge\n"
        + "2025-01-07,acme,NORTH,Hosting,20.00,USD,charge\n"
        + "2023-05-01,kaze,EAST,Setup,1000,JPY,charge\n"
        + "2025-01-08,kaze,EXEMPT,Setup,1000,JPY,charge\n"
    )
    assert tax_summary(entries) == [
        TaxSummaryRow("EAST", "JPY", Money.of("1000", "JPY"), Money.of("80", "JPY")),
        TaxSummaryRow("EXEMPT", "JPY", Money.of("1000", "JPY"), Money.of("0", "JPY")),
        TaxSummaryRow("NORTH", "EUR", Money.of("40.00", "EUR"), Money.of("4.00", "EUR")),
        TaxSummaryRow("NORTH", "USD", usd("120.00"), usd("12.00")),
    ]


def test_render_revenue_report():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,\"1,500.00\",USD,charge\n"
        + "2025-02-05,acme,NORTH,Hosting,250.00,GBP,charge\n"
    )
    assert render_revenue_report(entries) == (
        "2025-01  1,500.00 USD\n"
        "2025-02  312.50 USD\n"
        "TOTAL  1,812.50 USD"
    )


LEDGER_WITH_CREDITS = (
    HEADER
    + "2025-02-03,acme,NORTH,Hosting,100.00,USD,charge\n"
    + "2025-01-15,bolt,NORTH,Hosting,100.00,EUR,charge\n"
    + "2025-02-10,acme,NORTH,Refund,(150.00),USD,credit\n"
    + "2025-02-12,bolt,NORTH,Hosting,\"1,000.00\",USD,charge\n"
)


def test_revenue_by_month_nets_credit_notes():
    result = revenue_by_month(read_entries(LEDGER_WITH_CREDITS))
    assert result == {"2025-01": usd("110.00"), "2025-02": usd("950.00")}


def test_customer_balances_net_of_credits():
    rows = customer_balances(read_entries(LEDGER_WITH_CREDITS))
    assert rows == [("bolt", usd("1110.00")), ("acme", usd("-50.00"))]
