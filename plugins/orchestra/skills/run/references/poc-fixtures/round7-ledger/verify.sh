#!/usr/bin/env bash
# Usage: verify.sh <candidate-dir>
#
# Grades a candidate tree (a copy of buggy/ after the candidate worked on it).
# Only the candidate's ledger/ package is graded: it is copied into a temporary
# directory together with the ORIGINAL tests/ and pytest.ini, so nothing the
# candidate added elsewhere (conftest.py, edited tests, config) can influence
# the result. Nothing is written into the candidate directory. Always exits 0.
set -u

HERE=$(cd "$(dirname "$0")" && pwd -P)

if [ $# -ne 1 ] || [ ! -d "$1" ]; then
    echo "usage: verify.sh <candidate-dir>"
    exit 0
fi
CAND=$(cd "$1" && pwd -P)

WORK=$(mktemp -d "${TMPDIR:-/tmp}/r7-verify.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
export PYTHONDONTWRITEBYTECODE=1
unset PYTHONPATH

echo "round7-ledger verify: $CAND"

if [ ! -d "$CAND/ledger" ]; then
    echo "visible suite: FAIL (no ledger/ package in candidate)"
    total=$(grep -cE '^def test_' "$HERE/hidden/test_hidden.py")
    echo "0/$total hidden checks passed"
    exit 0
fi

mkdir -p "$WORK/cand" "$WORK/hidden"
cp -R "$CAND/ledger" "$WORK/cand/ledger"
find "$WORK/cand" -name __pycache__ -type d -prune -exec rm -rf {} +
cp -R "$HERE/buggy/tests" "$WORK/cand/tests"
cp "$HERE/buggy/pytest.ini" "$WORK/cand/pytest.ini"

# 1. Visible suite (original tests) against the candidate package.
(cd "$WORK/cand" && uvx --from pytest pytest -q -p no:cacheprovider tests) >"$WORK/visible.log" 2>&1
visible_rc=$?
visible_summary=$(grep -E '(passed|failed|error)' "$WORK/visible.log" | tail -n 1)
if [ "$visible_rc" -eq 0 ]; then
    echo "visible suite: PASS ($visible_summary)"
else
    echo "visible suite: FAIL ($visible_summary)"
    grep -E '^FAILED ' "$WORK/visible.log" | sed -E 's/ - .*//; s/^/  /'
fi

# 2. tests/ must be byte-identical to the original.
if [ -d "$CAND/tests" ] && diff -rq -x __pycache__ -x '*.pyc' -x .pytest_cache "$HERE/buggy/tests" "$CAND/tests" >/dev/null 2>&1; then
    echo "tests/ byte-identical to original: yes"
else
    echo "tests/ byte-identical to original: NO"
fi

# 3. Hidden checks, run from an isolated directory with the candidate package on the path.
cp "$HERE/hidden/conftest.py" "$HERE/hidden/test_hidden.py" "$WORK/hidden/"
printf '[pytest]\n' >"$WORK/hidden/pytest.ini"
(cd "$WORK/hidden" && PYTHONPATH="$WORK/cand" uvx --from pytest pytest -q -rA -p no:cacheprovider test_hidden.py) >"$WORK/hidden.log" 2>&1
total=$(grep -cE '^def test_' "$HERE/hidden/test_hidden.py")
passed=$(grep -cE '^PASSED ' "$WORK/hidden.log")
echo "$passed/$total hidden checks passed"

ids() {
    sed -nE 's/^def test_([A-Z][0-9]+)_.*/\1/p' "$HERE/hidden/test_hidden.py"
}
line="per family:"
for fam in R P C K G; do
    fam_total=$(ids | grep -c "^$fam")
    fam_pass=$(grep -E "^PASSED .*::test_${fam}[0-9]+_" "$WORK/hidden.log" | wc -l | tr -d ' ')
    line="$line $fam $fam_pass/$fam_total"
done
echo "$line"
failed_ids=""
for id in $(ids); do
    if ! grep -qE "^PASSED .*::test_${id}_" "$WORK/hidden.log"; then
        failed_ids="$failed_ids $id"
    fi
done
echo "hidden failures:${failed_ids:- none}"
exit 0
