from datetime import date

from ledger import AGING_BUCKETS, Entry, Money, aging, statement


def usd(value):
    return Money.of(value, "USD")


def e(day, amount, customer="acme"):
    return Entry(day, customer, "NORTH", "x", usd(amount))


def test_running_balance():
    rows = statement("acme", [e(date(2025, 1, 9), "-20.00"), e(date(2025, 1, 2), "100.00"), e(date(2025, 1, 3), "5.00", "bolt")])
    assert [(r.date, r.balance) for r in rows] == [
        (date(2025, 1, 2), usd("100.00")),
        (date(2025, 1, 9), usd("80.00")),
    ]


def test_aging_bucket_boundaries():
    as_of = date(2025, 4, 30)
    entries = [
        e(date(2025, 3, 31), "1.00"),   # 30 days
        e(date(2025, 3, 30), "2.00"),   # 31 days
        e(date(2025, 3, 1), "4.00"),    # 60 days
        e(date(2025, 2, 28), "8.00"),   # 61 days
        e(date(2025, 1, 30), "16.00"),  # 90 days
        e(date(2025, 1, 29), "32.00"),  # 91 days
        e(date(2025, 5, 1), "64.00"),   # after as_of
    ]
    (row,) = aging(entries, as_of)
    assert tuple(row.buckets) == AGING_BUCKETS
    assert row.buckets == {
        "0-30": usd("1.00"),
        "31-60": usd("6.00"),
        "61-90": usd("24.00"),
        "90+": usd("32.00"),
    }
    assert row.total == usd("63.00")


def test_statement_empty():
    assert statement("nobody", [e(date(2025, 1, 2), "1.00")]) == []
