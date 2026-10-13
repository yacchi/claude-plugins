package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

var (
	now    = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cutoff = now.Add(-24 * time.Hour)
)

func iso(t time.Time) string { return t.Format("2006-01-02T15:04:05Z") }

type line struct {
	ts                                 time.Time
	in, out, cacheWrite, cacheRead, h1 int
	model                              string
	sidechain                          bool
	id                                 string
}

func (l line) String() string {
	if l.model == "" {
		l.model = "claude-opus-5"
	}
	usage := map[string]any{
		"input_tokens": l.in, "cache_creation_input_tokens": l.cacheWrite,
		"cache_read_input_tokens": l.cacheRead, "output_tokens": l.out,
	}
	if l.h1 > 0 {
		usage["cache_creation"] = map[string]any{
			"ephemeral_5m_input_tokens": l.cacheWrite - l.h1, "ephemeral_1h_input_tokens": l.h1,
		}
	}
	message := map[string]any{"model": l.model, "usage": usage}
	if l.id != "" {
		message["id"] = l.id
	}
	b, _ := json.Marshal(map[string]any{
		"timestamp": iso(l.ts), "isSidechain": l.sidechain, "sessionId": "s-1", "message": message,
	})
	return string(b)
}

func aggregate(lines ...string) *claudeAcc {
	acc := newClaudeAcc()
	for _, l := range lines {
		acc.addLine(l, cutoff)
	}
	return acc
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decodeCfg(t *testing.T, text string) *config.Resolved {
	t.Helper()
	v, err := jsonx.Decode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return &config.Resolved{Values: v.(*jsonx.Object)}
}

func TestParseSince(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{{"24h", 24 * time.Hour}, {"30m", 30 * time.Minute}, {"7d", 7 * 24 * time.Hour}, {" 2h ", 2 * time.Hour}} {
		got, ok := ParseSince(c.in, now)
		if !ok || !got.Equal(now.Add(-c.want)) {
			t.Errorf("%q -> %v %v", c.in, got, ok)
		}
	}
	for _, in := range []string{"", "  ", "yesterday", "1w", "h", "-1h", "1.5h", "99999999999d"} {
		if _, ok := ParseSince(in, now); ok {
			t.Errorf("%q parsed", in)
		}
	}
	got, ok := ParseSince("2026-08-01T00:00:00Z", now)
	if !ok || !got.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("iso -> %v %v", got, ok)
	}
}

func TestParseTimestamp(t *testing.T) {
	utc := func(y int, m time.Month, d, h, mi, s, ns int) time.Time {
		return time.Date(y, m, d, h, mi, s, ns, time.UTC)
	}
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"2026-03-01T10:00:00Z", utc(2026, 3, 1, 10, 0, 0, 0)},
		{"2026-03-01T10:00:00z", utc(2026, 3, 1, 10, 0, 0, 0)},
		{"2026-03-01T10:00:00.123Z", utc(2026, 3, 1, 10, 0, 0, 123_000_000)},
		{"2026-03-01T10:00:00.1234567891Z", utc(2026, 3, 1, 10, 0, 0, 123_456_000)},
		{"2026-03-01T10:00:00.5", utc(2026, 3, 1, 10, 0, 0, 500_000_000)},
		{"2026-03-01 10:00:00", utc(2026, 3, 1, 10, 0, 0, 0)},
		{"2026-03-01T10:00:00+09:00", utc(2026, 3, 1, 1, 0, 0, 0)},
		{"2026-03-01T10:00:00-0530", utc(2026, 3, 1, 15, 30, 0, 0)},
		{"2026-03-01T10:00", utc(2026, 3, 1, 10, 0, 0, 0)},
		{"2026-03-01", utc(2026, 3, 1, 0, 0, 0, 0)},
		{"20260301", utc(2026, 3, 1, 0, 0, 0, 0)},
		{"  2026-03-01T10:00:00Z  ", utc(2026, 3, 1, 10, 0, 0, 0)},
	} {
		got, ok := ParseTimestamp(c.in)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%q -> %v %v, want %v", c.in, got, ok, c.want)
		}
	}
	for _, in := range []any{"", "nope", "2026-13-01", "2026-02-30", "2026-03-01T25:00:00Z", "2026-03-01T10:00:00+25:00", 42, nil, true} {
		if _, ok := ParseTimestamp(in); ok {
			t.Errorf("%v parsed", in)
		}
	}
}

func TestWindowFilteringAndTotals(t *testing.T) {
	acc := aggregate(
		line{ts: now.Add(-time.Hour), in: 10, out: 5, cacheWrite: 2, cacheRead: 100}.String(),
		line{ts: now.Add(-72 * time.Hour), in: 999, out: 999}.String(),
	)
	if acc.records != 1 || acc.tokens != (Tokens{12, 5, 100}) {
		t.Errorf("%+v", acc.tokens)
	}
}

func TestSidechainSplitAndModelBreakdown(t *testing.T) {
	acc := aggregate(
		line{ts: now, in: 10, out: 1}.String(),
		line{ts: now, in: 3, out: 2, model: "claude-haiku-5", sidechain: true}.String(),
	)
	if acc.main.InputTokens != 10 || acc.sidechain.InputTokens != 3 || acc.byModel["claude-haiku-5"].OutputTokens != 2 || len(acc.byModel) != 2 {
		t.Errorf("%+v %+v %v", acc.main, acc.sidechain, acc.byModel)
	}
}

func TestOneResponseOnSeveralLinesCountsOnce(t *testing.T) {
	var lines []string
	for i := 0; i < 4; i++ {
		lines = append(lines, line{ts: now, in: 10, out: 5, cacheRead: 100, id: "msg_A"}.String())
	}
	lines = append(lines, line{ts: now, in: 1, out: 2, id: "msg_B"}.String())
	acc := aggregate(lines...)
	if acc.records != 2 || acc.tokens != (Tokens{11, 7, 100}) || acc.byModel["claude-opus-5"].OutputTokens != 7 {
		t.Errorf("%+v records=%d", acc.tokens, acc.records)
	}
}

func TestLaterLineReplacesTheEarlierOne(t *testing.T) {
	acc := aggregate(
		line{ts: now, out: 3, id: "msg_A"}.String(),
		line{ts: now, out: 40, id: "msg_A"}.String(),
	)
	if acc.records != 1 || acc.tokens.OutputTokens != 40 || acc.main.OutputTokens != 40 {
		t.Errorf("%+v records=%d", acc.tokens, acc.records)
	}
}

func TestDedupeSpansTranscriptsSharingAnAccumulator(t *testing.T) {
	acc := newClaudeAcc()
	l := line{ts: now, in: 7, id: "msg_A"}.String()
	acc.addLine(l, cutoff)
	acc.addLine(l, cutoff)
	if acc.tokens.InputTokens != 7 || acc.records != 1 {
		t.Errorf("%+v", acc.tokens)
	}
}

func TestLinesWithoutAMessageIDAreAllCounted(t *testing.T) {
	acc := aggregate(line{ts: now, in: 2}.String(), line{ts: now, in: 2}.String())
	if acc.tokens.InputTokens != 4 {
		t.Errorf("%+v", acc.tokens)
	}
}

func TestCacheWritesSplitByTTLForPricing(t *testing.T) {
	acc := aggregate(
		line{ts: now, in: 3, cacheWrite: 100, h1: 60, cacheRead: 5, out: 2, id: "msg_A"}.String(),
		line{ts: now, cacheWrite: 10, id: "msg_B"}.String(), // only the total: the 5-minute TTL
	)
	want := Detail{FreshInput: 3, CacheWrite5m: 40 + 10, CacheWrite1h: 60, CacheRead: 5, Output: 2}
	if got := *acc.byModelDetail["claude-opus-5"]; got != want {
		t.Errorf("detail = %+v, want %+v", got, want)
	}
	if acc.tokens.InputTokens != 113 {
		t.Errorf("cache writes fold into input: %d", acc.tokens.InputTokens)
	}
}

func TestGarbageAndShapeDriftLinesAreSkipped(t *testing.T) {
	acc := aggregate(
		"", "not json at all", `{"truncated": `, `[1, 2, 3]`,
		`{"message": {"usage": {"input_tokens": 5}}}`,
		fmt.Sprintf(`{"timestamp": %q, "message": "a string"}`, iso(now)),
		fmt.Sprintf(`{"timestamp": %q, "message": {"usage": []}}`, iso(now)),
		`{"timestamp": "nope", "message": {"usage": {"input_tokens": 5}}}`,
		fmt.Sprintf(`{"timestamp": %q, "message": {"model": 42, "usage": {"input_tokens": "7", "output_tokens": 4}}}`, iso(now.Add(-time.Hour))),
		fmt.Sprintf(`{"timestamp": %q, "message": {"usage": {"input_tokens": true, "output_tokens": -3, "cache_read_input_tokens": 2.5}}}`, iso(now)),
	)
	if acc.records != 2 || acc.tokens != (Tokens{0, 4, 0}) || len(acc.byModel) != 0 {
		t.Errorf("%+v records=%d byModel=%v", acc.tokens, acc.records, acc.byModel)
	}
}

func TestCwdScopingUsesTheProjectSlug(t *testing.T) {
	home := t.TempDir()
	cwd := "/Users/x/p"
	if projectSlug(cwd) != "-Users-x-p" {
		t.Fatal(projectSlug(cwd))
	}
	write(t, filepath.Join(home, ".claude", "projects", "-Users-x-p", "a.jsonl"), line{ts: now, in: 11}.String())
	write(t, filepath.Join(home, ".claude", "projects", "-other", "b.jsonl"), line{ts: now, in: 500}.String())
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	o := ReportOptions{Since: cutoff, Now: now, Sources: []string{"claude"}, Home: home, Cwd: cwd}
	scoped := BuildReport(o).Sources.Entries["claude"]
	if scoped.Status != "ok" || scoped.Scope != "cwd" || scoped.Tokens.InputTokens != 11 || scoped.Sessions != 1 {
		t.Errorf("%+v", scoped)
	}
	o.AllProjects = true
	wide := BuildReport(o).Sources.Entries["claude"]
	if wide.Scope != "all-projects" || wide.Tokens.InputTokens != 511 || wide.Sessions != 2 {
		t.Errorf("%+v", wide)
	}
}

func TestMissingTreeIsUnavailableNotZeroOK(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	r := BuildReport(ReportOptions{Since: cutoff, Now: now, Sources: []string{"claude"}, Home: t.TempDir(), Cwd: "/x"})
	e := r.Sources.Entries["claude"]
	if e.Status != "unavailable" || e.Tokens != (Tokens{}) || e.Note == "" {
		t.Errorf("%+v", e)
	}
	if r.Totals != (Tokens{}) {
		t.Error("an unavailable source contributed to the totals")
	}
}

func TestSubagentsAndBothRootsAreCountedOnce(t *testing.T) {
	home, cfg := t.TempDir(), t.TempDir()
	cwd := "/x"
	slug := projectSlug(cwd)
	homeProject := filepath.Join(home, ".claude", "projects", slug)
	cfgProject := filepath.Join(cfg, "projects", slug)
	write(t, filepath.Join(homeProject, "shared.jsonl"), line{ts: now, in: 7}.String())
	write(t, filepath.Join(cfgProject, "config.jsonl"), line{ts: now, in: 11}.String())
	sub := filepath.Join(cfgProject, "session-id", "subagents", "workflows", "wf_x")
	write(t, filepath.Join(sub, "agent-y.jsonl"), line{ts: now, in: 13}.String())
	// Not under a subagents directory: ignored.
	write(t, filepath.Join(cfgProject, "session-id", "other", "z.jsonl"), line{ts: now, in: 1000}.String())
	write(t, filepath.Join(cfgProject, "session-id", "notes.jsonl"), line{ts: now, in: 1000}.String())
	// The config dir pointing at home's own tree is the same root.
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	o := ReportOptions{Since: cutoff, Now: now, Sources: []string{"claude"}, Home: home, Cwd: cwd}
	e := BuildReport(o).Sources.Entries["claude"]
	if e.Tokens.InputTokens != 7+11+13 || e.Sessions != 3 {
		t.Errorf("%+v", e)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	e = BuildReport(o).Sources.Entries["claude"]
	if e.Tokens.InputTokens != 7 || e.Sessions != 1 {
		t.Errorf("the same root counted twice: %+v", e)
	}
}

func TestTelemetryAggregation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.jsonl")
	rec := func(event, executor, ts string, usage string) string {
		return fmt.Sprintf(`{"event": %q, "executor": %q, "ts": %q, "usage": %s}`, event, executor, ts, usage)
	}
	write(t, path,
		rec("dispatch", "pi", iso(now.Add(-time.Hour)), `{"input_tokens": 10, "output_tokens": 20, "cached_input_tokens": 30}`),
		rec("dispatch", "pi", iso(now.Add(-2*time.Hour)), `{"input_tokens": 1, "output_tokens": true, "cached_input_tokens": -4}`),
		rec("dispatch", "pi", iso(now.Add(-72*time.Hour)), `{"input_tokens": 999}`),
		rec("dispatch", "claude", iso(now), `{"input_tokens": 999}`),
		rec("run_summary", "pi", iso(now), `{"input_tokens": 999}`),
		`{"event": "dispatch", "executor": "pi", "ts": "`+iso(now)+`"}`,
		"not json",
	)
	cfg := decodeCfg(t, `{"telemetry": {"enabled": true, "dir": "`+dir+`"}}`)
	e := telemetryUsage(cutoff, cfg, "pi")
	if e.Status != "ok" || e.Records != 2 || e.Tokens != (Tokens{11, 20, 30}) || e.Scope != "global" {
		t.Errorf("%+v", e)
	}

	off := telemetryUsage(cutoff, decodeCfg(t, `{"telemetry": {"enabled": false, "dir": "`+dir+`"}}`), "pi")
	if off.Status != "unavailable" || off.Tokens != (Tokens{}) || !strings.Contains(off.Note, "disabled") {
		t.Errorf("%+v", off)
	}
	missing := telemetryUsage(cutoff, decodeCfg(t, `{"telemetry": {"enabled": true, "dir": "`+filepath.Join(dir, "nope")+`"}}`), "pi")
	if missing.Status != "unavailable" || missing.Note != "no telemetry records file yet" {
		t.Errorf("%+v", missing)
	}
	empty := telemetryUsage(now, cfg, "pi")
	if empty.Status != "empty" || empty.Records != 0 {
		t.Errorf("%+v", empty)
	}
}

// fixture builds a Claude tree, a run ledger and a telemetry log.
func fixture(t *testing.T) (ReportOptions, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-1")
	projects := filepath.Join(home, ".claude", "projects")
	write(t, filepath.Join(projects, "-p", "s1.jsonl"),
		line{ts: now, in: 10, out: 1, id: "a"}.String(),
		line{ts: now, in: 4, out: 2, model: "claude-mystery-9", sidechain: true, id: "b"}.String())
	write(t, filepath.Join(projects, "-p", "s1", "subagents", "agent-1.jsonl"), line{ts: now, in: 5, id: "c"}.String())
	write(t, filepath.Join(projects, "-p", "s1", "subagents", "workflows", "wf_one", "a.jsonl"), line{ts: now.Add(-100 * time.Hour), in: 100, id: "d"}.String())
	write(t, filepath.Join(projects, "-p", "s1", "subagents", "workflows", "wf_one", "b.jsonl"), line{ts: now.Add(-90 * time.Hour), in: 1, id: "e"}.String())
	write(t, filepath.Join(projects, "-q", "s2.jsonl"), line{ts: now, in: 1000, id: "f"}.String())

	led := filepath.Join(home, "runs")
	rec := func(executor string, in, cost int, ts time.Time) string {
		return fmt.Sprintf(`{"ts":%q,"executor":%q,"status":"ok","input_tokens":%d,"output_tokens":%d,"cached_input_tokens":3,"cost_micro_usd":%d}`,
			iso(ts), executor, in, in*2, cost)
	}
	write(t, filepath.Join(led, "sess-1", "001-wf_one.jsonl"), rec("pi", 1, 100, now.Add(-100*time.Hour)), rec("claude", 77, 77, now), "junk")
	write(t, filepath.Join(led, "sess-1", "002-wf_two.jsonl"), rec("pi", 2, 200, now.Add(-50*time.Hour)))
	write(t, filepath.Join(led, "sess-1", "no.run.jsonl"), rec("pi", 4, 400, now.Add(-10*time.Hour)))
	write(t, filepath.Join(led, "sess-2", "001-wf_one.jsonl"), rec("pi", 8, 800, now.Add(-20*time.Hour)))
	write(t, filepath.Join(led, "flat.jsonl"), rec("pi", 1000, 1000, now))

	tel := filepath.Join(home, "tel")
	write(t, filepath.Join(tel, "records.jsonl"),
		fmt.Sprintf(`{"event":"dispatch","executor":"pi","ts":%q,"usage":{"input_tokens":6,"output_tokens":5}}`, iso(now.Add(-time.Hour))))
	cfg := decodeCfg(t, `{"telemetry": {"enabled": true, "dir": "`+tel+`"}, "ledger": {"dir": "`+led+`"}}`)
	return ReportOptions{Since: cutoff, Now: now, Sources: Sources, AllProjects: true, Cfg: cfg, Home: home}, home
}

func TestReportShapeAndRequestedSourcesOnly(t *testing.T) {
	o, _ := fixture(t)
	r := BuildReport(o)
	if r.Scope.Kind != "window" || r.Since == "" || r.Now == "" || r.Scope.Since != r.Since {
		t.Errorf("%+v", r.Scope)
	}
	// 10+4+5+1000 inside the window; the 90h/100h workflow lines are outside.
	c := r.Sources.Entries["claude"]
	if c.Status != "ok" || c.Tokens.InputTokens != 1019 || c.Sidechain.InputTokens != 4 || c.Sessions != 3 {
		t.Errorf("%+v", c)
	}
	if p := r.Sources.Entries["pi"]; p.Status != "ok" || p.Tokens.InputTokens != 6 {
		t.Errorf("%+v", p)
	}
	if r.Totals.InputTokens != 1025 {
		t.Errorf("totals %+v", r.Totals)
	}
	if !reflect.DeepEqual(r.Cost.UnpricedSources, []string{"claude:claude-mystery-9"}) || !reflect.DeepEqual(r.Cost.PricedSources, []string{"claude", "pi"}) {
		t.Errorf("%+v", r.Cost)
	}

	o.Sources = []string{"pi"}
	only := BuildReport(o)
	if !reflect.DeepEqual(only.Sources.Names, []string{"pi"}) || only.Sources.Entries["claude"] != nil {
		t.Errorf("%v", only.Sources.Names)
	}
}

func TestTotalsOnlySumOKSources(t *testing.T) {
	list := SourceList{Names: []string{"claude", "pi"}, Entries: map[string]*Source{
		"claude": {Status: "ok", Tokens: Tokens{1, 2, 3}},
		"pi":     {Status: "unavailable", Tokens: Tokens{100, 100, 100}},
	}}
	if got := sumTotals(list); got != (Tokens{1, 2, 3}) {
		t.Errorf("%+v", got)
	}
}

func TestEveryEmittedNumberIsANonNegativeInt(t *testing.T) {
	o, _ := fixture(t)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		case json.Number:
			if strings.ContainsAny(string(x), ".eE-") {
				t.Errorf("%s = %s is not a non-negative int", path, x)
			}
		}
	}
	for _, r := range []Report{BuildReport(o), func() Report { o.RunIDs = []string{"wf_one"}; return BuildReport(o) }()} {
		b, err := jsonx.MarshalIndent(r)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		walk("$", v)
	}
}

func TestTextOutputLeaksNoPathsOrIDs(t *testing.T) {
	o, home := fixture(t)
	for _, set := range []func(*ReportOptions){
		func(*ReportOptions) {},
		func(o *ReportOptions) { o.RunIDs = []string{"wf_one"} },
		func(o *ReportOptions) { o.SessionIDs = []string{"sess-1", "s1"} },
	} {
		oo := o
		set(&oo)
		text := FormatText(BuildReport(oo))
		for _, leak := range []string{home, "/projects/", "-p/", "s2.jsonl", "prompt"} {
			if strings.Contains(text, leak) {
				t.Errorf("text leaks %q:\n%s", leak, text)
			}
		}
	}
}

func TestReportWritesNothing(t *testing.T) {
	o, home := fixture(t)
	snapshot := func() map[string]time.Time {
		out := map[string]time.Time{}
		_ = filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				out[p] = fi.ModTime()
			}
			return nil
		})
		return out
	}
	before := snapshot()
	BuildReport(o)
	ListRuns(RunsOptions{Cfg: o.Cfg, Home: home, AllProjects: true})
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Error("the report changed the file tree")
	}
}

func TestRunScopeIsRecursiveAndIgnoresTime(t *testing.T) {
	o, _ := fixture(t)
	o.RunIDs = []string{"wf_one"}
	r := BuildReport(o)
	if r.Scope.Kind != "run" || r.Since != "" || r.Now != "" {
		t.Errorf("%+v", r.Scope)
	}
	c := r.Sources.Entries["claude"]
	if c.Scope != "run" || c.Tokens.InputTokens != 101 || c.Sessions != 2 {
		t.Errorf("%+v", c)
	}
	// wf_one is in two sessions of the ledger: both files, pi records only.
	p := r.Sources.Entries["pi"]
	if p.Scope != "run" || p.Records != 2 || p.Tokens.InputTokens != 9 || p.CostMicroUSD != 900 || len(p.Runs) != 0 {
		t.Errorf("%+v", p)
	}
}

func TestSessionScopeBreaksDownRuns(t *testing.T) {
	o, _ := fixture(t)
	o.SessionIDs = []string{"sess-1", "s1"}
	r := BuildReport(o)
	c := r.Sources.Entries["claude"]
	// s1.jsonl, its subagent transcript and both workflow transcripts, with
	// no time cutoff.
	if c.Scope != "session" || c.Tokens.InputTokens != 10+4+5+100+1 || c.Sessions != 4 {
		t.Errorf("%+v", c)
	}
	p := r.Sources.Entries["pi"]
	want := []RunCount{{"wf_one", 1}, {"wf_two", 1}, {"no-run", 1}}
	if !reflect.DeepEqual(p.Runs, want) || p.Records != 3 || p.CostMicroUSD != 700 {
		t.Errorf("%+v", p)
	}
	text := FormatText(r)
	if !strings.Contains(text, "runs: no-run=1 wf_one=1 wf_two=1") || !strings.Contains(text, "scope: session sess-1,s1") {
		t.Errorf("text:\n%s", text)
	}
}

func TestScopeWithoutMatchesIsNotAttributable(t *testing.T) {
	o, _ := fixture(t)
	o.SessionIDs = []string{"nope"}
	r := BuildReport(o)
	for _, n := range []string{"claude", "pi"} {
		if e := r.Sources.Entries[n]; !e.NotAttributable {
			t.Errorf("%s: %+v", n, e)
		}
	}
	b, _ := jsonx.Marshal(r.Sources)
	if !strings.Contains(string(b), `"claude": {"attributable": false, "reason": "no matching transcript data"}`) {
		t.Errorf("%s", b)
	}
	if text := FormatText(r); !strings.Contains(text, "pi: not attributable (no matching run ledger data)") {
		t.Errorf("text:\n%s", text)
	}
}

func TestUnknownSourceInScopeIsNotAttributable(t *testing.T) {
	// A source with no scoped collector says so rather than reporting zeros.
	saved := Sources
	t.Cleanup(func() { Sources = saved })
	Sources = []string{"claude", "pi", "other"}
	o, _ := fixture(t)
	o.Sources = []string{"other"}
	o.RunIDs = []string{"x"}
	if e := BuildReport(o).Sources.Entries["other"]; e == nil || !e.NotAttributable || e.Reason != "session scope is not attributable" {
		t.Errorf("%+v", e)
	}
}

func TestListRunsSortedByLastActivity(t *testing.T) {
	o, home := fixture(t)
	runs := ListRuns(RunsOptions{Cfg: o.Cfg, Home: home, AllProjects: true})
	// wf_one last had a ledger line now (the claude-executor record counts as
	// activity), wf_two 50 hours ago; the flat ledger file is not read.
	if len(runs) != 2 || runs[0].RunID != "wf_two" || runs[1].RunID != "wf_one" {
		t.Fatalf("runs = %+v", runs)
	}
	one := runs[1]
	if one.Files != 2 || one.Ordinal == nil || *one.Ordinal != 1 || one.FirstTS != "2026-08-15T08:00:00+00:00" || one.LastTS != "2026-08-19T12:00:00+00:00" {
		t.Errorf("wf_one = %+v", one)
	}
	if runs[0].Files != 0 || runs[0].Ordinal == nil || *runs[0].Ordinal != 2 {
		t.Errorf("wf_two = %+v", runs[0])
	}
	text := FormatRunsText(runs)
	if !strings.HasPrefix(text, "002 wf_two 0 ") || !strings.Contains(text, "\n001 wf_one 2 2026-08-15T08:00:00+00:00 2026-08-19T12:00:00+00:00\n") {
		t.Errorf("text:\n%s", text)
	}
	if got := ListRuns(RunsOptions{Cfg: o.Cfg, Home: home, AllProjects: true, Sources: []string{"pi"}}); len(got) != 2 {
		t.Errorf("pi only: %+v", got)
	}
	if got := ListRuns(RunsOptions{Cfg: o.Cfg, Home: home, AllProjects: true, Sources: []string{"claude"}}); len(got) != 1 || got[0].Ordinal != nil {
		t.Errorf("claude only: %+v", got)
	}
}

// A full run id matches that run in every session's ledger directory (the
// CLI resolves ordinals to run ids beforehand, in the current session only).
func TestRunIDMatchesEverySession(t *testing.T) {
	o, _ := fixture(t)
	o.RunIDs = []string{"wf_one"}
	if p := BuildReport(o).Sources.Entries["pi"]; p.Records != 2 {
		t.Errorf("%+v", p)
	}
	o.RunIDs = []string{"sess-1"} // a session id is never a run id
	if p := BuildReport(o).Sources.Entries["pi"]; !p.NotAttributable {
		t.Errorf("%+v", p)
	}
}

func TestSnapshot24hShape(t *testing.T) {
	// The snapshot uses the real clock, so build the fixture around it.
	savedNow, savedCutoff := now, cutoff
	t.Cleanup(func() { now, cutoff = savedNow, savedCutoff })
	now = time.Now().UTC().Truncate(time.Second)
	cutoff = now.Add(-24 * time.Hour)
	o, _ := fixture(t)
	t.Setenv("HOME", o.Home)
	snap, ok := Snapshot24h(o.Cfg).(*jsonx.Object)
	if !ok {
		t.Fatalf("%T", Snapshot24h(o.Cfg))
	}
	b, _ := jsonx.Marshal(snap)
	got := string(b)
	for _, want := range []string{`"claude/claude-opus-5": {"input_tokens": 1015`, `"claude/claude-mystery-9"`, `"pi/unknown": {"input_tokens": 6`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}
