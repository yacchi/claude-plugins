from datetime import date

from ledger import Finding, read_entries, run_checks

HEADER = "date,customer,region,description,amount,currency,kind\n"


def test_duplicate_rows_are_reported():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-01-06,acme,NORTH,Support,40.00,USD,charge\n"
    )
    findings = run_checks(entries, today=date(2025, 2, 1))
    assert [f.code for f in findings] == ["duplicate"]
    assert findings[0].customer == "acme"


def test_mixed_currency_and_future_dated():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-01-06,acme,NORTH,Hosting,90.00,EUR,charge\n"
        + "2025-03-01,bolt,NORTH,Hosting,10.00,USD,charge\n"
    )
    findings = run_checks(entries, today=date(2025, 2, 1))
    assert [(f.code, f.customer) for f in findings] == [
        ("mixed-currency", "acme"),
        ("future-dated", "bolt"),
    ]


def test_positive_credit_is_flagged():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Refund,25.00,USD,credit\n"
        + "2025-01-06,bolt,NORTH,Refund,-25.00,USD,credit\n"
    )
    findings = run_checks(entries, today=date(2025, 2, 1))
    assert findings == [
        Finding(
            code="positive-credit",
            customer="acme",
            message="credit 'Refund' on 2025-01-05 is not negative",
        )
    ]


def test_clean_ledger_has_no_findings():
    entries = read_entries(
        HEADER
        + "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
        + "2025-01-09,acme,NORTH,Refund,-30.00,USD,credit\n"
    )
    assert run_checks(entries, today=date(2025, 2, 1)) == []
