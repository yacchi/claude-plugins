package srt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"

	"golang.org/x/sys/unix"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// Blocked is one connection srt refused; Port is -1 when the line had none.
type Blocked struct {
	Host string
	Port int
}

// NetDenial is a blocked (host, port) as dispatch results carry it.
type NetDenial struct {
	Host string `json:"host"`
	Port *int   `json:"port"`
}

// RunOptions configure one process under srt. Settings is the srt settings
// (sandbox.SrtSettings) with the static Domains as allowedDomains; learned
// domains are added on every write and push. Without Settings only the
// debug log is watched. With ControlPath the control channel is that FIFO
// (MakeFIFO / PushLine), not a pipe.
type RunOptions struct {
	Settings     map[string]any
	Domains      []string
	SettingsPath string
	Session      string
	Quiet        []string
	Label        string
	ControlPath  string
}

// Run is one process under srt.
type Run struct {
	Base         map[string]any
	Domains      []string
	SettingsPath string
	Session      string
	Quiet        []string
	Label        string
	ControlPath  string
	// Stale is why the last FIFO push could not be delivered ("" = fine).
	Stale string

	mu       sync.Mutex
	controlW *os.File
	live     map[string]bool // nil: nothing pushed yet
	logPath  string
	offset   int64
	partial  string
	blocked  []Blocked
	log      *os.File
	stderr   io.Writer
}

// NewRun is a Run for o.
func NewRun(o RunOptions) *Run {
	label := o.Label
	if label == "" {
		label = "srt"
	}
	return &Run{Base: o.Settings, Domains: append([]string(nil), o.Domains...),
		SettingsPath: o.SettingsPath, Session: o.Session, Quiet: append([]string(nil), o.Quiet...),
		Label: label, ControlPath: o.ControlPath, stderr: os.Stderr}
}

// --- JSON in Python's shapes -------------------------------------------------

// asciiEscape is json.dumps' ensure_ascii applied to compact output.
func asciiEscape(b []byte) []byte {
	var out bytes.Buffer
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.Bytes()
}

// settingsFile is json.dump(settings, indent=1, sort_keys=True) + "\n".
func settingsFile(settings map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(settings); err != nil {
		return nil, err
	}
	return asciiEscape(buf.Bytes()), nil
}

// controlLine is json.dumps(settings, sort_keys=True) + "\n".
func controlLine(settings map[string]any) ([]byte, error) {
	b, err := fsutil.MarshalJSON(settings)
	if err != nil {
		return nil, err
	}
	return append(asciiEscape(b), '\n'), nil
}

// canonical is a comparable rendering of any JSON value.
func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var back any
	if json.Unmarshal(b, &back) != nil {
		return ""
	}
	c, _ := json.Marshal(back)
	return string(c)
}

// --- settings ----------------------------------------------------------------

// Settings is the base settings with the static plus learned domains.
func (r *Run) Settings(learned []string) map[string]any {
	var copy map[string]any
	b, _ := json.Marshal(r.Base)
	_ = json.Unmarshal(b, &copy)
	if copy == nil {
		copy = map[string]any{}
	}
	network, _ := copy["network"].(map[string]any)
	if network == nil {
		network = map[string]any{}
		copy["network"] = network
	}
	network["allowedDomains"] = dedupe(append(append([]string{}, r.Domains...), learned...))
	return copy
}

// WriteSettings writes settings to SettingsPath (dir 0700, file 0600).
func (r *Run) WriteSettings(settings map[string]any) (string, error) {
	dir := filepath.Dir(r.SettingsPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	data, err := settingsFile(settings)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(r.SettingsPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	return r.SettingsPath, f.Close()
}

func setOf(list []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range list {
		m[s] = true
	}
	return m
}

func sameSet(a, b map[string]bool) bool {
	if a == nil || b == nil || len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// Prepare writes the settings with the learned domains of now.
func (r *Run) Prepare() (string, error) {
	learned := sandbox.LearnedDomains()
	path, err := r.WriteSettings(r.Settings(learned))
	if err != nil {
		return "", err
	}
	r.live = setOf(learned)
	return path, nil
}

// --- spawn -------------------------------------------------------------------

// Spawn starts cmd with the control channel as its fd 3: a fresh pipe's
// read end (the write end is kept for Push), or the FIFO at ControlPath,
// of which only srt keeps a descriptor.
func (r *Run) Spawn(cmd *exec.Cmd) error {
	if len(cmd.ExtraFiles) > 0 {
		return errors.New("srt: the control channel must be the child's only extra fd")
	}
	var readEnd, writeEnd *os.File
	var err error
	if r.ControlPath != "" {
		readEnd, err = MakeFIFO(r.ControlPath)
	} else {
		readEnd, writeEnd, err = os.Pipe()
	}
	if err != nil {
		return err
	}
	cmd.ExtraFiles = []*os.File{readEnd} // fd 3 == sandbox.SrtControlFD
	err = cmd.Start()
	readEnd.Close()
	cmd.ExtraFiles = nil
	if err != nil {
		if writeEnd != nil {
			writeEnd.Close()
		}
		return err
	}
	r.controlW = writeEnd
	return nil
}

// Close releases the control pipe and the log handle.
func (r *Run) Close() {
	if r.controlW != nil {
		r.controlW.Close()
		r.controlW = nil
	}
	if r.log != nil {
		r.log.Close()
		r.log = nil
	}
}

// --- debug log ---------------------------------------------------------------

// Watch tails path for blocked connections, from its current end or start.
func (r *Run) Watch(path string, fromEnd bool) {
	r.logPath, r.partial, r.offset = path, "", 0
	if fromEnd {
		if st, err := os.Stat(path); err == nil {
			r.offset = st.Size()
		}
	}
}

// OpenLog is an append handle on the watched log, for a caller that splits
// srt's debug lines out of a shared stderr itself (StderrSplit).
func (r *Run) OpenLog() (io.Writer, error) {
	if err := os.MkdirAll(filepath.Dir(r.logPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(r.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	r.log = f
	return f, nil
}

// Log appends one line to the open log.
func (r *Run) Log(line []byte) {
	if r.log != nil {
		r.log.Write(line)
	}
}

// Tail reads what the log gained since the last call and records (and
// audits) every blocked connection in it.
func (r *Run) Tail() error {
	if r.logPath == "" {
		return nil
	}
	st, err := os.Stat(r.logPath)
	if err != nil {
		return nil
	}
	size := st.Size()
	if size < r.offset { // truncated: start over
		r.offset, r.partial = 0, ""
	}
	if size == r.offset {
		return nil
	}
	f, err := os.Open(r.logPath)
	if err != nil {
		return err
	}
	data := make([]byte, size-r.offset)
	n, err := f.ReadAt(data, r.offset)
	f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	r.offset += int64(n)
	lines := strings.Split(r.partial+strings.ToValidUTF8(string(data[:n]), "�"), "\n")
	r.partial = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		host, port, ok := sandbox.ParseSrtDenial(line)
		if !ok {
			continue
		}
		r.blocked = append(r.blocked, Blocked{host, port})
		var p any
		if port >= 0 {
			p = int64(port)
		}
		if err := Audit(map[string]any{"ts": fsutil.NowISOZ(), "session": nullable(r.Session),
			"host": host, "port": p, "decision": "blocked"}); err != nil {
			return err
		}
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Audit appends one record to <state>/audit/network.jsonl (dir 0700, file
// 0600), keys sorted.
func Audit(record map[string]any) error {
	dir := filepath.Join(sandbox.StateDir(), sandbox.AuditDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	line, err := fsutil.MarshalJSON(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, sandbox.NetworkAudit), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- live domains ------------------------------------------------------------

// Push sends one full-config line to srt when the learned domains changed.
func (r *Run) Push() error {
	if r.ControlPath != "" {
		return r.pushFIFO()
	}
	if r.controlW == nil || r.Base == nil {
		return nil
	}
	learned := sandbox.LearnedDomains()
	if sameSet(setOf(learned), r.live) {
		return nil
	}
	settings := r.Settings(learned)
	line, err := controlLine(settings)
	if err != nil {
		return err
	}
	if _, err := r.controlW.Write(line); err != nil {
		fmt.Fprintf(r.stderr, "agent-exec: %s: srt control pipe: %s\n", r.Label, err)
		r.controlW.Close()
		r.controlW = nil
		return nil
	}
	r.live = setOf(learned)
	_, _ = r.WriteSettings(settings)
	return nil
}

func (r *Run) onDisk() any {
	b, err := os.ReadFile(r.SettingsPath)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

// pushFIFO pushes through the FIFO from whichever process runs it: one
// full line, only when the settings differ from the file on disk, which is
// rewritten after a delivered push. Pushes of one session are serialized.
func (r *Run) pushFIFO() error {
	if r.Base == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.SettingsPath), 0o700); err != nil {
		return err
	}
	unlock, err := fsutil.Lock(r.SettingsPath+".lock", fsutil.Exclusive, -1)
	if err != nil {
		return err
	}
	defer unlock()
	learned := sandbox.LearnedDomains()
	settings := r.Settings(learned)
	if disk := r.onDisk(); disk != nil && canonical(settings) == canonical(disk) {
		r.live = setOf(learned)
		return nil
	}
	line, err := controlLine(settings)
	if err != nil {
		return err
	}
	if why := PushLine(r.ControlPath, line); why != "" {
		if why != r.Stale {
			fmt.Fprintf(r.stderr, "agent-exec: %s: network settings stale: %s\n", r.Label, why)
		}
		r.Stale = why
		return nil
	}
	r.Stale = ""
	r.live = setOf(learned)
	_, err = r.WriteSettings(settings)
	return err
}

// Tick tails the log for blocked connections and pushes newly approved
// domains. Never fails: problems are reported on stderr.
func (r *Run) Tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, step := range []func() error{r.Tail, r.Push} {
		if err := step(); err != nil {
			fmt.Fprintf(r.stderr, "agent-exec: %s: network: %s\n", r.Label, err)
		}
	}
}

// Mark is the position Denials counts from.
func (r *Run) Mark() int { return len(r.blocked) }

// Denials are the blocked (host, port) pairs logged since mark,
// de-duplicated, without the quiet domains (nil quiet: the run's own).
func (r *Run) Denials(mark int, quiet []string) []NetDenial {
	if quiet == nil {
		quiet = r.Quiet
	}
	out := []NetDenial{}
	seen := map[Blocked]bool{}
	if mark < 0 || mark > len(r.blocked) {
		mark = len(r.blocked)
	}
	for _, b := range r.blocked[mark:] {
		if sandbox.DomainMatches(b.Host, quiet) || seen[b] {
			continue
		}
		seen[b] = true
		d := NetDenial{Host: b.Host}
		if b.Port >= 0 {
			p := b.Port
			d.Port = &p
		}
		out = append(out, d)
	}
	return out
}

// --- control FIFO ------------------------------------------------------------

// MakeFIFO creates a fresh control FIFO at path (dir 0700, FIFO 0600) and
// opens it read-write: srt reads it as its control fd and, holding a
// writer itself, never sees EOF when a pushing process closes its end.
func MakeFIFO(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// PushLine writes data (one full-config line, or nothing to only probe) to
// the control FIFO at path. It returns "" or why the channel cannot be
// used: missing, not a FIFO, or no reader (the srt behind it is gone). It
// never blocks on a FIFO nobody reads, and a line is written whole.
func PushLine(path string, data []byte) string {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "no control channel (" + path + ")"
	}
	if err != nil {
		return "control channel: " + err.Error()
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		return "control channel is not a FIFO (" + path + ")"
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENXIO) {
			return "nothing reads the control channel (" + path + ")"
		}
		return "control channel: " + err.Error()
	}
	defer unix.Close(fd)
	// Blocking from here: a non-blocking write of a line longer than the
	// pipe buffer could stop half-way and leave srt a broken line.
	if err := unix.SetNonblock(fd, false); err != nil {
		return "control channel: " + err.Error()
	}
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return "control channel: " + err.Error()
		}
		data = data[n:]
	}
	return ""
}

// --- CLI executor runs -------------------------------------------------------

// CLIRun is the Run of a spec with backend srt (settings written, log
// watched from its current end), or nil for any other spec.
func CLIRun(s sandbox.Spec, label string) (*Run, error) {
	if s.Backend != "srt" || s.Srt == nil {
		return nil, nil
	}
	info := s.Srt
	run := NewRun(RunOptions{Settings: info.Settings, Domains: info.Domains,
		SettingsPath: info.SettingsPath, Session: info.Session, Quiet: info.Quiet, Label: label})
	if _, err := run.Prepare(); err != nil {
		return nil, err
	}
	run.Watch(info.LogPath, true)
	return run, nil
}

// ExecPrepare readies an exec of an srt-wrapped argv: the settings are
// written and /dev/null is put on the control fd (no parent is left to
// push updates; srt refuses to run when the fd it was told to read is
// closed). When fd 3 already belongs to this process (the Go runtime may
// hold it), it refuses rather than replace it: the caller must spawn with
// an exec.Cmd and ExtraFiles instead.
func ExecPrepare(s sandbox.Spec) error {
	run, err := CLIRun(s, "srt")
	if err != nil || run == nil {
		return err
	}
	fd, err := unix.Open(os.DevNull, unix.O_RDONLY, 0) // inheritable on purpose
	if err != nil {
		return err
	}
	if fd == sandbox.SrtControlFD {
		return nil
	}
	defer unix.Close(fd)
	if _, err := unix.FcntlInt(uintptr(sandbox.SrtControlFD), unix.F_GETFD, 0); err == nil {
		return fmt.Errorf("srt: fd %d is in use; spawn srt with its control fd instead of exec", sandbox.SrtControlFD)
	}
	return unix.Dup2(fd, sandbox.SrtControlFD)
}
