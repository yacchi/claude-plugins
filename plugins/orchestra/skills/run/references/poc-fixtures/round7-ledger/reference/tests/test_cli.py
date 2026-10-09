import io

from ledger.cli import main, run

CSV = (
    "date,customer,region,description,amount,currency,kind\n"
    "2025-01-05,acme,NORTH,Hosting,100.00,USD,charge\n"
    "2025-02-07,bolt,NORTH,Hosting,\"1,000.00\",EUR,charge\n"
    "2025-02-09,acme,NORTH,Refund,-20.00,USD,credit\n"
)


def test_revenue_command(tmp_path):
    path = tmp_path / "ledger.csv"
    path.write_text(CSV, encoding="utf-8")
    out = io.StringIO()
    assert run(["revenue", str(path)], out) == 0
    assert out.getvalue() == (
        "2025-01  100.00 USD\n"
        "2025-02  1,080.00 USD\n"
        "TOTAL  1,180.00 USD\n"
    )


def test_balances_command(tmp_path):
    path = tmp_path / "ledger.csv"
    path.write_text(CSV, encoding="utf-8")
    out = io.StringIO()
    run(["balances", str(path)], out)
    assert out.getvalue() == "bolt  1,100.00 USD\nacme  80.00 USD\n"


def test_missing_file_exits_2(tmp_path, capsys):
    assert main(["revenue", str(tmp_path / "nope.csv")]) == 2
    assert capsys.readouterr().err.startswith("ledger: ")
