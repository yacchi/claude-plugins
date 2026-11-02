#!/usr/bin/env bash
#
# scripts/release_test.sh
#
# scripts/release.sh を使い捨ての origin / clone で検証する。
# origin は一時 bare リポジトリ、claude は PATH 上の偽物なので実環境には触れない。
#
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RELEASE="$SCRIPT_DIR/release.sh"

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

# ユーザーの git 設定 (署名など) の影響を受けないようにする
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
unset RELEASE_TRAILERS

TRAILERS=$'Co-Authored-By: Claude Haiku 5.5 <noreply@anthropic.com>\nClaude-Session: https://example.invalid/session_test'

# claude の偽物: 呼び出しを "<cwd>\t<args>" で記録し、plugin update はインストール記録の version を書き換える
mkdir -p "$ROOT/bin"
cat >"$ROOT/bin/claude" <<'SH'
#!/bin/sh
printf '%s\t%s\n' "$(pwd)" "$*" >>"$FAKE_CLAUDE_LOG"
if [ "$1" = plugin ] && [ "$2" = update ]; then
    if [ -n "${FAKE_NO_UPDATE:-}" ]; then
        exit 0
    fi
    python3 -I - "$CLAUDE_CONFIG_DIR/plugins/installed_plugins.json" "$3" "$5" "$(pwd)" \
        "$FAKE_SOURCE_DIR/plugins/${3%@*}/.claude-plugin/plugin.json" <<'PY'
import json, os, sys
path, key, scope, cwd, manifest = sys.argv[1:6]
version = json.load(open(manifest))["version"]
data = json.load(open(path))
for rec in data["plugins"].get(key, []):
    if rec.get("scope") != scope:
        continue
    if scope != "user" and os.path.realpath(rec.get("projectPath", "")) != os.path.realpath(cwd):
        continue
    rec["version"] = version
with open(path, "w") as f:
    json.dump(data, f, indent=2)
PY
fi
exit 0
SH
chmod +x "$ROOT/bin/claude"

PASS_COUNT=0
FAIL_COUNT=0
CASE_INDEX=0
RC=0
OUT=""
LAST_OUT=""
MAIN_BEFORE=""
ORIGIN_BEFORE=""

# --- 補助関数 ---

write_plugin() {  # write_plugin <name> <version>: カレントの plugins/<name>/.claude-plugin/plugin.json を書く
    mkdir -p "plugins/$1/.claude-plugin"
    printf '{\n  "name": "%s",\n  "version": "%s"\n}\n' "$1" "$2" >"plugins/$1/.claude-plugin/plugin.json"
}

write_marketplace() {  # write_marketplace <foo-version> <bar-version>
    mkdir -p .claude-plugin
    cat >.claude-plugin/marketplace.json <<EOF
{
  "name": "yacchi-plugins",
  "plugins": [
    {"name": "foo", "source": "./plugins/foo", "version": "$1"},
    {"name": "bar", "source": "./plugins/bar", "version": "$2"}
  ]
}
EOF
}

commit_all() {  # commit_all <subject>: plugins/ と .claude-plugin/ の変更をコミットする
    git add -A -- plugins .claude-plugin
    git commit -q -m "$1"
}

# setup_repo <dir>: 一時 origin と clone (WORK) を作り、foo はこの PC に user / project 両方でインストール済みにする
setup_repo() {
    local base="$1"
    local seed="$base/seed"
    ORIGIN="$base/origin.git"
    WORK="$base/work"
    PROJ="$base/project"
    CFG="$base/config"
    LOG="$base/claude.log"

    mkdir -p "$PROJ" "$CFG/plugins"
    : >"$LOG"

    git init -q --bare -b main "$ORIGIN"
    git init -q -b main "$seed"
    (
        cd "$seed"
        write_plugin foo 1.0.0
        write_plugin bar 1.0.0
        write_marketplace 1.0.0 1.0.0
        printf 'base\n' >plugins/foo/README.md
        git add -A
        git commit -q -m "chore: 初期化"
        git remote add origin "$ORIGIN"
        git push -q origin main
    )
    git clone -q "$ORIGIN" "$WORK"
    cd "$WORK"

    cat >"$CFG/plugins/installed_plugins.json" <<EOF
{
  "version": 2,
  "plugins": {
    "foo@yacchi-plugins": [
      {"scope": "user", "installPath": "$CFG/cache/foo", "version": "1.0.0"},
      {"scope": "project", "projectPath": "$PROJ", "installPath": "$CFG/cache/foo-project", "version": "1.0.0"}
    ]
  }
}
EOF
}

# run_release [args...]: release.sh を clone の中で実行し、終了コードを RC、出力を OUT に入れる
run_release() {
    RC=0
    OUT="$(cd "$WORK" && PATH="$ROOT/bin:$PATH" CLAUDE_CONFIG_DIR="$CFG" FAKE_CLAUDE_LOG="$LOG" FAKE_SOURCE_DIR="$WORK" "$RELEASE" "$@" 2>&1)" || RC=$?
    LAST_OUT="$OUT"
}

# expect <label> <command...>: 失敗したら記録する (ケースは続行する)
expect() {
    if "${@:2}" >/dev/null 2>&1; then
        return 0
    fi
    printf '  check failed: %s\n' "$1"
    if [ -n "$LAST_OUT" ]; then
        printf '%s\n' "$LAST_OUT" | sed 's/^/      release.sh: /'
    fi
    return 0
}

expect_out() {  # expect_out <label> <text>: 直前の release.sh の出力に text が含まれること
    expect "$1" grep -qF -- "$2" <<<"$LAST_OUT"
}

expect_log() {  # expect_log <label> <text>: claude の呼び出し記録に text が含まれること
    expect "$1" grep -qF -- "$2" "$LOG"
}

snapshot() {  # snapshot: 拒否されるはずの実行の前に main と origin/main を記録する
    MAIN_BEFORE="$(git rev-parse main)"
    ORIGIN_BEFORE="$(git --git-dir="$ORIGIN" rev-parse main)"
}

expect_untouched() {  # expect_untouched <branch>: 拒否後に main / origin / ブランチ / 作業ツリーが変わっていないこと
    expect "main unchanged" [ "$(git rev-parse main)" = "$MAIN_BEFORE" ]
    expect "origin/main unchanged" [ "$(git --git-dir="$ORIGIN" rev-parse main)" = "$ORIGIN_BEFORE" ]
    expect "still on $1" [ "$(git symbolic-ref --short HEAD)" = "$1" ]
    expect "branch $1 kept" git show-ref --verify --quiet "refs/heads/$1"
    expect "no claude plugin update run" [ -z "$(grep -F 'plugin update' "$LOG" || true)" ]
}

# check_merge_message <branch> <commit subject> <version> <trailers or empty> <expected merge subject>
check_merge_message() {
    git switch -q main
    git switch -q -c "$1"
    write_plugin foo "$3"
    write_marketplace "$3" 1.0.0
    commit_all "$2"

    if [ -n "$4" ]; then
        export RELEASE_TRAILERS="$4"
    else
        unset RELEASE_TRAILERS
    fi
    run_release
    unset RELEASE_TRAILERS

    local expected="$5"
    if [ -n "$4" ]; then
        expected="$(printf '%s\n\n%s' "$5" "$4")"
    fi
    expect "$1: exit 0" [ "$RC" -eq 0 ]
    expect "$1: merge message" [ "$(git log -1 --format=%B main)" = "$expected" ]
}

# --- ケース ---

case_happy_path() {
    setup_repo "$1"
    git switch -q -c feature/foo-greet
    printf 'hello v2\n' >plugins/foo/README.md
    write_plugin foo 1.1.0
    write_marketplace 1.1.0 1.0.0
    commit_all "feat(foo): 挨拶を追加する (v1.1.0)"
    mkdir -p .memo
    printf 'memo\n' >.memo/notes.txt
    printf 'handoff\n' >HANDOFF-test.md

    export RELEASE_TRAILERS="$TRAILERS"
    run_release feature/foo-greet
    unset RELEASE_TRAILERS

    expect "exit 0" [ "$RC" -eq 0 ]
    expect "merge subject" [ "$(git log -1 --format=%s main)" = "Merge feature/foo-greet (挨拶を追加する、v1.1.0)" ]
    expect "merge message has trailers" [ "$(git log -1 --format=%B main)" = "$(printf 'Merge feature/foo-greet (挨拶を追加する、v1.1.0)\n\n%s' "$TRAILERS")" ]
    expect "is a merge commit" [ "$(git cat-file -p main | grep -c '^parent ')" -eq 2 ]
    expect "origin/main equals main" [ "$(git rev-parse main)" = "$(git --git-dir="$ORIGIN" rev-parse main)" ]
    expect "branch deleted" test -z "$(git branch --list feature/foo-greet)"
    expect "untracked .memo kept" test -f .memo/notes.txt
    expect "untracked HANDOFF kept" test -f HANDOFF-test.md
    expect "installed records now 1.1.0" [ "$(grep -c '"version": "1.1.0"' "$CFG/plugins/installed_plugins.json")" -eq 2 ]
    expect_log "user record updated from repo root" "$WORK"$'\t'"plugin update foo@yacchi-plugins --scope user -y"
    expect_log "project record updated inside projectPath" "$PROJ"$'\t'"plugin update foo@yacchi-plugins --scope project -y"
    expect_out "user summary line" "foo@yacchi-plugins [user]: 1.1.0 に更新済み"
    expect_out "project summary line" "foo@yacchi-plugins [project] ($PROJ): 1.1.0 に更新済み"
    expect_out "restart reminder" "再起動"
}

case_no_version_bump() {
    setup_repo "$1"
    git switch -q -c feature/foo-nobump
    printf 'changed\n' >plugins/foo/README.md
    commit_all "fix(foo): バージョンを上げ忘れ"
    snapshot
    run_release feature/foo-nobump
    expect "non-zero exit" [ "$RC" -ne 0 ]
    expect_out "message mentions version bump" "バージョンを上げてから"
    expect_untouched feature/foo-nobump
}

case_version_mismatch() {
    setup_repo "$1"
    git switch -q -c feature/foo-mismatch
    write_plugin foo 1.1.0
    write_marketplace 1.0.5 1.0.0
    commit_all "feat(foo): 不一致 (v1.1.0)"
    snapshot
    run_release feature/foo-mismatch
    expect "non-zero exit" [ "$RC" -ne 0 ]
    expect_out "message mentions mismatch" "一致しません"
    expect_untouched feature/foo-mismatch
}

case_dirty_tree() {
    setup_repo "$1"
    git switch -q -c feature/foo-dirty
    write_plugin foo 1.1.0
    write_marketplace 1.1.0 1.0.0
    commit_all "feat(foo): 汚れ (v1.1.0)"
    printf 'uncommitted\n' >>plugins/foo/README.md
    snapshot
    run_release feature/foo-dirty
    expect "non-zero exit" [ "$RC" -ne 0 ]
    expect_out "message mentions uncommitted changes" "未コミット"
    expect_untouched feature/foo-dirty
    expect "uncommitted change kept" grep -q uncommitted plugins/foo/README.md
}

case_plugin_not_installed() {
    setup_repo "$1"
    git switch -q -c feature/bar-bump
    write_plugin bar 1.1.0
    write_marketplace 1.0.0 1.1.0
    commit_all "feat(bar): 追加 (v1.1.0)"
    run_release feature/bar-bump
    expect "exit 0" [ "$RC" -eq 0 ]
    expect_out "skip message" "bar@yacchi-plugins: このPCにはインストールされていません (スキップ)"
    expect "no update for bar" [ -z "$(grep -F 'bar@yacchi-plugins' "$LOG" || true)" ]
    expect "merged" [ "$(git log -1 --format=%s main)" = "Merge feature/bar-bump (追加、v1.1.0)" ]
}

case_merge_message() {
    setup_repo "$1"
    check_merge_message feature/msg-version-set "feat(foo): 機能A (v1.1.0)" 1.1.0 "$TRAILERS" \
        "Merge feature/msg-version-set (機能A、v1.1.0)"
    check_merge_message feature/msg-version-unset "fix(foo): 修正B (v1.2.0)" 1.2.0 "" \
        "Merge feature/msg-version-unset (修正B、v1.2.0)"
    check_merge_message feature/msg-plain-set "feat(foo): 機能C" 1.3.0 "$TRAILERS" \
        "Merge feature/msg-plain-set (機能C)"
    check_merge_message feature/msg-plain-unset "feat(foo): 機能D" 1.4.0 "" \
        "Merge feature/msg-plain-unset (機能D)"
}

case_merge_conflict() {
    setup_repo "$1"
    git switch -q -c feature/foo-conflict
    printf 'branch side\n' >plugins/foo/conflict.txt
    write_plugin foo 1.1.0
    write_marketplace 1.1.0 1.0.0
    commit_all "feat(foo): 競合 (v1.1.0)"

    git switch -q main
    printf 'main side\n' >plugins/foo/conflict.txt
    git add -A -- plugins
    git commit -q -m "chore(foo): mainで先に変更"
    git push -q origin main
    git switch -q feature/foo-conflict

    snapshot
    run_release feature/foo-conflict
    expect "non-zero exit" [ "$RC" -ne 0 ]
    expect_out "message mentions conflict" "コンフリクト"
    expect "no merge in progress" test ! -e "$(git rev-parse --git-path MERGE_HEAD)"
    expect "tracked tree clean" test -z "$(git status --porcelain --untracked-files=no)"
    expect_untouched feature/foo-conflict
}

case_installed_record_stale() {
    setup_repo "$1"
    git switch -q -c feature/foo-stale
    write_plugin foo 1.1.0
    write_marketplace 1.1.0 1.0.0
    commit_all "feat(foo): 古いまま (v1.1.0)"
    export FAKE_NO_UPDATE=1
    run_release feature/foo-stale
    expect "non-zero exit when a record is not updated" [ "$RC" -ne 0 ]
    expect_out "stale record reported" "不一致"
}

run_case() {  # run_case <name> <function>
    local dir out rc=0
    CASE_INDEX=$((CASE_INDEX + 1))
    dir="$ROOT/case$CASE_INDEX"
    mkdir -p "$dir"
    out="$("$2" "$dir" 2>&1)" || rc=$?
    if [ "$rc" -eq 0 ] && ! printf '%s\n' "$out" | grep -q 'check failed:'; then
        PASS_COUNT=$((PASS_COUNT + 1))
        printf 'PASS: %s\n' "$1"
    else
        FAIL_COUNT=$((FAIL_COUNT + 1))
        printf 'FAIL: %s (exit %s)\n' "$1" "$rc"
        printf '%s\n' "$out" | sed 's/^/    /'
    fi
}

run_case "happy path: user and project records, merge trailers, untracked files kept" case_happy_path
run_case "refused: version not bumped" case_no_version_bump
run_case "refused: plugin.json and marketplace.json differ" case_version_mismatch
run_case "refused: tracked working tree dirty" case_dirty_tree
run_case "plugin not installed on this PC is skipped" case_plugin_not_installed
run_case "merge message: trailers set/unset, with/without version" case_merge_message
run_case "merge conflict: aborted, back on branch, non-zero" case_merge_conflict
run_case "installed record not updated: reported, non-zero" case_installed_record_stale

printf '\n%d passed, %d failed\n' "$PASS_COUNT" "$FAIL_COUNT"
if [ "$FAIL_COUNT" -ne 0 ]; then
    exit 1
fi
