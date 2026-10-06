# テスト膨張の構造的原因と対策プラン

- 日付: 2026-10-06
- 対象バージョン: orchestra 0.41.0
- 観測元: ai2-lcs-cdk issue #901 の実装 (3 タスク並列、run `wf_62a50c22-a57`)
- 分類: 構造的問題 (プロンプト設計に起因し、個別の worker/reviewer の判断ミスではない)

## 観測された事象

| 項目 | 値 |
|---|---|
| 変更ファイル | 15 (実装 5 / テスト 10) |
| 追加行 | 1,391 (削除 94) |
| うち実装 | 約 340 行 |
| うちテスト | 約 1,050 行 |
| **後からレビューで削除できた行** | **134 行 (テストの約 13%)** |
| 削除後もテストは全て通過 | 通過 (lcs-edge-qs 553、TS SDK 526、Go 133) |

削除されたのは次の種類で、いずれも「仕様の証明」であって「実装不備の検知」ではなかった。

- 定数式そのものの実測 (既定バックオフが `500 + floor(random*100)` であることの検証)
- 変更していない既存挙動の再テスト (リトライ枯渇の回数とエラーメッセージ、`buildFn` が nil を返す場合)
- 契約で「挙動不変」と指定した箇所に、不変であることを証明するために追加されたテスト (53 行)
- 経過時間アサーション (reviewer 自身が flaky と指摘)

ユーザーの評価軸: **「多すぎるテストは保守性・修正漏れに繋がる。重要なのは予想される実装の不備等を
テストで検知できること。仕様の正しさを証明することでは無い」**

## 構造的原因

### 原因 1: ケース表が「下限」として解釈される

`skills/run/SKILL.md` §6 の規則 1 は「Concretize the spec to literally-implementable level. Not "handle
boundary values correctly" — enumerate input/output examples」と求める。これは曖昧さの除去としては正しいが、
instructor が実際に書くのは**必須ケースの表**になり、worker はそれを満たした上で「漏れがあると落とされる」
と考えて追加する。表は上限ではなく下限として機能する。

§6 の密度に関する注記も「**Compress the scaffolding, never the contract:** enumerated I/O examples, boundary
values, and the verification command are compression-exempt」として、ケース列挙を圧縮対象外にしている。
これは契約の**明確さ**には効くが、テストの**量**に対する上限を一切与えていない。

### 原因 2: reviewer に「3 本以上追加せよ」という固定の下限がある

- `agents/orchestra-review.md:23` — 「**Write and run at least 3 adversarial tests of your own**」
- 同ファイル `:3` (frontmatter description) — 「writes at least 3 additional adversarial edge-case tests」
- `skills/run/references/authoring.md:29` — 「The pre-authored tests are exactly `orchestra-review`'s
  "≥3 additional adversarial tests"」

これは「タスク数 × ゲート数 × 3」の下限を機械的に生む。今回は 3 タスクなので最低 9 本が、必要性と無関係に
追加される。しかも worker 側のケース表 (原因 1) と重複しない範囲を探すよう指示されているため、**縁の薄い
ケースへ押し出される**圧力がかかる。

### 原因 3: 「検証」と「カバレッジ追加」が同一視されている

reviewer の役割は「worker の主張を信用せず自分で確かめる」ことだが、その手段として
「テストを**書いて残す**」が指定されている。確かめるだけなら probe を書いて走らせ、捨ててよい。
実際、今回 T1 と T3 の reviewer は自作テストを削除し、T2 の reviewer は 71 行を残した。**同じ指示から
異なる結果**が出ており、成果物かどうかの規定が無いことの表れである。

§7 の「Review costs ~2x the implementation's output tokens and is worth it」は**欠陥検出**の価値としては
正しいが、その 2 倍のコストが**永続的なテスト資産**として積まれる前提になっていない。

### 原因 4: 「何をテストしないか」の指針が存在しない

SKILL.md と references 全体に、テストを**書かない**判断基準が無い。契約密度 (§6)、レビューの経済性 (§7)、
ゲート規律 (§11) はいずれも「不足」を防ぐ方向のみで、「過剰」を防ぐ方向の記述が無い。

### 原因 5: 「挙動不変」の指定が、不変の証明を誘発する

今回 `Handler.ts` について契約で「**Preserve the existing behavior exactly** — this is a
readability/single-source change, not a behavior change」と指定した。worker はこれに応えて
`AsrProducerHandler.test.ts` に 53 行を追加した。**不変を要求すると、不変の証明が成果物として返ってくる。**
不変の保証は既存スイートが担うべきで、新規テストは不要である。

## 対策プラン

### P1. reviewer の adversarial テストを「成果物」から「probe」に変える (最優先)

`agents/orchestra-review.md`

- `:23` の「Write and run at least 3 adversarial tests of your own」を、**仮説の列挙**を下限とする形に
  変更する。下限はテスト (永続・高コスト) ではなく仮説 (使い捨て・低コスト) に置く。

  改定案:
  > **List the implementation mistakes most likely for this contract** (at least 3), then probe each one.
  > A probe is a throwaway test: run it, record the result, and **delete it**. Keep a probe as a permanent
  > test ONLY when (a) it actually found a defect, or (b) the mistake it targets is both likely and silent
  > (would not surface as a failure elsewhere). Report every kept test in the verdict with its
  > justification. Probing more is free; keeping more is not.

- frontmatter `:3` の description も同様に書き換える (「writes at least 3 additional adversarial edge-case
  tests」→ probe と仮説の表現に)。
- `references/authoring.md:29` の「The pre-authored tests are exactly orchestra-review's "≥3 additional
  adversarial tests"」も追随させる。

### P2. VERDICT_SCHEMA に `tests_kept` を追加する

`skills/run/SKILL.md` §5 のテンプレート。

```javascript
tests_kept: {
  type: 'array',
  items: {
    type: 'object',
    required: ['file', 'case', 'mistake_detected'],
    properties: {
      file: { type: 'string' },
      case: { type: 'string' },
      mistake_detected: { type: 'string' },  // どの実装ミスを検知するか
      found_defect: { type: 'boolean' },     // 実際に欠陥を見つけたか
    },
  },
},
tests_loc_added: { type: 'integer' },
impl_loc_changed: { type: 'integer' },
```

効果: 膨張が verdict の時点で instructor に**可視化**される。今回は 134 行が landed した後に人の指摘で
初めて見つかった。`tests_loc_added` / `impl_loc_changed` が返っていれば、比率が 3 倍を超えた段階で
instructor が判断できた。

### P3. §6 にテスト経済性の節を追加する

`skills/run/SKILL.md` §6 に、規則 8 として追加。

> 8. **Name the implementation mistakes to detect, not a case table to satisfy.** A case table reads as a
>    floor: the worker satisfies it and then adds more. Instead state what is likely to go wrong —
>    "`current.version || -1` silently maps 0 to -1", "guregu's `Update.If` ANDs successive calls, so a
>    three-way OR must be one `If`". A test that detects an anticipated mistake earns its maintenance cost;
>    a test that proves the specification does not.
>
>    **Do NOT require tests for:** a constant expression's exact value (a jitter formula, a default
>    timeout); behavior the change does not touch; the proposition "this behavior is unchanged" (the
>    existing suite is that guard); elapsed wall-clock time.
>
>    **When the contract says a behavior must not change, say explicitly: "do not add tests to prove it."**
>    Otherwise the worker returns the proof as a deliverable.

### P4. reviewer に構造固定の判定基準を与える

`agents/orchestra-review.md` の rejection rules に 1 行追加。

> A test that would still pass if the implementation were replaced by a **different correct
> implementation** guards behavior. A test that would fail is pinning structure — report it under
> `optional_hardening`, never require it.

これは機械的に適用できる判定で、「定数式の実測」と「既存挙動の再テスト」の両方を同じ基準で落とせる。

### P5. §7 のレビュー経済性の記述を補う

`skills/run/SKILL.md` §7 の「Review costs ~2x the implementation's output tokens and is worth it」に続けて:

> That 2x buys **defect detection**, not permanent test assets. The PoC's reviewer found a real boundary bug
> — the value was the finding, not the file. Keep the test when it guards the found defect; delete the
> probes that found nothing.

## 検証方法

次回の orchestrated run で以下を確認する。

- verdict に `tests_kept` が返り、各エントリに「検知する実装ミス」が書かれている
- `tests_loc_added / impl_loc_changed` が 3 を超えない (超えた場合は instructor が判断する契機になる)
- reviewer が probe を削除し、残したものだけを報告している
- 「挙動不変」と指定した箇所に新規テストが追加されていない

## 未検討

- 比率 3 の妥当性。今回は約 3.1 倍 (1,050 / 340) で、13% が削除可能だった。母数が 1 件なので目安にすぎない
- `orchestra-delegate` 経由の fallback 経路 (§8) にも同じ下限が効いているかは未確認
- `programme.md` のプランニングワークフロー側に同種の下限があるかは未確認
