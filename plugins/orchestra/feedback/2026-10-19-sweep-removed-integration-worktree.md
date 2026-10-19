# run 終了時の sweep が、使用中の統合ワークツリーを半壊させた

- 日付: 2026-10-19
- 対象バージョン: orchestra 0.56.0 (agent-exec の shim は marketplace clone を指す)
- 観測元: ai2-lcs-cdk の管理画面の切り出し (programme、5 パッケージ、run `wf_3eb945ef-77f`)。統合先は `--into prog-admin`、ワークツリーは `.../asr-orchestra/2145cdba-prog-admin`
- 分類: 不具合 (データ消失の危険)。今回は統合ブランチ `orchestra/2145cdba/prog-admin` が残ったので、成果物は失われなかった

## 起きたこと

1. programme の Workflow が完了した (5 パッケージとも merged)。統合ワークツリーは、全部コミット済みでクリーンな状態だった。
2. instructor は SKILL.md §5 の「At run end, always: ... `agent-exec isolate sweep --include-current --text`」に従い、haiku の relay で sweep を起動した。
   **同時に**、統合ワークツリーで全体の検証 (pnpm test / tsc / synth) を走らせる Sonnet のエージェントも起動した。
3. sweep の結果 (抜粋):
   ```
   error        prog-admin       0.0d      .../asr-orchestra/2145cdba-prog-admin  (git worktree remove failed)
   removed      K0 ... K3
   orphan       (branch)         0.0d      orchestra/2145cdba/prog-admin
   summary: removed 4, would-remove 0, dirty 22, skipped 0, external 0, errors 1, branches 1 orphan / 0 deleted
   ```
4. 統合ワークツリーは次の状態で残った。
   - `.git/worktrees/2145cdba-prog-admin` は消えている (`git status` → `not a git repository: .../.git/worktrees/2145cdba-prog-admin`)。
   - ディレクトリの中身は途中まで消えていた: ルートの `tsconfig.packages.json` が無い、`node_modules` も一部欠損 (`shell-quote` など)。
   - 並行していた検証は、全ステップが環境エラーで落ちた (TS5083、`Cannot find module`、`run-s` 起動不能)。
   - sweep 自体も relay の 120 秒を超え、バックグラウンドに回ってから約 6 分かかった。node_modules 込みのツリーを消していたためとみられる。
5. 復旧: 壊れたディレクトリを退避し、`git worktree prune` → `git worktree add <同じパス> orchestra/2145cdba/prog-admin` → `pnpm install`。

## 原因 (コードを読んだ範囲)

### 1. 統合ワークツリーが、回収済みのタスクのワークツリーと同じ扱いで消される

`internal/isolate/sweep.go` は `List()` の全エントリを同じ条件で判定し、`uncollected()` が dirty でなければ `Remove()` する。

`internal/isolate/remove.go` の `uncollected()` は次のとおりで、統合ワークツリーはむしろ**消されやすい**。

```go
ignoredOK := (created != "" && created == ignoredDigest(ignored)) || matches ||
    ReadRole(path) == RoleIntegration
```

- 統合ワークツリーは integrator がコミットするので、追跡ファイルの差分は通常ゼロになる (`trackedOK`)。
- ignored の変化 (node_modules、cdk.out) は、`RoleIntegration` なら無条件に OK とされる。

その結果、「コミット済みだが、まだユーザーのブランチに取り込まれていない統合ブランチ」を持つワークツリーが「回収済み」と判定され、削除対象になる。

- 統合ブランチ自体は orphan として残った。sweep もブランチは消さないので、成果物は守られている。
- 一方で、ワークツリーは「取り込むまで作業場所として使う」ものだ。今回のように検証・デプロイ・追加修正をまだそこで行う段階で消されると、作業が止まる。

### 2. 使用中かどうかを見ていない

`Current && !IncludeCurrent` 以外の生存チェック (`sessionIsLive`) は、**他の**セッションの heartbeat だけを見る。

- 同じセッションの別プロセス (並行する検証エージェント、pnpm、tsx) がそのツリーで動いていても、`--include-current` を付けると消しにいく。
- `worktree_lease` のような「書き手がいる」印も、sweep は参照していないようだった。

### 3. 削除が非原子的で、失敗すると半壊のまま残る

`git worktree remove` が途中で失敗した (使用中のファイル、または時間切れ)。それでも、メタデータ (`.git/worktrees/<name>`) は消えた。ディレクトリは中身が欠けたまま残った。

- sweep の報告は `error ... (git worktree remove failed)` の 1 行だけだった。ツリーが使えない状態になったこと、つまり git から切り離された半端なディレクトリが残ったことは示されない。

### 4. SKILL.md の指示が programme の統合ワークツリーと噛み合わない

§5「At run end, always ... never conditionally」と programme.md の rolling integration base が衝突している。programme では run が終わっても、統合ワークツリーで次の作業が続く。

- 次の作業の例: 全体の検証、Sandbox へのデプロイ、ユーザーのブランチへの squash。
- 「無条件に `--include-current`」は、この段階の統合ワークツリーを巻き込む。
- 同じセッションの以前の run でも同じ危険に気付き、「統合ブランチを取り込む前に統合ワークツリーを sweep しない」と手元にメモしていた。今回は SKILL の「always」に従って実行してしまった。

## 対策の案

1. **統合ワークツリーは sweep の既定の対象から外す。**
   - 対象から外す条件: `RoleIntegration` のツリーは、統合ブランチがどのローカルブランチ (統合ブランチ自身を除く) にも取り込まれていない間は、`skipped` (`integration branch not yet taken; pass --include-integration`) にする。
   - 少なくとも `ignoredOK` の `RoleIntegration` 例外を、「消してよい」方向に効かせない。
2. **使用中のツリーを消さない。**
   - 削除の前に、そのパスを cwd に持つプロセス、または開いているファイルがあるかを確かめる (`lsof +D` は重いので、`/proc` 相当か、lease ファイルで)。あれば `skipped (in use)` にする。
   - `worktree_lease` があるならそれも見る。
3. **削除を二段にする。**
   - 先にディレクトリを同じファイルシステム内の退避名へ rename する。次に `git worktree prune`。最後に退避先を消す。
   - 途中で失敗しても「元のパスに半壊のツリー」は残らない。失敗時は rename を戻す。
   - 失敗の報告には、残った状態 (メタデータ有無、ディレクトリ有無) と復旧手順を出す。
4. **SKILL.md §5 の「always, never conditionally」に programme の例外を書く。**
   - programme の run では、run 末尾の sweep は `--include-current` を付けても統合ワークツリーを除外する、と明記する (1 が入れば自動)。
   - 統合ワークツリーの片付けは「統合ブランチをユーザーのブランチへ取り込んだ後」に、明示的な指示で行う。
   - あわせて「sweep は、そのツリーを使う他の作業と並行して走らせない」も書く。
5. relay の 120 秒の上限と、node_modules 込みの削除時間が噛み合っていない。sweep を `--detach` で起動して結果を待つ形にするか、上の 3 で退避先の削除を非同期にする。

## 対応 (v0.57.0)

- 原因 1 の補足: 再現試験では、統合ワークツリーは base 以降のコミットが差分に出るため `dirty` になり、削除されなかった。削除対象と判定された経路は特定できていない。ただし、判定を「clean」に倒す穴を 2 つ確認した。1 つは baseline マーカーが欠けたときの `ReadBaseline` の HEAD フォールバック。もう 1 つは、`git diff` の失敗を握りつぶした結果、空の差分になることだ。さらに、削除が成功していれば、`Remove` は `git branch -D` で統合ブランチまで消していた。
- 対策 1: 審査ゲート `uncollected()` で、統合ツリーを「ブランチ tip が他のローカルブランチから到達できない間は dirty」と判定する。remove・sweep・SessionEnd のすべてに効き、collect 済みでも消えない。`--include-integration` は作らず、上書きは `--force` に一本化した。あわせて、diff の失敗と baseline マーカーの欠落も dirty とした。
- 対策 2: 既存の `worktree_lease` (書き手の lease) と新設の activity スタンプ (`orchestra-activity.json`) が新しい間、および `isolate integrate` のロックが握られている間は `busy` として残す。activity スタンプは、linked worktree 内で実行されたすべての Bash 呼び出し (読むだけのテスト実行を含む) で hook が更新する。サブエージェントの分は SubagentStop で外れ、それ以外は 600 秒で切れる。hook の外で動くバックグラウンドのプロセスと CLI executor は検出しない。
- 対策 1 の補足: 「取り込み済み」は、orchestra/* 以外のローカルブランチに merge されたか、squash で変更ファイルの内容が一致する状態を指す。取り込み済みの統合ツリーは、未コミットの変更だけを数え、collect なしで消せる。
- 対策 3: 削除の順序を「退避名へ rename → `git worktree prune` → `branch -D` → 退避先を削除」に変えた。失敗時は rename を戻し、残った状態と復旧手順を報告する。退避先が残った場合は、次の sweep が消す。
- 対策 4: SKILL.md §5、programme.md、isolation.md、workflow-template.md、cleanup スキルを更新した。
- 対策 5: 削除の遅い部分は、git の状態が整った後に動くようになった。relay が時間切れで打ち切っても、残るのは退避先だけになる。`--detach` は入れていない。
