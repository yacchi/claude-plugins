# 外部エグゼキュータ(pi)の運用リファレンス

`external_executors`を設定してpiをorchestraのパイプラインに組み込む際の、モデル選定・CLI利用方法・料金の詳細。`SKILL.md` §9はこの機能の存在と設定スキーマだけを説明しているため、実際にディスパッチする段になったらこのファイルを読むこと。

**CLIの実行役は`pi`(§2)だけ。** v0.43.0で`copilot`と`opencode`を、v0.52.0で`codex`を削除した(§1、§7)。

背景にある調査結果(なぜこのポリシーになったか)は`poc-findings.md`を、再試験の手順は`poc-fixtures/README.md`を参照。

## 1. Codex CLIエグゼキュータの削除(v0.52.0)

Codex CLI(`codex exec`)を直接起動するエグゼキュータ`codex`は、v0.52.0で完全に削除した。理由は、(1) piの`openai-codex/*`プロバイダが同じChatGPTサブスクリプションの同じモデルを動かし、優先度でも常にpiがCodexより先だったこと、(2) PoCでCodex CLIを使う利点が見つからなかったこと、(3) macOSではCodexをsrtに載せられなかったこと(OSの証明書ストアに頼るため)。

`orchestra.yaml`の`priority`に`codex`が残っていると、`route`はそれを`unknown-executor:codex`として外して次の候補へ進む(`route`が`codex`を返すことはない。`external_executors.codex`も使われない)。警告は出ないので、該当キーは削除すること。同じモデルを使いたいときは、piの`class_policy`に`openai-codex/<model>`を指定する(§2)。別プラグインの`codex`とその`codex:codex-rescue`エージェントはorchestraの外にあり、orchestraはそこへルーティングしない。

当時のPoCの計測値は`poc-findings.md`に履歴として残している。

## 2. pi(`dispatch: cli`)— 唯一のCLI実行役

piは複数プロバイダに単一インターフェースで到達するコーディングエージェントCLIである。v0.43.0で`copilot`と`opencode`を外し、CLIの実行役は`pi`だけになった(§7)。

**なぜpiか(2026-10-08の再ベンチ、`poc-findings.md`の同日節)。** 同じ`gpt-5.6-luna`が、pi経由で49-70秒、opencode経由で86秒、Codex CLI経由で316秒(7月)。入力トークンはpiがopencodeの約1/3-1/4、Copilot CLIの新規入力はopencodeの約19倍。品質は同じモデルなら同じで、速度とトークンはハーネスで決まる。さらに、ChatGPTアカウントのCodex CLIはGPT-6系(`gpt-6-luna`/`gpt-6-sol`/`gpt-6.1-sol`)をHTTP 400で拒否するが、piの`openai-codex`プロバイダは同じサブスクリプションでそれらを実行できる。

**モデルIDは`provider/id`形式。** 裸のIDは解決しない。

| プロバイダ | 例 | 課金 |
|---|---|---|
| `openai-codex/*` | `openai-codex/gpt-5.6-luna`、`openai-codex/gpt-6.1-sol` | ChatGPTサブスクリプション |
| `github-copilot/*` | `github-copilot/gpt-5.6-luna`、`github-copilot/claude-haiku-5.5` | トークン従量(2026-06以降のGitHub課金) |

既定の`class_policy`は、`light`/`standard`が`openai-codex/gpt-5.6-luna`/medium、`deep`が`openai-codex/gpt-6.1-sol`/high、`independent-review`が`openai-codex/gpt-6.1-sol`/mediumである。`gpt-6-luna`は再ベンチで36/38(`order=asc|desc`の読み違い、`poc-findings.md`参照)だったため既定には入れていない。

設定に書かれた未知のエグゼキュータ名(削除済みの`copilot`/`opencode`を除く)は`unknown-executor:<name>`としてスキップされる。削除済みの名前は設定エラーになり、移行ガイドが表示される。

**版の固定と専用のエージェントディレクトリ(v0.47.0)。** piは`@earendil-works/pi-coding-agent`を`tools.pi`で`1.2.0`に固定し、`agent-exec tools install pi`で導入する。`agent_dir: managed`(既定)では`<state>/pi-agent`を`PI_CODING_AGENT_DIR`に使い、ユーザーの設定・モデル・AGENTS.md・パッケージが実行役に混ざらないようにする。`context_files: false`で`--no-context-files`を付ける。ログインはClaudeが`agent-exec pi login`を実行し(orchestra専用のherdrでpiの`/login`を操作し、URLとデバイスコードをJSONで返す)、それをユーザーに見せてブラウザで開いてもらい、`agent-exec pi login --wait`で完了を待つ。ブラウザがlocalhostに届かないときは最終リダイレクトURLを`--paste`で渡し、中止は`--cancel`。または自分のpiでログインすれば`auth.json`のリンクで引き継がれる。詳細は`config.md`の§6・§7。

**起動コマンド(agent-execが組み立てる。`command`テンプレートは不要):**

```bash
PI_SKIP_VERSION_CHECK=1 pi -p --mode json --no-approve --no-extensions --no-skills \
  --no-prompt-templates --no-themes --model <provider/id> --thinking <effort> [--session <id>] < PROMPT.md
```

プロンプトはstdin、cwdはワークツリー(agent-execがそのディレクトリで子プロセスを起動する。手で叩くときも先にcdすること)。`--no-*`はユーザーのpi拡張・スキル・テンプレートがワーカーに混ざるのを防ぐ。`--no-approve`は作業先リポジトリの`.pi/`(設定・拡張・スキル)を読み込まない指定で、ワーカーが他人のリポジトリに置かれた拡張を実行しないようにする。`PI_SKIP_VERSION_CHECK=1`は更新確認で子プロセスが止まるのを避ける。orchestraのパイプラインからは生コマンドを組み立てず`agent-exec dispatch --class light`(または`standard`/`deep`/`independent-review`)を使うこと。手動テストなら`agent-exec dispatch --executor pi --model <provider/id> --effort medium --workdir "$WORKDIR" --prompt-file TASK.md`(ルーティングを通さない直接実行。隔離の既定は`never`)。

**セッション継続。** `--session <id>`で前ラウンドのセッションを再開する。`agent-exec dispatch --task <id>`は§5のセッションストアから自動で再開する。リトライラウンドには、契約の全文ではなく具体的な指摘だけを渡す。

```bash
agent-exec dispatch --executor pi --resume "$SESSION_ID" --model openai-codex/gpt-5.6-luna --effort medium \
  --workdir "$WORKDIR" --prompt-file FEEDBACK.md
```

**usageとコストの意味。** 使用量は出力JSONの`message_end`レコードから集計する(input/output/reasoning/cacheのトークン)。`cost_micro_usd`はpiが報告する**定価ベースの推定値**で、`openai-codex/*`では実際の請求ではない(そのプロバイダはChatGPTサブスクリプションで動く)。実際にドルが動くのは`github-copilot/*`などの従量プロバイダだけである。台帳の合計を実コストとして読まないこと。`agent-exec usage --run <workflow-run-id>`で実行単位の合計を読む。

**罠: 終了コード0でも失敗がありうる。** 最終メッセージの`stopReason`が`"error"`の実行は、終了コード0でも**失敗**として扱う(agent-execは`status: unavailable`として報告する。未対応モデルを指定したときもこの形で失敗する)。終了コードだけで成功判定しないこと。

**導入。** 公式のインストーラとnpm版は、PATH上の`node`で動く。そのため、miseなどでnodeのバージョンが切り替わる環境では壊れやすい。**スタンドアロンのリリースバイナリの利用を推奨する**(GitHubのリリースから取得してPATH上に置く)。導入後に`pi`でプロバイダへログインし(`openai-codex`はChatGPTアカウント)、`agent-exec doctor --json`の`ready.pi.ok`を確認する。

**認可設定。** Claude Code側で必要な許可ルールは`Bash(agent-exec:*)`の1つだけである。Claude Code内では`bin/`が`agent-exec`をBashのPATHに載せるので不要で、`agent-exec install`はClaude Code外のシェル(piのワーカーのシェル、自分のターミナル)向けにシムをPATH上(既定`~/.local/bin/agent-exec`)に置く。シムはプラグインのマーケットプレイス・クローンパスを指すため、`/plugin update`でラッパーの中身も追従する。`agent-exec`を使わない場合は`Bash(pi:*)`を許可する。

**封じ込めの注意。** piはツールの実行前に承認を求めない(`-p`では確認なしに読み書き・コマンド実行を行う)。ワークツリー隔離(`agent-exec dispatch`が未コミット作業のあるリポジトリで自動で行う)を使い、ワーカーのプロンプトにstashの禁止と`agent-exec shelf`の使用を明記すること。CLI実行役はClaudeのフックの外で動くため、フックによるガードは効かない。

**リレーの返信形式(`STATUS:`判別子)。** リレーエージェントの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`ならその後に回答本文と`SESSION_ID: <id>`行、`unavailable`なら一行の短い理由(quota/credits/auth/rate-limit)を続ける。生のCLIログはinstructorのコンテキストに渡さない。`agent-exec dispatch`(直接実行の`--executor pi`も)は`{ status, answer, session_id, reason, exit_code }`の正規化JSONを直接返すので、リレーはそれを読んで転記するだけでよい。

CLI実行役(pi)の子プロセスは、既定(`sandbox.mode: auto`)でOSのサンドボックスに包んで起動する。`required`では包めない環境で起動せず(`status: unavailable`、`reason: sandbox`、クールダウンなし)、`off`では包まない。

**通信の許可リスト(srt)。** `sandbox.network.mode: allowlist`(既定)で`tools.srt`が見つかり(Linuxでは`bwrap`と`socat`も必要)、SeatbeltかbubblewrapがNGでないときは、piは`srt -d -s <state>/cli-srt/<タスクID>.json --control-fd 3 -c "<実行役のコマンド>"`で起動する(`backend: srt`)。srtが自前のSeatbelt/bubblewrapで同じ書き込み・読み取りの方針を強制するので、orchestraのサンドボックスとは重ねない。設定の`allowedDomains`は`allowed_domains`、tracked なファイルから決まるパッケージレジストリ(`ecosystem`)、その実行役の`network_domains`(piは未設定ならモデルのプロバイダ接頭辞から決まる)、承認済みドメインの和である(config.md参照)。Unixソケット・PTYは許可しない。srtのデバッグ出力は実行役の回答や標準エラーに混ぜず`<state>/cli-srt/<タスクID>.log`に分け、`Connection blocked to <host>:<port>`の行を`<state>/audit/network.jsonl`(`session`はタスクID)に追記し、結果に`network_denials: [{host, port}]`(重複なし、`quiet_domains`を除く)として返す。これだけでは`status`は変わらない。実行中に`agent-exec sandbox allow-domain <host>`で承認したドメインは、制御パイプ経由でその実行にも反映される。srtが使えないときは従来どおり(Seatbelt/bwrap/Landlock)で動き、`limits`と`reason`に`network not restricted: <理由>`が入る。

| OS | バックエンド | 強制できること | `limits`に出る制約 |
|---|---|---|---|
| macOS | Seatbelt(`sandbox-exec`) | 書き込み範囲、秘密情報の読み取り拒否、サンドボックス外へのシグナル拒否 | なし |
| Linux | bubblewrap(`bwrap`) | 同上(PID名前空間でシグナルも遮断) | 存在しないファイルの改ざん防止 |
| Linux(bwrapが使えないとき) | Landlock(カーネル5.13以上) | 書き込み範囲、シグナル(ABI 6以上) | `deny_read`、`pi-config`、`orchestra-config` |

Ubuntu 24.04以降は非特権のユーザー名前空間がAppArmorで塞がれているため、bwrapの事前確認に失敗してLandlockに切り替わる。Landlockは許可したディレクトリの内側を個別に禁止できないので、秘密情報の読み取り拒否と、piの設定・拡張機能やorchestraの設定の書き込み禁止が効かない。できる環境ではbwrapを使うこと。どちらの経路が選ばれたかは`agent-exec sandbox probe`か`agent-exec doctor`で確かめる。

**書き込める場所。** タスクの作業ツリー、そのgitの作業用ディレクトリ(`.git/worktrees/<名前>`)と`objects`、一時ディレクトリ、実行役の状態(`~/.pi/agent`)、主要なキャッシュ(`~/.cache`、`~/Library/Caches`、`~/.npm`、`~/go/pkg/mod`と`~/go/pkg/sumdb`、pnpmのストア、`~/.pnpm-store`、`~/.yarn/berry/cache`、`~/.bun/install/cache`、`~/.cargo/registry`と`~/.cargo/git`とその隣のロック・管理ファイル(`.package-cache`、`.package-cache-mutate`、`.global-cache`と`-journal`)、`~/.gradle/caches`、`~/.m2/repository`、`~/.ivy2/cache`、`~/.nuget/packages`、`~/.pub-cache/hosted`と`/git`、`~/.composer/cache`、`~/.terraform.d/plugin-cache`)、設定の`sandbox.allow_write`、学習済みの許可。herdrのサーバーも同じキャッシュに書ける。開けるのはキャッシュそのものだけで、実行ファイルを置く場所(`~/.cargo/bin`、`~/Library/pnpm`の`bin`と`global`、`~/.bun/bin`など)は開けない。`uvx`の一時環境は一時ディレクトリに作る(`UV_TOOL_DIR`)。pnpmのストアは`$PNPM_HOME/store`(未設定なら`$XDG_DATA_HOME/pnpm/store`、それもなければmacOSは`~/Library/pnpm/store`、Linuxは`~/.local/share/pnpm/store`)で、`pnpm_config_store_dir`でそこに固定する。固定しないと、pnpmは警告だけ出してストアを作業ツリー(か書ける最上位の親、例えば`/private/tmp/.pnpm-store`)に移し、拒否として現れない。`pnpm_config_store_dir`か`npm_config_store_dir`を自分で設定していればその値を使う。`npm_config_store_dir`は設定しない(npmが毎回未知の設定として警告し、次のメジャー版で拒否するため)。これしか読まないpnpm 10以前はストアを作業ツリーに移すことがあるが、`isolation.exclude_paths`で取り込みから外れる。

**常に書き込めない場所。** `$XDG_STATE_HOME/claude-orchestra/sandbox-learned.json`(未設定または相対パスなら`~/.local/state/claude-orchestra/sandbox-learned.json`)、`~/.claude/orchestra.yaml`/`.yml`、`~/.claude/orchestra/executor-state.json`、`~/.local/share/uv/tools`、タスクのリポジトリ内の`.claude/orchestra*.y*ml`、piが読み込む設定と拡張(`~/.pi/agent/`の`extensions` `skills` `prompts` `themes` `packages` `npm` `git` `bin` `install`、および`settings.json` `mcp.json` `models.json` `AGENTS.md` `SYSTEM.md` `APPEND_SYSTEM.md` `keybindings.json`)。どれも、後で別のプロセスがコードや指示として読み込む場所である。許可の追加でも開けられない。

**拒否の検知と自動許可。** piのツール出力とエラー出力から、`operation not permitted` / `permission denied` / `read-only file system`の前後にあるパスを拾う(coreutils・Go・Python・Nodeの各形式、UTF-8環境の引用符を含む)。ユーザー自身にも書けない場所(`find /`で出るような拒否)と`deny_read`の内側は対象にしない。

- 拒否されたパスがキャッシュの置き場所(`~/.cache`、`~/Library/Caches`、`~/.npm`、pnpm/yarnのストア、`~/go/pkg/mod`、`~/.cargo/registry`など、取得物を検証する生態系のもの)の中なら、自動で許可する。許可するのは「置き場所＋1階層」で、置き場所の直下への拒否なら置き場所そのもの。許可はマシンごとの`$XDG_STATE_HOME/claude-orchestra/sandbox-learned.json`(未設定または相対パスなら`~/.local/state/claude-orchestra/sandbox-learned.json`)に記録し、同じセッションを再開する(1回のdispatchにつき最大2回)。
- それ以外への拒否で、実行役が失敗を報告したときは`status: needs-permission`を返す。指示役はスキル§8の方針で自分で判断する(ユーザーに聞くのは認証情報、リポジトリとツールチェーンのキャッシュ以外への書き込み、公開・デプロイ・プッシュに当たるもの、仕様との関係が不明なものだけ)。許可するなら`agent-exec sandbox allow <path> --reason TEXT`のあと同じタスクIDで再dispatchする(セッションが再開される)。wave実行中は`agent-exec approvals decide`を使う(許可・再開・監査記録を一度に行う)。
- 実行役が成功を報告したときは`ok`のまま、`sandbox.denials`に拒否を添える(`auto: false`)。作業が実際に済んでいるかはレビューが判定し、不合格なら指示役が同じ方針で判断する。

一覧・追加・削除・診断は`agent-exec sandbox list|allow|forget|probe`、通信の承認は`agent-exec sandbox allow-domain <host> [--reason TEXT]`(`list`がドメインの許可を`source`・追加日時・理由つきで表示する)。

## 2b. 暴走対策(watchdog)

CLI実行役の子プロセスを監視し、次のいずれかでプロセスグループごと止めて`status: runaway`を返す(クールダウンや代替先への切り替えはしない)。

- 出力が`idle_seconds`(既定600秒)途切れた。ツールの実行中は`tool_idle_seconds`(既定900秒)。
- 同じツールを同じ引数で`repeat_limit`(既定3)回続けて呼んだ。間に別の呼び出しが挟まる繰り返しは数えない。
- 総時間が区分ごとの上限(light 1200秒、standard 2400秒、deep 5400秒、independent-review 2400秒)を超えた。

根拠は2026-10-08の計測。opencode経由のワーカーが`/`を起点にglobを1回実行し、2539秒止まっていた。piの通常の実行11本では、同じ呼び出しの連続は最大1回、間を挟んだ同じ呼び出しは最大3回だった(修正後の検証のやり直し)。

piの`stopReason: "error"`は`unavailable`として扱うため、一時的なエラーでもその回の残りでは使い切り扱い(sticky exhausted)になる。

## 3. 公式の単価表と、実際の請求に関する注意

**GitHub Copilot(piの`github-copilot/*`プロバイダ経由)はトークン従量課金。** 2026-06以降、GitHubは[Models and pricing](https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing)でトークン単価を公開している(2026-10-08確認。価格は変動するため重要な判断の前に再取得すること)。100万トークンあたり:

| Model | 入力 | 出力 |
|---|---|---|
| `gpt-6-luna` | $0.10 | $0.50 |
| `claude-haiku-5.5` | $0.10(100Kトークン超のプロンプトでは$0.50) | $0.50(同$2.50) |
| `gpt-5.6-luna` | $0.20 | $1.20 |
| `gpt-6-sol` / `gpt-6.1-sol` | $2.00 | $10.00 |

以前のpremium request / AIU課金の表と`premiumRequests: 1`の注意書きは廃止した(v0.43.0でCopilot CLIを削除、§7)。piが`github-copilot/*`で報告する`cost`はこの従量課金に対応する実額である。一方、**`openai-codex/*`でpiが報告する`cost`は定価ベースの推定値で、請求ではない**(ChatGPTサブスクリプション。§2)。

The default-on run ledger is the deterministic source for reading pi cost per workflow run or session: use `agent-exec usage --run <workflow-run-id>` or `agent-exec usage --session <session-id>` rather than estimating from a time window.

**Claude(`Agent`ツール経由)の単価**、[Anthropicの公式料金ページ](https://platform.claude.com/docs/en/docs/about-claude/pricing)より(2026-10-08確認)、100万トークンあたり:

| Model | 入力 | 出力 |
|---|---|---|
| `haiku`(Claude Haiku 5.5) | $0.10(100Kトークン以下のプロンプト) | $0.50 |
| `sonnet`(Sonnet 5.5) | $2.00 | $10.00 |
| `opus`(Opus 5.5) | $4.00 | $20.00 |

`haiku`エイリアスはClaude Haiku 5.5を指す。`Agent`ツールの完了結果は入出力の内訳のない`subagent_tokens`という合算値しか返さないため、Claudeサブエージェント実行のコストは全て入力/全て出力の両極からの幅としてしか算出できない。サブスクリプションで動く場合、実際の請求は別である。

## 4. 優先度とリアクティブ・フォールバック(priority)

`priority`は、クラス/ロール(と`light`についてはタスクアーキタイプ)ごとに「試す順序」を宣言するトップレベルの設定キーである。値は`external_executors`のキー(`pi`、...)または組み込みClaude実行を表すsentinel `claude`を並べた順序付きリストで、あるエグゼキュータが**unavailable**と判定された時点でのみ次の候補に降格する(=**リアクティブ・フォールバック**)。タスクの実行結果が単に誤っていた場合はフォールバックの対象ではない(後述)。

**この左から右への降格ウォークは、instructorが手で行うものではなく、`agent-exec route --class <cls> [--archetype A] [--exhausted a,b]`(と、それを内包する`agent-exec dispatch`)がコード側で実行する。** `route`は4層configをマージした上で各候補を`enabled`・binary/agent解決可否・`doctor`の`ready.<x>.ok`・`class_policy`該当有無・呼び出し側が渡した`--exhausted`でゲートし、生き残った先頭候補を返す。instructor側に残る唯一の手作業は、あるタスクで`dispatch`が`unavailable`を報告したエグゼキュータを、同じラン内の以降すべての`route`/`dispatch`呼び出しに`--exhausted`として持ち越すこと(sticky exhaustion)だけである — `run` SKILL.md §5の`dispatchClass()`はこれをモジュールレベルの`Set`で自動的に行う。以下のスキーマとJS実装イメージは、`route`が内部で何をしているかを理解するためのものであり、現在ではこれを手で書き写す必要はない。

設定のスキーマは次の通り(§3のClaude/従量課金単価表と対応させて読むこと):

```yaml
# Ordered executor preference per class/role (and, for the implementation classes,
# per task archetype). `agent-exec route` tries candidates left-to-right and drops to
# the NEXT one on a *reactive fallback signal*: the current executor is unavailable
# — NOT the task failing. "Unavailable" = hit a rate-limit / usage-window cap
# (Claude, pi), ran out of credits / quota (a metered pi provider), is
# `enabled: false`, or its agent/CLI did not resolve in this environment. A task
# that RUNS but returns a wrong result is NOT a fallback signal — that stays on the
# SAME executor and goes through the normal review/retry loop.
#
# Entries are an `external_executors` key (`pi`, ...) or the sentinel
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
# A class with no list here routes to `claude` (the `tiers.<name>` model) only;
# `external_executors.*.classes` is not read by routing.
priority:
  light:
    # Claude Haiku 5.5 first (subscription, fastest in the 2026-10-08 re-bench);
    # pi Luna/medium is the fallback when claude is genuinely unavailable.
    investigation: [claude, pi]
    default: [claude, pi]
  standard:
    # Claude Haiku 5.5 leads the standard band (round7 judgment benchmark 3/3 vs
    # Luna 1/3); pi Luna/medium is the fallback.
    default: [claude, pi]
  deep:
    default: [claude, pi]
  review:
    default: [claude]
  independent-review:
    default: [pi]
```

**残量照会はサブスクリプションの使用ウィンドウだけに限る(`usage_routing`)。** piの従量プロバイダの残クレジットを事前に問い合わせるコマンドは存在しない。例外はサブスクリプションのレートリミット窓(5時間・7日)で、`agent-exec route`/`dispatch`は候補の背後にあるプラン(`claude`ならClaudeのサブスクリプション、class modelが`openai-codex/...`の`pi`ならChatGPTのサブスクリプション)の使用率を読み、`usage_routing.thresholds`(既定 five_hour 80% / seven_day 90%)以上の候補を`usage:<group>:<window>=<pct>%`で降格する。降格はcooldownと同じくソフトで、全候補が降格・cooldown中ならそれらを無視してウォークをやり直し`usage_bypassed: true`を付ける。認証情報は読むだけで更新・書き込み・ログ出力はせず、照会結果は`<state dir>/usage-limits.json`に`cache_seconds`(既定120秒)だけキャッシュし(失敗も含む)、エンドポイントは非公開APIなのでどの失敗もfail-open(データなし=降格なし)である。他のプロバイダ(`github-copilot/...`など)はデータがなく降格しない。`--no-usage`(route/dispatch)と`ORCHESTRA_USAGE_ROUTING=0`がescape hatchで、現在値は`agent-exec doctor`の`usage_limits`で確認できる。それ以外のフォールバックの判定材料は、実際にディスパッチした時点で観測される「unavailableシグナル」だけである。

**認可(authorization)の事前チェックは残量照会とは別物で、こちらは推奨する。** 上の「残量照会を同梱しない」は、リモートで変動する*残量*(クレジット/使用ウィンドウ)を事前問い合わせしない、という意味であって*認可*には当てはまらない。`dispatch: cli`のエグゼキュータ(pi)は、セッションに該当するBash許可ルール(`agent-exec`利用時は`Bash(agent-exec:*)`、手動セットアップ時は`Bash(pi:*)`)が欠けていれば**この実行では絶対に成功しない**(これ以外に必要な環境変数はない)。許可ルールが欠けている場合、残量と違って一過性ではなく、リレーを起動してもパーミッションに即拒否される。

**この事前チェックは`agent-exec doctor`を1回呼ぶだけで済む。** シム(`agent-exec`本体)がPATH上にインストールされているか・どのターゲット(マーケットプレイス・クローンか、壊れやすいcacheパスか)を指しているか、実行中のバイナリ(`build`: `binary`/`source_hash`/`go`)、4層configのマージ結果、各`external_executors.<name>`の`enabled`/`available`、`permissions.allow`中の`Bash(agent-exec:*)`ルールの有無(ベストエフォート — `~/.claude/settings.json`と`./.claude/settings.json(.local)`のみを走査し、enterprise policyやCLIの`--allowedTools`は見えない)、そして各cliエグゼキュータの総合可否`ready.<name>.ok`(と未充足の`missing[]`)を、`agent-exec doctor`(人間向け`--text`、instructor向け`--json`)の1回の呼び出しでまとめて返す。instructorは`priority`の候補に`dispatch: cli`エグゼキュータを含める前にこれを実行し、`ready.<name>.ok`が`false`ならそのランでは最初からunavailable扱いにして次候補へ降格させる — 個別に`settings.json`を読んだり`which`を叩いたりする必要はない。これは無駄なリレー起動を省く最適化であり、仮にチェックを省いても下記のリアクティブなフォールバック(リレーの`STATUS: unavailable`)が同じ結論に達するバックストップとして残る。`doctor`はシムが未インストールでも(ブートストラップパス経由で直接)呼び出せるため、シムのインストール前診断にも使える。**(v0.11.0以降の補足)** この事前チェック自体は今や`agent-exec route`/`dispatch`が内部で毎回自動的に行うため、instructorが`priority`候補を組み立てる前に別途`doctor`を呼んでフィルタする、という手順はもう不要になった — `route`/`dispatch`を呼ぶだけで、この段落が説明する`ready.<name>.ok`ゲートは常に適用済みの状態になる。`doctor`単体は、セットアップ診断(シムの設置状況の確認)や`config.values.telemetry.enabled`の確認など、他の目的でなお有用。

**unavailable と failed の区別(最重要)。** この2つを混同すると降格ロジックが壊れる:

- **unavailable**(→次の優先度候補に降格し、そのランの残り全体でsticky-skip): レートリミット/使用ウィンドウ上限、クレジット/quotaの枯渇(従量プロバイダ)、設定上の未知のエグゼキュータ名(`unknown-executor:<name>`)、`enabled: false`、agent/CLI名がこの環境で解決しない、`class_policy.<class>`が欠落している、のいずれか。
- **failed**(→同じエグゼキュータに留まり、通常のverify/retryループに入る): エグゼキュータは実際に走って何らかの出力を返したが、その結果が誤り/不完全だった場合。「動いたが間違えた」はフォールバック理由にならない — それはそのエグゼキュータの検証・再試行の問題であり、別のエグゼキュータに変えても解決するとは限らないため、まずは同じ系統でリトライさせる。

**エグゼキュータ別のunavailableシグナル(§1・§2・§3で説明した各エグゼキュータの実体と対応):**

- **Claude(`claude`sentinel、`agent()`経由)**: `agent()`が`null`を返す(リトライ後も終了しないエラー)場合をunavailableとみなし、次の候補に降格する。(`null`はユーザーによるagentスキップも意味しうるが、いずれも「先に進む」という結論は同じなので、このオーバーロードは許容する。)
- **pi(`dispatch: cli`)**: §2で示した安価なHaikuの**リレーエージェント**がpiを実行し、その結果(終了コード・出力内容)を検査する。終了コードが非ゼロ、最終メッセージの`stopReason`が`error`、またはエラー/quota/credits/認証系のリミット表示が見えた場合にunavailableと報告する。このリレーは生のCLIログをinstructorに渡してはならず、判定結果だけを短く返す。

  **ただしリミット表示の走査対象は「裏付けのある出力」に限る(default-deny)。** 走査するのはstderr全文・JSONとしてパースできなかったstdout行(平文のquota/認証エラーはまさにここに出る)・そして`session.error`のようなエラー種別イベントだけであり、`assistant.message`に加えて`tool.execution_complete`・`system.message`・reasoning・リクエストのエコーといった**ワーカー自身のテキストを運ぶイベントは走査しない**。これは実害から導かれた規則である: ワーカーが書いていたREADMEに含まれるHTTP 429のバックオフ表や、「Implement JWT auth ...」というタスクプロンプトのエコーが、`exit_code: 0`で正常に回答を返したランを`unavailable`(reason: rate-limit / auth)に転ばせていた。cooldownはこの判定を1時間の実可用性喪失へ増幅するため、`record_unavailable_cooldown`側にも「終了コード0かつ非空の回答があるならcooldownを書かない」という二重の歯止めを置いてある。

**リレーの`STATUS:`判別子。** piのようにワーカーがモデル自身の判断でエラーを報告するのではなく「リレーエージェントが後からCLIの結果を検査する」構成では、Workflowスクリプトが機械的に分岐できる目印が必要になる。そのためpiリレーの返信は必ず1行目を`STATUS: ok`または`STATUS: unavailable`にする。`ok`の場合はそれに続けて回答本文、さらに(セッション継続が必要なら)`SESSION_ID: <id>`行を返す。`unavailable`の場合はそれに続けて一行の短い理由(quota/credits/auth/rate-limitのいずれか)だけを返す。これは§2で示したセッション継続レシピ(§2)と同一の形式であり、実際に§2のリレープロンプトは`STATUS:`行を必ず含めるよう既に更新済みなので、`priority`のフォールバックとセッション継続は同じ1本のリレープロンプトから両方読み取れる。

**sticky exhaustion(枯渇の記憶)。** 1回のオーケストレーション実行の中でunavailableと判定されたエグゼキュータは、`Set`などに記録して以降のすべてのタスクでスキップする。タスクごとに毎回同じエグゼキュータを再プローブしない — 一度枯渇したエグゼキュータが同じランの途中で復活していないか確認する意味はない。

**クロスランcooldown(枯渇の時間減衰)。** 同じunavailableの再発を次のランで毎回発見し直さないため、`agent-exec dispatch`(直接実行を含む)は、パースした結果が`status=unavailable`になった瞬間に、LLMを介さず`~/.claude/orchestra/executor-state.json`(`{executor: {reason, until}}`)へ自動保存する。理由ごとのcooldown秒数は設定(`rate-limit: 900`、`quota: 3600`、`credits: 3600`、`auth: 0`、`nonzero-exit: 0`)から取り、`agent-exec route`/`dispatch`はファイルを読み、期限内のエントリを既存の`--exhausted`と同じ候補ゲートへ渡す(スキップ理由は`cooldown:<reason>`)。これは2026-07-28 09:23〜09:32(UTC)に、外部エグゼキュータ側の本物の枯渇(`session.error` / `errorType: quota` / HTTP 402)を9分間で12回連続して発見し直したための最適化である。ただしcooldownはクラスをunroutableにしない: 全候補がcooldown中ならcooldownを無視してウォークをやり直し、結果に`cooldown_bypassed: true`を付ける。認証とnonzero-exitはリソース枯渇ではなく一過性なので`0`(cooldownなし)であり、missing/corruptな状態ファイル、書き込み不能なパス、保存失敗もすべてfail-openで従来どおりルーティングする。`--no-cooldown`(route/dispatch)、`cooldown.enabled: false`、`agent-exec ui`ダッシュボードからのクリアがescape hatchであり、期限内のエントリは`agent-exec doctor`の`cooldowns`で確認できる。これはラン内の`Set`によるsticky exhaustionを置き換えず、その下に加わるクロスラン層である。cooldownはディスパッチ時に実際に観測したシグナルだけを反応的に記憶し、リモートの残量APIは問い合わせない — 事前の照会はサブスクリプションの使用ウィンドウを読む`usage_routing`の役割であり、両者は同じウォークの中でソフトな降格として並ぶ。

**候補の順序は`priority`だけが決める。** `priority.<class>`(アーキタイプ、なければ`default`)のリストがそのクラス/ロールの候補集合と順序であり、リストが無いクラスは`claude`だけになる。`external_executors.<key>.classes`はルーティングでは読まれない。実際のモデル/effort/dispatch設定は`class_policy`から取り、`enabled: false`はそのエグゼキュータを無条件に除外する。

**タスクアーキタイプの分類**は分解(decomposition)時点でのinstructorの判断であり、実行時に動的に切り替えるものではない: `investigation`は読み取り専用の調査・研究・コードベース探索的なfan-outで、ファイル変更もreviewループも伴わないもの。`default`はそれ以外すべて(review/retryループを通るファイル変更を伴う実装)。他のクラス/ロール(`deep`・`review`・`independent-review`)は通常`default`だけで十分。

**deep-mergeの注意点は他の設定キーと同じ**: `priority`のリストはリストとして扱われるため、より詳細なレイヤ(project設定など)で上書きされると要素単位でマージされず丸ごと置き換わる。部分的に変えたいだけでも、そのクラス/ロール/アーキタイプの配列は全体を書き直す必要がある。

**Workflowスクリプトでの実装イメージ(参考 — v0.11.0以降は不要な手書きロジック)。** 以下は`agent-exec route`/`dispatch`が存在する前に、instructorがこのフォールバック・ウォークを自前で実装するとしたら何をする必要があったかを示す参考コードであり、`route`が内部で行っているマッピング・正規化・降格判定の意味論を理解するために残している。**実際のWorkflow実行では、このコードを書き写さず、同梱のパイプライン(`run` SKILL.md §5)を使うこと。** 各候補を実際のディスパッチ手段(Claudeの`agent()`、piのリレーエージェント)にマップし、戻り値を`{ status: 'ok'|'unavailable', answer?, reason?, sessionId? }`という共通の形に正規化してから、フォールバックのループに渡す、という考え方自体は`route`/`dispatch`の内部実装と同じである:

```javascript
// candidates: 解決済みの、このクラス/ロール(+アーキタイプ)向け順序リスト
// 例: ['pi', 'claude']
const exhausted = new Set() // このランの間だけ有効なsticky exhaustion

async function runOn(exec, taskPrompt, workdir) {
  if (exec === 'claude') {
    const answer = await agent(taskPrompt, { label: 'light', model: tierModelFor('light') })
    if (answer === null) {
      return { status: 'unavailable', reason: 'agent() returned null (terminal error or skip)' }
    }
    return { status: 'ok', answer }
  }

  if (exec === 'pi') {
    // 認可の事前チェック(§4冒頭)で pi が未認可と判明していれば、instructor は
    // そもそも candidates から外しているはず。ここに到達した時点でもし未認可なら、
    // リレーは即 STATUS: unavailable を返す — それがランタイム側のバックストップ。
    // §2の pi リレーレシピと同じ STATUS: 判別子付きリレー。
    const reply = await agent(
      'Run: agent-exec dispatch --executor pi --model openai-codex/gpt-5.6-luna --effort medium ' +
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

**外部エグゼキュータの自由文出力をJSONに正規化する(特に外部エグゼキュータの`independent-review`)。** Claudeの`agent()`は`schema`オプションでStructuredOutputツールを強制できるが、`dispatch: agent`の外部エグゼキュータはそのツールを必ずしも尊重せず、自由文を返しうる。そこで外部エグゼキュータにverdict/レビューを求めるときは、(1)プロンプトで明示的にJSONだけを出力させ(スキーマ例を本文に埋め込み、「JSON以外は出力するな」と指示する)、(2)戻り値を寛容にパースして`VERDICT_SCHEMA`(run SKILL.md §5)と同じ形に正規化する。生の自由文をそのまま`verdict.pass`として扱わないこと — 独立レビューであってもフォールバック契約(structured verdict)と同じ形に畳んでからinstructorに返す:

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

外部エグゼキュータのindependent-reviewに渡すプロンプト末尾の指示例: 『Reply with ONLY a JSON object of the form {"pass": boolean, "summary": string, "feedback": [{"case","expected","actual"}]}. Output nothing else — no prose, no explanation.』

## 5. `agent-exec config` / `route` / `dispatch` (追加サブコマンド)

以下は、これまでinstructorがコンテキスト内で行っていた作業(configのマージ、`priority`の降格ウォーク、可否の事前チェック)を`agent-exec`側に寄せるためのものである。特に`route`/`dispatch`(v0.11.0で追加)により、エグゼキュータ選択はinstructorの判断ではなく`agent-exec`が実行するコードになった。

- **`--prompt-file`は`dispatch`(ルーティング・直接実行とも)で繰り返し指定できる。** 指定順にファイルを読み、連続するファイルの間を空行1つで連結して1つのプロンプトにする。1回だけ指定した場合は従来どおり。ファイルが存在しない、または読めない場合は使用法エラーとして終了コード2で終了し、stderrに該当パスを出し、ディスパッチは行わない。
- **`agent-exec config [--json]`** — 4層のorchestra設定(built-in defaults → `~/.claude/orchestra.yaml` → `<プロジェクト基点>/.claude/orchestra.yaml` → `<プロジェクト基点>/.claude/orchestra.local.yaml`。基点は下記`dispatch prepare`の項)を、SKILL.md §9と同じ規則(mapping key-by-keyでマージ、スカラー/リストは丸ごと置換、明示的な`null`は値として扱う)で決定的にdeep-mergeし、`external_executors.<name>`のうち`enabled: true`かつ`dispatch: cli`のものについては実行ファイルのPATH上の可否を`"available"`として付与したうえで、解決済み設定をJSONとしてstdoutに出す。これにより、instructorがYAMLレイヤをコンテキスト内でマージする必要がなくなる。トップレベルの`warnings`配列は互換のために残っているが常に`[]`(レガシー設定の検出はv0.55.0で廃止)。
- **`agent-exec dispatch --executor pi --model M --effort E --workdir W --prompt-file F [--prompt-file G ...] [--class C] [--run-id ID] [--resume SID] [--isolate never|auto|always] [--task T]`**(直接実行)— ルーティングを通さず、指定したエグゼキュータ(`pi`)をCLIフラグ規約と安全側デフォルト込みで1回実行する正規化ずみの入口。§2で示した生のpi起動コマンドと等価なものを短く書ける。隔離の既定は`never`。
  - **エグゼキュータをサブプロセスとして起動してstdout/stderrをキャプチャし、そのJSONLをパースして`{ status, answer, session_id, reason, exit_code }`という正規化JSONオブジェクトを1つ、agent-exec自身のstdoutに出して終了コード0で返す。** `status`は`"ok"`または`"unavailable"`(quota/credits/auth/rate-limit/nonzero-exit/errorのいずれかが`reason`に入る)。エグゼキュータ側のunavailableは非ゼロ終了で表現されず、この`status`フィールドで表現される点に注意 — Haikuリレーはこの1つのJSONを読むだけでよく、jqでのJSONLパースはリレー側にはもう不要(§2)。セッションストアは使わない(再開は`--resume`で明示する)。
- **`agent-exec route --class <light|standard|deep|review|independent-review> [--archetype default|investigation] [--exhausted a,b] [--json|--text]`**(v0.11.0で追加)— §4の`priority`ウォークをコード側で実行する読み取り専用サブコマンド。4層configをマージし、各候補を`enabled`・binary/agent解決可否・`doctor`相当の`ready.<x>.ok`・`class_policy`該当有無・呼び出し側の`--exhausted`でゲートしたうえで、生き残った先頭候補を`{ class, archetype, executor, dispatch, model, effort, agent_type, candidates, remaining, skipped, source }`として返す(`skipped`には各候補が外れた理由が`{executor, reason}`で入る)。instructorはこれを呼ぶだけで、`priority`リストを手で読んで降格判定する作業から解放される。
- **`agent-exec dispatch --class <cls> --prompt-file F [--prompt-file G ...] --workdir W [...]`**(v0.11.0で追加)— `route`をさらに一歩進めた、実際にディスパッチまで行うサブコマンド。内部で`route`を呼び、勝者が`dispatch: cli`のエグゼキュータ(pi)なら実際に実行して`{ status: "ok"|"unavailable", answer, session_id, resumed, reason, exit_code, executor, model, effort, route }`を返す(`session_id`/`resumed`はセッション概念を持たないエグゼキュータでは`null`/`false`)。勝者がClaudeまたは`dispatch: agent`のエグゼキュータ(明示的にオーバーライドされた場合)なら、実行はinstructor自身の`agent()`/Agentツール呼び出しでしか行えないため`{ status: "delegate", executor, model, effort, agent_type, route }`を返すに留める。有効な候補が一つも残らなければ`{ status: "unroutable", route }`。**これが`run` SKILL.md §5の`dispatchClass()`が包んでいる1回の呼び出しそのものであり、instructorが`light`/`standard`タスクのエグゼキュータを自分で決めることは無くなった — 1つの安価なリレーエージェント経由でこれを尋ね、返ってきたJSONで分岐するだけになる。** `--task <id>`かつ`--resume`未指定なら、`dispatch: cli`エグゼキュータについてタスクごとのセッションストアを自動参照する(上記「タスクごとのセッションストアと自動再開」参照)。`--no-resume`はこの参照だけをオプトアウトする。保存済みレコードを表示するコマンドはなく、実際に使われたかは結果の`resumed`/`session_id`で分かる。
  - **`dispatch prepare`と設定の基点。** プロジェクト層(`.claude/orchestra.yaml|.yml`・`.claude/orchestra.local.yaml|.yml`)は、cwdから git の toplevel まで(toplevel を含む)上へたどり、これらのどれかを持つ最も近いディレクトリ(「プロジェクト基点」)から読む。見つからない、または git 管理外なら cwd(従来どおり)。`prepare`はこの基点を`"config_root"`としてトークンに焼き込み、`dispatch --token`(同期・detach の子とも)は自分の cwd にかかわらずその基点で設定を解決する(`config_root`を持たない古いトークンは cwd のまま)。`prepare`の出力には、実際にマージした層ファイルの絶対パスが載る(JSON では配列`config_layers`、テキストでは`config: <カンマ区切りのパス or (defaults only)>`の1行)。詳細は`config.md` §1。
  - **`--detach`と`dispatch wait`(中継の待ち方)。** `dispatch --token T ... --capture --detach`は子プロセスを起動してすぐ`{"status":"detached",...}`を返す。`agent-exec dispatch wait --token T [--max-wait N] [--summary]`は最大 N 秒(既定 560)待ち、まだ動いていれば`{"status":"running","token","pid","waited_s","wait_calls"}`を返す。子が終わると最終 JSON を`<tokens>/detach/<T>.result.json`に保存し、状態ファイルは`"finished"`・`"collected": true`を足して残す。**何度`wait`しても同じ最終結果を返す(冪等)。** 空でない`answer`は全文を`<tokens>/detach/<T>.answer.txt`に書き、結果に`answer_path`を足す。`--summary`は1行のコンパクトな JSON を出し、`answer`を先頭 400 コードポイントに切る(切ったら`"answer_truncated": true`)。記録が何もない正しい形式のトークンは exit 0 で`{"status":"unknown-token","token":T}`。古い`detach/<T>.*`は保持期間を過ぎ、pid が死んでいれば`dispatch`の保持期間掃除が消す。SKILL.md §5 の`dispatchClass()`は、開始1回と`wait --summary`の繰り返しをスクリプトの`while`で回し、中継には1回に1コマンドだけを実行させる。詳細は`config.md` §1「Detached dispatch」。
- **`agent-exec write-file --path P [--expect-chars N]`** — 標準入力をすべて UTF-8 として読み、そのまま`P`へ原子的に書く(同じディレクトリの一時ファイル+`os.replace`、親ディレクトリは作る)。`--expect-chars N`は、末尾の改行を1つ除いた内容のコードポイント数と N を比べ、違えば書かずに`{"status":"mismatch",...}`を exit 1 で返す。成功は`{"status":"ok","path","chars"}`を exit 0。SKILL.md §5 の`writeCorrectionPacket()`が、訂正パケットを LLM に「書かせる」代わりに使う。
- **`agent-exec doctor [--json | --text]`**(既定`--json`)— シム自身のインストール状況(パス・PATH上か・マーケットプレイス/cacheどちらを指しているか)、実行中のバイナリ(`build`: `binary`/`source_hash`/`go`。初回呼び出しでブートストラップがビルドするため、`doctor`は事前ビルドも兼ねる)、`agent-exec config`と同じ4層configマージ結果、`permissions.allow`中の`Bash(agent-exec:*)`ルールの有無(ベストエフォート)、そして各cliエグゼキュータの総合可否`ready.<name>.ok`(未充足の`missing[]`付き)を1回でまとめて返す、読み取り専用の診断サブコマンド。上記§4の認可事前チェックはこれ1本に集約する。**解決済みのconfig全体(`tiers`/`available`注記つき`external_executors`/`priority`)を`config.values`として同梱する**(解決失敗時は`null`、理由は`config.error`)ため、instructorは起動時の`doctor`1回で「可否verdict」と「解決済みモデルポリシー」の両方を得られ、`agent-exec config`を別途呼ぶ必要はない(config単体が欲しいときだけ`config`を使う)。シムが未インストールでもブートストラップパス経由で直接呼び出せ、レポート自体が「未準備」を表現するため、内部エラーでない限り常に終了コード0。
  - `executors`セクションは、有効化済み`dispatch: cli`のものだけでなく**既知の全エグゼキュータ**(`pi`)を`enabled`/`dispatch`/`binary`/`available`つきで網羅する — 既定の`dispatch: cli`のpiは`ready.<name>.ok`(と`missing[]`)を持つ。`dispatch: agent`へ明示的にオーバーライドされた場合のみ、`available`(バイナリのPATH上の有無、参考情報)と、実際の可否はサブエージェントのセッション解決に依存し`agent-exec`からは判定不能である旨の`note`付きに切り替わり、`ready`のverdictは作られない(判定不能なため)。`setup`スキルはこれにより、`pi`個別の`command -v`を実行する必要がない。

## 6. `agent-exec telemetry` — オプトインの匿名テレメトリ

`agent-exec`にはメンテナがorchestraを改善するための、オプトイン・匿名化された「クラッシュダンプ的」テレメトリ機構がある。設定・意味論の全体像は`run`スキルのSKILL.md §10を参照。ここではCLIサブコマンドと、`dispatch`側の自動ログの仕様のみを記す。

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
| `executor` | `claude` \| `pi` |
| `cls` | `light` \| `standard` \| `deep` \| `review` |
| `status` | `ok` \| `unavailable` |
| `reason` | `quota` \| `rate-limit` \| `credits` \| `auth` \| `nonzero-exit` \| `error` |
| `resumed` | boolean |
| `task_count`/`pass`/`fail`/`exhausted`/`fallbacks` | 非負整数(`run_summary`専用) |
| `classes`/`rounds`/`executors_used`/`external_enabled` | dict型ヒストグラム(`run_summary`専用) |

**`agent-exec dispatch`の自動ログ。** `dispatch`はCLIエグゼキュータを実行して結果を出力した後に、`event: dispatch`のレコードを1件、`agent-exec`自身が自動でtelemetryに追記する(`status`/`reason`/`cls`など)。これはLLMを介さず`agent-exec`内部で完結する。エグゼキュータの`answer`(回答本文)がtelemetryに記録されることは絶対にない。`cls`はルーティング時は`--class`のクラス、直接実行(`--executor`)では`--class C`を渡したときにそのクラスでタグ付けされる。

**run_summaryはinstructor自身ではなくリレー経由。** 1回のオーケストレーション実行の終わりに、telemetryが有効なら(`doctor`の`config.values.telemetry.enabled`で判定)、instructorは安価なHaikuリレーエージェントに`agent-exec telemetry record --json '...'`を1回実行させ、`run_summary`レコードを1件だけ記録する — instructor自身が直接実行することは絶対にない。無効なら何もせずスキップする。これはinstructorのコンテキストを汚さないための設計であり、また`record`はどのみちカテゴリ値/数値フィールドしか受け付けないため、instructorが直接叩いても得られる自由度は無い。

## 7. 削除したエグゼキュータ(v0.43.0)

v0.43.0で次の2つのCLI実行役を削除した(`codex`はv0.52.0で削除、§1)。CLIの実行役は`pi`(§2)だけである。

- **`copilot`(GitHub Copilot CLI)** — 同じモデルでもpiより遅く、新規入力トークンが桁違いに多い(2026-10-08の再ベンチでopencodeの約19倍)うえ、課金はpiの`github-copilot/*`プロバイダと同じトークン従量で、勝てる点が無い。Copilotのモデルは今後も`pi`の`github-copilot/*`経由で使える(§3の単価表)。
- **`opencode`** — 同じ課金の同じモデルに対して、piより遅く(同じ`gpt-5.6-luna`で86秒対49-70秒)、入力トークンは約3-4倍。長時間のランナウェイ(単一の`glob`が2539秒)も観測した。Codex系モデルを呼ぶ用途はpiの`openai-codex/*`が担う。

理由は両者とも「同じ課金で厳密に劣る」ことである(データと表は`poc-findings.md`の2026-10-08節)。ユーザー設定の`priority`に`copilot`/`opencode`が残っていても、§1の`codex`と同じく`route`は`unknown-executor:<name>`として黙って外す(警告は出ない)。`external_executors`のブロックも使われない。キーを削除するか`orchestra:setup`を再実行すること。
