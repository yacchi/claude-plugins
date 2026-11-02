#!/usr/bin/env bash
#
# scripts/release.sh <branch>
#
# feature/* または hotfix/* ブランチを main に merge して origin に push し、
# このPCにインストール済みのプラグインへ適用する。
# 引数を省略した場合は現在のブランチを対象にする。
#
# merge コミットの本文は環境変数 RELEASE_TRAILERS (トレーラー行、verbatim) で渡す。
# 未設定または空なら本文なし。
#
# 前提条件を満たさない場合は何も変更せずに終了する。
#
set -eu

MARKETPLACE="yacchi-plugins"
CLAUDE_HOME="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
INSTALLED_FILE="$CLAUDE_HOME/plugins/installed_plugins.json"

JSON_PY='
import json
import sys

mode = sys.argv[1]
arg = sys.argv[2] if len(sys.argv) > 2 else ""
data = json.load(sys.stdin)
if mode == "version":
    print(data.get("version") or "")
elif mode == "marketplace":
    for entry in data.get("plugins") or []:
        if entry.get("name") == arg and entry.get("version"):
            print(entry["version"])
elif mode == "installs":
    plugins = data["plugins"] if "plugins" in data else data
    for rec in plugins.get(arg) or []:
        print("|".join([rec.get("scope") or "", rec.get("projectPath") or "", rec.get("version") or ""]))
'

die() {
    printf 'エラー: %s\n' "$*" >&2
    exit 1
}

warn() {
    printf '警告: %s\n' "$*" >&2
}

info() {
    printf '%s\n' "$*"
}

# JSON を標準入力から読み、値を標準出力へ出す。jq を優先し、無ければ python3 を使う。
#   version            : トップレベルの version
#   marketplace <name> : plugins のうち name が一致するエントリの version
#   installs <key>     : インストールレコードを "scope|projectPath|version" の行で出す
json_query() {
    if command -v jq >/dev/null 2>&1; then
        case "$1" in
            version)
                jq -r '.version // empty'
                ;;
            marketplace)
                # shellcheck disable=SC2016
                jq -r --arg name "$2" '.plugins[] | select(.name == $name) | .version // empty'
                ;;
            installs)
                # shellcheck disable=SC2016
                jq -r --arg key "$2" '((if has("plugins") then .plugins else . end)[$key] // []) | .[] | "\(.scope // "")|\(.projectPath // "")|\(.version // "")"'
                ;;
        esac
    else
        python3 -I -c "$JSON_PY" "$@"
    fi
}

# インストール記録を "scope|projectPath|version" の行で出す。ファイルが無ければ何も出さない。
installed_records() {
    if [ ! -f "$INSTALLED_FILE" ]; then
        return 0
    fi
    json_query installs "$1@$MARKETPLACE" <"$INSTALLED_FILE"
}

# 1 レコード分の claude plugin update を実行する。project/local は projectPath で実行する。
update_record() {
    case "$2" in
        project|local)
            (cd "$3" && claude plugin update "$1" --scope "$2" -y) </dev/null
            ;;
        *)
            claude plugin update "$1" --scope "$2" -y </dev/null
            ;;
    esac
}

# --- 引数とブランチの決定 ---

git rev-parse --git-dir >/dev/null 2>&1 || die "git リポジトリの中で実行してください"

if [ "$#" -gt 1 ]; then
    die "引数はブランチ名 1 つだけです。使い方: scripts/release.sh [<branch>]"
fi
case "${1:-}" in
    -*)
        die "オプションは受け付けません。使い方: scripts/release.sh [<branch>]"
        ;;
esac

if [ "$#" -eq 1 ]; then
    branch="$1"
else
    branch="$(git symbolic-ref --short -q HEAD || true)"
fi

case "$branch" in
    feature/*|hotfix/*) ;;
    *)
        die "対象は feature/* または hotfix/* のブランチです (対象: '${branch:-(detached HEAD)}')"
        ;;
esac

git show-ref --verify --quiet "refs/heads/$branch" || die "ローカルにブランチ '$branch' がありません"

if ! command -v jq >/dev/null 2>&1 && ! command -v python3 >/dev/null 2>&1; then
    die "jq か python3 のどちらかが必要です"
fi
command -v claude >/dev/null 2>&1 || die "claude コマンドが見つかりません"

# --- 1. 前提条件 ---

if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
    die "追跡中のファイルに未コミットの変更があります。コミットまたは退避してから再実行してください (未追跡ファイルは対象外です)"
fi

git fetch -q origin || die "git fetch origin に失敗しました"
git show-ref --verify --quiet refs/heads/main || die "ローカルに main ブランチがありません"
git show-ref --verify --quiet refs/remotes/origin/main || die "origin/main がありません"
if [ "$(git rev-parse main)" != "$(git rev-parse origin/main)" ]; then
    die "main が origin/main と一致していません。main を origin/main に揃えてから再実行してください"
fi

subject="$(git log --no-merges -1 --format=%s "main..$branch")"
[ -n "$subject" ] || die "'$branch' に main へ未反映のコミットがありません"

changed="$(git diff --name-only "main...$branch" | sed -n 's#^plugins/\([^/]*\)/.*#\1#p' | sort -u)"

for name in $changed; do
    manifest_path="plugins/$name/.claude-plugin/plugin.json"

    manifest="$(git show "$branch:$manifest_path" 2>/dev/null)" || die "'$branch' に $manifest_path がありません"
    branch_version="$(printf '%s\n' "$manifest" | json_query version)" || die "$manifest_path を読めません"

    marketplace_json="$(git show "$branch:.claude-plugin/marketplace.json" 2>/dev/null)" || die "'$branch' に .claude-plugin/marketplace.json がありません"
    marketplace_version="$(printf '%s\n' "$marketplace_json" | json_query marketplace "$name")" || die ".claude-plugin/marketplace.json を読めません"

    [ -n "$branch_version" ] || die "$manifest_path に version がありません"
    [ -n "$marketplace_version" ] || die ".claude-plugin/marketplace.json に '$name' のエントリ (version) がありません"
    [ "$branch_version" = "$marketplace_version" ] || die "'$name' のバージョンが一致しません: $manifest_path=$branch_version, .claude-plugin/marketplace.json=$marketplace_version"

    main_version=""
    if main_manifest="$(git show "main:$manifest_path" 2>/dev/null)"; then
        main_version="$(printf '%s\n' "$main_manifest" | json_query version)" || die "main の $manifest_path を読めません"
    fi
    [ "$branch_version" != "$main_version" ] || die "'$name' のバージョンが main と同じ ($branch_version) です。バージョンを上げてから再実行してください"
done

# claude plugin validate . はブランチの内容に対して実行する (作業ツリーの現在の内容ではなく)
validate_dir="$(mktemp -d)"
validate_status=0
git archive "$branch" | tar -x -C "$validate_dir"
(cd "$validate_dir" && claude plugin validate .) || validate_status=$?
rm -rf "$validate_dir"
[ "$validate_status" -eq 0 ] || die "claude plugin validate . が失敗しました (ブランチ '$branch' の内容で検証)"

info "前提条件を満たしています"

# --- 2. merge ---

desc="$subject"
version=""
version_re='^(.*) \((v[0-9]+\.[0-9]+\.[0-9]+)\)$'
if [[ "$desc" =~ $version_re ]]; then
    desc="${BASH_REMATCH[1]}"
    version="${BASH_REMATCH[2]}"
fi
type_re='^[a-z]+(\([^)]*\))?!?: (.*)$'
if [[ "$desc" =~ $type_re ]]; then
    desc="${BASH_REMATCH[2]}"
fi

if [ -n "$version" ]; then
    merge_title="Merge $branch (${desc}、${version})"
else
    merge_title="Merge $branch ($desc)"
fi

merge_args=(-m "$merge_title")
if [ -n "${RELEASE_TRAILERS:-}" ]; then
    merge_args+=(-m "$RELEASE_TRAILERS")
fi

git switch -q main
if ! git merge -q --no-ff --no-edit --cleanup=verbatim "${merge_args[@]}" "$branch"; then
    if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
        git merge --abort
    fi
    git switch -q "$branch"
    die "'$branch' を main に merge できませんでした (コンフリクトの可能性があります)。merge を取り消し '$branch' に戻しました。解消してから再実行してください"
fi
info "main に merge しました: $merge_title"

# --- 3. push ---

git push -q origin main || die "git push origin main に失敗しました。ローカルの main に merge コミットが残っています"
git branch -q -d "$branch"
info "origin に push し、ブランチ '$branch' を削除しました"

# --- 4. このPCへ適用 ---

info "marketplace '$MARKETPLACE' を更新します"
claude plugin marketplace update "$MARKETPLACE" || die "marketplace '$MARKETPLACE' の更新に失敗しました (main への push は完了済み)"

if [ -z "$changed" ]; then
    info "変更されたプラグインはありません"
fi

for name in $changed; do
    key="$name@$MARKETPLACE"
    records="$(installed_records "$name")" || die "$INSTALLED_FILE を読めません"
    if [ -z "$records" ]; then
        info "$key: このPCにはインストールされていません (スキップ)"
        continue
    fi
    while IFS='|' read -r scope project_path _; do
        if ! update_record "$key" "$scope" "$project_path"; then
            warn "$key [$scope]: claude plugin update が失敗しました"
        fi
    done <<<"$records"
done

mismatch=0
for name in $changed; do
    key="$name@$MARKETPLACE"
    released="$(git show "main:plugins/$name/.claude-plugin/plugin.json" | json_query version)" || die "plugins/$name/.claude-plugin/plugin.json を読めません"
    records="$(installed_records "$name")" || die "$INSTALLED_FILE を読めません"
    [ -n "$records" ] || continue
    while IFS='|' read -r scope project_path installed_version; do
        label="$key [$scope]"
        if [ -n "$project_path" ]; then
            label="$label ($project_path)"
        fi
        if [ "$installed_version" = "$released" ]; then
            info "$label: $released に更新済み"
        else
            printf '不一致: %s: 期待 %s に対して %s のままです\n' "$label" "$released" "$installed_version" >&2
            mismatch=$((mismatch + 1))
        fi
    done <<<"$records"
done

info ""
info "注意: 新しいバージョンを読み込むには、起動中の Claude Code セッションを再起動してください。"

if [ "$mismatch" -gt 0 ]; then
    die "$mismatch 件のインストール記録が released バージョンと一致しません"
fi
