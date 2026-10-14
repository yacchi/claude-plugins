package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// Denial is one sandbox denial in a finished run. Source is "tool" (a pi
// tool_execution_end record) or "stderr"; Intentional marks a hit on
// deny_read or a tamper/pi-config deny (never granted, never escalated).
type Denial struct {
	Path        string `json:"path"`
	Op          string `json:"op"` // write | read
	Source      string `json:"source"`
	Intentional bool   `json:"intentional,omitempty"`
}

var (
	markerRe = regexp.MustCompile(`(?i)operation not permitted|permission denied|read-only file system`)
	// GNU coreutils quotes names as 'x' in the C locale but as ‘x’ under a
	// UTF-8 locale: both, or a Linux denial is missed.
	quotedRe     = regexp.MustCompile("'([^'\\n]+)'|\"([^\"\\n]+)\"|\u2018([^\u2019\\n]+)\u2019")
	readPrefixRe = regexp.MustCompile(`(?i)^\s*(?:find|ls|du|cat|grep|egrep|fgrep|rg|ag|stat|head|tail|less|more|` +
		`wc|tree|file|readlink|realpath|md5|shasum|sha256sum|diff)\s*:`)
	readSyscallRe = regexp.MustCompile("(?i)\\b(?:scandir|opendir|readdir|lstat|stat|access|readlink|realpath)\\b\\s*['\u2018]")
)

// lineOp: tools/syscalls that only READ never hit a sandbox write denial.
func lineOp(line string) string {
	if readPrefixRe.MatchString(line) || readSyscallRe.MatchString(line) {
		return "read"
	}
	return "write"
}

func candidates(line string) []string {
	var out []string
	for _, m := range quotedRe.FindAllStringSubmatch(line, -1) {
		tok := m[1]
		if tok == "" {
			tok = m[2]
		}
		if tok == "" {
			tok = m[3]
		}
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	stripped := quotedRe.ReplaceAllString(line, " ")
	for _, tok := range strings.Fields(stripped) {
		tok = strings.TrimRight(tok, ":,;")
		if (len(tok) > 1 && (strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "./") ||
			strings.HasPrefix(tok, "../"))) || tok == "/" {
			out = append(out, tok)
		}
	}
	return out
}

func nearestExisting(path string) string {
	p := path
	for p != "" && !lexists(p) {
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if p == "" {
		return "/"
	}
	return p
}

// toolEndTexts is the text of every pi tool_execution_end record in a JSONL
// stdout: result.content[].text, a string content, or a string result.
func toolEndTexts(stdout string) []string {
	var texts []string
	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil || ev["type"] != "tool_execution_end" {
			continue
		}
		switch r := ev["result"].(type) {
		case map[string]any:
			switch c := r["content"].(type) {
			case []any:
				for _, block := range c {
					if b, ok := block.(map[string]any); ok {
						if t, ok := b["text"].(string); ok {
							texts = append(texts, t)
						}
					}
				}
			case string:
				texts = append(texts, c)
			}
		case string:
			texts = append(texts, r)
		}
	}
	return texts
}

func writableByUser(p string) bool { return unix.Access(p, unix.W_OK) == nil }

// DetectDenials finds the sandbox denials of a finished run under s from
// its stdout (pi JSONL: tool_execution_end records) and stderr. A
// candidate path is a denial only if, judged from this UNSANDBOXED parent,
// it is outside the writable set AND its nearest existing ancestor is
// writable by the user (else it is an ordinary permission error). Hits on
// deny_read and on the tamper/pi-config denies are Intentional.
func DetectDenials(s Spec, stdout, stderr string) []Denial {
	cwd := s.Cwd
	if cwd == "" {
		cwd = s.Tree
	}
	return detectDenials(toolEndTexts(stdout), stderr, cwd, s.Policy, writableByUser)
}

func detectDenials(toolTexts []string, stderr, cwd string, p *Policy, access func(string) bool) []Denial {
	if p == nil {
		p = &Policy{}
	}
	var denyRx []*regexp.Regexp
	for _, r := range p.DenyWriteRegex {
		if rx, err := regexp.Compile(r); err == nil {
			denyRx = append(denyRx, rx)
		}
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd = realPath(cwd)
	type src struct{ name, text string }
	var sources []src
	for _, t := range toolTexts {
		sources = append(sources, src{"tool", t})
	}
	if stderr != "" {
		sources = append(sources, src{"stderr", stderr})
	}
	seen := map[string]bool{}
	out := []Denial{}
	underAny := func(path string, list []string) bool {
		for _, d := range list {
			if under(path, d) {
				return true
			}
		}
		return false
	}
	for _, s := range sources {
		for _, line := range fsutil.SplitLines(s.text) {
			if !markerRe.MatchString(line) {
				continue
			}
			op := lineOp(line)
			for _, cand := range candidates(line) {
				path := cand
				if !strings.HasPrefix(cand, "/") {
					path = filepath.Join(cwd, cand)
				}
				real := realPath(path)
				if seen[real] {
					continue
				}
				if underAny(real, p.DenyRead) {
					seen[real] = true
					out = append(out, Denial{Path: real, Op: op, Source: s.name, Intentional: true})
					continue
				}
				if op == "read" {
					continue
				}
				rxHit := false
				for _, rx := range denyRx {
					if rx.MatchString(real) {
						rxHit = true
						break
					}
				}
				if underAny(real, p.DenyWrite) || rxHit {
					// tamper protection / pi config: denied on purpose
					seen[real] = true
					out = append(out, Denial{Path: real, Op: op, Source: s.name, Intentional: true})
					continue
				}
				if underAny(real, p.Writable) || !access(nearestExisting(real)) {
					continue
				}
				seen[real] = true
				out = append(out, Denial{Path: real, Op: op, Source: s.name})
			}
		}
	}
	return out
}
