# 外部エグゼキュータ(Codex/Copilot)の運用リファレンス

`external_executors`を設定してCodex/Copilotをorchestraのパイプラインに組み込む際の、モデル選定・CLI利用方法・料金の詳細。`SKILL.md` §9はこの機能の存在と設定スキーマだけを説明しているため、実際にディスパッチする段になったらこのファイルを読むこと。

背景にある調査結果(なぜこのポリシーになったか)は`poc-findings.md`を、再試験の手順は`poc-fixtures/README.md`を参照。

## 1. Codexのモデルポリシー(Sol / Terra / Luna + effort)

Codex CLI 0.144+はGPT-5.6を3つのサイズティアで提供している — Luna(最小)、Terra(中間)、Sol(最大)。それぞれ`model_reasoning_effort`(`none`/`minimal`/`low`/`medium`/`high`/`xhigh`)と組み合わせられる。以下のベンチマーク数値はサードパーティのPoC([discus0434/customizable-agent-teams](https://zenn.dev/discus0434/articles/customizable-agent-teams))由来であり、方向性の参考であって特定タスクでの保証ではない:

| Model / effort | Coding Index | コスト/タスク | 時間/タスク |
|---|---|---|---|
| Luna / high | 63 | $0.03 | 40s |
| Terra / high | 67 | $0.10 | 66s |
| Sol / low | 70 | $0.06 | 39s |
| Sol / xhigh | 78 | $0.35 | 144s |

**現在のロール割り当て**(section 3のClaude側ティア表と対応):
- **`worker`** → **Luna/medium**。最安・最速ティア。effortは`high`ではなく`medium` — このプラグイン自身のPoCで、`high`effortが実践的なタスクで実バグを出し、`medium`ではそのバグが再現しなかった(しかも安く速かった)ことが判明したため(`poc-findings.md` §8)。effortは高ければ良いというものではない。
- **`independent-verifier`** → Sol/low。workerティア自体のコスト・速度を悪化させずに、別系統のモデルでレビューを追加できる。
- **`hard_worker`** → Sol/xhigh。本当に設計の余地がある/難しい問題のために温存する。
- **Terra**はデフォルトポリシーに含めていない — このプラグイン自身のPoCでは、これまで試したタスクにおいてSol/lowと競合する(時にはより安い)結果が出ており、「Terraは(Lunaに)支配される」という一括りの扱いとは矛盾するが、どちらの方向でも精度の差別化はできなかった(`poc-findings.md` §2-3)。Sol/lowのコストが気になる場合、`independent-verifier`としてTerraを再検討する価値はある。

**長文コンテキストに関する注意:** Lunaは長文コンテキストの想起が測定可能なレベルで弱い。名目上`worker`ティアのタスクであっても、大規模リポジトリの深い探索が必要な場合(単なる小さな自己完結の変更ではない場合)は、そのタスクに限り`hard_worker`(Sol/xhigh)にエスカレーションすること — サンプル設定の`long_context_escalation`フィールドがこのトリガーを明示的に文書化しているので、毎回ゼロから判断する必要はない。

## 2. Copilotのモデルカタログ・候補・CLI利用方法

Copilotには(Codexの`codex:codex-rescue`のような)専用のClaude Codeプラグイン/スキルが存在しないため、以下の利用パターンはこのプラグイン独自のガイダンスである。

**モデルカタログ**(2026-07-16時点、Copilot CLI 1.0.70に対して`--model`をプローブして調査。Copilotのカタログはこのプラグインとは独立に変化するため、信用する前に再確認すること):

```
claude-sonnet-5        claude-opus-4.8        gpt-5.6-sol       gpt-5.4-mini
claude-sonnet-4.6       claude-opus-4.8-fast   gpt-5.6-terra     gpt-5-mini
claude-sonnet-4.5       claude-opus-4.7        gpt-5.6-luna      gemini-3.1-pro-preview
claude-haiku-4.5        claude-opus-4.6        gpt-5.5           gemini-3.5-flash
claude-fable-5          claude-opus-4.5        gpt-5.4           kimi-k2.7-code
                                                gpt-5.3-codex     mai-code-1-flash-picker
```

**モデルIDと表示名の違いに注意:** モデルの表示名([料金ページ](https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing)や対話的ピッカーに表示される名前)は、必ずしも`--model`のCLI識別子と一致しない — 例えば「MAI-Code-1-Flash」は`mai-code-1-flash-picker`として呼び出す。表示名で404になったからといって、そのモデルが使えないと即断しないこと。まずID表記の揺れ(例: 末尾に`-picker`が付く等)を試すこと。

**`worker`ロールの候補**(このプラグイン自身のPoCで実タスクに対して検証済み。詳細データは`poc-findings.md`参照)。`gemini-3.5-flash`と`gpt-5.4-mini`/`gpt-5-mini`はテストして通過したが、**ユーザーの明示的な指示により除外**している — 汎用・小型の「アシスタント」系モデルは、ベンチマーク速度によらず実開発用途には不向きと判断されたため。Copilotの`auto`モードも、3ラウンド連続で最もコストがかかる(コーディング風プロンプトに対して`gpt-5.3-codex`にルーティングする)割に、より安い選択肢に対する精度の優位がなかったため打ち切った。

| Model | 備考 |
|---|---|
| `gpt-5.6-luna` | デフォルトの選択 — テストした全ラウンド(3言語のフルスタックアプリ含む)を通じて最速・最安の検証済み候補。5回の再現性検証では強いが完璧ではない結果(190項目中189通過)だった — 生の出力を信用せず、検証と組み合わせて使うこと。 |
| `kimi-k2.7-code` | 専用のコードモデル。一貫して正しい結果を出すが、同等のタスクに対してLunaより内部のreasoningが冗長(ターン数・トークン数が多い)。 |
| `mai-code-1-flash-picker` | 動作確認済み、安価・高速 — ただし、フルスタックアプリのラウンドで`gpt-5.6-luna`/highと同じ優先度ソートの反転ミスを犯した。有望だが、まだ「一番良い」と証明されたわけではない。 |
| `claude-haiku-4.5` | Copilot経由でも利用可能だが、これを使うと外部エグゼキュータにディスパッチする目的(プロバイダの多様性)が失われる。Copilotのツールハーネスをどうしても使いたい場合のみ。 |

同梱のサンプル(`examples/orchestra.yaml`)は、CopilotのCopilotの`worker`ポリシーを`{ "model": "gpt-5.6-luna", "effort": "medium" }`に設定している — 検証済みの実開発向け候補の中で最速。`effort`はCodex側との一貫性のため明示的に設定してある。

**CLI利用 — 単発実行:**

```bash
copilot -p "$(cat TASK.md)" --model gpt-5.6-luna --effort medium \
  --allow-all-tools --add-dir "$WORKDIR" --output-format json > run.jsonl
```

`--add-dir`は、`--allow-all-paths`の代わりにファイルアクセスを作業ディレクトリに限定する。`--output-format json`はJSONLイベントを出力し、最終的な人間可読の回答とセッションIDの両方がそこに含まれる:

```bash
jq -r 'select(.type=="assistant.message") | .data.content' run.jsonl | tail -1   # 回答
jq -r 'select(.type=="result") | .sessionId' run.jsonl                          # セッションID
```

**CLI利用 — リトライラウンドをまたいでセッションを継続する**(Codexの`--resume-last`に相当。このプラグイン自身のPoCで実際に動作を確認済み — 再開したセッションは前ターンの指示を正しく想起した):

```bash
copilot --resume="$SESSION_ID" -p "$(cat FEEDBACK.md)" --model gpt-5.6-luna --effort medium \
  --allow-all-tools --add-dir "$WORKDIR" --output-format json > retry.jsonl
```

orchestrateパイプラインの`cli`ディスパッチは、リトライラウンドごとに**新しい**リレーエージェントを生成する設計のため(CLIの出力をinstructorのコンテキストから遠ざけるため)、セッションIDは単一のリレーエージェントの記憶にではなく、*Workflowスクリプト*自体を通じて引き継ぐ必要がある。最初のラウンドのリレーに、回答と一緒にセッションIDを返させ、スクリプト内でパースして、次のラウンドのリレープロンプトに渡し、`command`の代わりに`resume_command`を使わせる。

**リレーの返信形式(`STATUS:`判別子)**: `role_priority`のリアクティブ・フォールバック(§4)がCopilotの枯渇/認証/quota切れを検知できるように、リレーエージェントの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`ならその後に回答本文と`SESSION_ID: <id>`行を続け、`unavailable`ならその後に一行の短い理由(quota/credits/auth/rate-limitのいずれか)を続ける。生のCLIログ(jsonlの中身そのもの)はinstructorのコンテキストに絶対に渡さないこと — リレーが読んで判定した結果だけを返す:

```javascript
// Round 1: セッションがまだ無いので `command` を使う。
const first = await agent(
  'Run: copilot -p {promptfile} --model gpt-5.6-luna --effort medium ' +
  '--allow-all-tools --add-dir ' + workdir + ' --output-format json > /tmp/run.jsonl ; ' +
  'then inspect the result. Reply with STATUS: ok as the first line if it succeeded, or ' +
  'STATUS: unavailable if the CLI exited nonzero or the output shows a quota/credit/auth/' +
  'rate-limit error. If ok, on the following lines give the answer from ' +
  '`jq -r \'select(.type=="assistant.message") | .data.content\' /tmp/run.jsonl | tail -1`, ' +
  'then `SESSION_ID: ` followed by ' +
  '`jq -r \'select(.type=="result") | .sessionId\' /tmp/run.jsonl`. ' +
  'If unavailable, follow with one short line naming the reason. Never paste raw CLI logs.',
  { label: 'copilot-relay-r1', model: 'haiku', effort: 'low' },
)
const statusMatch = /^STATUS:\s*(ok|unavailable)/m.exec(first)
const sessionId = /SESSION_ID:\s*(\S+)/.exec(first)?.[1]

// Round 2+: 同じセッションを再開する。sessionIdを埋め込んだ `resume_command` を使う。
const retry = await agent(
  'Run: copilot --resume=' + sessionId + ' -p {promptfile} --model gpt-5.6-luna --effort medium ' +
  '--allow-all-tools --add-dir ' + workdir + ' --output-format json > /tmp/run2.jsonl ; ' +
  'then inspect the result and reply with STATUS: ok or STATUS: unavailable as the first line ' +
  '(same rule as round 1), followed by the final answer only — no logs.',
  { label: 'copilot-relay-r2', model: 'haiku', effort: 'low' },
)
```

## 3. 公式の単価表と、実際の請求に関する注意

GitHubはCopilotモデルの単価を[Models and pricing](https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing)で公開している(2026-07-16確認。価格は変動するため、重要な判断の前に再取得すること)。デフォルトコンテキストでの100万トークンあたりの単価:

| Model | カテゴリ | 入力 | キャッシュ入力 | 出力 |
|---|---|---|---|---|
| `gpt-5.3-codex` | Powerful | $1.75 | $0.175 | $14.00 |
| `gpt-5.6-luna` | Lightweight | $1.00 | $0.10 | $6.00 |
| `gpt-5.6-terra` | Versatile | $2.50 | $0.25 | $15.00 |
| `gpt-5.6-sol` | Powerful | $5.00 | $0.50 | $30.00 |
| `mai-code-1-flash` | Lightweight | $0.75 | $0.075 | $4.50 |
| `kimi-k2.7-code` | Versatile | $0.95 | $0.19 | $4.00 |

`mai-code-1-flash`は、このテーブルの中で最安のLightweightモデルになっている(Lunaより安い) — CLIでの利用可否を継続的に再確認する理由がもう1つ増えたことになる。

**注意 — この表は、Copilotのサブスクリプションが実際に1回あたり請求する額とは限らない。** このプラグイン自身のPoCでの全実行が、使用モデルによらず最終`result`イベントの`usage`に`"premiumRequests": 1`と記録していた — これは生のトークン単価ではなく、旧来の「フラットなプレミアムリクエスト」課金モデル(モデルごとの倍率をリクエスト枠に掛ける方式)と整合的である。GitHubの料金ページは、現行の課金世代についての倍率表を公開していないため、標準的なサブスクリプションの実際のコスト影響がこのドル建ての数字に連動しているのか、別の枠倍率の仕組みなのかはCLIだけからはわからない。上表は方向性としての序列(おおよそSol > Terra > Codex ≈ Kimi > Luna > MAI、コストの観点で)として扱い、大量利用を伴う判断の前には各自のCopilot利用状況/請求ダッシュボードで実際のコストを確認すること。

**対照的に、Codex CLIのコストは直接測定できる**: `codex exec --json`は`turn.completed`イベントで`usage.input_tokens`・`usage.cached_input_tokens`・`usage.output_tokens`を返し、これは上表と単純に掛け合わせられる(CodexもCopilotも同じGPT-5.6モデル群に課金される)。`poc-findings.md`のCodex側のコスト数値はすべてここから算出している — Codexのコスト数値は確度が高く、Copilotのものは(`premiumRequests: 1`の注意点により)方向性の参考程度と考えること。

**Claude(Sonnet/Haiku、`Agent`ツール経由)の単価**、[Anthropicの公式料金ページ](https://platform.claude.com/docs/en/docs/about-claude/pricing)より(2026-07-16確認)、100万トークンあたり:

| Model | 基本入力 | キャッシュ書き込み(5分) | キャッシュ読み込み | 出力 |
|---|---|---|---|---|
| `sonnet`(Sonnet 5、2026-08-31までの導入価格) | $2.00 | $2.50 | $0.20 | $10.00 |
| `haiku`(Haiku 4.5) | $1.00 | $1.25 | $0.10 | $5.00 |

Sonnet 5は2026-08-31以降、入力/出力ともに$3/$15に戻る — その日付以降はこの表を信用する前に再確認すること。Codexと異なり、`Agent`ツールの完了結果は入出力の内訳のない`subagent_tokens`という合算値しか返さないため、Claudeサブエージェント実行のコストは(Codexのような点推定ではなく)全て入力/全て出力の両極からの幅としてしか算出できない。

## 4. 優先度とリアクティブ・フォールバック(role_priority)

`role_priority`は、ロール(と`worker`についてはタスクアーキタイプ)ごとに「試す順序」を宣言するトップレベルの設定キーである。値は`external_executors`のキー(`codex`、`copilot`、...)または組み込みClaude実行を表すsentinel `claude`を並べた順序付きリストで、instructorはこれを左から順に試し、あるエグゼキュータが**unavailable**と判定された時点でのみ次の候補に降格する(=**リアクティブ・フォールバック**)。タスクの実行結果が単に誤っていた場合はフォールバックの対象ではない(後述)。

設定のスキーマは次の通り(§1のCodexティア表・§3のClaude/Copilot単価表と対応させて読むこと):

```yaml
# Ordered executor preference per role (and, for `worker`, per task archetype).
# The instructor tries candidates left-to-right and drops to the NEXT one on a
# *reactive fallback signal*: the current executor is unavailable — NOT the task
# failing. "Unavailable" = hit a rate-limit / usage-window cap (Claude, Codex),
# ran out of AI credits / premium requests (Copilot), is `enabled: false`, or
# its agent/CLI did not resolve in this environment. A task that RUNS but returns
# a wrong result is NOT a fallback signal — that stays on the SAME executor and
# goes through the normal verify/retry loop.
#
# Entries are an `external_executors` key (`codex`, `copilot`, ...) or the
# sentinel `claude` (the built-in `tiers.<role>` model). An executor named here
# must have a matching `model_policy.<role>` on its `external_executors` block,
# or it is skipped as misconfigured. Once an executor is found unavailable during
# a run it stays skipped for the rest of THAT run (sticky exhaustion) — do not
# re-probe it per task.
#
# Each role's value is a mapping of task-archetype -> ordered list; `default` is
# the catch-all. `worker` additionally recognizes `investigation` (read-only
# exploration / research fan-out, no verifier loop) as distinct from `default`
# (file-changing implementation that goes through the verify/retry loop). Other
# roles normally only need `default`.
#
# Omit `role_priority` entirely to keep the legacy behavior: the Claude
# `tiers.<role>` model, with external executors woven in ad hoc per their own
# `roles` list.
role_priority:
  worker:
    # Copilot gpt-5.6-luna/medium first — the fastest+cheapest validated worker
    # for investigation fan-out. NOTE: copilot ships `enabled: false` below, so
    # as-shipped this list effectively starts at codex; enable copilot to
    # actually prefer it (that skip IS the reactive fallback in action).
    investigation: [copilot, codex, claude]
    default: [claude, codex]
  hard_worker:
    default: [claude, codex]
  verifier:
    default: [claude]
  independent-verifier:
    default: [codex]
```

**プロアクティブな残量照会は同梱しない。** `cli`ディスパッチと同じ安全原則により、Copilotの残クレジットやCodex/Claudeの使用ウィンドウ残量を事前に問い合わせるコマンドはこのプラグインには存在せず、今後も追加しない。フォールバックの判定材料は、実際にディスパッチした時点で観測される「unavailableシグナル」だけである。

**unavailable と failed の区別(最重要)。** この2つを混同すると降格ロジックが壊れる:

- **unavailable**(→次の優先度候補に降格し、そのランの残り全体でsticky-skip): レートリミット/使用ウィンドウ上限、AIクレジット/premium requestの枯渇(Copilot)、`enabled: false`、agent/CLI名がこの環境で解決しない、`model_policy.<role>`が欠落している、のいずれか。
- **failed**(→同じエグゼキュータに留まり、通常のverify/retryループに入る): エグゼキュータは実際に走って何らかの出力を返したが、その結果が誤り/不完全だった場合。「動いたが間違えた」はフォールバック理由にならない — それはそのエグゼキュータの検証・再試行の問題であり、別のエグゼキュータに変えても解決するとは限らないため、まずは同じ系統でリトライさせる。

**エグゼキュータ別のunavailableシグナル(§1・§2・§3で説明した各エグゼキュータの実体と対応):**

- **Claude(`claude`sentinel、`agent()`経由)**: `agent()`が`null`を返す(リトライ後も終了しないエラー)場合をunavailableとみなし、次の候補に降格する。(`null`はユーザーによるagentスキップも意味しうるが、いずれも「先に進む」という結論は同じなので、このオーバーロードは許容する。)
- **Codex(`dispatch: agent`、`codex:codex-rescue`)**: `codex:codex-rescue`エージェントが`null`/エラーを返した場合、またはリレーされたテキストが使用上限/レートリミット/「resets at」/429系のメッセージを報告した場合。
- **Copilot(`dispatch: cli`)**: §2で示した安価なHaikuの**リレーエージェント**がCopilot CLIを実行し、その結果(終了コード・出力内容)を検査する。終了コードが非ゼロ、またはエラー/quota/credits/premium-request/認証系のリミット表示が見えた場合にunavailableと報告する。このリレーは生のCLIログをinstructorに渡してはならず、判定結果だけを短く返す。

**リレーの`STATUS:`判別子。** Copilotのようにワーカーがモデル自身の判断でエラーを報告するのではなく「リレーエージェントが後からCLIの結果を検査する」構成では、Workflowスクリプトが機械的に分岐できる目印が必要になる。そのためCopilotリレーの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`の場合はそれに続けて回答本文、さらに(セッション継続が必要なら)`SESSION_ID: <id>`行を返す。`unavailable`の場合はそれに続けて一行の短い理由(quota/credits/auth/rate-limitのいずれか)だけを返す。これは§2で示したセッション継続レシピ(`copilot-relay-r1`/`copilot-relay-r2`)と同一の形式であり、実際に§2のリレープロンプトは`STATUS:`行を必ず含めるよう既に更新済みなので、`role_priority`のフォールバックとセッション継続は同じ1本のリレープロンプトから両方読み取れる。

**sticky exhaustion(枯渇の記憶)。** 1回のオーケストレーション実行の中でunavailableと判定されたエグゼキュータは、`Set`などに記録して以降のすべてのタスクでスキップする。タスクごとに毎回同じエグゼキュータを再プローブしない — 一度枯渇したCopilotが同じランの途中で復活していないか確認する意味はない。

**`role_priority`は`roles`より優先してオーダリングを決める。** `role_priority.<role>`が存在する場合、そのロールの候補集合と順序についてはこちらが正となる。`external_executors.<key>.roles`は後方互換のために残されており、`role_priority`がそのロールに存在しない場合にのみ、従来の「各エグゼキュータの`roles`リストから逆引きして織り込む」方式が適用される。いずれの方式でも、実際のモデル/effort/dispatch設定は`model_policy`から取り、`enabled: false`はどちらの方式でもそのエグゼキュータを無条件に除外する。

**タスクアーキタイプの分類**は分解(decomposition)時点でのinstructorの判断であり、実行時に動的に切り替えるものではない: `investigation`は読み取り専用の調査・研究・コードベース探索的なfan-outで、ファイル変更もverifierループも伴わないもの。`default`はそれ以外すべて(verify/retryループを通るファイル変更を伴う実装)。他のロール(`hard_worker`・`verifier`・`independent-verifier`)は通常`default`だけで十分。

**deep-mergeの注意点は他の設定キーと同じ**: `role_priority`のリストはリストとして扱われるため、より詳細なレイヤ(project設定など)で上書きされると要素単位でマージされず丸ごと置き換わる。部分的に変えたいだけでも、そのロール/アーキタイプの配列は全体を書き直す必要がある。

**Workflowスクリプトでの実装イメージ。** 各候補を実際のディスパッチ手段(Claudeの`agent()`、Codexの`codex:codex-rescue`、Copilotのリレーエージェント)にマップし、戻り値を`{ status: 'ok'|'unavailable', answer?, reason?, sessionId? }`という共通の形に正規化してから、フォールバックのループに渡す:

```javascript
// candidates: 解決済みの、このロール(+アーキタイプ)向け順序リスト
// 例: ['copilot', 'codex', 'claude']
const exhausted = new Set() // このランの間だけ有効なsticky exhaustion

async function runOn(exec, taskPrompt, workdir) {
  if (exec === 'claude') {
    const answer = await agent(taskPrompt, { label: 'worker', model: tierModelFor('worker') })
    if (answer === null) {
      return { status: 'unavailable', reason: 'agent() returned null (terminal error or skip)' }
    }
    return { status: 'ok', answer }
  }

  if (exec === 'codex') {
    const answer = await agent(taskPrompt, { label: 'codex-rescue', agent: 'codex:codex-rescue' })
    if (answer === null) {
      return { status: 'unavailable', reason: 'codex:codex-rescue returned null' }
    }
    if (/usage.?limit|rate.?limit|resets at|\b429\b/i.test(answer)) {
      return { status: 'unavailable', reason: 'codex usage-limit signal in relayed text' }
    }
    return { status: 'ok', answer }
  }

  if (exec === 'copilot') {
    // §2の copilot-relay レシピと同じ STATUS: 判別子付きリレー。
    const reply = await agent(
      'Run: copilot -p {promptfile} --model gpt-5.6-luna --effort medium ' +
      '--allow-all-tools --add-dir ' + workdir + ' --output-format json > /tmp/run.jsonl ; ' +
      'then inspect the result and reply with STATUS: ok or STATUS: unavailable as the ' +
      'first line (unavailable = nonzero exit or a quota/credit/auth/rate-limit signal), ' +
      'followed by the answer and SESSION_ID: line on ok, or a one-line reason on unavailable.',
      { label: 'copilot-relay', model: 'haiku', effort: 'low' },
    )
    const status = /^STATUS:\s*(ok|unavailable)/m.exec(reply)?.[1]
    if (status !== 'ok') {
      const reason = reply.split('\n').slice(1).join(' ').trim() || 'copilot relay reported unavailable'
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

`runOn`が返す`status`が`'unavailable'`のときだけ`exhausted`に加えて次の候補へ進み、それ以外(`'ok'`)は即座にそのエグゼキュータの結果を採用して呼び出し元(verify/retryループ)に返す。タスクが「動いたが結果が誤り」だった場合の再試行は、この`dispatchWithFallback`の外側 — 同じ`exec`に対する通常のverifier/retryロジック側の責務であり、`role_priority`の降格ロジックには一切関与しない。
