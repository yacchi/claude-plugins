package srt

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// The herdr server's control FIFO, pushed to by any later process: a long
// line arrives whole, a FIFO nobody reads is reported without blocking, a
// push is skipped when the settings on disk are current, and a push that
// cannot be delivered is never recorded as applied.
func TestControlFIFO(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "x"))
	path := filepath.Join(sandbox.StateDir(), sandbox.SrtSettingsDir, "s.ctl")

	if why := PushLine(path, []byte("{}\n")); !strings.Contains(why, "no control channel") {
		t.Errorf("missing FIFO: %q", why)
	}
	srtEnd, err := MakeFIFO(path) // srt's descriptor
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("FIFO mode %v", st.Mode())
	}
	line := append([]byte(`{"pad": "`+strings.Repeat("x", 1<<18)+`"}`), '\n')
	got := make(chan []byte)
	go func() {
		var buf []byte
		chunk := make([]byte, 1<<16)
		for !bytes.HasSuffix(buf, []byte("\n")) {
			n, err := srtEnd.Read(chunk)
			if err != nil {
				break
			}
			buf = append(buf, chunk[:n]...)
		}
		got <- buf
	}()
	if why := PushLine(path, line); why != "" {
		t.Fatalf("push: %s", why)
	}
	if b := <-got; !bytes.Equal(b, line) {
		t.Errorf("long line arrived as %d bytes, want %d", len(b), len(line))
	}

	// Skipped when unchanged, delivered when a domain is learned.
	settings := map[string]any{"network": map[string]any{"allowedDomains": []string{"a.example.com"}}}
	run := NewRun(RunOptions{Settings: settings, Domains: []string{"a.example.com"},
		SettingsPath: strings.TrimSuffix(path, ".ctl") + ".json", ControlPath: path})
	var stderr bytes.Buffer
	run.stderr = &stderr
	disk, err := run.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	fd := int(srtEnd.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	if err := run.Push(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<16)
	if n, err := unix.Read(fd, buf); err == nil {
		t.Fatalf("unchanged settings pushed: %q", buf[:n])
	}
	if err := sandbox.AddDomain("b.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := run.Push(); err != nil {
		t.Fatal(err)
	}
	n, err := unix.Read(fd, buf)
	if err != nil {
		t.Fatalf("nothing pushed: %v", err)
	}
	var pushed map[string]any
	if err := json.Unmarshal(buf[:n], &pushed); err != nil {
		t.Fatal(err)
	}
	domains := pushed["network"].(map[string]any)["allowedDomains"]
	if !reflect.DeepEqual(domains, []any{"a.example.com", "b.example.com"}) {
		t.Errorf("pushed %v", domains)
	}
	onDisk := func() any {
		var v any
		b, _ := os.ReadFile(disk)
		_ = json.Unmarshal(b, &v)
		return v
	}
	if !reflect.DeepEqual(onDisk(), pushed) {
		t.Errorf("disk %v, pushed %v", onDisk(), pushed)
	}

	// srt is gone: reported once, and the file on disk is not claimed.
	srtEnd.Close()
	if err := sandbox.AddDomain("c.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := run.Push(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.Stale, "nothing reads") || !strings.Contains(stderr.String(), "network settings stale") {
		t.Errorf("stale %q stderr %q", run.Stale, stderr.String())
	}
	if !reflect.DeepEqual(onDisk(), pushed) {
		t.Errorf("undelivered settings written: %v", onDisk())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if why := PushLine(path, []byte("{}\n")); !strings.Contains(why, "not a FIFO") {
		t.Errorf("regular file: %q", why)
	}
}
