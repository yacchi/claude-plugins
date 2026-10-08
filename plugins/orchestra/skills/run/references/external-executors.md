# 外部エグゼキュータ(Codex/pi)の運用リファレンス

`external_executors`を設定してCodex/piをorchestraのパイプラインに組み込む際の、モデル選定・CLI利用方法・料金の詳細。`SKILL.md` §9はこの機能の存在と設定スキーマだけを説明しているため、実際にディスパッチする段になったらこのファイルを読むこと。

**CLIの実行役は`pi`(§2)と`codex`(§1)の2つだけ。** v0.43.0で`copilot`と`opencode`を削除した(§7)。

背景にある調査結果(なぜこのポリシーになったか)は`poc-findings.md`を、再試験の手順は`poc-fixtures/README.md`を参照。

## 1. Codexのモデルポリシー(Sol / Terra / Luna + effort)

Codex CLI 0.144+はGPT-5.6を3つのサイズティアで提供している — Luna(最小)、Terra(中間)、Sol(最大)。それぞれ`model_reasoning_effort`(`none`/`minimal`/`low`/`medium`/`high`/`xhigh`)と組み合わせられる。以下のベンチマーク数値はサードパーティのPoC([discus0434/customizable-agent-teams](https://zenn.dev/discus0434/articles/customizable-agent-teams))由来であり、方向性の参考であって特定タスクでの保証ではない:

| Model / effort | Coding Index | コスト/タスク | 時間/タスク |
|---|---|---|---|
| Luna / high | 63 | $0.03 | 40s |
| Terra / high | 67 | $0.10 | 66s |
| Sol / low | 70 | $0.06 | 39s |
| Sol / xhigh | 78 | $0.35 | 144s |

**現在のクラス/ロール割り当て**(section 3のClaude側ティア表と対応):
- **`standard`** → **Luna/medium**。最安・最速ティア。effortは`high`ではなく`medium` — このプラグイン自身のPoCで、`high`effortが実践的なタスクで実バグを出し、`medium`ではそのバグが再現しなかった(しかも安く速かった)ことが判明したため(`poc-findings.md` §8)。effortは高ければ良いというものではない。
- **`review`**(`independent-review`向け)→ Sol/low。standardクラス自体のコスト・速度を悪化させずに、別系統のモデルでレビューを追加できる。
- **`deep`** → Sol/xhigh。本当に設計の余地がある/難しい問題のために温存する。
- **Terra**はデフォルトポリシーに含めていない — このプラグイン自身のPoCでは、これまで試したタスクにおいてSol/lowと競合する(時にはより安い)結果が出ており、「Terraは(Lunaに)支配される」という一括りの扱いとは矛盾するが、どちらの方向でも精度の差別化はできなかった(`poc-findings.md` §2-3)。Sol/lowのコストが気になる場合、`independent-review`としてTerraを再検討する価値はある。

**長文コンテキストに関する注意:** Lunaは長文コンテキストの想起が測定可能なレベルで弱い。名目上light/standardクラスのタスクであっても、大規模リポジトリの深い探索が必要な場合(単なる小さな自己完結の変更ではない場合)は、そのタスクに限り`deep`(Sol/xhigh)にエスカレーションすること — サンプル設定の`long_context_escalation`フィールドがこのトリガーを明示的に文書化しているので、毎回ゼロから判断する必要はない。

**(方針転換、v0.27.0)** Codexは今や`dispatch: cli`が既定であり、piと同じ`agent-exec dispatch`/`run`経路で`codex exec --json` / `codex exec resume <SESSION_ID> --json`として直接起動する。理由は3つ:

1. `codex-companion.mjs`(`codex:codex-rescue`が内部で叩くラッパー)が提供する継続手段は`--resume-last`(このリポジトリで最も新しいスレッド)だけで、並列タスク下では**間違ったセッションを拾う**。CLIを直接叩けば明示的なセッションUUIDを渡せる。
2. 直接起動なら`turn.completed`イベントからセッションIDとトークン内訳をその場で得られる — 旧来の、`~/.codex/sessions/**/*.jsonl`を相関IDや経路の指紋でスキャンする事後突合(`parse_codex_rollout_lines`/`match_codex_rollouts`)は、それでも`dispatch: agent`側の帰属機構としては引き続き使われる(下記)。
3. §1のモデルポリシー(Sol/Terra/Luna + effort)自体は変わらない — 変わるのは起動経路だけである。

`codex:codex-rescue`を通る**`dispatch: agent`は、明示的なオーバーライドとして引き続きフルサポートされる**(`external_executors.codex.dispatch: agent`を`orchestra.yaml`で指定する)。この経路を選んだ場合、実行コストは相関ID(パス指紋が主・マーカーが副)でラン単位に帰属し、台帳はセッション単位でも既定で有効なので、`agent-exec usage --run <workflow-run-id>`または`agent-exec usage --session <session-id>`で確認する。相関マーカーの配置理由とルールは`references/config.md`を参照すること。

**タスクごとのセッションストアと自動再開(`dispatch: cli`のCodex専用)。** パス`~/.claude/orchestra/sessions/codex-<task>.json`(ディレクトリ0700・ファイル0600、作成はO_CREAT|O_EXCL、更新はatomic replace)に、タスクごとの直近セッションを`{"executor":"codex","task":"<task id>","session_id":"<uuid>","workdir":"<abspath>","updated":"<ISO8601 UTC>"}`の形で保持する。`agent-exec dispatch --task <id>`(`--resume`未指定)は、このストアを`(executor, task)`で引き、ヒットすれば自動再開(`resumed: true`)、ミスなら新規セッション(`resumed: false` — ミスはエラーでも警告でもない通常状態)。明示`--resume SID`は常にストアより優先。`--no-resume`はそのコール限りストア参照自体をオプトアウトする。保存された`workdir`が今回の`--workdir`と異なる場合はミス扱い(=別ツリーで再利用されたタスクIDが他人のセッションを拾わない)。保存はretentionの新しい設定キーを持たず、台帳/トークンの掃除と同じ`retention_days`を再利用する(`_sweep_retention`)。`agent-exec dispatch session --task <id> [--executor codex] [--json]`で保存済みレコード(無ければ`{"status":"none"}`)を確認でき、これ自体は何も作成・変更しない。

`run` SKILL.md §5/§11の相関ラウンド(correction round)は、まさにこのセッションストアを使ってCodexの会話を再開する — `dispatchClass()`が`--task <id>`付きの`correctionTokenDelta`/`correctionTokenFull`をディスパッチするだけで、agent-exec側が自動再開を判断する。

## 2. pi(`dispatch: cli`)— 唯一のCLI実行役

piは複数プロバイダに単一インターフェースで到達するコーディングエージェントCLIである。v0.43.0で`copilot`と`opencode`を外し、CLIの実行役は`pi`と`codex`の2つになった(§7)。

**なぜpiか(2026-10-08の再ベンチ、`poc-findings.md`の同日節)。** 同じ`gpt-5.6-luna`が、pi経由で49-70秒、opencode経由で86秒、Codex CLI経由で316秒(7月)。入力トークンはpiがopencodeの約1/3-1/4、Copilot CLIの新規入力はopencodeの約19倍。品質は同じモデルなら同じで、速度とトークンはハーネスで決まる。さらに、ChatGPTアカウントのCodex CLIはGPT-6系(`gpt-6-luna`/`gpt-6-sol`/`gpt-6.1-sol`)をHTTP 400で拒否するが、piの`openai-codex`プロバイダは同じサブスクリプションでそれらを実行できる。

**モデルIDは`provider/id`形式。** 裸のIDは解決しない。

| プロバイダ | 例 | 課金 |
|---|---|---|
| `openai-codex/*` | `openai-codex/gpt-5.6-luna`、`openai-codex/gpt-6.1-sol` | ChatGPTサブスクリプション(Codexと共有) |
| `github-copilot/*` | `github-copilot/gpt-5.6-luna`、`github-copilot/claude-haiku-5.5` | トークン従量(2026-06以降のGitHub課金) |

既定の`class_policy`は、`light`/`standard`が`openai-codex/gpt-5.6-luna`/medium、`deep`が`openai-codex/gpt-6.1-sol`/high、`independent-review`が`openai-codex/gpt-6.1-sol`/mediumである。`gpt-6-luna`は再ベンチで36/38(`order=asc|desc`の読み違い、`poc-findings.md`参照)だったため既定には入れていない。

**クォータ共有。** `openai-codex/*`上のpiとCodexは同じChatGPTサブスクリプションを使うため、片方が枯渇またはcooldownになるともう片方も`exhausted-shared:` / `cooldown-shared:`としてスキップされる。設定に書かれた未知のエグゼキュータ名(削除済みの`copilot`/`opencode`など)は`unknown-executor:<name>`としてスキップされ、エラーにはならない — 残っていれば消すこと。

**起動コマンド(agent-execが組み立てる。`command`テンプレートは不要):**

```bash
PI_SKIP_VERSION_CHECK=1 pi -p --mode json --no-approve --no-extensions --no-skills \
  --no-prompt-templates --no-themes --model <provider/id> --thinking <effort> [--session <id>] < PROMPT.md
```

プロンプトはstdin、cwdはワークツリー(agent-execがそのディレクトリで子プロセスを起動する。手で叩くときも先にcdすること)。`--no-*`はユーザーのpi拡張・スキル・テンプレートがワーカーに混ざるのを防ぐ。`--no-approve`は作業先リポジトリの`.pi/`(設定・拡張・スキル)を読み込まない指定で、ワーカーが他人のリポジトリに置かれた拡張を実行しないようにする。`PI_SKIP_VERSION_CHECK=1`は更新確認で子プロセスが止まるのを避ける。orchestraのパイプラインからは生コマンドを組み立てず`agent-exec dispatch --class light`(または`standard`/`deep`/`independent-review`)を使うこと。手動テストなら`agent-exec run pi --capture --model <provider/id> --effort medium --workdir "$WORKDIR" --prompt-file TASK.md`。

**セッション継続。** `--session <id>`で前ラウンドのセッションを再開する。`agent-exec dispatch --task <id>`は§5のセッションストアから自動で再開する。リトライラウンドには、契約の全文ではなく具体的な指摘だけを渡す。

```bash
agent-exec run pi --capture --resume "$SESSION_ID" --model openai-codex/gpt-5.6-luna --effort medium \
  --workdir "$WORKDIR" --prompt-file FEEDBACK.md
```

**usageとコストの意味。** 使用量は出力JSONの`message_end`レコードから集計する(input/output/reasoning/cacheのトークン)。`cost_micro_usd`はpiが報告する**定価ベースの推定値**で、`openai-codex/*`では実際の請求ではない(そのプロバイダはChatGPTサブスクリプションで動く)。実際にドルが動くのは`github-copilot/*`などの従量プロバイダだけである。台帳の合計を実コストとして読まないこと。`agent-exec usage --run <workflow-run-id>`で実行単位の合計を読む。

**罠: 終了コード0でも失敗がありうる。** 最終メッセージの`stopReason`が`"error"`の実行は、終了コード0でも**失敗**として扱う(agent-execは`status: unavailable`として報告する。未対応モデルを指定したときもこの形で失敗する)。終了コードだけで成功判定しないこと。

**導入。** 公式のインストーラとnpm版は、PATH上の`node`で動く。そのため、miseなどでnodeのバージョンが切り替わる環境では壊れやすい。**スタンドアロンのリリースバイナリの利用を推奨する**(GitHubのリリースから取得してPATH上に置く)。導入後に`pi`でプロバイダへログインし(`openai-codex`はChatGPTアカウント)、`agent-exec doctor --json`の`ready.pi.ok`を確認する。

**認可設定。** Claude Code側で必要な許可ルールは`Bash(agent-exec:*)`の1つだけである。`agent-exec install`が`agent-exec`のシムをPATH上(既定`~/.local/bin/agent-exec`)に置く。シムはプラグインのマーケットプレイス・クローンパスを指すため、`/plugin update`でラッパーの中身も追従する。`agent-exec`を使わない場合は`Bash(pi:*)`を許可する。

**封じ込めの注意。** piはツールの実行前に承認を求めない(`-p`では確認なしに読み書き・コマンド実行を行う)。ワークツリー隔離(`agent-exec dispatch`が未コミット作業のあるリポジトリで自動で行う)を使い、ワーカーのプロンプトにstashの禁止と`agent-exec shelf`の使用を明記すること。CLI実行役はClaudeのフックの外で動くため、フックによるガードは効かない。

**リレーの返信形式(`STATUS:`判別子)。** リレーエージェントの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`ならその後に回答本文と`SESSION_ID: <id>`行、`unavailable`なら一行の短い理由(quota/credits/auth/rate-limit)を続ける。生のCLIログはinstructorのコンテキストに渡さない。`agent-exec run pi --capture`は`{ status, answer, session_id, reason, exit_code }`の正規化JSONを直接返すので、リレーはそれを読んで転記するだけでよい。

**Codexの生CLIレシピ(§1参照、`dispatch: cli`が既定):**

```bash
agent-exec codex exec --json --model {model} --config model_reasoning_effort={effort} \
  --sandbox workspace-write --output-last-message {outfile} --cd {workdir} < {promptfile}
# resume:
agent-exec codex exec resume {session_id} --json --model {model} --config model_reasoning_effort={effort} \
  --sandbox workspace-write --output-last-message {outfile} --cd {workdir} < {promptfile}
```

piが`--session`を持つのと同じ意味で、Codexも`exec resume <SESSION_ID>`によりセッション継続可能(session-continuable)なエグゼキュータである — この点でpiと並ぶ扱いになる。

`dispatch: agent`(`codex:codex-rescue`サブエージェント経由、上記の明示的オーバーライド)はこれらのBashパーミッション設定を必要としない — 生のBashコマンドではなくサブエージェント呼び出しだからである。

## 3. 公式の単価表と、実際の請求に関する注意

**GitHub Copilot(piの`github-copilot/*`プロバイダ経由)はトークン従量課金。** 2026-06以降、GitHubは[Models and pricing](https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing)でトークン単価を公開している(2026-10-08確認。価格は変動するため重要な判断の前に再取得すること)。100万トークンあたり:

| Model | 入力 | 出力 |
|---|---|---|
| `gpt-6-luna` | $0.10 | $0.50 |
| `claude-haiku-5.5` | $0.10(100Kトークン超のプロンプトでは$0.50) | $0.50(同$2.50) |
| `gpt-5.6-luna` | $0.20 | $1.20 |
| `gpt-6-sol` / `gpt-6.1-sol` | $2.00 | $10.00 |

以前のpremium request / AIU課金の表と`premiumRequests: 1`の注意書きは廃止した(v0.43.0でCopilot CLIを削除、§7)。piが`github-copilot/*`で報告する`cost`はこの従量課金に対応する実額である。一方、**`openai-codex/*`でpiが報告する`cost`は定価ベースの推定値で、請求ではない**(ChatGPTサブスクリプション。§2)。

**Codex CLIのコストは直接測定できる**: `codex exec --json`は`turn.completed`イベントで`usage.input_tokens`・`usage.cached_input_tokens`・`usage.output_tokens`を返し、モデルごとの単価と掛け合わせられる。ただしChatGPTサブスクリプションで動く場合、これも定価換算であって請求ではない。

The default-on run ledger is the deterministic source for reading pi/Codex cost per workflow run or session: use `agent-exec usage --run <workflow-run-id>` or `agent-exec usage --session <session-id>` rather than estimating from a time window.

**Claude(`Agent`ツール経由)の単価**、[Anthropicの公式料金ページ](https://platform.claude.com/docs/en/docs/about-claude/pricing)より(2026-10-08確認)、100万トークンあたり:

| Model | 入力 | 出力 |
|---|---|---|
| `haiku`(Claude Haiku 5.5) | $0.10(100Kトークン以下のプロンプト) | $0.50 |
| `sonnet`(Sonnet 5.5) | $2.00 | $10.00 |
| `opus`(Opus 5.5) | $4.00 | $20.00 |

`haiku`エイリアスはClaude Haiku 5.5を指す。Codexと異なり、`Agent`ツールの完了結果は入出力の内訳のない`subagent_tokens`という合算値しか返さないため、Claudeサブエージェント実行のコストは全て入力/全て出力の両極からの幅としてしか算出できない。サブスクリプションで動く場合、実際の請求は別である。

## 4. 優先度とリアクティブ・フォールバック(priority)

`priority`は、クラス/ロール(と`light`についてはタスクアーキタイプ)ごとに「試す順序」を宣言するトップレベルの設定キーである。値は`external_executors`のキー(`codex`、`pi`、...)または組み込みClaude実行を表すsentinel `claude`を並べた順序付きリストで、あるエグゼキュータが**unavailable**と判定された時点でのみ次の候補に降格する(=**リアクティブ・フォールバック**)。タスクの実行結果が単に誤っていた場合はフォールバックの対象ではない(後述)。

**この左から右への降格ウォークは、instructorが手で行うものではなく、`agent-exec route --class <cls> [--archetype A] [--exhausted a,b]`(と、それを内包する`agent-exec dispatch`)がコード側で実行する。** `route`は4層configをマージした上で各候補を`enabled`・binary/agent解決可否・`doctor`の`ready.<x>.ok`・`class_policy`該当有無・呼び出し側が渡した`--exhausted`でゲートし、生き残った先頭候補を返す。instructor側に残る唯一の手作業は、あるタスクで`dispatch`が`unavailable`を報告したエグゼキュータを、同じラン内の以降すべての`route`/`dispatch`呼び出しに`--exhausted`として持ち越すこと(sticky exhaustion)だけである — `run` SKILL.md §5の`dispatchClass()`はこれをモジュールレベルの`Set`で自動的に行う。以下のスキーマとJS実装イメージは、`route`が内部で何をしているかを理解するためのものであり、現在ではこれを手で書き写す必要はない。

設定のスキーマは次の通り(§1のCodexティア表・§3のClaude/従量課金単価表と対応させて読むこと):

```yaml
# Ordered executor preference per class/role (and, for the implementation classes,
# per task archetype). The instructor tries candidates left-to-right and drops to
# the NEXT one on a *reactive fallback signal*: the current executor is unavailable
# — NOT the task failing. "Unavailable" = hit a rate-limit / usage-window cap
# (Claude, Codex), ran out of credits / quota (a metered pi provider), is
# `enabled: false`, or its agent/CLI did not resolve in this environment. A task
# that RUNS but returns a wrong result is NOT a fallback signal — that stays on the
# SAME executor and goes through the normal review/retry loop.
#
# Entries are an `external_executors` key (`codex`, `pi`, ...) or the sentinel
# `claude` (the built-in `tiers.<name>` model). An executor named here must have a
# matching `class_policy.<class>` on its `external_executors` block, or it is
# skipped as misconfigured. Once an executor is found unavailable during a run it
# stays skipped for the rest of THAT run (sticky exhaustion) — do not re-probe it
# per task.
#
# Each key's value is a mapping of task-archetype -> ordered list; `default` is the
# catch-all. `light` additionally recognizes `investigation` (read-only exploration
# / research fan-out, no review loop) as distinct from `default` (file-changing
# implementation that goes through the review/retry loop). Other keys normally only
# need `default`.
#
# Omit `priority` entirely to keep the legacy behavior: the Claude `tiers.<name>`
# model, with external executors woven in ad hoc per their own `classes` list.
priority:
  light:
    # Claude Haiku 5.5 first (subscription, fastest in the 2026-10-08 re-bench);
    # pi Luna/medium is the fallback when claude is genuinely unavailable.
    investigation: [claude, pi]
    default: [claude, pi]
  standard:
    # pi Luna clears sonnet-class work, so it leads the standard band.
    default: [pi, claude, codex]
  deep:
    default: [claude, pi, codex]
  review:
    default: [claude]
  independent-review:
    default: [pi, codex]
```

**プロアクティブな残量照会は同梱しない。** `cli`ディスパッチと同じ安全原則により、piの従量プロバイダの残クレジットやCodex/Claudeの使用ウィンドウ残量を事前に問い合わせるコマンドはこのプラグインには存在せず、今後も追加しない。フォールバックの判定材料は、実際にディスパッチした時点で観測される「unavailableシグナル」だけである。

**認可(authorization)の事前チェックは残量照会とは別物で、こちらは推奨する。** 上の「残量照会を同梱しない」は、リモートで変動する*残量*(クレジット/使用ウィンドウ)を事前問い合わせしない、という意味であって*認可*には当てはまらない。`dispatch: cli`のエグゼキュータ(pi・Codex)は、セッションに該当するBash許可ルール(`agent-exec`利用時は`Bash(agent-exec:*)`、手動セットアップ時は`Bash(pi:*)`)が欠けていれば**この実行では絶対に成功しない**(これ以外に必要な環境変数はない)。許可ルールが欠けている場合、残量と違って一過性ではなく、リレーを起動してもパーミッションに即拒否される。

**この事前チェックは`agent-exec doctor`を1回呼ぶだけで済む。** シム(`agent-exec`本体)がPATH上にインストールされているか・どのターゲット(マーケットプレイス・クローンか、壊れやすいcacheパスか)を指しているか、`uv`の有無、4層configのマージ結果、各`external_executors.<name>`の`enabled`/`available`、`permissions.allow`中の`Bash(agent-exec:*)`ルールの有無(ベストエフォート — `~/.claude/settings.json`と`./.claude/settings.json(.local)`のみを走査し、enterprise policyやCLIの`--allowedTools`は見えない)、そして各cliエグゼキュータの総合可否`ready.<name>.ok`(と未充足の`missing[]`)を、`agent-exec doctor`(人間向け`--text`、instructor向け`--json`)の1回の呼び出しでまとめて返す。instructorは`priority`の候補に`dispatch: cli`エグゼキュータを含める前にこれを実行し、`ready.<name>.ok`が`false`ならそのランでは最初からunavailable扱いにして次候補へ降格させる — 個別に`settings.json`を読んだり`which`を叩いたりする必要はない。これは無駄なリレー起動を省く最適化であり、仮にチェックを省いても下記のリアクティブなフォールバック(リレーの`STATUS: unavailable`)が同じ結論に達するバックストップとして残る。`doctor`はシムが未インストールでも(ブートストラップパス経由で直接)呼び出せるため、シムのインストール前診断にも使える。**(v0.11.0以降の補足)** この事前チェック自体は今や`agent-exec route`/`dispatch`が内部で毎回自動的に行うため、instructorが`priority`候補を組み立てる前に別途`doctor`を呼んでフィルタする、という手順はもう不要になった — `route`/`dispatch`を呼ぶだけで、この段落が説明する`ready.<name>.ok`ゲートは常に適用済みの状態になる。`doctor`単体は、セットアップ診断(シムの設置状況の確認)や`config.values.telemetry.enabled`の確認など、他の目的でなお有用。

**unavailable と failed の区別(最重要)。** この2つを混同すると降格ロジックが壊れる:

- **unavailable**(→次の優先度候補に降格し、そのランの残り全体でsticky-skip): レートリミット/使用ウィンドウ上限、クレジット/quotaの枯渇(従量プロバイダ・Codex)、`ChatGPT`サブスクリプション共有グループ内の相手の枯渇/cooldown(`exhausted-shared:`/`cooldown-shared:`)、設定上の未知のエグゼキュータ名(`unknown-executor:<name>`)、`enabled: false`、agent/CLI名がこの環境で解決しない、`class_policy.<class>`が欠落している、のいずれか。
- **failed**(→同じエグゼキュータに留まり、通常のverify/retryループに入る): エグゼキュータは実際に走って何らかの出力を返したが、その結果が誤り/不完全だった場合。「動いたが間違えた」はフォールバック理由にならない — それはそのエグゼキュータの検証・再試行の問題であり、別のエグゼキュータに変えても解決するとは限らないため、まずは同じ系統でリトライさせる。

**エグゼキュータ別のunavailableシグナル(§1・§2・§3で説明した各エグゼキュータの実体と対応):**

- **Claude(`claude`sentinel、`agent()`経由)**: `agent()`が`null`を返す(リトライ後も終了しないエラー)場合をunavailableとみなし、次の候補に降格する。(`null`はユーザーによるagentスキップも意味しうるが、いずれも「先に進む」という結論は同じなので、このオーバーロードは許容する。)
- **Codex(既定の`dispatch: cli`)**: piと同じ判定パス — §2で示した安価なHaikuの**リレーエージェント**が`codex exec --json`を実行し、その結果(終了コード・イベント内容)を検査する。終了コードが非ゼロ、または使用上限/レートリミット/「resets at」/429系のメッセージが見えた場合にunavailableと報告する。
- **Codex(`dispatch: agent`、`codex:codex-rescue`への明示的オーバーライド時のみ)**: `codex:codex-rescue`エージェントが`null`/エラーを返した場合、またはリレーされたテキストが同様のメッセージを報告した場合。
- **pi(`dispatch: cli`)**: §2で示した安価なHaikuの**リレーエージェント**がpiを実行し、その結果(終了コード・出力内容)を検査する。終了コードが非ゼロ、最終メッセージの`stopReason`が`error`、またはエラー/quota/credits/認証系のリミット表示が見えた場合にunavailableと報告する。このリレーは生のCLIログをinstructorに渡してはならず、判定結果だけを短く返す。

  **ただしリミット表示の走査対象は「裏付けのある出力」に限る(default-deny)。** 走査するのはstderr全文・JSONとしてパースできなかったstdout行(平文のquota/認証エラーはまさにここに出る)・そして`session.error`のようなエラー種別イベントだけであり、`assistant.message`に加えて`tool.execution_complete`・`system.message`・reasoning・リクエストのエコーといった**ワーカー自身のテキストを運ぶイベントは走査しない**。これは実害から導かれた規則である: ワーカーが書いていたREADMEに含まれるHTTP 429のバックオフ表や、「Implement JWT auth ...」というタスクプロンプトのエコーが、`exit_code: 0`で正常に回答を返したランを`unavailable`(reason: rate-limit / auth)に転ばせていた。cooldownはこの判定を1時間の実可用性喪失へ増幅するため、`record_unavailable_cooldown`側にも「終了コード0かつ非空の回答があるならcooldownを書かない」という二重の歯止めを置いてある。

**リレーの`STATUS:`判別子。** piのようにワーカーがモデル自身の判断でエラーを報告するのではなく「リレーエージェントが後からCLIの結果を検査する」構成では、Workflowスクリプトが機械的に分岐できる目印が必要になる。そのためpiリレーの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`の場合はそれに続けて回答本文、さらに(セッション継続が必要なら)`SESSION_ID: <id>`行を返す。`unavailable`の場合はそれに続けて一行の短い理由(quota/credits/auth/rate-limitのいずれか)だけを返す。これは§2で示したセッション継続レシピ(§2)と同一の形式であり、実際に§2のリレープロンプトは`STATUS:`行を必ず含めるよう既に更新済みなので、`priority`のフォールバックとセッション継続は同じ1本のリレープロンプトから両方読み取れる。

**sticky exhaustion(枯渇の記憶)。** 1回のオーケストレーション実行の中でunavailableと判定されたエグゼキュータは、`Set`などに記録して以降のすべてのタスクでスキップする。タスクごとに毎回同じエグゼキュータを再プローブしない — 一度枯渇したエグゼキュータが同じランの途中で復活していないか確認する意味はない。

**クロスランcooldown(枯渇の時間減衰)。** 同じunavailableの再発を次のランで毎回発見し直さないため、`agent-exec run --capture`と`agent-exec dispatch`は、パースした結果が`status=unavailable`になった瞬間に、LLMを介さず`~/.claude/orchestra/executor-state.json`(`{executor: {reason, until}}`)へ自動保存する。理由ごとのcooldown秒数は設定(`rate-limit: 900`、`quota: 3600`、`credits: 3600`、`auth: 0`、`nonzero-exit: 0`)から取り、`agent-exec route`/`dispatch`はファイルを読み、期限内のエントリを既存の`--exhausted`と同じ候補ゲートへ渡す(スキップ理由は`cooldown:<reason>`)。これは2026-07-28 09:23〜09:32(UTC)に、外部エグゼキュータ側の本物の枯渇(`session.error` / `errorType: quota` / HTTP 402)を9分間で12回連続して発見し直したための最適化である。ただしcooldownはクラスをunroutableにしない: 全候補がcooldown中ならcooldownを無視してウォークをやり直し、結果に`cooldown_bypassed: true`を付ける。認証とnonzero-exitはリソース枯渇ではなく一過性なので`0`(cooldownなし)であり、missing/corruptな状態ファイル、書き込み不能なパス、保存失敗もすべてfail-openで従来どおりルーティングする。`--no-cooldown`(route/dispatch)、`agent-exec cooldown`(確認)、`agent-exec cooldown clear [executor]`(リセット)、`cooldown.enabled: false`がescape hatchである。これはラン内の`Set`によるsticky exhaustionを置き換えず、その下に加わるクロスラン層である。なお、これは既存の**プロアクティブな残量照会を同梱しない**方針を変更しない — cooldownはディスパッチ時に実際に観測したシグナルだけを反応的に記憶し、リモートの残量APIを問い合わせない。

**`priority`は`classes`より優先してオーダリングを決める。** `priority.<class>`が存在する場合、そのクラス/ロールの候補集合と順序についてはこちらが正となる。`external_executors.<key>.classes`は後方互換のために残されており、`priority`がそのクラス/ロールに存在しない場合にのみ、従来の「各エグゼキュータの`classes`リストから逆引きして織り込む」方式が適用される。いずれの方式でも、実際のモデル/effort/dispatch設定は`class_policy`から取り、`enabled: false`はどちらの方式でもそのエグゼキュータを無条件に除外する。

**タスクアーキタイプの分類**は分解(decomposition)時点でのinstructorの判断であり、実行時に動的に切り替えるものではない: `investigation`は読み取り専用の調査・研究・コードベース探索的なfan-outで、ファイル変更もreviewループも伴わないもの。`default`はそれ以外すべて(review/retryループを通るファイル変更を伴う実装)。他のクラス/ロール(`deep`・`review`・`independent-review`)は通常`default`だけで十分。

**deep-mergeの注意点は他の設定キーと同じ**: `priority`のリストはリストとして扱われるため、より詳細なレイヤ(project設定など)で上書きされると要素単位でマージされず丸ごと置き換わる。部分的に変えたいだけでも、そのクラス/ロール/アーキタイプの配列は全体を書き直す必要がある。

**Workflowスクリプトでの実装イメージ(参考 — v0.11.0以降は不要な手書きロジック)。** 以下は`agent-exec route`/`dispatch`が存在する前に、instructorがこのフォールバック・ウォークを自前で実装するとしたら何をする必要があったかを示す参考コードであり、`route`が内部で行っているマッピング・正規化・降格判定の意味論を理解するために残している。**実際にWorkflowスクリプトを書くときは、これを書き写すのではなく`run` SKILL.md §5の`dispatchClass()`(1回の`agent-exec dispatch --class <cls> ... --capture`呼び出しを1つの安価なリレーエージェントに任せるだけの実装)を使うこと。** 各候補を実際のディスパッチ手段(Claudeの`agent()`、Codexの`codex:codex-rescue`、piのリレーエージェント)にマップし、戻り値を`{ status: 'ok'|'unavailable', answer?, reason?, sessionId? }`という共通の形に正規化してから、フォールバックのループに渡す、という考え方自体は`route`/`dispatch`の内部実装と同じである:

```javascript
// candidates: 解決済みの、このクラス/ロール(+アーキタイプ)向け順序リスト
// 例: ['pi', 'codex', 'claude']
const exhausted = new Set() // このランの間だけ有効なsticky exhaustion

async function runOn(exec, taskPrompt, workdir) {
  if (exec === 'claude') {
    const answer = await agent(taskPrompt, { label: 'light', model: tierModelFor('light') })
    if (answer === null) {
      return { status: 'unavailable', reason: 'agent() returned null (terminal error or skip)' }
    }
    return { status: 'ok', answer }
  }

  if (exec === 'codex') {
    const answer = await agent(taskPrompt, { label: 'codex-rescue', agentType: 'codex:codex-rescue' })
    if (answer === null) {
      return { status: 'unavailable', reason: 'codex:codex-rescue returned null' }
    }
    if (/usage.?limit|rate.?limit|resets at|\b429\b/i.test(answer)) {
      return { status: 'unavailable', reason: 'codex usage-limit signal in relayed text' }
    }
    return { status: 'ok', answer }
  }

  if (exec === 'pi') {
    // 認可の事前チェック(§4冒頭)で pi が未認可と判明していれば、instructor は
    // そもそも candidates から外しているはず。ここに到達した時点でもし未認可なら、
    // リレーは即 STATUS: unavailable を返す — それがランタイム側のバックストップ。
    // §2の pi リレーレシピと同じ STATUS: 判別子付きリレー。
    const reply = await agent(
      'Run: agent-exec run pi --capture --model openai-codex/gpt-5.6-luna --effort medium ' +
      '--workdir ' + workdir + ' --prompt-file {promptfile} ; this prints one normalized JSON ' +
      'object { status, answer, session_id, reason, exit_code } to stdout — parse it directly. ' +
      'Reply with STATUS: ok or STATUS: unavailable as the first line (mirroring the JSON\'s ' +
      '`status`), followed by the `answer` and SESSION_ID: line on ok, or the `reason` value ' +
      'on unavailable.',
      { label: 'pi-relay', model: 'haiku', effort: 'low' },
    )
    const status = /^STATUS:\s*(ok|unavailable)/m.exec(reply)?.[1]
    if (status !== 'ok') {
      const reason = reply.split('\n').slice(1).join(' ').trim() || 'pi relay reported unavailable'
      return { status: 'unavailable', reason }
    }
    const sessionId = /SESSION_ID:\s*(\S+)/.exec(reply)?.[1]
    const answer = reply.replace(/^STATUS:.*\n/, '')
    return { status: 'ok', answer, sessionId }
  }

  return { status: 'unavailable', reason: 'unknown or misconfigured executor: ' + exec }
}

async function dispatchWithFallback(candidates, taskPrompt, workdir) {
  for (const exec of candidates) {
    if (exhausted.has(exec)) continue
    const r = await runOn(exec, taskPrompt, workdir)
    if (r.status === 'unavailable') {
      exhausted.add(exec)
      log(exec + ' unavailable (' + r.reason + ') — falling back')
      continue
    }
    return { exec, ...r }
  }
  return { status: 'all-exhausted' }
}
```

`runOn`が返す`status`が`'unavailable'`のときだけ`exhausted`に加えて次の候補へ進み、それ以外(`'ok'`)は即座にそのエグゼキュータの結果を採用して呼び出し元(review/retryループ)に返す。タスクが「動いたが結果が誤り」だった場合の再試行は、この`dispatchWithFallback`の外側 — 同じ`exec`に対する通常のreview/retryロジック側の責務であり、`priority`の降格ロジックには一切関与しない。

**外部エグゼキュータの自由文出力をJSONに正規化する(特にCodexの`independent-review`)。** Claudeの`agent()`は`schema`オプションでStructuredOutputツールを強制できるが、`dispatch: agent`の外部エグゼキュータ(CLI裏付けの`codex:codex-rescue`など)はそのツールを必ずしも尊重せず、自由文を返しうる。そこで外部エグゼキュータにverdict/レビューを求めるときは、(1)プロンプトで明示的にJSONだけを出力させ(スキーマ例を本文に埋め込み、「JSON以外は出力するな」と指示する)、(2)戻り値を寛容にパースして`VERDICT_SCHEMA`(run SKILL.md §5)と同じ形に正規化する。生の自由文をそのまま`verdict.pass`として扱わないこと — 独立レビューであってもフォールバック契約(structured verdict)と同じ形に畳んでからinstructorに返す:

```javascript
// 外部エグゼキュータの自由文から verdict JSON を取り出して VERDICT_SCHEMA 形に正規化する。
// モデルがコードフェンスで包んでも、貪欲な波括弧マッチが中のオブジェクトを拾う。
function parseExternalVerdict(text) {
  if (text == null) return null
  const braced = /\{[\s\S]*\}/.exec(text) // 最初の { から最後の } まで
  if (braced) {
    try {
      const v = JSON.parse(braced[0])
      if (typeof v.pass === 'boolean') {
        return { pass: v.pass, summary: String(v.summary ?? ''), feedback: v.feedback ?? [],
                 tests_kept: v.tests_kept ?? [], tests_loc_added: v.tests_loc_added,
                 impl_loc_changed: v.impl_loc_changed }
      }
    } catch (_) { /* パース失敗 → 下の安全側フォールバックへ */ }
  }
  // JSONを取り出せなかった = レビューが形式に従わなかった。安全側に倒して
  // pass=false + 生テキスト先頭をsummaryに入れて返し、retry/降格判断はループ側に委ねる。
  return { pass: false, summary: 'unparseable external verdict: ' + text.slice(0, 500), feedback: [] }
}
```

Codex independent-reviewに渡すプロンプト末尾の指示例: 『Reply with ONLY a JSON object of the form {"pass": boolean, "summary": string, "feedback": [{"case","expected","actual"}]}. Output nothing else — no prose, no explanation.』

## 5. `agent-exec config` / `run` / `route` / `dispatch` (追加サブコマンド)

`agent-exec <executor> [raw args...]`のパススルーは変わらず残る。以下は、その上に足された**追加的**なサブコマンド群で、これまでinstructorがコンテキスト内で行っていた作業(configのマージ、`priority`の降格ウォーク、可否の事前チェック)を`agent-exec`側に寄せるためのものである。特に`route`/`dispatch`(v0.11.0で追加)により、エグゼキュータ選択はinstructorの判断ではなく`agent-exec`が実行するコードになった。

- **`--prompt-file`は`dispatch`/`run`の両方で繰り返し指定できる。** 指定順にファイルを読み、連続するファイルの間を空行1つで連結して1つのプロンプトにする。1回だけ指定した場合は従来どおり。ファイルが存在しない、または読めない場合は使用法エラーとして終了コード2で終了し、stderrに該当パスを出し、ディスパッチは行わない。
- **`agent-exec config [--json]`** — 4層のorchestra設定(built-in defaults → `~/.claude/orchestra.yaml` → `./.claude/orchestra.yaml` → `./.claude/orchestra.local.yaml`)を、SKILL.md §9と同じ規則(mapping key-by-keyでマージ、スカラー/リストは丸ごと置換、明示的な`null`は値として扱う)で決定的にdeep-mergeし、`external_executors.<name>`のうち`enabled: true`かつ`dispatch: cli`のものについては実行ファイルの`shutil.which`可否を`"available"`として付与したうえで、解決済み設定をJSONとしてstdoutに出す。これにより、instructorがYAMLレイヤをコンテキスト内でマージする必要がなくなる。トップレベルには`warnings`配列(pre-0.4語彙検出`legacy_vocab`、`orchestra.json`検出`legacy_json`。無ければ`[]`)も含まれるため、`setup`スキルはレガシー検出のために生のレイヤファイルを読む必要がない。
- **`agent-exec run <profile> --model M --effort E --workdir W --prompt-file F [--prompt-file G ...] [...]`** — profile(`pi`・`codex`)ごとのCLIフラグ規約と安全側デフォルトを一本化した、正規化ずみのディスパッチ入口。§2で示した生のpi起動コマンドと等価なものを、`agent-exec run pi --model openai-codex/gpt-5.6-luna --effort medium --workdir "$WORKDIR" --prompt-file TASK.md`のように短く書けるようにするもの。
  - **`--capture`を付けると、`os.execvpe`によるプロセス置換ではなく、エグゼキュータをサブプロセスとして起動してstdout/stderrをキャプチャし、そのJSONLをパースして`{ status, answer, session_id, reason, exit_code }`という正規化JSONオブジェクトを1つ、agent-exec自身のstdoutに出して終了コード0で返す。** `status`は`"ok"`または`"unavailable"`(quota/credits/auth/rate-limit/nonzero-exit/errorのいずれかが`reason`に入る)。エグゼキュータ側のunavailableは非ゼロ終了で表現されず、この`status`フィールドで表現される点に注意 — Haikuリレーはこの1つのJSONを読むだけでよく、jqでのJSONLパースはリレー側にはもう不要(§2)。`--capture`を付けない場合は従来どおり`os.execvpe`によるハンドオフのままで、挙動に変化はない。
- **`agent-exec route --class <light|standard|deep|review|independent-review> [--archetype default|investigation] [--exhausted a,b] [--json|--text]`**(v0.11.0で追加)— §4の`priority`ウォークをコード側で実行する読み取り専用サブコマンド。4層configをマージし、各候補を`enabled`・binary/agent解決可否・`doctor`相当の`ready.<x>.ok`・`class_policy`該当有無・呼び出し側の`--exhausted`でゲートしたうえで、生き残った先頭候補を`{ class, archetype, executor, dispatch, model, effort, agent_type, candidates, remaining, skipped, source }`として返す(`skipped`には各候補が外れた理由が`{executor, reason}`で入る)。instructorはこれを呼ぶだけで、`priority`リストを手で読んで降格判定する作業から解放される。
- **`agent-exec dispatch --class <cls> --prompt-file F [--prompt-file G ...] --workdir W [...]`**(v0.11.0で追加)— `route`をさらに一歩進めた、実際にディスパッチまで行うサブコマンド。内部で`route`を呼び、勝者が`dispatch: cli`のエグゼキュータ(pi・既定のCodex)なら実際に実行して`{ status: "ok"|"unavailable", answer, session_id, resumed, reason, exit_code, executor, model, effort, route }`を返す(`session_id`/`resumed`はセッション概念を持たないエグゼキュータでは`null`/`false`)。勝者がClaudeまたは`dispatch: agent`のエグゼキュータ(明示的にオーバーライドされたCodexなど)なら、実行はinstructor自身の`agent()`/Agentツール呼び出しでしか行えないため`{ status: "delegate", executor, model, effort, agent_type, route }`を返すに留める。有効な候補が一つも残らなければ`{ status: "unroutable", route }`。**これが`run` SKILL.md §5の`dispatchClass()`が包んでいる1回の呼び出しそのものであり、instructorが`light`/`standard`タスクのエグゼキュータを自分で決めることは無くなった — 1つの安価なリレーエージェント経由でこれを尋ね、返ってきたJSONで分岐するだけになる。** `--task <id>`かつ`--resume`未指定なら、`dispatch: cli`エグゼキュータについてタスクごとのセッションストアを自動参照する(上記「タスクごとのセッションストアと自動再開」参照)。`--no-resume`はこの参照だけをオプトアウトする。`agent-exec dispatch session --task <id> [--executor codex] [--json]`は保存済みレコードを読むだけの副作用なしサブコマンド。
- **`agent-exec doctor [--json | --text]`**(既定`--json`)— シム自身のインストール状況(パス・PATH上か・マーケットプレイス/cacheどちらを指しているか)、`uv`の有無、`agent-exec config`と同じ4層configマージ結果(同じ`warnings`配列を`config.warnings`として含む)、`permissions.allow`中の`Bash(agent-exec:*)`ルールの有無(ベストエフォート)、そして各cliエグゼキュータの総合可否`ready.<name>.ok`(未充足の`missing[]`付き)を1回でまとめて返す、読み取り専用の診断サブコマンド。上記§4の認可事前チェックはこれ1本に集約する。**解決済みのconfig全体(`tiers`/`available`注記つき`external_executors`/`priority`)を`config.values`として同梱する**(解決失敗時は`null`、理由は`config.error`)ため、instructorは起動時の`doctor`1回で「可否verdict」と「解決済みモデルポリシー」の両方を得られ、`agent-exec config`を別途呼ぶ必要はない(config単体が欲しいときだけ`config`を使う)。シムが未インストールでもブートストラップパス経由で直接呼び出せ、レポート自体が「未準備」を表現するため、内部エラーでない限り常に終了コード0。
  - `executors`セクションは、有効化済み`dispatch: cli`のものだけでなく**既知の全エグゼキュータ**(`codex`・`pi`)を`enabled`/`dispatch`/`binary`/`available`つきで網羅する — 既定の`dispatch: cli`のCodexは他のcliエグゼキュータと同じく`ready.<name>.ok`(と`missing[]`)を持つ。`dispatch: agent`へ明示的にオーバーライドされた場合のみ、`available`(バイナリのPATH上の有無、参考情報)と、実際の可否はサブエージェントのセッション解決に依存し`agent-exec`からは判定不能である旨の`note`付きに切り替わり、`ready`のverdictは作られない(判定不能なため)。`setup`スキルはこれにより、`codex`/`pi`いずれについても個別の`command -v`を実行する必要がない。

## 6. `agent-exec telemetry` — オプトインの匿名テレメトリ

`agent-exec`にはメンテナがorchestraを改善するための、オプトイン・匿名化された「クラッシュダンプ的」テレメトリ機構がある。設定・意味論の全体像は`run`スキルのSKILL.md §10を参照。ここではCLIサブコマンドと、`run --capture`側の自動ログの仕様のみを記す。

**既定はOFF。** `orchestra.yaml`の`telemetry.enabled`が`true`の場合のみ有効になる(`examples/orchestra.yaml`参照)。解決済みの値は`doctor`の`config.values.telemetry.enabled`にも現れる(§5の`config.values`と同じ仕組み)。無効時は、記録系サブコマンドはすべて何もせず終了コード0を返す(サイレントno-op)。

**サブコマンド:**

- **`agent-exec telemetry enable [--scope user|project|local]`** / **`disable [--scope user|project|local]`** — 指定スコープ(既定: user = `~/.claude/orchestra.yaml`)のorchestra.yamlの`telemetry.enabled`をトグルする。コメント保存型の手術的編集で、ファイルが無ければスタブを作成する — YAMLの手編集は不要。
- **`agent-exec telemetry record (--json STR | --file F)`** — サニタイズ済みレコードを1件だけ追記する。telemetryが無効なら何もせず終了コード0(no-op)。レコードの内容をstdoutにエコーすることは絶対にない。
- **`agent-exec telemetry show [--json]`** — 蓄積済みレコードを確認する。
- **`agent-exec telemetry archive [--out FILE]`** — 蓄積済みレコードを`.tar.gz`にまとめる。
- **`agent-exec telemetry clear`** — 蓄積済みレコードを削除する。

`show`/`archive`/`clear`の3つは、`enabled`の値に関わらず常に動作する — telemetryを無効化しても、既に書かれたレコードの閲覧・アーカイブ・削除は妨げられない(無効化が止めるのは新規書き込みだけ)。

**redactionはallowlistで担保される — 呼び出し側の申告を信用しない。** `agent-exec`のコード内に、フィールド名と許容される列挙値のALLOWLISTがあり、これを通過するのはenumで列挙済みのカテゴリ値と非負整数だけである。`schema_version`/`ts`/`os`は`agent-exec`自身がスタンプする(呼び出し側からは渡せない)。この結果、プロンプト・タスク本文・ファイル名・パス・タスクID・`summary`文字列・コード・エラーメッセージ文字列を保存することは構造的に不可能になる — 自由記述を受け付けるフィールドが存在せず、かつenumフィールドは完全一致でチェックされるため、enum欄に自由文を紛れ込ませても黙って弾かれるだけで記録はされない。

許容フィールド一覧:

| フィールド | 値 |
|---|---|
| `event` | `run_summary` \| `dispatch` |
| `lane` | `express` \| `orchestrated` |
| `orchestra_version` | semver |
| `executor` | `claude` \| `codex` \| `pi` |
| `cls` | `light` \| `standard` \| `deep` \| `review` |
| `status` | `ok` \| `unavailable` |
| `reason` | `quota` \| `rate-limit` \| `credits` \| `auth` \| `nonzero-exit` \| `error` |
| `resumed` | boolean |
| `task_count`/`pass`/`fail`/`exhausted`/`fallbacks` | 非負整数(`run_summary`専用) |
| `classes`/`rounds`/`executors_used`/`external_enabled` | dict型ヒストグラム(`run_summary`専用) |

**`agent-exec run ... --capture`の自動ログ。** §5の`run`サブコマンドに`--cls CLASS`(`light`/`standard`/`deep`/`review`)を渡すと、そのディスパッチの能力クラスとしてタグ付けされる。`--capture`を付けたときは、結果を出力した後に`event: dispatch`のレコードを1件、`agent-exec`自身が自動でtelemetryに追記する(`status`/`reason`/`cls`など)。これはLLMを介さず`agent-exec`内部で完結する。Copilotの`answer`(回答本文)がtelemetryに記録されることは絶対にない。`agent-exec dispatch --class <cls> ... --capture`(§5)も内部で`run`と同じCLI実行パスを通るため、`--class`から`cls`が自動的に埋まった同じ`dispatch`レコードが同様に自動で記録される — instructor側で`--cls`を別途渡す必要はない。

**run_summaryはinstructor自身ではなくリレー経由。** 1回のオーケストレーション実行の終わりに、telemetryが有効なら(`doctor`の`config.values.telemetry.enabled`で判定)、instructorは安価なHaikuリレーエージェントに`agent-exec telemetry record --json '...'`を1回実行させ、`run_summary`レコードを1件だけ記録する — instructor自身が直接実行することは絶対にない。無効なら何もせずスキップする。これはinstructorのコンテキストを汚さないための設計であり、また`record`はどのみちカテゴリ値/数値フィールドしか受け付けないため、instructorが直接叩いても得られる自由度は無い。

## 7. 削除したエグゼキュータ(v0.43.0)

v0.43.0で次の2つのCLI実行役を削除した。CLIの実行役は`pi`(§2)と`codex`(§1)だけである。

- **`copilot`(GitHub Copilot CLI)** — 同じモデルでもpiより遅く、新規入力トークンが桁違いに多い(2026-10-08の再ベンチでopencodeの約19倍)うえ、課金はpiの`github-copilot/*`プロバイダと同じトークン従量で、勝てる点が無い。Copilotのモデルは今後も`pi`の`github-copilot/*`経由で使える(§3の単価表)。
- **`opencode`** — 同じ課金の同じモデルに対して、piより遅く(同じ`gpt-5.6-luna`で86秒対49-70秒)、入力トークンは約3-4倍。長時間のランナウェイ(単一の`glob`が2539秒)も観測した。Codex系モデルを呼ぶ用途はpiの`openai-codex/*`が担う。

理由は両者とも「同じ課金で厳密に劣る」ことである(データと表は`poc-findings.md`の2026-10-08節)。ユーザー設定に`copilot`/`opencode`のブロックや`priority`の項目が残っていても、未知のエグゼキュータ名として`unknown-executor:<name>`でスキップされ、エラーにはならない。ただし残してもその項目は何も効かないので削除すること。過去のPoC節(`poc-findings.md`の2026-07〜09)にある両者の記述は履歴としてそのまま残してある。
