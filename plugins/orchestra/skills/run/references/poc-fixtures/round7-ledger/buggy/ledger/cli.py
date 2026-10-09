"""Command-line entry point: ``python -m ledger <command> FILE [...]``."""

from __future__ import annotations

import argparse
import sys
from typing import Sequence, TextIO

from .dates import parse_date
from .errors import LedgerError
from .invoice import build_invoice, render_invoice
from .money import format_money
from .parsing import read_entries
from .reporting import customer_balances, render_revenue_report, tax_summary
from .statements import AGING_BUCKETS, aging


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="ledger", description="Ledger reports")
    sub = parser.add_subparsers(dest="command", required=True)

    revenue = sub.add_parser("revenue", help="USD revenue per month")
    revenue.add_argument("file")

    balances = sub.add_parser("balances", help="USD balance per customer")
    balances.add_argument("file")

    taxes = sub.add_parser("taxes", help="tax per region and currency")
    taxes.add_argument("file")

    invoice = sub.add_parser("invoice", help="invoice one customer")
    invoice.add_argument("file")
    invoice.add_argument("customer")

    aging_cmd = sub.add_parser("aging", help="receivables aging in USD")
    aging_cmd.add_argument("file")
    aging_cmd.add_argument("--as-of", required=True)
    return parser


def _load(path: str):
    with open(path, newline="", encoding="utf-8") as handle:
        return read_entries(handle)


def run(argv: Sequence[str], out: TextIO) -> int:
    args = _build_parser().parse_args(list(argv))
    entries = _load(args.file)
    if args.command == "revenue":
        print(render_revenue_report(entries), file=out)
    elif args.command == "balances":
        for customer, balance in customer_balances(entries):
            print(f"{customer}  {format_money(balance)}", file=out)
    elif args.command == "taxes":
        for row in tax_summary(entries):
            print(
                f"{row.region}  {row.currency}  {format_money(row.taxable)}  {format_money(row.tax)}",
                file=out,
            )
    elif args.command == "invoice":
        print(render_invoice(build_invoice(args.customer, entries)), file=out)
    elif args.command == "aging":
        as_of = parse_date(args.as_of)
        print("customer  " + "  ".join(AGING_BUCKETS), file=out)
        for row in aging(entries, as_of):
            cells = "  ".join(format_money(row.buckets[name]) for name in AGING_BUCKETS)
            print(f"{row.customer}  {cells}", file=out)
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    try:
        return run(sys.argv[1:] if argv is None else argv, sys.stdout)
    except (LedgerError, OSError) as exc:
        print(f"ledger: {exc}", file=sys.stderr)
        return 2
