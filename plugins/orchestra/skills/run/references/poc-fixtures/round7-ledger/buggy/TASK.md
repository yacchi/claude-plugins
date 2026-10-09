# Task: make the `ledger` package conform to its specification

This directory contains `ledger`, a small invoicing/ledger package
(`ledger/`), its test suite (`tests/`) and its specification (`SPEC.md`).

The test suite currently fails. Make the package behave as `SPEC.md`
describes.

Rules:

- `SPEC.md` is the authority. Where the code or a comment disagrees with it,
  the specification wins.
- Do not modify, add or delete anything under `tests/`.
- Keep the public API listed in `SPEC.md` §1 (names, signatures, return types).
- Use only the Python standard library.
- Run the suite with `uvx --from pytest pytest` from this directory.

When you are done, reply with a short summary of what you changed and why.
