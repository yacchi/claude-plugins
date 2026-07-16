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

orchestrateパイプラインの`cli`ディスパッチは、リトライラウンドごとに**新しい**リレーエージェントを生成する設計のため(CLIの出力をinstructorのコンテキストから遠ざけるため)、セッションIDは単一のリレーエージェントの記憶にではなく、*Workflowスクリプト*自体を通じて引き継ぐ必要がある。最初のラウンドのリレーに、回答と一緒にセッションIDを返させ(例: 2行の返信 — 回答、次に`SESSION_ID: <id>`)、スクリプト内でパースして、次のラウンドのリレープロンプトに渡し、`command`の代わりに`resume_command`を使わせる:

```javascript
// Round 1: セッションがまだ無いので `command` を使う。
const first = await agent(
  'Run: copilot -p {promptfile} --model gpt-5.6-luna --effort medium ' +
  '--allow-all-tools --add-dir ' + workdir + ' --output-format json > /tmp/run.jsonl ; ' +
  'then reply with exactly two lines: the answer from ' +
  '`jq -r \'select(.type=="assistant.message") | .data.content\' /tmp/run.jsonl | tail -1`, ' +
  'then `SESSION_ID: ` followed by ' +
  '`jq -r \'select(.type=="result") | .sessionId\' /tmp/run.jsonl`.',
  { label: 'copilot-relay-r1', model: 'haiku', effort: 'low' },
)
const sessionId = /SESSION_ID:\s*(\S+)/.exec(first)?.[1]

// Round 2+: 同じセッションを再開する。sessionIdを埋め込んだ `resume_command` を使う。
const retry = await agent(
  'Run: copilot --resume=' + sessionId + ' -p {promptfile} --model gpt-5.6-luna --effort medium ' +
  '--allow-all-tools --add-dir ' + workdir + ' --output-format json > /tmp/run2.jsonl ; ' +
  'reply with only the final answer, no logs.',
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
