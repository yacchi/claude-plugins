from ledger import read_entries, write_entries

HEADER = "date,customer,region,description,amount,currency,kind\n"


def test_round_trip():
    text = (
        HEADER
        + '2025-01-05,acme,NORTH,"Hosting, January","1,234.50",USD,charge\n'
        + "2025-01-09,acme,NORTH,Refund,-12.50,USD,credit\n"
        + "2025-01-10,kaze,EXEMPT,Setup,1500,JPY,charge\n"
    )
    entries = read_entries(text)
    exported = write_entries(entries)
    assert exported == (
        HEADER
        + '2025-01-05,acme,NORTH,"Hosting, January",1234.50,USD,charge\n'
        + "2025-01-09,acme,NORTH,Refund,-12.50,USD,credit\n"
        + "2025-01-10,kaze,EXEMPT,Setup,1500,JPY,charge\n"
    )
    assert read_entries(exported) == entries
