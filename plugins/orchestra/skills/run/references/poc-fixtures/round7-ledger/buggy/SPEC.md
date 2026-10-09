# `ledger` package specification

This document is the authoritative definition of the behaviour of the `ledger`
package. Where the code or a comment disagrees with this document, this
document wins. All amounts are `decimal.Decimal`; floats are never used.

## 1. Public API

Everything listed here is importable from the top-level package
(`from ledger import ...`). The names, call signatures and return types are the
public API.

| Area | Names |
|---|---|
| Money | `Money`, `round_amount`, `format_money`, `sum_money`, `get_currency`, `minor_units`, `supported_codes`, `REPORTING_CURRENCY` |
| Calendar | `parse_date`, `is_leap_year`, `days_in_month`, `month_key`, `iter_month_segments` |
| Errors | `LedgerError`, `ParseError`, `ValidationError`, `CurrencyMismatchError` |
| CSV | `COLUMNS`, `KINDS`, `parse_amount`, `read_entries`, `write_entries` |
| Domain | `Entry`, `Subscription` |
| Pricing | `line_amount`, `apply_discount`, `Discounted` |
| Proration | `prorate`, `segments`, `subscription_entry` |
| Tax | `rate_for`, `known_regions`, `clear_rate_cache`, `tax_for`, `apply_tax`, `TaxedAmount` |
| FX | `book_rate`, `to_usd` |
| Invoices | `build_invoice`, `render_invoice`, `Invoice`, `InvoiceLine` |
| Reports | `revenue_by_month`, `customer_balances`, `tax_summary`, `render_revenue_report`, `TaxSummaryRow` |
| Statements | `statement`, `aging`, `AGING_BUCKETS`, `StatementRow`, `AgingRow` |
| Checks | `run_checks`, `Finding` |

`ParseError` and `ValidationError` are subclasses of `LedgerError`;
`CurrencyMismatchError` is a subclass of `ValidationError`.

## 2. Money and currencies

### 2.1 Currencies

| Code | Minor units | Smallest step |
|---|---|---|
| USD | 2 | 0.01 |
| EUR | 2 | 0.01 |
| GBP | 2 | 0.01 |
| JPY | 0 | 1 |
| KWD | 3 | 0.001 |

Any other code is unsupported: `Money(..., "CHF")` raises `ValidationError`.
The reporting currency (`REPORTING_CURRENCY`) is `USD`.

### 2.2 The `Money` type

`Money(amount, currency)` is an immutable value. `Money.of(value, currency)`
accepts a `Decimal`, an `int` or a numeric string; a `float` raises
`ValidationError`. Two `Money` values are equal when their currencies are equal
and their amounts are numerically equal (`Money.of("1.5", "USD") ==
Money.of("1.50", "USD")`).

`+` and `-` between two `Money` values of the same currency give the exact
(unrounded) sum/difference. Combining different currencies raises
`CurrencyMismatchError`. `sum_money(items, currency)` adds `items` exactly and
returns `Money.zero(currency)` for an empty iterable.

### 2.3 Rounding rule (applies everywhere)

`round_amount(amount, currency)` rounds a `Decimal` to the minor units of
`currency` using **round half away from zero**: if the value is exactly halfway
between two representable values, the result is the one farther from zero;
otherwise it is the nearest representable value.

| Input | Currency | Result |
|---|---|---|
| `1.234` | USD | `1.23` |
| `1.236` | USD | `1.24` |
| `0.125` | USD | `0.13` |
| `0.135` | USD | `0.14` |
| `-0.125` | USD | `-0.13` |
| `2.5` | JPY | `3` |
| `3.5` | JPY | `4` |
| `-2.5` | JPY | `-3` |
| `1.0005` | KWD | `1.001` |
| `1.2344` | KWD | `1.234` |

`Money.rounded()` returns `Money(round_amount(amount, currency), currency)`.

**Every rounding performed anywhere in the package uses exactly this rule**,
always to the minor units of the currency of the value being rounded. Rounding
happens at precisely these points and nowhere else:

1. `tax_for` — the tax on one amount (§7.2).
2. `apply_discount` — the discount amount (§6.2).
3. `line_amount` — the extended line price (§6.1).
4. `prorate` — once, on the final total (§5).
5. `to_usd` — each converted amount (§8).
6. `format_money` and `write_entries` — when rendering (§2.4, §10).

Sums of `Money` values (invoice totals, report totals, balances) are exact
additions of values that were already rounded, so they are never rounded
again.

### 2.4 Formatting

`format_money(money)` rounds the amount with `round_amount`, then renders it
with `,` as thousands separator, exactly *minor units* decimal places, a
leading `-` for negative values, one space, and the currency code. A value that
rounds to zero is rendered without a sign. `str(money)` equals
`format_money(money)`.

| Money | Rendering |
|---|---|
| `-1234.5 USD` | `-1,234.50 USD` |
| `1234567.891 USD` | `1,234,567.89 USD` |
| `1234.6 JPY` | `1,235 JPY` |
| `0.125 USD` | `0.13 USD` |
| `-0.001 USD` | `0.00 USD` |
| `12.3456 KWD` | `12.346 KWD` |

## 3. Calendar

### 3.1 Dates

`parse_date(text)` accepts exactly `YYYY-MM-DD` (four-digit year, two-digit
month and day, surrounding whitespace ignored) naming a real calendar day, and
returns a `datetime.date`. Anything else raises `ParseError`
(`"2025-2-01"`, `"2025-02-30"`, `"01/02/2025"`).

### 3.2 Month lengths

The package uses the Gregorian calendar.

`is_leap_year(year)` is true when the year is divisible by 4, except years
divisible by 100, which are leap years only when also divisible by 400.

| Year | Leap? |
|---|---|
| 2024 | yes |
| 2025 | no |
| 2000 | yes |
| 1900 | no |
| 2100 | no |

`days_in_month(year, month)`:

| Month | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 | 12 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| Days | 31 | 28, or 29 in a leap year | 31 | 30 | 31 | 30 | 31 | 31 | 30 | 31 | 30 | 31 |

So `days_in_month(2024, 2) == 29`, `days_in_month(2100, 2) == 28`,
`days_in_month(2025, 8) == 31`, `days_in_month(2025, 9) == 30`.
A month outside 1..12 raises `ValueError`.

### 3.3 Month keys and segments

`month_key(d)` is `"YYYY-MM"` (zero-padded), e.g. `month_key(date(2025, 3, 9))
== "2025-03"`.

`iter_month_segments(start, end)` splits the inclusive range `start..end` into
consecutive `(segment_start, segment_end)` pairs, one per calendar month
touched, each inclusive. `end < start` raises `ValidationError`.
`iter_month_segments(date(2025,1,25), date(2025,3,5))` yields
`(2025-01-25, 2025-01-31)`, `(2025-02-01, 2025-02-28)`,
`(2025-03-01, 2025-03-05)`.

## 4. Domain records

`Entry(date, customer, region, description, amount, kind="charge")`: `amount`
is a `Money`, and its sign is the sign of the booking (charges are normally
positive, credits normally negative). `kind` must be one of `KINDS` =
`("charge", "credit", "adjustment")`, otherwise `ValidationError`. **`kind` is
descriptive only: no function in the package changes an amount's sign or value
because of its kind.** `entry.currency` is `entry.amount.currency`.

`Subscription(customer, plan, region, monthly_price, tags=())` — a negative
`monthly_price` raises `ValidationError`.

## 5. Proration

`prorate(monthly_price, start, end)` returns the charge for the inclusive date
range `start..end`:

1. Split the range into calendar-month segments (§3.3).
2. Each segment contributes `price × active_days ÷ days_in_month`, where
   `active_days` is the number of days in the segment (both ends included) and
   `days_in_month` is the length of **that segment's** month (§3.2).
3. The contributions are added exactly (no intermediate rounding).
4. The sum is rounded once with `round_amount` in the price's currency.

`end < start` raises `ValidationError`. A range covering a whole month gives
exactly the monthly price.

Worked examples (USD):

| Price | Range | Computation | Result |
|---|---|---|---|
| 30.00 | 2025-06-11 .. 2025-06-30 | 30 × 20/30 = 20 | `20.00` |
| 31.00 | 2025-08-10 .. 2025-08-31 | 31 × 22/31 = 22 | `22.00` |
| 29.00 | 2024-02-15 .. 2024-02-29 | 29 × 15/29 = 15 | `15.00` |
| 28.00 | 2100-02-01 .. 2100-02-14 | 28 × 14/28 = 14 | `14.00` |
| 62.00 | 2025-03-20 .. 2025-04-10 | 62 × 12/31 + 62 × 10/30 = 24 + 20.666… = 44.666… | `44.67` |
| 93.00 | 2025-12-20 .. 2026-01-10 | 93 × 12/31 + 93 × 10/31 = 36 + 30 | `66.00` |
| 30.25 | 2025-06-16 .. 2025-06-30 | 30.25 × 15/30 = 15.125 | `15.13` |

`segments(start, end)` returns one `Segment` per calendar month with
`start`, `end`, `active_days` and `month_days` (the month length from §3.2).

`subscription_entry(subscription, start, end)` returns an `Entry` dated
`start`, for the subscription's customer and region, with description
`"<plan> <start ISO>..<end ISO>"`, kind `"charge"` and amount
`prorate(subscription.monthly_price, start, end)`.

## 6. Pricing

### 6.1 Line amounts

`line_amount(unit_price, quantity)` = `round_amount(unit_price × quantity)` in
the unit price's currency. `quantity` accepts a `Decimal`, an `int` or a
numeric string (converted exactly, as in §2); a `float` raises `ValidationError`.
A negative quantity raises `ValidationError`.
Example: `2.25 USD × 0.5 = 1.125 → 1.13 USD`; `0.333 USD × 3 = 0.999 → 1.00 USD`.

### 6.2 Discounts

`apply_discount(amount, percent)` returns `Discounted(gross, discount)` where
`discount = round_amount(amount × percent ÷ 100)` and `net = gross − discount`.
`percent` outside `0..100` (inclusive) raises `ValidationError`.
Example: `4.50 USD` at `5` percent: `0.225 → discount 0.23`, `net 4.27`.

## 7. Sales tax

### 7.1 Rates

| Region | Rate | Effective from |
|---|---|---|
| NORTH | 10% | 2000-01-01 |
| SOUTH | 6% | 2000-01-01 |
| SOUTH | 7.5% | 2025-07-01 |
| EAST | 8% | 2000-01-01 |
| EAST | 5% | 2024-10-01 |
| EAST | 9% | 2026-04-01 |
| EXEMPT | 0% | 2000-01-01 |

`rate_for(region, on)` returns the rate (as a `Decimal` fraction, e.g.
`Decimal("0.075")`) of the region's revision with the latest
`effective_from` that is on or before `on`. Examples: SOUTH on 2025-06-30 →
`0.06`; SOUTH on 2025-07-01 → `0.075`; EAST on 2024-09-30 → `0.08`; EAST on
2024-10-01 → `0.05`; EAST on 2026-04-01 → `0.09`. A date before 2000-01-01
or an unknown region raises `ValidationError`. `known_regions()` returns the
region names sorted.

**The result of `rate_for` depends only on its two arguments.** It must be the
same no matter which other calls (to `rate_for` or any other function) were
made before, in which order. `clear_rate_cache()` discards any memoized
lookups; calling it, or not calling it, never changes any result.

### 7.2 Tax on an amount

`tax_for(amount, region, on)` = `round_amount(amount × rate_for(region, on))`
in the amount's currency (§2.3). Negative amounts give negative tax.

| Amount | Region | Date | Exact | Tax |
|---|---|---|---|---|
| 100.00 USD | NORTH | 2025-01-15 | 10.000 | `10.00 USD` |
| 12.25 USD | NORTH | 2025-01-15 | 1.225 | `1.23 USD` |
| -12.25 USD | NORTH | 2025-01-15 | -1.225 | `-1.23 USD` |
| 10.00 USD | SOUTH | 2025-06-30 | 0.600 | `0.60 USD` |
| 10.00 USD | SOUTH | 2025-07-01 | 0.750 | `0.75 USD` |
| 25 JPY | NORTH | 2025-01-15 | 2.5 | `3 JPY` |

`apply_tax(amount, region, on)` returns `TaxedAmount(net=amount,
tax=tax_for(...))`; its `gross` is `net + tax`.

## 8. Conversion to USD

Book rates (USD per one unit):

| Currency | USD | EUR | GBP | JPY | KWD |
|---|---|---|---|---|---|
| Rate | 1 | 1.10 | 1.25 | 0.0065 | 3.25 |

`to_usd(money)` = `Money(round_amount(amount × rate, "USD"), "USD")`. USD input
is returned rounded to cents. Examples: `100.00 EUR → 110.00 USD`;
`10000 JPY → 65.00 USD`; `2.000 KWD → 6.50 USD`; `10 JPY → 0.065 → 0.07 USD`;
`1.15 EUR → 1.265 → 1.27 USD`.

## 9. CSV import

### 9.1 Layout

The first row is the header, exactly these columns in this order
(case-insensitive, surrounding whitespace ignored):

```
date,customer,region,description,amount,currency,kind
```

Standard CSV quoting applies (a field containing `,` must be quoted). On
export (§10), quoting is minimal: a field is quoted only when it contains a
comma, a double quote, CR or LF, and embedded double quotes are doubled. Every
field has surrounding whitespace removed. Rows that are empty or contain only
whitespace are skipped. Each data row must have exactly 7 fields.

- `date`: §3.1.
- `customer`: non-empty.
- `region`: one of `known_regions()`.
- `currency`: a supported code (§2.1), case-insensitive (`usd` → `USD`).
- `kind`: one of `KINDS`, case-insensitive; an empty field means `charge`.
- `amount`: §9.2.

`read_entries(source)` takes the CSV as a `str` or a text file object and
returns the entries in file order. Any problem raises `ParseError` whose
message starts with `line N: ` where N is the 1-based line number in the file
(the header is line 1, the first data row is line 2; skipped blank lines still
count).

### 9.2 Amount syntax

`parse_amount(text)` returns a `Decimal`. After removing surrounding
whitespace, the text is one of:

- `BODY` — a non-negative amount;
- `-BODY` — a negative amount;
- `(BODY)` — a **negative** amount (accounting notation).

`BODY` is either plain digits (`1234`) or digits grouped by commas in groups
of exactly three after the first group of 1–3 digits (`1,234` or `12,345,678`),
optionally followed by `.` and one or more digits. Commas are thousands
separators and are dropped. Nothing else is accepted (no `+`, no spaces
inside, no sign inside parentheses, no leading `.`); invalid text raises
`ParseError`.

| Text | Value |
|---|---|
| `42` | `42` |
| `  42  ` | `42` |
| `1,234.56` | `1234.56` |
| `1,234,567.89` | `1234567.89` |
| `-12.50` | `-12.50` |
| `(150.00)` | `-150.00` |
| `(1,234.50)` | `-1234.50` |
| `0.75` | `0.75` |
| `1,23.00` | ParseError |
| `12,34` | ParseError |
| `(-5)` | ParseError |
| `-(5)` | ParseError |
| `+5` | ParseError |
| `.5` | ParseError |
| `(5` | ParseError |
| `` (empty) | ParseError |

A row's amount is exactly `parse_amount(amount field)`; the `kind` column
never changes it (§4). For example, the row
`2025-03-04,acme,NORTH,Refund,(150.00),USD,credit` has amount
`-150.00 USD`, and `2025-03-05,acme,NORTH,Fix,(20.00),USD,adjustment` has
amount `-20.00 USD`.

## 10. CSV export

`write_entries(entries, out=None)` returns the CSV text (and writes it to `out`
if given): the header row from §9.1, then one row per entry in the given order,
`\n` line endings. The amount is written with `round_amount`, exactly *minor
units* decimal places, a leading `-` when negative, no thousands separator
(`-1234.50`, `1500` for JPY). `read_entries(write_entries(es)) == es` for
entries whose amounts are already at minor units.

## 11. Invoices

`build_invoice(customer, entries)` uses only the entries whose `customer`
equals `customer` (other customers' entries are ignored). No such entries →
`ValidationError`; entries in more than one currency → `ValidationError`.

- Lines are sorted by date ascending; entries with the same date keep their
  input order.
- Each `InvoiceLine` carries the entry's date, description, region, kind and
  amount, and `tax = tax_for(amount, region, date)` using **that line's own**
  region and date. `line.total = amount + tax`.
- `invoice.subtotal` = sum of line amounts, `invoice.tax` = sum of line taxes,
  `invoice.total = subtotal + tax`, all in the invoice currency.

Example: lines `100.00 USD NORTH 2025-01-10` and `50.00 USD NORTH 2025-01-20`
→ taxes `10.00` and `5.00`, subtotal `150.00`, tax `15.00`, total
`165.00 USD`. A SOUTH invoice with `100.00 USD` on 2025-06-15 and `100.00 USD`
on 2025-07-15 has taxes `6.00` and `7.50` (tax `13.50 USD`).

`render_invoice(invoice)` returns text whose first line is
`Invoice for <customer> (<currency>)` and whose last three lines are
`Subtotal: <format_money>`, `Tax: <format_money>`, `Total: <format_money>`.

## 12. Reports

All report amounts are USD. Each entry is converted **individually** with
`to_usd` (§8, rounded per entry); the converted values are then added
exactly. Every entry is included with its own sign, whatever its kind.

### 12.1 `revenue_by_month(entries)`

Returns a `dict[str, Money]` mapping `month_key(entry.date)` to the USD total
of that month's entries. Iteration order of the dict is chronological
(`"2024-12"` before `"2025-01"` before `"2025-02"`); months without entries
are absent.

Example: the CSV rows

```
2025-02-03,acme,NORTH,Hosting,100.00,USD,charge
2025-01-15,bolt,NORTH,Hosting,100.00,EUR,charge
2025-02-10,acme,NORTH,Refund,(150.00),USD,credit
```

give `{"2025-01": 110.00 USD, "2025-02": -50.00 USD}`.

### 12.2 `customer_balances(entries)`

Returns a list of `(customer, Money)` tuples, one per customer, with the
customer's USD total. Sorted by balance, **largest first**; customers with
equal balances are ordered by name ascending using plain case-sensitive
code-point string comparison (so `"Bravo"` sorts before `"alpha"`).

Example: `zed 10.00`, `amy 10.00`, `bob 25.00`, `cat -5.00` →
`[("bob", 25.00), ("amy", 10.00), ("zed", 10.00), ("cat", -5.00)]`.

### 12.3 `tax_summary(entries)`

Returns a `list[TaxSummaryRow]` with one `TaxSummaryRow(region, currency, taxable, tax)` per distinct
`(region, currency)` pair, in the entry's own currency (not converted):
`taxable` = sum of the entries' amounts, `tax` = sum of
`tax_for(amount, region, date)` per entry. Rows are sorted by region
ascending, then currency ascending.

### 12.4 `render_revenue_report(entries)`

One line per month from `revenue_by_month`, in its order, formatted
`"<YYYY-MM>  <format_money(total)>"` (two spaces), followed by
`"TOTAL  <format_money(sum of the month totals)>"`; lines joined with `\n`, no
trailing newline.

## 13. Statements

### 13.1 `statement(customer, entries)`

The customer's entries sorted by date (stable), each as a `StatementRow` with
`balance` = running exact sum of amounts up to and including that row, in the
entry currency. No entries → `[]`. Mixed currencies → `ValidationError`.

### 13.2 `aging(entries, as_of)`

Entries dated after `as_of` are ignored. For each remaining entry,
`age = (as_of − entry.date)` in days, and its `to_usd` amount goes into
bucket `"0-30"` (age 0..30), `"31-60"` (31..60), `"61-90"` (61..90) or
`"90+"` (91 or more). Returns one `AgingRow(customer, buckets)` per customer,
sorted by customer name; `buckets` has all four keys of `AGING_BUCKETS`
(zero when empty); `row.total` is the sum of the four buckets.

## 14. Checks

`run_checks(entries, today)` returns `Finding(code, customer, message)` values,
grouped in this order:

1. `duplicate` — two or more entries identical in every field.
2. `mixed-currency` — a customer with entries in more than one currency
   (one finding per such customer, customers sorted by name).
3. `future-dated` — each entry dated after `today`.
4. `positive-credit` — each entry of kind `credit` whose amount is greater
   than zero.

A ledger with none of these conditions gives `[]`.

## 15. Command line

`python -m ledger revenue FILE` prints `render_revenue_report`;
`balances FILE` prints `"<customer>  <format_money(balance)>"` per
`customer_balances` row; `taxes FILE`, `invoice FILE CUSTOMER` and
`aging FILE --as-of YYYY-MM-DD` print the corresponding reports. A
`LedgerError` or I/O error prints `ledger: <message>` to stderr and exits
with status 2.
