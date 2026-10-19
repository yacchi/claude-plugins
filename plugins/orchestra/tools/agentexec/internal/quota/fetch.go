package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// The live endpoints and the platform hooks (replaced by tests).
var (
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	goos           = runtime.GOOS
	// keychainRead returns the generic password stored under service.
	keychainRead = func(ctx context.Context, service string) ([]byte, error) {
		return exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-w").Output()
	}
)

// maxBody caps how much of a usage answer is read.
const maxBody = 1 << 20

// credential is one bearer token; it is never stored in an Entry, the cache
// or an error.
type credential struct {
	token, accountID string
}

// keychainService is Claude Code's keychain item: suffixed with the first 8
// hex digits of sha256(CLAUDE_CONFIG_DIR) when that variable is set.
func keychainService(configDir string) string {
	const base = "Claude Code-credentials"
	if configDir == "" {
		return base
	}
	sum := sha256.Sum256([]byte(configDir))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

func parseClaudeCredential(raw []byte, now time.Time) (credential, string) {
	v, err := fsutil.DecodeJSON([]byte(strings.TrimSpace(string(raw))))
	if err != nil {
		return credential{}, ErrNoToken
	}
	o, _ := v.(map[string]any)
	oauth, _ := o["claudeAiOauth"].(map[string]any)
	token, _ := oauth["accessToken"].(string)
	if token == "" {
		return credential{}, ErrNoToken
	}
	if exp, ok := Number(oauth["expiresAt"]); ok && exp/1000 <= seconds(now) {
		return credential{}, ErrExpired
	}
	return credential{token: token}, ""
}

func claudeCredential(ctx context.Context, now time.Time) (credential, string) {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	label := ErrNoToken
	if goos == "darwin" {
		if raw, err := keychainRead(ctx, keychainService(configDir)); err == nil {
			c, l := parseClaudeCredential(raw, now)
			if l == "" {
				return c, ""
			}
			label = l
		}
	}
	file := filepath.Join(paths.Home(), ".claude", ".credentials.json")
	if configDir != "" {
		file = filepath.Join(fsutil.ExpandUser(configDir), ".credentials.json")
	}
	if raw, err := os.ReadFile(file); err == nil {
		c, l := parseClaudeCredential(raw, now)
		if l == "" {
			return c, ""
		}
		if label != ErrExpired {
			label = l
		}
	}
	return credential{}, label
}

// piAuthFiles are pi's auth.json candidates: the agent dir orchestra runs pi
// with, $PI_CODING_AGENT_DIR, then pi's default ~/.pi/agent.
func piAuthFiles(cfg *config.Resolved) []string {
	out := []string{}
	add := func(dir string) {
		if dir == "" {
			return
		}
		p := filepath.Join(dir, "auth.json")
		for _, seen := range out {
			if seen == p {
				return
			}
		}
		out = append(out, p)
	}
	_, dir := tools.PiAgentDir(cfg)
	add(dir)
	add(os.Getenv(tools.PiAgentDirEnv))
	add(filepath.Join(paths.Home(), ".pi", "agent"))
	return out
}

func codexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return fsutil.ExpandUser(v)
	}
	return filepath.Join(paths.Home(), ".codex")
}

// codexCredential is the ChatGPT token: pi's openai-codex login first, then
// the Codex CLI's. A missing or expired one is skipped.
func codexCredential(cfg *config.Resolved, now time.Time) (credential, string) {
	label := ErrNoToken
	for _, file := range piAuthFiles(cfg) {
		entry, _ := readObject(file)["openai-codex"].(map[string]any)
		token, _ := entry["access"].(string)
		if token == "" {
			continue
		}
		if exp, ok := Number(entry["expires"]); ok && exp/1000 <= seconds(now) {
			label = ErrExpired
			continue
		}
		account, _ := entry["accountId"].(string)
		return credential{token: token, accountID: account}, ""
	}
	tokens, _ := readObject(filepath.Join(codexHome(), "auth.json"))["tokens"].(map[string]any)
	if token, _ := tokens["access_token"].(string); token != "" {
		account, _ := tokens["account_id"].(string)
		return credential{token: token, accountID: account}, ""
	}
	return credential{}, label
}

// get performs one usage request and decodes the JSON answer. Errors are
// labels only.
func get(ctx context.Context, url string, headers map[string]string) (map[string]any, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, ErrNetwork
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, ErrNetwork
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrHTTP(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, ErrNetwork
	}
	v, err := fsutil.DecodeJSON(body)
	if err != nil {
		return nil, ErrParse
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, ErrParse
	}
	return o, ""
}

func fetchClaude(ctx context.Context, _ *config.Resolved, now time.Time) Entry {
	cred, label := claudeCredential(ctx, now)
	if label != "" {
		return Entry{Error: label}
	}
	doc, label := get(ctx, claudeUsageURL, map[string]string{
		"Authorization":  "Bearer " + cred.token,
		"anthropic-beta": "oauth-2025-04-20",
		"User-Agent":     "claude-code/2.1.0",
	})
	if label != "" {
		return Entry{Error: label}
	}
	return parseClaudeUsage(doc)
}

// parseClaudeUsage maps {"five_hour": {"utilization": 8.0, "resets_at":
// ISO}, "seven_day": {...}}; utilization is already a percent.
func parseClaudeUsage(doc map[string]any) Entry {
	window := func(v any) *Window {
		o, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		pct, ok := Number(o["utilization"])
		if !ok {
			return nil
		}
		w := &Window{UsedPercent: pct}
		if s, isStr := o["resets_at"].(string); isStr {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				r := float64(t.Unix())
				w.ResetsAt = &r
			}
		}
		return w
	}
	return Entry{FiveHour: window(doc[FiveHour]), SevenDay: window(doc[SevenDay])}
}

func fetchCodex(ctx context.Context, cfg *config.Resolved, now time.Time) Entry {
	cred, label := codexCredential(cfg, now)
	if label != "" {
		return Entry{Error: label}
	}
	headers := map[string]string{
		"Authorization": "Bearer " + cred.token,
		"User-Agent":    "codex-cli",
		"OpenAI-Beta":   "codex-1",
	}
	if cred.accountID != "" {
		headers["ChatGPT-Account-Id"] = cred.accountID
	}
	doc, label := get(ctx, codexUsageURL, headers)
	if label != "" {
		return Entry{Error: label}
	}
	return parseCodexUsage(doc)
}

// windowSeconds names the windows by their length.
var windowSeconds = map[float64]string{18000: FiveHour, 604800: SevenDay}

// parseCodexUsage maps rate_limit.primary_window/secondary_window by
// limit_window_seconds, falling back to primary = five_hour and secondary =
// seven_day when the length is absent or unknown.
func parseCodexUsage(doc map[string]any) Entry {
	rl, _ := doc["rate_limit"].(map[string]any)
	e := Entry{}
	set := func(key, fallback string) {
		o, ok := rl[key].(map[string]any)
		if !ok {
			return
		}
		pct, ok := Number(o["used_percent"])
		if !ok {
			return
		}
		w := &Window{UsedPercent: pct}
		if r, ok := Number(o["reset_at"]); ok {
			w.ResetsAt = &r
		}
		name := fallback
		if secs, ok := Number(o["limit_window_seconds"]); ok {
			if n, known := windowSeconds[secs]; known {
				name = n
			}
		}
		switch name {
		case FiveHour:
			if e.FiveHour == nil {
				e.FiveHour = w
			}
		case SevenDay:
			if e.SevenDay == nil {
				e.SevenDay = w
			}
		}
	}
	set("primary_window", FiveHour)
	set("secondary_window", SevenDay)
	return e
}
