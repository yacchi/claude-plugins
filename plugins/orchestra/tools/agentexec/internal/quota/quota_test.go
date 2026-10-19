package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

const secret = "sk-ant-oat01-SECRET-TOKEN-DO-NOT-LEAK"

var now = time.Unix(1_800_000_000, 0)

// isolate points every location the package reads at temp dirs and makes
// any live lookup fail the test; callers then install the fakes they want.
func isolate(t *testing.T) (*config.Resolved, string) {
	t.Helper()
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv(RoutingEnv, "")
	t.Setenv(SnapshotEnv, "")
	saved := map[string]Fetcher{}
	for g, f := range fetchers {
		saved[g] = f
		fetchers[g] = func(context.Context, *config.Resolved, time.Time) Entry {
			t.Fatalf("live %s lookup in a test that must not reach it", g)
			return Entry{}
		}
	}
	oldKeychain, oldOS, oldClaude, oldCodex := keychainRead, goos, claudeUsageURL, codexUsageURL
	keychainRead = func(context.Context, string) ([]byte, error) {
		t.Fatal("keychain read in a test that must not reach it")
		return nil, nil
	}
	claudeUsageURL, codexUsageURL = "http://127.0.0.1:1/unreachable", "http://127.0.0.1:1/unreachable"
	t.Cleanup(func() {
		for g, f := range saved {
			fetchers[g] = f
		}
		keychainRead, goos, claudeUsageURL, codexUsageURL = oldKeychain, oldOS, oldClaude, oldCodex
	})
	values := config.Defaults()
	values["usage_routing"] = map[string]any{"enabled": true, "cache_seconds": int64(120)}
	return &config.Resolved{Values: values}, home
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledByEnvTouchesNothing(t *testing.T) {
	cfg, _ := isolate(t)
	t.Setenv(RoutingEnv, "0")
	t.Setenv(SnapshotEnv, filepath.Join(t.TempDir(), "snap.json"))
	if got := Snapshot(cfg, Groups, now); got != nil {
		t.Fatalf("disabled snapshot %v", got)
	}
	if _, err := os.Stat(CachePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache written while disabled: %v", err)
	}
}

func TestFixtureReplacesLiveAndCacheAndGarbledIsNoData(t *testing.T) {
	cfg, _ := isolate(t)
	snap := filepath.Join(t.TempDir(), "snap.json")
	writeFile(t, snap, `{"claude": {"five_hour": {"used_percent": 85, "resets_at": null}},
		"openai-codex": {"five_hour": {"used_percent": 5}}, "other": {}}`)
	t.Setenv(SnapshotEnv, snap)
	got := Snapshot(cfg, []string{"claude", "openai-codex", "github-copilot"}, now)
	if len(got) != 2 || got["claude"].FiveHour.UsedPercent != 85 || got["claude"].Source != SourceFixture ||
		got["openai-codex"].FiveHour.UsedPercent != 5 {
		t.Fatalf("fixture snapshot %+v", got)
	}
	if _, err := os.Stat(CachePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a fixture must not write the cache")
	}
	for _, junk := range []string{"{not json", `[1,2]`, `{"claude": "x", "openai-codex": {"five_hour": "y"}}`} {
		writeFile(t, snap, junk)
		got = Snapshot(cfg, Groups, now)
		for g, e := range got {
			if e.FiveHour != nil || e.SevenDay != nil {
				t.Errorf("garbled %q gave %s windows %+v", junk, g, e)
			}
		}
	}
}

func TestLiveLookupOncePerTTLIncludingFailures(t *testing.T) {
	cfg, _ := isolate(t)
	var claudeCalls, codexCalls atomic.Int32
	fetchers[GroupClaude] = func(context.Context, *config.Resolved, time.Time) Entry {
		claudeCalls.Add(1)
		return Entry{FiveHour: &Window{UsedPercent: 42}}
	}
	fetchers[GroupCodex] = func(context.Context, *config.Resolved, time.Time) Entry {
		codexCalls.Add(1)
		return Entry{Error: ErrNetwork}
	}
	first := Snapshot(cfg, Groups, now)
	if first["claude"].Source != SourceAPI || first["openai-codex"].Error != ErrNetwork {
		t.Fatalf("first %+v", first)
	}
	second := Snapshot(cfg, Groups, now.Add(119*time.Second))
	if claudeCalls.Load() != 1 || codexCalls.Load() != 1 {
		t.Fatalf("within the TTL: claude %d codex %d calls", claudeCalls.Load(), codexCalls.Load())
	}
	if second["claude"].Source != SourceCache || second["claude"].FiveHour.UsedPercent != 42 ||
		second["openai-codex"].Error != ErrNetwork {
		t.Fatalf("cached %+v", second)
	}
	Snapshot(cfg, []string{"claude"}, now.Add(121*time.Second))
	if claudeCalls.Load() != 2 || codexCalls.Load() != 1 {
		t.Fatalf("after the TTL only the requested group is fetched: claude %d codex %d",
			claudeCalls.Load(), codexCalls.Load())
	}
}

func TestCacheLivesInStateDirWithMode0600(t *testing.T) {
	cfg, home := isolate(t)
	fetchers[GroupClaude] = func(context.Context, *config.Resolved, time.Time) Entry {
		return Entry{FiveHour: &Window{UsedPercent: 1}}
	}
	Snapshot(cfg, []string{"claude"}, now)
	want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "claude-orchestra", "usage-limits.json")
	fi, err := os.Stat(want)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache %s: %v %v", want, fi, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "orchestra")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the cache must not live in the synced ~/.claude/orchestra")
	}
}

func TestGarbledCacheIsRefetchedNotFatal(t *testing.T) {
	cfg, _ := isolate(t)
	var calls atomic.Int32
	fetchers[GroupClaude] = func(context.Context, *config.Resolved, time.Time) Entry {
		calls.Add(1)
		return Entry{FiveHour: &Window{UsedPercent: 3}}
	}
	for _, junk := range []string{"\x00garbage", `{"claude": [1]}`, `{"claude": {"fetched_at": "soon"}}`} {
		writeFile(t, CachePath(), junk)
		if got := Snapshot(cfg, []string{"claude"}, now); got["claude"].Source != SourceAPI {
			t.Errorf("cache %q: %+v", junk, got)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("calls %d", calls.Load())
	}
}

// usageServer answers the usage endpoint and records the headers seen.
func usageServer(t *testing.T, status int, body string) (*httptest.Server, *http.Header) {
	t.Helper()
	seen := &http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func claudeCredJSON(expiresAt time.Time) string {
	return `{"claudeAiOauth":{"accessToken":"` + secret + `","refreshToken":"r-` + secret +
		`","expiresAt":` + jsonInt(expiresAt.UnixMilli()) + `}}`
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func useLiveFetchers() {
	fetchers[GroupClaude] = fetchClaude
	fetchers[GroupCodex] = fetchCodex
}

func TestTokenLeakageNeverInEntryCacheOrError(t *testing.T) {
	cfg, _ := isolate(t)
	useLiveFetchers()
	goos = "darwin"
	keychainRead = func(context.Context, string) ([]byte, error) {
		return []byte(claudeCredJSON(now.Add(time.Hour))), nil
	}
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{500, `{"error": "bad token ` + secret + `"}`, "http-500"},
		{200, `not json ` + secret, ErrParse},
		{0, "", ErrNetwork},
	} {
		if tc.status == 0 {
			claudeUsageURL = "http://127.0.0.1:1/usage?x=" // nothing listens
		} else {
			srv, seen := usageServer(t, tc.status, tc.body)
			claudeUsageURL = srv.URL
			defer func() {
				if seen.Get("Authorization") != "Bearer "+secret || seen.Get("anthropic-beta") != "oauth-2025-04-20" {
					t.Errorf("headers %v", *seen)
				}
			}()
		}
		_ = os.Remove(CachePath())
		got := Snapshot(cfg, []string{"claude"}, now)["claude"]
		if got.Error != tc.want {
			t.Errorf("status %d: error %q, want %q", tc.status, got.Error, tc.want)
		}
		b, _ := json.Marshal(got.Object())
		cache, _ := os.ReadFile(CachePath())
		if strings.Contains(string(b), "SECRET") || strings.Contains(string(cache), "SECRET") {
			t.Fatalf("token leaked: entry %s cache %s", b, cache)
		}
	}
}

func TestExpiredOrMissingTokenSkipsTheRequest(t *testing.T) {
	cfg, home := isolate(t)
	useLiveFetchers()
	srv, seen := usageServer(t, 200, `{}`)
	claudeUsageURL, codexUsageURL = srv.URL, srv.URL
	goos = "linux"
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), claudeCredJSON(now.Add(-time.Minute)))
	got := Snapshot(cfg, Groups, now)
	if got["claude"].Error != ErrExpired || got["openai-codex"].Error != ErrNoToken {
		t.Fatalf("snapshot %+v", got)
	}
	if len(*seen) != 0 {
		t.Fatal("a request was sent without a usable token")
	}
}

func TestKeychainServiceFollowsClaudeConfigDir(t *testing.T) {
	if got := keychainService(""); got != "Claude Code-credentials" {
		t.Fatalf("unset: %q", got)
	}
	if got := keychainService("/Users/fujie/.config/ccprofile/profiles/max"); got != "Claude Code-credentials-1614b2a5" {
		t.Fatalf("profile dir: %q", got)
	}
	cfg, _ := isolate(t)
	useLiveFetchers()
	goos = "darwin"
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/fujie/.config/ccprofile/profiles/max")
	var asked string
	keychainRead = func(_ context.Context, service string) ([]byte, error) {
		asked = service
		return nil, errors.New("not found")
	}
	if got := Snapshot(cfg, []string{"claude"}, now)["claude"]; got.Error != ErrNoToken {
		t.Fatalf("entry %+v", got)
	}
	if asked != "Claude Code-credentials-1614b2a5" {
		t.Fatalf("asked the keychain for %q", asked)
	}
}

func TestClaudeUtilizationIsAlreadyPercent(t *testing.T) {
	cfg, home := isolate(t)
	useLiveFetchers()
	goos = "linux"
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), claudeCredJSON(now.Add(time.Hour)))
	srv, _ := usageServer(t, 200, `{"five_hour":{"utilization":83.0,"resets_at":"2026-10-09T12:19:59.594340+00:00"},
		"seven_day":{"utilization":0.5,"resets_at":null},"seven_day_opus":null}`)
	claudeUsageURL = srv.URL
	got := Snapshot(cfg, []string{"claude"}, now)["claude"]
	if got.Error != "" || got.FiveHour.UsedPercent != 83 || got.SevenDay.UsedPercent != 0.5 {
		t.Fatalf("entry %+v", got)
	}
	if got.FiveHour.ResetsAt == nil || *got.FiveHour.ResetsAt != float64(time.Date(2026, 10, 9, 12, 19, 59, 0, time.UTC).Unix()) {
		t.Fatalf("resets_at %v", got.FiveHour.ResetsAt)
	}
}

func TestCodexWindowsMappedByLengthNotPosition(t *testing.T) {
	e := parseCodexUsage(map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": int64(91), "limit_window_seconds": int64(604800), "reset_at": int64(1792057830)},
		"secondary_window": map[string]any{"used_percent": 7.5, "limit_window_seconds": int64(18000)},
	}})
	if e.SevenDay == nil || e.SevenDay.UsedPercent != 91 || *e.SevenDay.ResetsAt != 1792057830 ||
		e.FiveHour == nil || e.FiveHour.UsedPercent != 7.5 {
		t.Fatalf("by length %+v %+v", e.FiveHour, e.SevenDay)
	}
	e = parseCodexUsage(map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": int64(12)},
		"secondary_window": map[string]any{"used_percent": int64(34)},
	}})
	if e.FiveHour.UsedPercent != 12 || e.SevenDay.UsedPercent != 34 {
		t.Fatalf("positional fallback %+v %+v", e.FiveHour, e.SevenDay)
	}
}

func TestCodexCredentialPiFirstThenCodexCLI(t *testing.T) {
	cfg, home := isolate(t)
	useLiveFetchers()
	config.ObjectAt(config.ObjectAt(cfg.Values, "external_executors"), "pi")["agent_dir"] = "user"
	srv, seen := usageServer(t, 200,
		`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1791558187}}}`)
	codexUsageURL = srv.URL
	writeFile(t, filepath.Join(home, ".pi", "agent", "auth.json"),
		`{"openai-codex":{"type":"oauth","access":"pi-`+secret+`","expires":`+jsonInt(now.Add(time.Hour).UnixMilli())+`,"accountId":"acct-pi"}}`)
	writeFile(t, filepath.Join(home, ".codex", "auth.json"),
		`{"tokens":{"access_token":"cx-`+secret+`","account_id":"acct-cx"}}`)
	if got := Snapshot(cfg, []string{GroupCodex}, now)[GroupCodex]; got.Error != "" || got.FiveHour == nil {
		t.Fatalf("entry %+v", got)
	}
	if seen.Get("Authorization") != "Bearer pi-"+secret || seen.Get("ChatGPT-Account-Id") != "acct-pi" ||
		seen.Get("OpenAI-Beta") != "codex-1" {
		t.Fatalf("pi login first: %v", *seen)
	}
	writeFile(t, filepath.Join(home, ".pi", "agent", "auth.json"),
		`{"openai-codex":{"access":"pi-`+secret+`","expires":`+jsonInt(now.Add(-time.Hour).UnixMilli())+`}}`)
	_ = os.Remove(CachePath())
	Snapshot(cfg, []string{GroupCodex}, now)
	if seen.Get("Authorization") != "Bearer cx-"+secret || seen.Get("ChatGPT-Account-Id") != "acct-cx" {
		t.Fatalf("expired pi login falls back to the Codex CLI: %v", *seen)
	}
}

func TestGroupFollowsTheClassModelProvider(t *testing.T) {
	cfg := &config.Resolved{Values: config.Defaults()}
	policy := config.ObjectAt(config.ObjectAt(config.ObjectAt(config.ObjectAt(cfg.Values,
		"external_executors"), "pi"), "class_policy"), "deep")
	policy["model"] = "github-copilot/gpt-5"
	if g := Group(cfg, "claude", "deep"); g != GroupClaude {
		t.Fatalf("claude: %q", g)
	}
	if g := Group(cfg, "pi", "deep"); g != "github-copilot" {
		t.Fatalf("pi on copilot: %q", g)
	}
	policy["model"] = "openai-codex/gpt-6.1-sol"
	if g := Group(cfg, "pi", "deep"); g != GroupCodex {
		t.Fatalf("pi on codex: %q", g)
	}
}
