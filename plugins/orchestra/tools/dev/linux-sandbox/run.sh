#!/bin/sh
# Linux sandbox check for agent-exec: builds the image, then runs the
# scenarios (and the pytest suite) as user `worker` in throwaway containers.
#   run.sh [bwrap|landlock|suite|all]     (default: all)
set -u

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH= cd -- "$HERE/../../../../.." && pwd)
IMAGE=orchestra-linux-sandbox
MODE=${1:-all}

case "$MODE" in
    bwrap|landlock|suite|all) ;;
    *) echo "usage: $0 [bwrap|landlock|suite|all]" >&2; exit 2 ;;
esac

docker build -q -t "$IMAGE" "$HERE" >/dev/null || { echo "FAIL image build"; exit 1; }

OUT=$(mktemp)
trap 'rm -f "$OUT"' EXIT

run() {
    # $1 = scenarios.py mode, rest = extra docker flags. --privileged lets
    # bwrap create namespaces (an ordinary desktop); the default container
    # blocks them (Ubuntu >= 24.04 AppArmor userns restriction -> Landlock).
    # --init: a PID 1 that reaps orphans, else killed process groups linger
    # as zombies and the suite's "no process left" assertions fail.
    mode=$1; shift
    docker run --rm --init "$@" -v "$REPO:/repo:ro" "$IMAGE" \
        python3 /repo/plugins/orchestra/tools/dev/linux-sandbox/scenarios.py "$mode" \
        | tee -a "$OUT"
}

[ "$MODE" = bwrap ] || [ "$MODE" = all ] && run bwrap --privileged
[ "$MODE" = landlock ] || [ "$MODE" = all ] && run landlock
[ "$MODE" = suite ] || [ "$MODE" = all ] && run suite

PASS=$(grep -c '^PASS ' "$OUT")
FAIL=$(grep -c '^FAIL ' "$OUT")
echo "summary: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] && [ "$PASS" -gt 0 ]
