package isolate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// RequestRootEnv overrides where request workspaces live.
const RequestRootEnv = "ORCHESTRA_REQUEST_ROOT"

// requestFile is the manifest at the top of a request workspace.
const requestFile = "request.json"

// requestLockTimeout bounds the wait for the request lock, so a stuck holder
// is reported instead of hanging every request operation.
const requestLockTimeout = 120 * time.Second

// requestNow stamps request ids and creation times; tests replace it.
var requestNow = time.Now

// Request is one user request's workspace: the assemblies it produced, one
// per repository it touched.
type Request struct {
	ID      string        `json:"id"`
	Session string        `json:"session"` // the Claude Code session id; "" outside one
	Created string        `json:"created"` // RFC 3339, local time
	Dir     string        `json:"dir"`     // the workspace; set from where the manifest was loaded
	Repos   []RequestRepo `json:"repos"`
}

// RequestRepo is one repository's assembly within a request.
type RequestRepo struct {
	Common      string   `json:"common"`      // the repository's git common dir (real path): its identity
	Checkout    string   `json:"checkout"`    // the checkout the assembly started from
	Name        string   `json:"name"`        // the assembly's directory name under the workspace
	Assembly    string   `json:"assembly"`    // the assembly worktree's path
	Task        string   `json:"task"`        // the assembly's task id (the request id)
	Uncommitted string   `json:"uncommitted"` // clean | include | ignore
	Files       []string `json:"files"`       // the checkout's uncommitted paths at creation; [] when clean
}

// RequestRoot is the directory request workspaces live in: the environment
// override, else the configured isolation.request_root, else the state dir.
// It is not created here.
func RequestRoot() (string, error) {
	if v := os.Getenv(RequestRootEnv); v != "" {
		return absoluteRequestRoot(v, RequestRootEnv)
	}
	if cfg, err := config.Resolve(); err == nil {
		if v, _ := cfg.String("isolation", "request_root"); v != "" {
			return absoluteRequestRoot(v, "isolation.request_root")
		}
	}
	return paths.RealPath(filepath.Join(paths.StateDir(), "requests")), nil
}

// absoluteRequestRoot expands a leading ~ and rejects a path that is still
// relative: a relative root would move with the working directory.
func absoluteRequestRoot(value, source string) (string, error) {
	expanded := value
	if value == "~" || strings.HasPrefix(value, "~/") {
		expanded = paths.Home() + value[1:]
	}
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("%s must be an absolute path (or start with ~/): %q", source, value)
	}
	return paths.RealPath(expanded), nil
}

// RepoCommon is the real path of root's git common dir: the identity that a
// checkout and all of its worktrees share.
func RepoCommon(root string) (string, error) {
	out, err := gitx.Run(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return paths.RealPath(dir), nil
}

// AssemblyAlive reports whether the assembly is still on disk with its role
// marker. An assembly that is gone or lost its marker no longer keeps its
// request open.
func AssemblyAlive(rr RequestRepo) bool {
	return isDir(rr.Assembly) && ReadRole(rr.Assembly) == RoleAssembly
}

// Repo is the entry for the repository whose common dir is common.
func (r Request) Repo(common string) (RequestRepo, bool) {
	for _, rr := range r.Repos {
		if rr.Common == common {
			return rr, true
		}
	}
	return RequestRepo{}, false
}

// requestComponent is the session part of a request id. Outside a Claude Code
// session, or with a session id that has no usable component, every caller
// shares the "local" one.
func requestComponent() string {
	if c := sessionComponent(currentSession()); c != "" {
		return c
	}
	return "local"
}

// newRequestID is today's date and the session component, with a numeric
// suffix when that workspace already exists.
func newRequestID(root string) string {
	base := requestNow().Format("20060102") + "-" + requestComponent()
	id := base
	for n := 2; lexists(filepath.Join(root, id)); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

// withRequests runs fn with the request root locked and the open requests
// (oldest first). A request with no live assembly is ended: its workspace is
// removed and it is not passed to fn.
func withRequests(fn func(root string, open []Request) error) error {
	root, err := RequestRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	lockPath := filepath.Join(root, ".lock")
	unlock, err := fsutil.Lock(lockPath, fsutil.Exclusive, requestLockTimeout)
	if errors.Is(err, fsutil.ErrLockTimeout) {
		return fmt.Errorf("another request operation holds %s", lockPath)
	}
	if err != nil {
		return err
	}
	defer unlock()
	open, err := loadOpenRequests(root)
	if err != nil {
		return err
	}
	return fn(root, open)
}

// loadOpenRequests reads every workspace's manifest, ends the requests with no
// live assembly and returns the rest oldest first. A manifest that cannot be
// read or decoded is skipped and left on disk.
func loadOpenRequests(root string) ([]Request, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var open []Request
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		var r Request
		if err := fsutil.ReadJSONFile(filepath.Join(dir, requestFile), &r); err != nil {
			continue
		}
		r.Dir = dir
		if !requestAlive(r) {
			removeWorkspace(dir)
			continue
		}
		open = append(open, r)
	}
	sort.SliceStable(open, func(i, j int) bool {
		ci, cj := createdAt(open[i]), createdAt(open[j])
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		return open[i].ID < open[j].ID
	})
	return open, nil
}

// requestAlive is whether any of the request's assemblies is still live.
func requestAlive(r Request) bool {
	for _, rr := range r.Repos {
		if AssemblyAlive(rr) {
			return true
		}
	}
	return false
}

// createdAt parses the manifest's creation time; an unreadable one sorts first.
func createdAt(r Request) time.Time {
	t, _ := time.Parse(time.RFC3339, r.Created)
	return t
}

// removeWorkspace drops an ended request's workspace. Only the manifest and
// the directories left empty go: a stray file keeps its directories, so
// nothing a user put there is ever deleted.
func removeWorkspace(dir string) {
	_ = os.Remove(filepath.Join(dir, requestFile))
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir {
			dirs = append(dirs, p)
		}
		return nil
	})
	// WalkDir lists a directory before its children; removing in reverse
	// empties the deepest directories first.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	_ = os.Remove(dir)
}

// saveRequest writes r's manifest into its workspace.
func saveRequest(r Request) error {
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWriteJSON(filepath.Join(r.Dir, requestFile), r, 0o644)
}

// pickCurrent is the newest open request of the calling session. Sessions are
// matched by their full id: the 8-character branch component is not unique.
func pickCurrent(open []Request) (Request, bool) {
	session := currentSession()
	for i := len(open) - 1; i >= 0; i-- {
		if open[i].Session == session {
			return open[i], true
		}
	}
	return Request{}, false
}
