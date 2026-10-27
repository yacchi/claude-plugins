// Package dispatch is the `dispatch` command's domain: prepared dispatch
// tokens, the per-task executor session store, the retention sweep of both,
// the routed (and direct) dispatch itself, detached runs with `wait`, and
// `write-file`.
package dispatch

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
)

// TokenPattern is the shape of a dispatch token. Anything else is rejected
// before a path is ever built from it.
var TokenPattern = regexp.MustCompile(`^dsp-[0-9a-f]{12}$`)

// ErrInvalidToken is a token that is malformed, unknown, unreadable or whose
// spec does not validate.
var ErrInvalidToken = errors.New("invalid token")

// Isolation modes. IsolateCli is always for a route agent-exec runs itself
// (a `dispatch: cli` executor, or any direct run) and no tree for a
// delegate route, whose caller isolates it.
const (
	IsolateAuto   = "auto"
	IsolateAlways = "always"
	IsolateNever  = "never"
	IsolateCli    = "cli"
)

// IsolateModes is the --isolate values as usage text.
const IsolateModes = "auto|always|never|cli"

// ValidIsolate reports whether mode is auto, always, never or cli.
func ValidIsolate(mode string) bool {
	return mode == IsolateAuto || mode == IsolateAlways || mode == IsolateNever || mode == IsolateCli
}

// PrepareSpec is what a token stands for. RunID and Task nil are "not
// given". ConfigRoot is the project config base the token pins ("" = none
// recorded, e.g. a token from before the field existed).
type PrepareSpec struct {
	Class, Archetype, Workdir, Isolate, ConfigRoot string
	PromptFiles                                    []string
	RunID, Task                                    *string
}

// stateBase is the directory the ledger dir lives in: tokens/ and
// sessions/ are its siblings.
func stateBase(cfg *config.Resolved) string {
	dir, err := filepath.Abs(ledger.Dir(cfg))
	if err != nil {
		dir = ledger.Dir(cfg)
	}
	return filepath.Dir(dir)
}

// TokenDir is the directory of the token spec files.
func TokenDir(cfg *config.Resolved) string { return filepath.Join(stateBase(cfg), "tokens") }

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// privateDir creates dir 0700 (parents with the default mode) and re-applies
// the mode to an existing one.
func privateDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// compactJSON is v as compact JSON without a trailing newline.
func compactJSON(v any) ([]byte, error) {
	b, err := fsutil.MarshalJSON(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mintToken() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "dsp-" + hex.EncodeToString(b[:]), nil
}

func optString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Prepare mints a token for s and writes its spec (0600, in a 0700 dir).
// Workdir, prompt files and the config root are made absolute; the prompt
// files need not exist yet (a correction round's feedback file is written
// between prepare and dispatch). An empty Archetype is "default", an empty
// Isolate "auto", an empty ConfigRoot cfg.Root. A run with a run id and task
// on the user's checkout with uncommitted work is refused before anything is
// written, until its request has decided how to start from that work.
func Prepare(cfg *config.Resolved, s PrepareSpec) (string, error) {
	SweepRetention(cfg)
	if s.Archetype == "" {
		s.Archetype = "default"
	}
	if s.Isolate == "" {
		s.Isolate = IsolateAuto
	}
	if !ValidIsolate(s.Isolate) {
		return "", errors.New("--isolate must be " + IsolateModes)
	}
	if (s.Isolate == IsolateAlways || s.Isolate == IsolateCli) && s.RunID != nil && *s.RunID != "" && s.Task != nil && *s.Task != "" {
		if _, user := isolate.UserCheckout(s.Workdir); user {
			res, err := isolate.CheckAssembly(s.Workdir, "")
			if err == nil && res.Status == "uncommitted" {
				return "", fmt.Errorf("%s; run 'agent-exec isolate request start --into %s --uncommitted include|ignore' once before preparing or dispatching", res.Reason, *s.RunID)
			}
		}
	}
	if s.ConfigRoot == "" {
		s.ConfigRoot = cfg.Root
	}
	prompts := make([]any, len(s.PromptFiles))
	for i, p := range s.PromptFiles {
		prompts[i] = abs(p)
	}
	spec := map[string]any{
		"class":        s.Class,
		"archetype":    s.Archetype,
		"workdir":      abs(s.Workdir),
		"run_id":       optString(s.RunID),
		"isolate":      s.Isolate,
		"task":         optString(s.Task),
		"prompt_files": prompts,
		"config_root":  abs(s.ConfigRoot),
	}
	payload, err := compactJSON(spec)
	if err != nil {
		return "", err
	}
	dir := TokenDir(cfg)
	if err := privateDir(dir); err != nil {
		return "", err
	}
	for range 10 {
		token, err := mintToken()
		if err != nil {
			return "", err
		}
		f, err := os.OpenFile(filepath.Join(dir, token+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(payload)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", werr
		}
		return token, nil
	}
	return "", errors.New("could not allocate dispatch token")
}

// LoadToken reads and validates token's spec. Every failure (malformed
// token, missing or symlinked file, bad JSON, a missing or mistyped field,
// a relative prompt path) is ErrInvalidToken.
func LoadToken(cfg *config.Resolved, token string) (PrepareSpec, error) {
	if !TokenPattern.MatchString(token) {
		return PrepareSpec{}, ErrInvalidToken
	}
	path := filepath.Join(TokenDir(cfg), token+".json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return PrepareSpec{}, ErrInvalidToken
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return PrepareSpec{}, ErrInvalidToken
	}
	v, err := fsutil.DecodeJSON(raw)
	if err != nil {
		return PrepareSpec{}, ErrInvalidToken
	}
	o, ok := v.(map[string]any)
	if !ok || o == nil {
		return PrepareSpec{}, ErrInvalidToken
	}
	for _, k := range []string{"class", "archetype", "workdir", "run_id", "isolate", "task", "prompt_files"} {
		if _, has := o[k]; !has {
			return PrepareSpec{}, ErrInvalidToken
		}
	}
	var s PrepareSpec
	var okAll = true
	str := func(k string) string {
		v := o[k]
		x, isStr := v.(string)
		if !isStr {
			okAll = false
		}
		return x
	}
	optStr := func(k string) *string {
		v := o[k]
		if v == nil {
			return nil
		}
		x, isStr := v.(string)
		if !isStr {
			okAll = false
			return nil
		}
		return &x
	}
	s.Class, s.Archetype, s.Workdir, s.Isolate = str("class"), str("archetype"), str("workdir"), str("isolate")
	s.RunID, s.Task = optStr("run_id"), optStr("task")
	if !okAll || !ValidIsolate(s.Isolate) {
		return PrepareSpec{}, ErrInvalidToken
	}
	pv := o["prompt_files"]
	list, isList := pv.([]any)
	if !isList {
		return PrepareSpec{}, ErrInvalidToken
	}
	for _, e := range list {
		p, isStr := e.(string)
		if !isStr || !filepath.IsAbs(p) {
			return PrepareSpec{}, ErrInvalidToken
		}
		s.PromptFiles = append(s.PromptFiles, p)
	}
	if root, _ := o["config_root"]; root != nil {
		if r, isStr := root.(string); isStr && filepath.IsAbs(r) {
			s.ConfigRoot = r
		}
	}
	return s, nil
}
