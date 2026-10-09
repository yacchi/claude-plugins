from datetime import date

import pytest

from ledger import (
    ParseError,
    ValidationError,
    days_in_month,
    is_leap_year,
    iter_month_segments,
    month_key,
    parse_date,
)


def test_parse_date():
    assert parse_date(" 2025-02-28 ") == date(2025, 2, 28)
    for bad in ("2025-2-01", "2025-02-30", "01/02/2025"):
        with pytest.raises(ParseError):
            parse_date(bad)


def test_is_leap_year():
    years = [2024, 2025, 2000, 1900]
    assert [is_leap_year(y) for y in years] == [True, False, True, False]


def test_days_in_month():
    cases = [(2025, 1, 31), (2024, 2, 29), (2025, 2, 28), (2025, 3, 31), (2025, 4, 30), (2025, 6, 30)]
    assert [days_in_month(y, m) for y, m, _ in cases] == [d for _, _, d in cases]


def test_days_in_month_rejects_bad_month():
    with pytest.raises(ValueError):
        days_in_month(2025, 13)


def test_month_key():
    assert month_key(date(2025, 3, 9)) == "2025-03"


def test_iter_month_segments():
    assert list(iter_month_segments(date(2025, 1, 25), date(2025, 3, 5))) == [
        (date(2025, 1, 25), date(2025, 1, 31)),
        (date(2025, 2, 1), date(2025, 2, 28)),
        (date(2025, 3, 1), date(2025, 3, 5)),
    ]
    with pytest.raises(ValidationError):
        list(iter_month_segments(date(2025, 3, 5), date(2025, 3, 4)))
