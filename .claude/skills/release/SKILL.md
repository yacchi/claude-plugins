---
name: release
description: 完成した feature/* または hotfix/* ブランチを main にリリースし、このPCに適用する。「リリースして」「リリース」「/release」と言われたときに使う。
---

# release

このリポジトリのブランチをリリースする。実体は `scripts/release.sh`。

## 手順

1. バージョン上げのコミットがあるか確認する。無ければ `.claude/CLAUDE.md` の規約に従って作る (`plugins/<name>/.claude-plugin/plugin.json` と `.claude-plugin/marketplace.json` の両方を同じコミットで上げる)。
2. 対象ブランチを決める。ユーザーが指定しなければ現在のブランチ。
3. 次を実行する。`<attribution>` にはこのセッションのシステムリマインダーで与えられた attribution 行 (Co-Authored-By と Claude-Session) をそのまま入れる。

   ```sh
   RELEASE_TRAILERS="<attribution>" scripts/release.sh <branch>
   ```

4. 結果を報告する。merge コミット、push、このPCへの適用結果、再起動が必要かどうか。

## 注意

- 「リリースして」と言われた時点で、push までの承認があるものとみなす。
- 前提条件を満たさない場合 (未コミットの変更、バージョン未更新、plugin.json と marketplace.json の不一致、`claude plugin validate .` の失敗など) は、スクリプトが何も変更せずに止まる。原因を直してから再実行する。
