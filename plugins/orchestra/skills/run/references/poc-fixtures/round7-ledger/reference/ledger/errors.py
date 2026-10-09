"""Exception hierarchy for the ledger package."""


class LedgerError(Exception):
    """Base class for every error raised by this package."""


class ParseError(LedgerError):
    """Raised when textual input (CSV rows, amounts, dates) cannot be parsed."""

    def __init__(self, message: str, line: int | None = None) -> None:
        self.line = line
        if line is not None:
            message = f"line {line}: {message}"
        super().__init__(message)


class ValidationError(LedgerError):
    """Raised when structurally valid input violates a business rule."""


class CurrencyMismatchError(ValidationError):
    """Raised when amounts in different currencies are combined."""

    def __init__(self, left: str, right: str) -> None:
        self.left = left
        self.right = right
        super().__init__(f"cannot combine {left} with {right}")
