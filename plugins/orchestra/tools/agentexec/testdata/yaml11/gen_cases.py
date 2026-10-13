"""Generate cases.json: what PyYAML's safe_load makes of each input.

    uv run --no-project --with pyyaml python testdata/yaml11/gen_cases.py \
        > testdata/yaml11/cases.json

Values are written in a typed canonical form so the Go side can check type
as well as value: {"int": "12"}, {"float": "1.5"}, {"date": ...},
{"datetime": ...}, {"map": [[key, value], ...]} (keys as json.dumps renders
them), lists, strings, booleans and null as themselves. A load that raises
is {"error": true}.
"""

import datetime
import json
import sys

import yaml

SCALARS = [
    # bool (YAML 1.1: yes/no/on/off in three casings; y/n stay strings)
    "yes", "Yes", "YES", "no", "No", "NO", "true", "True", "TRUE",
    "false", "False", "FALSE", "on", "On", "ON", "off", "Off", "OFF",
    "y", "Y", "n", "N", "yEs", "tRUE", "oN",
    # null
    "~", "null", "Null", "NULL", "", "NuLL",
    # int
    "0", "00", "07", "08", "010", "0o10", "0x1F", "0X1F", "0x_1f", "0b101",
    "0B1", "-0b11", "+0x1f", "1_000", "-1_000", "+12", "-0", "0_7",
    "1:30", "-1:30:00", "190:20:30", "09:30", "1:60", "123456789",
    # float (a decimal point is required; the exponent needs a sign)
    "12e3", "1e3", "1.0e3", "1.0e+3", "1.0E-3", ".5", "-.5", "+.5", "1.",
    "-1.", "1.5", "1_0.5", "1:30.5", ".inf", "-.inf", "+.inf", ".Inf",
    ".INF", ".nan", ".NaN", ".NAN", "nan", "inf", "6.8523015e+5",
    "685.230_15e+03", ".", "._", "1.2.3", "0.1", "1e-05", "100000000000000000.0",
    # timestamps
    "2001-12-14", "2001-12-14t21:59:43.10-05:00", "2001-12-14 21:59:43.10 -5",
    "2001-12-15T02:59:43.1Z", "2001-12-14 21:59:43", "2002-1-1",
    "2002-1-1 1:02:03", "2001-12-14T21:59:43.1234567+09:30",
    # strings and specials
    "hello", "hello world", "=", "<<", "-", "0.0.0.0", "1.2e", "~foo",
    "off-ish", "yes!", "@npm/pkg",
    # quoted / tagged / block scalars
    "'yes'", "\"1\"", "'~'", "!!str 1", "!!int '7'", "!!int \"0x10\"",
    "!!float 1", "!!bool yes", "!!null ''", "!!str yes",
]

DOCUMENTS = [
    "",
    "# only a comment\n",
    "a: 1\n",
    "a: 1\na: 2\n",
    "1: a\n~: c\n1.5: d\nfalse: e\n",
    "a: &x {b: 1, c: [1, 2]}\nd: *x\n",
    "base: &b {x: 1, y: 2}\nd:\n  <<: *b\n  y: 3\n  z: 4\n",
    "a: &a {x: 1, y: 1}\nb: &b {y: 2, z: 2}\nc:\n  w: 0\n  <<: [*a, *b]\n",
    "a: &a {x: 1}\nb: &b {x: 2}\nc:\n  <<: *a\n  <<: *b\n",
    "a: 1\n---\nb: 2\n",
    "- a\n- b\n",
    "hello\n",
    "a:\n  - yes\n  - 'no'\n  - 0755\n",
    "a: |\n  yes\n",
    "a: >\n  folded\n  text\n",
    "a: [1, 2\n",
    "a: b: c\n",
    "a:\n\t- tab\n",
    "=: x\n",
    "enforcement:\n  light_class: off\n  worker_vcs: no\n  turn_edits: 012\n",
    "external_executors:\n  pi:\n    enabled: on\n    classes: [light, deep]\n",
]


def canon(value):
    if isinstance(value, bool) or value is None or isinstance(value, str):
        return value
    if isinstance(value, int):
        return {"int": str(value)}
    if isinstance(value, float):
        return {"float": repr(value)}
    if isinstance(value, datetime.datetime):
        return {"datetime": value.isoformat()}
    if isinstance(value, datetime.date):
        return {"date": value.isoformat()}
    if isinstance(value, list):
        return [canon(v) for v in value]
    if isinstance(value, dict):
        return {"map": [[json_key(k), canon(v)] for k, v in value.items()]}
    raise TypeError(type(value))


def json_key(key):
    if isinstance(key, (datetime.date, datetime.datetime)):
        return key.isoformat()
    return next(iter(json.loads(json.dumps({key: 0}))))


def load(text):
    try:
        return canon(yaml.safe_load(text))
    except Exception:  # noqa: BLE001 -- any failure is the expected answer
        return {"error": True}


def main():
    cases = []
    for scalar in SCALARS:
        text = "v: %s\n" % scalar
        cases.append({"yaml": text, "want": load(text)})
    for text in DOCUMENTS:
        cases.append({"yaml": text, "want": load(text)})
    json.dump({"pyyaml": yaml.__version__, "cases": cases}, sys.stdout,
              indent=1, ensure_ascii=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
