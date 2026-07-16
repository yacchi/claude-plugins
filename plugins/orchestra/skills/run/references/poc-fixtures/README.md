# poc-fixtures: 再試験用フィクスチャ一式

`../poc-findings.md`に記録した6ラウンドのPoCを、別のモデル(Codex/Copilotの新モデル、Claudeの新バージョン、あるいは全く別のプロバイダ)で再実行し、横並び比較するためのフィクスチャ集。各ラウンドのタスク仕様書・独立検証ハーネス・(該当する場合は)正解実装とバグ入り実装をそのまま保存してある。

## 検証方針(全ラウンド共通)

1. 各ラウンドのタスクディレクトリを候補ごとにコピーし、独立したディレクトリで実行する(候補同士のファイルが混ざらないように)。
2. 候補への実行コマンドは、タスク仕様書(`TASK*.md`または`INSTRUCTIONS.md`)を読んで指示通りに実装させるだけ。プロンプト自体に評価基準やトラップの種明かしを含めないこと(candidateが「何を見られているか」を知ると意味がなくなる)。
3. 完了後、そのラウンドの検証スクリプト(`test*.js`/`verify-*.mjs`/`test_analysis_verify.py`)を**候補の出力に対して外側から**実行する。候補の自己申告(「全部正しく実装しました」等)は一切信用しない。
4. 新しいモデルを追加する前に、必ずまず`reference/`(正解実装)に対して検証スクリプトを実行し、全項目通過することを確認する。バグ入りラウンド(4・5)では`buggy/`に対しても実行し、意図した項目だけが失敗することを確認する。フィクスチャ自体が壊れていないかの安全確認。
5. 結果(時間・トークン数・コスト・正誤)は`poc-findings.md`の該当ラウンドの表に追記する形で記録する。新ラウンドを追加した場合は`poc-findings.md`に節を追加する。

## 実行コマンドのテンプレート

### Codex CLI

```bash
cd <candidate-dir>
codex exec -m <MODEL_ID> -c model_reasoning_effort=<EFFORT> \
  --skip-git-repo-check -s workspace-write --json \
  "Read <TASK_FILE> in this directory and implement exactly what it describes." \
  > run.log 2>&1
```

`<MODEL_ID>`は`gpt-5.6-luna`/`gpt-5.6-terra`/`gpt-5.6-sol`など。`--json`をつけると`run.log`から`turn.completed`イベントの`usage.input_tokens`/`usage.cached_input_tokens`/`usage.output_tokens`が取れ、`SKILL.md` §9.3の単価表と掛け合わせて実コストを計算できる。

### Copilot CLI

```bash
cd <candidate-dir>
copilot -p "Read <TASK_FILE> in this directory and implement exactly what it describes." \
  --model <MODEL_ID> --effort <EFFORT> --allow-all-tools --add-dir "$(pwd)" \
  --output-format json > run.jsonl 2> run.stderr
```

コストは`run.jsonl`の`outputTokens`合計のみ取得可能(入力トークンは非公開、下限値としてしか算出できない — `poc-findings.md`の各ラウンドの注意点を参照)。`grep -o '"outputTokens":[0-9]*' run.jsonl | awk -F: '{s+=$2} END{print s}'`で合計を取れる。

### Claude(Agent ツール経由)

```
Agent({
  description: "<説明>",
  model: "sonnet" | "haiku" | "opus",
  subagent_type: "claude",
  prompt: "Working directory: <candidate-dir>\n\nRead <TASK_FILE> in that exact directory and follow its instructions exactly. Only edit files inside that directory. Do not create new files beyond what's asked. Do not run git. When done, reply with only: DONE (plus, optionally, 1-2 lines summarizing what changed — no code/diffs)."
})
```

コストは完了イベントの`usage.subagent_tokens`(入出力の内訳なし)。`SKILL.md` §9.3のClaude単価表を使って、全量入力/全量出力の両極から幅(bound)を出す。

## ラウンドごとの内容

| ディレクトリ | タスク | 検証コマンド |
|---|---|---|
| `round1-formatbytes/` | `formatBytes(bytes)` の実装(境界値の繰り上がりトラップ) | `node test.js <candidate-dir>` |
| `round2-evalexpr/` | `evaluate(expr)` の実装(演算子優先順位・結合則・エラー文字列) | `node test2.js <candidate-dir>` |
| `round3-parsejson/` | `parseJson(text)` の実装(RFC 8259準拠パーサー) | `node test3.js <candidate-dir>` |
| `round4-ratelimiter/` | `buggy/`のバグ(refill計算の秒未満切り捨て)を修正させる | `buggy/`の中身を候補ディレクトリにコピーしてから修正させ、`node test4.js <candidate-dir>` |
| `round5-cache/` | `buggy/`のバグ(過剰なクエリ剥ぎ取り)を、誤った診断(TTL問題だと主張)込みの`TASK5.md`から見つけさせる | `buggy/`をコピーしてから、`node test5.js <candidate-dir>` |
| `round6-webapp/` | Go/Python/TypeScriptの3言語フルスタックアプリを`INSTRUCTIONS.md`から実装させる | 下記参照 |

### Round 4・5(バグ修正ラウンド)の使い方

```bash
mkdir -p /tmp/candidate-r4 && cp round4-ratelimiter/buggy/*.js round4-ratelimiter/TASK4.md /tmp/candidate-r4/
# candidate に /tmp/candidate-r4 で TASK4.md を読んで直させる
node round4-ratelimiter/test4.js /tmp/candidate-r4
```

Round 5も同様(`round5-cache/buggy/*.js`と`TASK5.md`を使う)。

### Round 6(フルスタックアプリ)の使い方

```bash
mkdir -p /tmp/candidate-r6/backend /tmp/candidate-r6/analysis /tmp/candidate-r6/frontend
cp round6-webapp/scaffold/backend/go.mod /tmp/candidate-r6/backend/
cp round6-webapp/scaffold/frontend/tsconfig.json /tmp/candidate-r6/frontend/
cp round6-webapp/INSTRUCTIONS.md /tmp/candidate-r6/
# candidate に /tmp/candidate-r6 で INSTRUCTIONS.md を読んで実装させる

# 検証は3本立て(Goサーバーを実際に起動してHTTPで叩く/pytest/tscの型チェック+ユニットテスト)
node round6-webapp/verify-backend.mjs /tmp/candidate-r6/backend
cp round6-webapp/test_analysis_verify.py /tmp/candidate-r6/analysis/
(cd /tmp/candidate-r6/analysis && uvx --from pytest pytest test_analysis_verify.py -v)
node round6-webapp/verify-frontend.mjs /tmp/candidate-r6/frontend
```

`scaffold/`(`go.mod`・`tsconfig.json`)は毎回同じものを候補に渡すこと — ツールバージョン起因の差異(例: TypeScript 7での`moduleResolution`廃止)がロジック品質の評価に混ざらないようにするため。`reference/`はこの仕様書に対する正解実装で、検証ハーネス自体が壊れていないかの確認や、期待値を再導出したい場合に使う。

## 新しいラウンドを追加する場合

1. まず正解実装を書き、検証スクリプトがそれに対して全通過することを確認する。
2. トラップ(意図的な難所)を仕込む場合は、バグ入り版に対して検証スクリプトが「狙った項目だけ」失敗することを確認してから使う。
3. `round7-<名前>/`のような形でこのディレクトリに追加し、`../poc-findings.md`に新しい節を追記し、この`README.md`の表にも1行追加する。
