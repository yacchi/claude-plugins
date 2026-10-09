"""CSV ledger import and export."""

from __future__ import annotations

import csv
import io
import re
from decimal import Decimal
from typing import Iterable, TextIO

from .currency import get_currency, is_supported
from .dates import parse_date
from .errors import ParseError, ValidationError
from .models import KINDS, Entry
from .money import Money, round_amount
from .rates import known_regions

COLUMNS = ("date", "customer", "region", "description", "amount", "currency", "kind")

_GROUPED = r"\d{1,3}(?:,\d{3})+"
_PLAIN = r"\d+"
_AMOUNT_BODY = re.compile(rf"(?:{_GROUPED}|{_PLAIN})(?:\.\d+)?")


def parse_amount(text: str) -> Decimal:
    """Parse a ledger amount such as ``1,234.50``, ``-12.00`` or ``(150.00)``."""
    raw = text.strip()
    negative = False
    if raw.startswith("(") and raw.endswith(")"):
        negative = True
        body = raw[1:-1]
    elif raw.startswith("-"):
        negative = True
        body = raw[1:]
    else:
        body = raw
    if not _AMOUNT_BODY.fullmatch(body):
        raise ParseError(f"invalid amount {text!r}")
    value = Decimal(body.replace(",", ""))
    return -value if negative else value


def _open(source: str | TextIO) -> TextIO:
    if isinstance(source, str):
        return io.StringIO(source)
    return source


def _check_header(header: list[str] | None) -> None:
    if header is None:
        raise ParseError("empty input: missing header row", 1)
    normalized = tuple(column.strip().lower() for column in header)
    if normalized != COLUMNS:
        raise ParseError(f"unexpected header {header!r}; expected {','.join(COLUMNS)}", 1)


def _parse_row(row: list[str], line: int, regions: frozenset[str]) -> Entry:
    if len(row) != len(COLUMNS):
        raise ParseError(f"expected {len(COLUMNS)} fields, got {len(row)}", line)
    date_text, customer, region, description, amount_text, currency, kind = (
        cell.strip() for cell in row
    )
    when = parse_date(date_text, line)
    if not customer:
        raise ParseError("customer is empty", line)
    if region not in regions:
        raise ParseError(f"unknown region {region!r}", line)
    currency = currency.upper()
    if not is_supported(currency):
        raise ParseError(f"unsupported currency {currency!r}", line)
    kind = kind.lower() or "charge"
    if kind not in KINDS:
        raise ParseError(f"unknown kind {kind!r}", line)
    try:
        amount = parse_amount(amount_text)
    except ParseError as exc:
        raise ParseError(str(exc), line) from None
    try:
        return Entry(
            date=when,
            customer=customer,
            region=region,
            description=description,
            amount=Money(amount, currency),
            kind=kind,
        )
    except ValidationError as exc:
        raise ParseError(str(exc), line) from None


def iter_entries(source: str | TextIO) -> Iterable[Entry]:
    reader = csv.reader(_open(source))
    _check_header(next(reader, None))
    regions = frozenset(known_regions())
    for index, row in enumerate(reader, start=2):
        if not row or all(not cell.strip() for cell in row):
            continue
        yield _parse_row(row, index, regions)


def read_entries(source: str | TextIO) -> list[Entry]:
    """Read a whole ledger CSV (text or file object) into entries, in file order."""
    return list(iter_entries(source))


def format_plain_amount(entry: Entry) -> str:
    """Amount as written by the exporter: ``-1234.50`` (minus sign, no grouping)."""
    places = get_currency(entry.currency).minor_units
    value = round_amount(entry.amount.amount, entry.currency)
    if value == 0:
        value = abs(value)
    return f"{value:.{places}f}"


def write_entries(entries: Iterable[Entry], out: TextIO | None = None) -> str:
    """Write ``entries`` as CSV; returns the text (and also writes to ``out`` if given)."""
    buffer = io.StringIO()
    writer = csv.writer(buffer, lineterminator="\n")
    writer.writerow(COLUMNS)
    for entry in entries:
        writer.writerow(
            [
                entry.date.isoformat(),
                entry.customer,
                entry.region,
                entry.description,
                format_plain_amount(entry),
                entry.currency,
                entry.kind,
            ]
        )
    text = buffer.getvalue()
    if out is not None:
        out.write(text)
    return text
