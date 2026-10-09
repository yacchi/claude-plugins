import pytest


@pytest.fixture(autouse=True)
def _fresh_rate_cache():
    """Each hidden check starts from a clean memo so state never leaks between checks."""
    from ledger import rates

    clear = getattr(rates, "clear_rate_cache", None)
    if clear is not None:
        clear()
    yield
