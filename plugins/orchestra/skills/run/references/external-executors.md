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

**現在のクラス/ロール割り当て**(section 3のClaude側ティア表と対応):
- **`standard`** → **Luna/medium**。最安・最速ティア。effortは`high`ではなく`medium` — このプラグイン自身のPoCで、`high`effortが実践的なタスクで実バグを出し、`medium`ではそのバグが再現しなかった(しかも安く速かった)ことが判明したため(`poc-findings.md` §8)。effortは高ければ良いというものではない。
- **`review`**(`independent-review`向け)→ Sol/low。standardクラス自体のコスト・速度を悪化させずに、別系統のモデルでレビューを追加できる。
- **`deep`** → Sol/xhigh。本当に設計の余地がある/難しい問題のために温存する。
- **Terra**はデフォルトポリシーに含めていない — このプラグイン自身のPoCでは、これまで試したタスクにおいてSol/lowと競合する(時にはより安い)結果が出ており、「Terraは(Lunaに)支配される」という一括りの扱いとは矛盾するが、どちらの方向でも精度の差別化はできなかった(`poc-findings.md` §2-3)。Sol/lowのコストが気になる場合、`independent-review`としてTerraを再検討する価値はある。

**長文コンテキストに関する注意:** Lunaは長文コンテキストの想起が測定可能なレベルで弱い。名目上light/standardクラスのタスクであっても、大規模リポジトリの深い探索が必要な場合(単なる小さな自己完結の変更ではない場合)は、そのタスクに限り`deep`(Sol/xhigh)にエスカレーションすること — サンプル設定の`long_context_escalation`フィールドがこのトリガーを明示的に文書化しているので、毎回ゼロから判断する必要はない。

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

**light/standardバンドの候補**(このプラグイン自身のPoCで実タスクに対して検証済み。詳細データは`poc-findings.md`参照)。`gemini-3.5-flash`と`gpt-5.4-mini`/`gpt-5-mini`はテストして通過したが、**ユーザーの明示的な指示により除外**している — 汎用・小型の「アシスタント」系モデルは、ベンチマーク速度によらず実開発用途には不向きと判断されたため。Copilotの`auto`モードも、3ラウンド連続で最もコストがかかる(コーディング風プロンプトに対して`gpt-5.3-codex`にルーティングする)割に、より安い選択肢に対する精度の優位がなかったため打ち切った。

| Model | 備考 |
|---|---|
| `gpt-5.6-luna` | デフォルトの選択 — テストした全ラウンド(3言語のフルスタックアプリ含む)を通じて最速・最安の検証済み候補。5回の再現性検証では強いが完璧ではない結果(190項目中189通過)だった — 生の出力を信用せず、検証と組み合わせて使うこと。 |
| `kimi-k2.7-code` | 専用のコードモデル。一貫して正しい結果を出すが、同等のタスクに対してLunaより内部のreasoningが冗長(ターン数・トークン数が多い)。 |
| `mai-code-1-flash-picker` | 動作確認済み、安価・高速 — ただし、フルスタックアプリのラウンドで`gpt-5.6-luna`/highと同じ優先度ソートの反転ミスを犯した。有望だが、まだ「一番良い」と証明されたわけではない。 |
| `claude-haiku-4.5` | Copilot経由でも利用可能だが、これを使うと外部エグゼキュータにディスパッチする目的(プロバイダの多様性)が失われる。Copilotのツールハーネスをどうしても使いたい場合のみ。 |

同梱のサンプル(`examples/orchestra.yaml`)は、Copilotの`light`/`standard`クラスの`class_policy`を`{ "model": "gpt-5.6-luna", "effort": "medium" }`に設定している — 検証済みの実開発向け候補の中で最速。`effort`はCodex側との一貫性のため明示的に設定してある。

**(v0.11.0以降の位置づけ)** 以下の`agent-exec copilot`/`agent-exec run copilot`の生コマンドは、Copilotへのディスパッチが実際には何をしているかを示す下層のメカニクスであり、手動テストやワンオフ実行には今もそのまま使える。だが**orchestrateパイプライン(`run` SKILL.md §5)からは、これらを直接組み立てるのではなく`agent-exec dispatch --class light`(または`standard`)を使うこと** — `dispatch`は`route`によるエグゼキュータ選択(Copilotが未認可/未準備なら自動的にClaudeへフォールバック)とtelemetryの自動記録を内包した1回の呼び出しであり、下記の`--model`/`--effort`/`--add-dir`等を毎回手で組み立てる必要がない。

**CLI利用 — 単発実行**(推奨は`agent-exec`ラッパー経由 — 呼び出し側は`agent-exec copilot ...`と書くだけでよい。理由・セットアップ手順は後述の「必須: Copilotディスパッチの認可設定」を参照):

```bash
agent-exec copilot -p "$(cat TASK.md)" --model gpt-5.6-luna --effort medium \
  --add-dir "$WORKDIR" --output-format json --disable-builtin-mcps > run.jsonl
```

**`COPILOT_ALLOW_ALL`/`--allow-all-tools`は不要(実測: M1)。** Copilot CLI 1.0.74で再測定した結果、`copilot -p`は非対話モードで、上記コマンドのように`--add-dir`と`--output-format json`だけを渡した状態でも、ファイル書き込み(`apply_patch`)・シェル実行(`bash`)・ネットワークアクセス(`bash`経由の`curl`が`example.com`へHTTP 200)を**確認なしに自律的に実行する**。旧版のこのファイルにあった「非対話実行には`COPILOT_ALLOW_ALL`/`--allow-all-tools`が必須」という記述はCLI 1.0.71での検証によるもので、1.0.74では成立しない — allow-allフラグ/環境変数はヘッドレスディスパッチに不要であり、`agent-exec`はもう注入しない。

`--disable-builtin-mcps`はハードニング目的のデフォルトフラグとして追加した。ビルトインMCPツール(`github-mcp-server`・`customize-cloud-agent`)をツール面から外し、トークン・レイテンシも削減する。

**封じ込め(containment)に関する注意(実測: M2) — 重要。** `copilot -p`は自身のツール許可フラグ(`--allow-tool`/`--deny-tool`/`--excluded-tools`)では確実に閉じ込められない。実測では、`--excluded-tools='bash'`でbashツールを除外しても、エージェントは**`task`ツール経由で同じシェルコマンドを実行するよう迂回した**。つまりこれらのフラグは個別ツール名単位の除外であり、エージェントがそれを回避する経路(サブタスク委譲など)を持っている限り、セキュリティ境界にはならない。これらは多層防御(defense-in-depth)の一枚であって境界(boundary)ではない — 真の最小権限を実現するには、コンテナ・`sandbox-exec`・制限ユーザー・ネットワーク egress 制御・使い捨てworktreeのような**OS外部のサンドボックス**が必要であり、これは本プラグインでは未実装(将来課題)である。

`--add-dir`は、`--allow-all-paths`の代わりにファイルアクセスを作業ディレクトリに限定する(ただしこれもプロセス内の制御であり、M2の外部サンドボックスに置き換わるものではない)。`--output-format json`はJSONLイベントを出力し、最終的な人間可読の回答とセッションIDの両方がそこに含まれる。

**このJSONLを`jq`で手動パースする必要はもうない。** `agent-exec run copilot --capture ...`(§5)がこのパース(回答の抽出・セッションIDの抽出・quota/credits/auth/rate-limitシグナルからのok/unavailable判定)を1コマンドに集約し、`{ status, answer, session_id, reason, exit_code }`という正規化されたJSONオブジェクトをstdoutにそのまま出す。リレーエージェントはこのJSONを読むだけでよく、jqでイベント種別ごとに`select`する処理はリレー側にはもう残っていない。

**CLI利用 — リトライラウンドをまたいでセッションを継続する**(Codexの`--resume-last`に相当。このプラグイン自身のPoCで実際に動作を確認済み — 再開したセッションは前ターンの指示を正しく想起した):

```bash
agent-exec run copilot --capture --resume "$SESSION_ID" --model gpt-5.6-luna --effort medium \
  --workdir "$WORKDIR" --prompt-file FEEDBACK.md
```

orchestrateパイプラインの`cli`ディスパッチは、リトライラウンドごとに**新しい**リレーエージェントを生成する設計のため(CLIの出力をinstructorのコンテキストから遠ざけるため)、セッションIDは単一のリレーエージェントの記憶にではなく、*Workflowスクリプト*自体を通じて引き継ぐ必要がある。最初のラウンドのリレーに、回答と一緒にセッションIDを返させ、スクリプト内でパースして、次のラウンドのリレープロンプトに渡し、`command`の代わりに`resume_command`を使わせる。

**リレーの返信形式(`STATUS:`判別子)**: `priority`のリアクティブ・フォールバック(§4)がCopilotの枯渇/認証/quota切れを検知できるように、リレーエージェントの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`ならその後に回答本文と`SESSION_ID: <id>`行を続け、`unavailable`ならその後に一行の短い理由(quota/credits/auth/rate-limitのいずれか)を続ける。生のCLIログ(jsonlの中身そのもの)はinstructorのコンテキストに絶対に渡さないこと — リレーが読んで判定した結果だけを返す。

**`agent-exec run copilot --capture`が正規化済みJSONを直接返すため、リレーはもうJSONLをパースしない。** リレーが実行するコマンドは`{ status, answer, session_id, reason, exit_code }`という1つのJSONオブジェクトをstdoutに出すので、リレーの仕事は「そのJSONを読んで`STATUS:`行に転記するだけ」になる — `jq`によるイベント種別ごとの`select`は不要:

```javascript
// Round 1: セッションがまだ無いので --resume を付けない。(agent-exec run copilot --capture 経由)
const first = await agent(
  'Run: agent-exec run copilot --capture --model gpt-5.6-luna --effort medium ' +
  '--workdir ' + workdir + ' --prompt-file {promptfile} ; ' +
  'this prints one normalized JSON object { status, answer, session_id, reason, exit_code } ' +
  'to stdout — parse it directly (no jq, no manual JSONL inspection needed). Reply with ' +
  'STATUS: ok as the first line if status=="ok", or STATUS: unavailable if status=="unavailable". ' +
  'If ok, on the following lines give the `answer` value, then `SESSION_ID: ` followed by the ' +
  '`session_id` value. If unavailable, follow with one short line naming the `reason` value. ' +
  'Never paste raw CLI logs.',
  { label: 'copilot-relay-r1', model: 'haiku', effort: 'low' },
)
const statusMatch = /^STATUS:\s*(ok|unavailable)/m.exec(first)
const sessionId = /SESSION_ID:\s*(\S+)/.exec(first)?.[1]

// Round 2+: 同じセッションを再開する。sessionIdを --resume に渡す。
const retry = await agent(
  'Run: agent-exec run copilot --capture --resume ' + sessionId + ' --model gpt-5.6-luna --effort medium ' +
  '--workdir ' + workdir + ' --prompt-file {promptfile} ; ' +
  'this prints the same normalized JSON object — parse it and reply with STATUS: ok or ' +
  'STATUS: unavailable as the first line (same rule as round 1), followed by the `answer` ' +
  'value only — no logs.',
  { label: 'copilot-relay-r2', model: 'haiku', effort: 'low' },
)
```

### 必須: Copilotディスパッチの認可設定(推奨: agent-exec / 代替: 手動env+パーミッション)

`dispatch: cli`のCopilotは、HaikuリレーエージェントがBashで`copilot ...`(または`agent-exec copilot ...`)を実行することで動く。非対話のリレーでこれが安定して通るには、Claude Code側の許可設定が必要になる。

**推奨: `agent-exec`ラッパーをインストールする。** `agent-exec`は、`--add-dir`/`--output-format json`/`--disable-builtin-mcps`を付けて実際のCLIをexecする薄いシェル/Pythonラッパーである。M1により`COPILOT_ALLOW_ALL`のようなツール自動許可の注入は不要になったため、ラッパーはもう何も注入しない — ユーザーが用意する設定は次の1行だけになる:

```jsonc
// settings.json (Copilotを有効化したスコープと同じファイル)
{
  "permissions": { "allow": ["Bash(agent-exec:*)"] }
}
```

`env`ブロックに何かを書く必要はない。ラッパーは`--allow-all-tools`のようなパーミッションバイパス系フラグも一切使わないため、Claude CodeのBash安全クラシファイアが検査する文字列にそうしたフラグは現れない。

セットアップは`agent-exec install`(インタラクティブインストーラ)で、PATH上のディレクトリ(既定`~/.local/bin/agent-exec`)に1行のPOSIX shシムを設置する。このシムはプラグインの**マーケットプレイス・クローンパス**(`~/.claude/plugins/marketplaces/<マーケットプレイス名>/plugins/orchestra/tools/agent-exec`)を絶対パスで指す — このパスは`git pull`で中身が更新されるだけで場所自体は変わらず安定しているため、`/plugin update`するだけでラッパーの中身も追従し、シムを再生成する必要がない。**変わってはいけないのは、バージョンでパスが変わるプラグインの`cache`側installPath**(`~/.claude/plugins/cache/<マーケットプレイス名>/orchestra/<version>/...`)の方であり、シムはそちらを指してはならない — マーケットプレイス・クローン側のパスは安定した正当な解決先である。

セットアップ後のコマンドテンプレートは次の通り(§2の例もこの形に統一している):

```bash
agent-exec copilot -p {promptfile} --model {model} --effort {effort} \
  --add-dir {workdir} --output-format json --disable-builtin-mcps
# resume:
agent-exec copilot --resume={session_id} -p {promptfile} --model {model} --effort {effort} \
  --add-dir {workdir} --output-format json --disable-builtin-mcps
```

**代替: `agent-exec`を使わない手動セットアップ。** `agent-exec`をインストールしない場合でも、必要な設定は1つだけである(以前のように環境変数とのセットは不要 — M1によりallow-all自体が不要になったため):

**`permissions.allow`に`Bash(copilot:*)`。** Claude Codeは各Bashツール呼び出しのトップレベルコマンドを`permissions.allow`と照合するが、リレーはサブエージェント／非対話で走るため、未許可のコマンドは**承認プロンプトを出せず即座に拒否**される(ユーザーが対話的に許可することもできない)。

```jsonc
// settings.json (Copilotを有効化したスコープと同じファイル)
{
  "permissions": { "allow": ["Bash(copilot:*)"] }
}
```

**検証済み**(2026-07-24、Copilot CLI 1.0.74、M1/M2): `--allow-all-tools`も`COPILOT_ALLOW_ALL`も付けない`copilot -p ...`が、`Bash(copilot:*)`/`Bash(agent-exec:*)`を許可済みのサブエージェントBashから正常に実行され、ファイル書き込み・シェル実行・ネットワークアクセスを確認なしに完了した(`apply_patch`/`bash`ツール経由)。旧版のこのファイルにあった「`COPILOT_ALLOW_ALL=true`環境変数が必須」という2026-07-17付・CLI 1.0.71での検証結果は、現行の1.0.74では再現しない(もはや不要)。

補足:

- 追加先は、Copilotを有効化したスコープに合わせる: プロジェクトなら`.claude/settings.json`、ユーザー個人なら`~/.claude/settings.json`。`/permissions`または`update-config`スキルで追加する。
- パーミッションのマッチはトップレベルコマンドの前方一致で、**子プロセスは対象外**。そのため`--add-dir`の付与を許可ルール側で強制することはできない(ルールは`copilot`/`agent-exec`で始まる任意の呼び出しを許可する)。`--add-dir <workdir>`によるファイルアクセス限定は`command`テンプレート(§2)側で担保する。
- **`--deny-tool`はオプションの参考程度の知見であって、境界(boundary)ではない(M2)。** さらに絞りたい向きに`--deny-tool 'shell(git push:*)'`のような否定ルールを`command`テンプレートに足す運用は可能だが、M2で実測した通りCopilotのツール除外系フラグ(`--allow-tool`/`--deny-tool`/`--excluded-tools`)はエージェントによる迂回(別ツール経由の再実行)を防げない。これは安全側の追加ヒントであって、これに依存した権限設計をしないこと。真の封じ込めは外部OSサンドボックス側の課題(§2冒頭のM2の注記、future work)である。

Codexの`dispatch: agent`(codex:codex-rescueサブエージェント経由)はこれらの設定を必要としない — 生のBashコマンドではなくサブエージェント呼び出しだからである。

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

## 4. 優先度とリアクティブ・フォールバック(priority)

`priority`は、クラス/ロール(と`light`についてはタスクアーキタイプ)ごとに「試す順序」を宣言するトップレベルの設定キーである。値は`external_executors`のキー(`codex`、`copilot`、...)または組み込みClaude実行を表すsentinel `claude`を並べた順序付きリストで、あるエグゼキュータが**unavailable**と判定された時点でのみ次の候補に降格する(=**リアクティブ・フォールバック**)。タスクの実行結果が単に誤っていた場合はフォールバックの対象ではない(後述)。

**この左から右への降格ウォークは、instructorが手で行うものではなく、`agent-exec route --class <cls> [--archetype A] [--exhausted a,b]`(と、それを内包する`agent-exec dispatch`)がコード側で実行する。** `route`は4層configをマージした上で各候補を`enabled`・binary/agent解決可否・`doctor`の`ready.<x>.ok`・`class_policy`該当有無・呼び出し側が渡した`--exhausted`でゲートし、生き残った先頭候補を返す。instructor側に残る唯一の手作業は、あるタスクで`dispatch`が`unavailable`を報告したエグゼキュータを、同じラン内の以降すべての`route`/`dispatch`呼び出しに`--exhausted`として持ち越すこと(sticky exhaustion)だけである — `run` SKILL.md §5の`dispatchClass()`はこれをモジュールレベルの`Set`で自動的に行う。以下のスキーマとJS実装イメージは、`route`が内部で何をしているかを理解するためのものであり、現在ではこれを手で書き写す必要はない。

設定のスキーマは次の通り(§1のCodexティア表・§3のClaude/Copilot単価表と対応させて読むこと):

```yaml
# Ordered executor preference per class/role (and, for the implementation classes,
# per task archetype). The instructor tries candidates left-to-right and drops to
# the NEXT one on a *reactive fallback signal*: the current executor is unavailable
# — NOT the task failing. "Unavailable" = hit a rate-limit / usage-window cap
# (Claude, Codex), ran out of AI credits / premium requests (Copilot), is
# `enabled: false`, or its agent/CLI did not resolve in this environment. A task
# that RUNS but returns a wrong result is NOT a fallback signal — that stays on the
# SAME executor and goes through the normal review/retry loop.
#
# Entries are an `external_executors` key (`codex`, `copilot`, ...) or the sentinel
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
    # Copilot gpt-5.6-luna/medium first — the fastest+cheapest validated executor
    # for the light band (and for investigation fan-out). copilot ships
    # `enabled: true` below (as of v0.11.0), so this list actually prefers it
    # whenever `agent-exec route` finds it ready; a genuine unavailable signal
    # is what makes it drop to claude, not a config default.
    investigation: [copilot, claude]
    default: [copilot, claude]
  standard:
    # luna clears sonnet-class work too, so it also leads the standard band.
    default: [copilot, claude, codex]
  deep:
    default: [claude, codex]
  review:
    default: [claude]
  independent-review:
    default: [codex]
```

**プロアクティブな残量照会は同梱しない。** `cli`ディスパッチと同じ安全原則により、Copilotの残クレジットやCodex/Claudeの使用ウィンドウ残量を事前に問い合わせるコマンドはこのプラグインには存在せず、今後も追加しない。フォールバックの判定材料は、実際にディスパッチした時点で観測される「unavailableシグナル」だけである。

**認可(authorization)の事前チェックは残量照会とは別物で、こちらは推奨する。** 上の「残量照会を同梱しない」は、リモートで変動する*残量*(クレジット/使用ウィンドウ)を事前問い合わせしない、という意味であって*認可*には当てはまらない。`dispatch: cli`のエグゼキュータ(Copilot)は、セッションに該当するBash許可ルール(`agent-exec`利用時は`Bash(agent-exec:*)`、手動セットアップ時は`Bash(copilot:*)`)が欠けていれば**この実行では絶対に成功しない**(M1により、これ以外に必要な環境変数はない — `COPILOT_ALLOW_ALL`は`agent-exec`利用・手動セットアップのいずれでも不要)。許可ルールが欠けている場合、残量と違って一過性ではなく、リレーを起動してもパーミッションに即拒否される。

**この事前チェックは`agent-exec doctor`を1回呼ぶだけで済む。** シム(`agent-exec`本体)がPATH上にインストールされているか・どのターゲット(マーケットプレイス・クローンか、壊れやすいcacheパスか)を指しているか、`uv`の有無、4層configのマージ結果、各`external_executors.<name>`の`enabled`/`available`、`permissions.allow`中の`Bash(agent-exec:*)`ルールの有無(ベストエフォート — `~/.claude/settings.json`と`./.claude/settings.json(.local)`のみを走査し、enterprise policyやCLIの`--allowedTools`は見えない)、そして各cliエグゼキュータの総合可否`ready.<name>.ok`(と未充足の`missing[]`)を、`agent-exec doctor`(人間向け`--text`、instructor向け`--json`)の1回の呼び出しでまとめて返す。instructorは`priority`の候補に`dispatch: cli`エグゼキュータを含める前にこれを実行し、`ready.<name>.ok`が`false`ならそのランでは最初からunavailable扱いにして次候補へ降格させる — 個別に`settings.json`を読んだり`which`を叩いたりする必要はない。これは無駄なリレー起動を省く最適化であり、仮にチェックを省いても下記のリアクティブなフォールバック(リレーの`STATUS: unavailable`)が同じ結論に達するバックストップとして残る。`doctor`はシムが未インストールでも(ブートストラップパス経由で直接)呼び出せるため、シムのインストール前診断にも使える。**(v0.11.0以降の補足)** この事前チェック自体は今や`agent-exec route`/`dispatch`が内部で毎回自動的に行うため、instructorが`priority`候補を組み立てる前に別途`doctor`を呼んでフィルタする、という手順はもう不要になった — `route`/`dispatch`を呼ぶだけで、この段落が説明する`ready.<name>.ok`ゲートは常に適用済みの状態になる。`doctor`単体は、セットアップ診断(シムの設置状況の確認)や`config.values.telemetry.enabled`の確認など、他の目的でなお有用。

**unavailable と failed の区別(最重要)。** この2つを混同すると降格ロジックが壊れる:

- **unavailable**(→次の優先度候補に降格し、そのランの残り全体でsticky-skip): レートリミット/使用ウィンドウ上限、AIクレジット/premium requestの枯渇(Copilot)、`enabled: false`、agent/CLI名がこの環境で解決しない、`class_policy.<class>`が欠落している、のいずれか。
- **failed**(→同じエグゼキュータに留まり、通常のverify/retryループに入る): エグゼキュータは実際に走って何らかの出力を返したが、その結果が誤り/不完全だった場合。「動いたが間違えた」はフォールバック理由にならない — それはそのエグゼキュータの検証・再試行の問題であり、別のエグゼキュータに変えても解決するとは限らないため、まずは同じ系統でリトライさせる。

**エグゼキュータ別のunavailableシグナル(§1・§2・§3で説明した各エグゼキュータの実体と対応):**

- **Claude(`claude`sentinel、`agent()`経由)**: `agent()`が`null`を返す(リトライ後も終了しないエラー)場合をunavailableとみなし、次の候補に降格する。(`null`はユーザーによるagentスキップも意味しうるが、いずれも「先に進む」という結論は同じなので、このオーバーロードは許容する。)
- **Codex(`dispatch: agent`、`codex:codex-rescue`)**: `codex:codex-rescue`エージェントが`null`/エラーを返した場合、またはリレーされたテキストが使用上限/レートリミット/「resets at」/429系のメッセージを報告した場合。
- **Copilot(`dispatch: cli`)**: §2で示した安価なHaikuの**リレーエージェント**がCopilot CLIを実行し、その結果(終了コード・出力内容)を検査する。終了コードが非ゼロ、またはエラー/quota/credits/premium-request/認証系のリミット表示が見えた場合にunavailableと報告する。このリレーは生のCLIログをinstructorに渡してはならず、判定結果だけを短く返す。

  **ただしリミット表示の走査対象は「裏付けのある出力」に限る(default-deny)。** 走査するのはstderr全文・JSONとしてパースできなかったstdout行(平文のquota/認証エラーはまさにここに出る)・そして`session.error`のようなエラー種別イベントだけであり、`assistant.message`に加えて`tool.execution_complete`・`system.message`・reasoning・リクエストのエコーといった**ワーカー自身のテキストを運ぶイベントは走査しない**。これは実害から導かれた規則である: ワーカーが書いていたREADMEに含まれるHTTP 429のバックオフ表や、「Implement JWT auth ...」というタスクプロンプトのエコーが、`exit_code: 0`で正常に回答を返したランを`unavailable`(reason: rate-limit / auth)に転ばせていた。cooldownはこの判定を1時間の実可用性喪失へ増幅するため、`record_unavailable_cooldown`側にも「終了コード0かつ非空の回答があるならcooldownを書かない」という二重の歯止めを置いてある。

**リレーの`STATUS:`判別子。** Copilotのようにワーカーがモデル自身の判断でエラーを報告するのではなく「リレーエージェントが後からCLIの結果を検査する」構成では、Workflowスクリプトが機械的に分岐できる目印が必要になる。そのためCopilotリレーの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`の場合はそれに続けて回答本文、さらに(セッション継続が必要なら)`SESSION_ID: <id>`行を返す。`unavailable`の場合はそれに続けて一行の短い理由(quota/credits/auth/rate-limitのいずれか)だけを返す。これは§2で示したセッション継続レシピ(`copilot-relay-r1`/`copilot-relay-r2`)と同一の形式であり、実際に§2のリレープロンプトは`STATUS:`行を必ず含めるよう既に更新済みなので、`priority`のフォールバックとセッション継続は同じ1本のリレープロンプトから両方読み取れる。

**sticky exhaustion(枯渇の記憶)。** 1回のオーケストレーション実行の中でunavailableと判定されたエグゼキュータは、`Set`などに記録して以降のすべてのタスクでスキップする。タスクごとに毎回同じエグゼキュータを再プローブしない — 一度枯渇したCopilotが同じランの途中で復活していないか確認する意味はない。

**クロスランcooldown(枯渇の時間減衰)。** 同じunavailableの再発を次のランで毎回発見し直さないため、`agent-exec run --capture`と`agent-exec dispatch`は、パースした結果が`status=unavailable`になった瞬間に、LLMを介さず`~/.claude/orchestra/executor-state.json`(`{executor: {reason, until}}`)へ自動保存する。理由ごとのcooldown秒数は設定(`rate-limit: 900`、`quota: 3600`、`credits: 3600`、`auth: 0`、`nonzero-exit: 0`)から取り、`agent-exec route`/`dispatch`はファイルを読み、期限内のエントリを既存の`--exhausted`と同じ候補ゲートへ渡す(スキップ理由は`cooldown:<reason>`)。これは2026-07-28 09:23〜09:32(UTC)に、Copilot側の本物の枯渇(`session.error` / `errorType: quota` / HTTP 402)を9分間で12回連続して発見し直したための最適化である。ただしcooldownはクラスをunroutableにしない: 全候補がcooldown中ならcooldownを無視してウォークをやり直し、結果に`cooldown_bypassed: true`を付ける。認証とnonzero-exitはリソース枯渇ではなく一過性なので`0`(cooldownなし)であり、missing/corruptな状態ファイル、書き込み不能なパス、保存失敗もすべてfail-openで従来どおりルーティングする。`--no-cooldown`(route/dispatch)、`agent-exec cooldown`(確認)、`agent-exec cooldown clear [executor]`(リセット)、`cooldown.enabled: false`がescape hatchである。これはラン内の`Set`によるsticky exhaustionを置き換えず、その下に加わるクロスラン層である。なお、これは既存の**プロアクティブな残量照会を同梱しない**方針を変更しない — cooldownはディスパッチ時に実際に観測したシグナルだけを反応的に記憶し、リモートの残量APIを問い合わせない。

**`priority`は`classes`より優先してオーダリングを決める。** `priority.<class>`が存在する場合、そのクラス/ロールの候補集合と順序についてはこちらが正となる。`external_executors.<key>.classes`は後方互換のために残されており、`priority`がそのクラス/ロールに存在しない場合にのみ、従来の「各エグゼキュータの`classes`リストから逆引きして織り込む」方式が適用される。いずれの方式でも、実際のモデル/effort/dispatch設定は`class_policy`から取り、`enabled: false`はどちらの方式でもそのエグゼキュータを無条件に除外する。

**タスクアーキタイプの分類**は分解(decomposition)時点でのinstructorの判断であり、実行時に動的に切り替えるものではない: `investigation`は読み取り専用の調査・研究・コードベース探索的なfan-outで、ファイル変更もreviewループも伴わないもの。`default`はそれ以外すべて(review/retryループを通るファイル変更を伴う実装)。他のクラス/ロール(`deep`・`review`・`independent-review`)は通常`default`だけで十分。

**deep-mergeの注意点は他の設定キーと同じ**: `priority`のリストはリストとして扱われるため、より詳細なレイヤ(project設定など)で上書きされると要素単位でマージされず丸ごと置き換わる。部分的に変えたいだけでも、そのクラス/ロール/アーキタイプの配列は全体を書き直す必要がある。

**Workflowスクリプトでの実装イメージ(参考 — v0.11.0以降は不要な手書きロジック)。** 以下は`agent-exec route`/`dispatch`が存在する前に、instructorがこのフォールバック・ウォークを自前で実装するとしたら何をする必要があったかを示す参考コードであり、`route`が内部で行っているマッピング・正規化・降格判定の意味論を理解するために残している。**実際にWorkflowスクリプトを書くときは、これを書き写すのではなく`run` SKILL.md §5の`dispatchClass()`(1回の`agent-exec dispatch --class <cls> ... --capture`呼び出しを1つの安価なリレーエージェントに任せるだけの実装)を使うこと。** 各候補を実際のディスパッチ手段(Claudeの`agent()`、Codexの`codex:codex-rescue`、Copilotのリレーエージェント)にマップし、戻り値を`{ status: 'ok'|'unavailable', answer?, reason?, sessionId? }`という共通の形に正規化してから、フォールバックのループに渡す、という考え方自体は`route`/`dispatch`の内部実装と同じである:

```javascript
// candidates: 解決済みの、このクラス/ロール(+アーキタイプ)向け順序リスト
// 例: ['copilot', 'codex', 'claude']
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

  if (exec === 'copilot') {
    // 認可の事前チェック(§4冒頭)で copilot が未認可と判明していれば、instructor は
    // そもそも candidates から外しているはず。ここに到達した時点でもし未認可なら、
    // リレーは即 STATUS: unavailable を返す — それがランタイム側のバックストップ。
    // §2の copilot-relay レシピと同じ STATUS: 判別子付きリレー。
    const reply = await agent(
      'Run: agent-exec run copilot --capture --model gpt-5.6-luna --effort medium ' +
      '--workdir ' + workdir + ' --prompt-file {promptfile} ; this prints one normalized JSON ' +
      'object { status, answer, session_id, reason, exit_code } to stdout — parse it directly. ' +
      'Reply with STATUS: ok or STATUS: unavailable as the first line (mirroring the JSON\'s ' +
      '`status`), followed by the `answer` and SESSION_ID: line on ok, or the `reason` value ' +
      'on unavailable.',
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
        return { pass: v.pass, summary: String(v.summary ?? ''), feedback: v.feedback ?? [] }
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

`agent-exec copilot [raw args...]`のパススルーは変わらず残る。以下は、その上に足された**追加的**なサブコマンド群で、これまでinstructorがコンテキスト内で行っていた作業(configのマージ、`priority`の降格ウォーク、可否の事前チェック)を`agent-exec`側に寄せるためのものである。特に`route`/`dispatch`(v0.11.0で追加)により、エグゼキュータ選択はinstructorの判断ではなく`agent-exec`が実行するコードになった。

- **`agent-exec config [--json]`** — 4層のorchestra設定(built-in defaults → `~/.claude/orchestra.yaml` → `./.claude/orchestra.yaml` → `./.claude/orchestra.local.yaml`)を、SKILL.md §9と同じ規則(mapping key-by-keyでマージ、スカラー/リストは丸ごと置換、明示的な`null`は値として扱う)で決定的にdeep-mergeし、`external_executors.<name>`のうち`enabled: true`かつ`dispatch: cli`のものについては実行ファイルの`shutil.which`可否を`"available"`として付与したうえで、解決済み設定をJSONとしてstdoutに出す。これにより、instructorがYAMLレイヤをコンテキスト内でマージする必要がなくなる。トップレベルには`warnings`配列(pre-0.4語彙検出`legacy_vocab`、`orchestra.json`検出`legacy_json`。無ければ`[]`)も含まれるため、`setup`スキルはレガシー検出のために生のレイヤファイルを読む必要がない。
- **`agent-exec run <profile> --model M --effort E --workdir W --prompt-file F [--resume SID] [--output FMT] [--capture]`** — profile(現状`copilot`)ごとのCLIフラグ規約と安全側デフォルト(`--disable-builtin-mcps`・`--add-dir`・`--output-format`)を一本化した、正規化ずみのディスパッチ入口。§2で示した生の`agent-exec copilot -p ... --model ... --effort ... --add-dir ... --output-format json --disable-builtin-mcps`と等価なコマンドを、`agent-exec run copilot --model gpt-5.6-luna --effort medium --workdir "$WORKDIR" --prompt-file TASK.md`のように短く書けるようにするもの。allow-allの注入は無い(M1のまま)。
  - **`--capture`を付けると、`os.execvpe`によるプロセス置換ではなく、copilotをサブプロセスとして起動してstdout/stderrをキャプチャし、そのJSONLをパースして`{ status, answer, session_id, reason, exit_code }`という正規化JSONオブジェクトを1つ、agent-exec自身のstdoutに出して終了コード0で返す。** `status`は`"ok"`または`"unavailable"`(quota/credits/auth/rate-limit/nonzero-exit/errorのいずれかが`reason`に入る)。エグゼキュータ側のunavailableは非ゼロ終了で表現されず、この`status`フィールドで表現される点に注意 — Haikuリレーはこの1つのJSONを読むだけでよく、jqでのJSONLパースはリレー側にはもう不要(§2)。`--capture`を付けない場合は従来どおり`os.execvpe`によるハンドオフのままで、挙動に変化はない。
- **`agent-exec route --class <light|standard|deep|review|independent-review> [--archetype default|investigation] [--exhausted a,b] [--json|--text]`**(v0.11.0で追加)— §4の`priority`ウォークをコード側で実行する読み取り専用サブコマンド。4層configをマージし、各候補を`enabled`・binary/agent解決可否・`doctor`相当の`ready.<x>.ok`・`class_policy`該当有無・呼び出し側の`--exhausted`でゲートしたうえで、生き残った先頭候補を`{ class, archetype, executor, dispatch, model, effort, agent_type, candidates, remaining, skipped, source }`として返す(`skipped`には各候補が外れた理由が`{executor, reason}`で入る)。instructorはこれを呼ぶだけで、`priority`リストを手で読んで降格判定する作業から解放される。
- **`agent-exec dispatch --class <cls> [--archetype A] [--exhausted a,b] --prompt-file F --workdir W [--resume SID] [--capture]`**(v0.11.0で追加)— `route`をさらに一歩進めた、実際にディスパッチまで行うサブコマンド。内部で`route`を呼び、勝者が`dispatch: cli`のエグゼキュータ(Copilot)なら実際に実行して`{ status: "ok"|"unavailable", answer, session_id, reason, exit_code, executor, model, effort, route }`を返す。勝者がClaudeまたは`dispatch: agent`のエグゼキュータ(Codex)なら、実行はinstructor自身の`agent()`/Agentツール呼び出しでしか行えないため`{ status: "delegate", executor, model, effort, agent_type, route }`を返すに留める。有効な候補が一つも残らなければ`{ status: "unroutable", route }`。**これが`run` SKILL.md §5の`dispatchClass()`が包んでいる1回の呼び出しそのものであり、instructorが`light`/`standard`タスクのエグゼキュータを自分で決めることは無くなった — 1つの安価なリレーエージェント経由でこれを尋ね、返ってきたJSONで分岐するだけになる。**
- **`agent-exec doctor [--json | --text]`**(既定`--json`)— シム自身のインストール状況(パス・PATH上か・マーケットプレイス/cacheどちらを指しているか)、`uv`の有無、`agent-exec config`と同じ4層configマージ結果(同じ`warnings`配列を`config.warnings`として含む)、`permissions.allow`中の`Bash(agent-exec:*)`ルールの有無(ベストエフォート)、そして各cliエグゼキュータの総合可否`ready.<name>.ok`(未充足の`missing[]`付き)を1回でまとめて返す、読み取り専用の診断サブコマンド。上記§4の認可事前チェックはこれ1本に集約する。**解決済みのconfig全体(`tiers`/`available`注記つき`external_executors`/`priority`)を`config.values`として同梱する**(解決失敗時は`null`、理由は`config.error`)ため、instructorは起動時の`doctor`1回で「可否verdict」と「解決済みモデルポリシー」の両方を得られ、`agent-exec config`を別途呼ぶ必要はない(config単体が欲しいときだけ`config`を使う)。シムが未インストールでもブートストラップパス経由で直接呼び出せ、レポート自体が「未準備」を表現するため、内部エラーでない限り常に終了コード0。
  - `executors`セクションは、有効化済み`dispatch: cli`のものだけでなく**既知の全エグゼキュータ**(`codex`・`copilot`)を`enabled`/`dispatch`/`binary`/`available`つきで網羅する — `dispatch: agent`のCodexも`available`(バイナリのPATH上の有無、参考情報)と、実際の可否はサブエージェントのセッション解決に依存し`agent-exec`からは判定不能である旨の`note`付きで含まれる。`ready.<name>.ok`は従来どおり有効化済み`dispatch: cli`エグゼキュータのみに付与され、Codexのようなagent dispatchには`ready`の verdict を作らない(判定不能なため)。`setup`スキルはこれにより、`codex`/`copilot`いずれについても個別の`command -v`を実行する必要がない。

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
| `executor` | `claude` \| `copilot` \| `codex` |
| `cls` | `light` \| `standard` \| `deep` \| `review` |
| `status` | `ok` \| `unavailable` |
| `reason` | `quota` \| `rate-limit` \| `credits` \| `auth` \| `nonzero-exit` \| `error` |
| `resumed` | boolean |
| `task_count`/`pass`/`fail`/`exhausted`/`fallbacks` | 非負整数(`run_summary`専用) |
| `classes`/`rounds`/`executors_used`/`external_enabled` | dict型ヒストグラム(`run_summary`専用) |

**`agent-exec run ... --capture`の自動ログ。** §5の`run`サブコマンドに`--cls CLASS`(`light`/`standard`/`deep`/`review`)を渡すと、そのディスパッチの能力クラスとしてタグ付けされる。`--capture`を付けたときは、結果を出力した後に`event: dispatch`のレコードを1件、`agent-exec`自身が自動でtelemetryに追記する(`status`/`reason`/`cls`など)。これはLLMを介さず`agent-exec`内部で完結する。Copilotの`answer`(回答本文)がtelemetryに記録されることは絶対にない。`agent-exec dispatch --class <cls> ... --capture`(§5)も内部で`run`と同じCLI実行パスを通るため、`--class`から`cls`が自動的に埋まった同じ`dispatch`レコードが同様に自動で記録される — instructor側で`--cls`を別途渡す必要はない。

**run_summaryはinstructor自身ではなくリレー経由。** 1回のオーケストレーション実行の終わりに、telemetryが有効なら(`doctor`の`config.values.telemetry.enabled`で判定)、instructorは安価なHaikuリレーエージェントに`agent-exec telemetry record --json '...'`を1回実行させ、`run_summary`レコードを1件だけ記録する — instructor自身が直接実行することは絶対にない。無効なら何もせずスキップする。これはinstructorのコンテキストを汚さないための設計であり、また`record`はどのみちカテゴリ値/数値フィールドしか受け付けないため、instructorが直接叩いても得られる自由度は無い。
