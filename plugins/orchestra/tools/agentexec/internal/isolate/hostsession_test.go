package isolate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/hostref"
)

// fakeHost is a stand-in herdr binary running body after logging its
// arguments; it returns the binary and its log.
func fakeHost(t *testing.T, body string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "herdr"), filepath.Join(dir, "calls.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\n%s\n", log, body)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func recordHerdr(t *testing.T, tree, bin string) {
	t.Helper()
	ref := hostref.Ref{Host: hostref.Herdr, Binary: bin, Session: "orchestra-abcdef12", Home: t.TempDir(),
		Workspace: "w1", Agent: "p-a"}
	if err := WriteHostSession(tree, ref); err != nil {
		t.Fatal(err)
	}
}

func logged(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestSweepNeverClosesTheSessionOfATreeItKeeps(t *testing.T) {
	repo := newRepo(t)
	bin, log := fakeHost(t, "")
	busy := create(t, repo, "busy", "")
	dirty := create(t, repo, "dirty", "")
	clean := create(t, repo, "clean", "")
	for _, tree := range []string{busy, dirty, clean} {
		recordHerdr(t, tree, bin)
	}
	seen := float64(time.Now().Unix())
	_ = writeMarker(busy, leaseFile, fmt.Sprintf(`{"agent_type":"engineer","last_seen":%f}`, seen))
	writeFile(t, filepath.Join(dirty, "worker.txt"), "w\n")

	sweep(t, repo, SweepOptions{DryRun: true})
	if got := logged(log); got != "" {
		t.Fatalf("a dry run closed sessions: %q", got)
	}
	got := byTask(sweep(t, repo, SweepOptions{}))
	if got["busy"].Status != "busy" || got["dirty"].Status != "dirty" || got["clean"].Status != "removed" {
		t.Fatalf("%+v", got)
	}
	calls := logged(log)
	if !strings.Contains(calls, "workspace close w1") || strings.Count(calls, "workspace close") != 1 {
		t.Errorf("only the removed tree's session closes: %q", calls)
	}
}

func TestAFailedOrHungCloseNeverStopsTheRemoval(t *testing.T) {
	for _, c := range []struct{ label, body string }{
		{"failed", `echo '{"error":{"code":"boom","message":"boom"}}' >&2; exit 1`},
		{"hung", "exec sleep 60"},
	} {
		t.Run(c.label, func(t *testing.T) {
			saved := hostCloseTimeout
			hostCloseTimeout = 200 * time.Millisecond
			t.Cleanup(func() { hostCloseTimeout = saved })
			repo := newRepo(t)
			bin, _ := fakeHost(t, c.body)
			tree := create(t, repo, "a", "")
			recordHerdr(t, tree, bin)
			res := remove(t, repo, "a", false, "")
			if res.Status != "removed" || exists(tree) || !strings.Contains(res.Warning, "could not close herdr workspace w1") {
				t.Errorf("%+v", res)
			}
		})
	}
}

// hostedWorkerStamps writes the fresh lease and activity stamps of a worker
// running in tree as the main thread of its own session (no agent fields).
func hostedWorkerStamps(t *testing.T, tree string) {
	t.Helper()
	t.Setenv("ORCHESTRA_LEASE_WINDOW", "120")
	t.Setenv("ORCHESTRA_ACTIVITY_WINDOW", "600")
	seen := float64(time.Now().UnixNano()) / 1e9
	lease := fmt.Sprintf(`{"agent_id":"","agent_type":"","last_seen":%f,"since":%f}`, seen, seen)
	if err := writeMarker(tree, leaseFile, lease); err != nil {
		t.Fatal(err)
	}
	activity := fmt.Sprintf(`{"agent_id":"","agent_type":"","last_seen":%f,"command":"go test ./..."}`, seen)
	if err := writeMarker(tree, activityFile, activity); err != nil {
		t.Fatal(err)
	}
}

func TestClearHostSessionForgetsTheWorkersStamps(t *testing.T) {
	repo := newRepo(t)
	tree := create(t, repo, "a", "")
	bin, _ := fakeHost(t, "")
	recordHerdr(t, tree, bin)
	hostedWorkerStamps(t, tree)
	if inUse(repo, "a", tree) == "" {
		t.Fatal("fresh stamps do not keep the tree busy")
	}

	if err := ClearHostSession(tree); err != nil {
		t.Fatalf("ClearHostSession: %v", err)
	}
	for _, name := range []string{hostSessionFile, leaseFile, activityFile} {
		p, err := markerPath(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if exists(p) {
			t.Errorf("%s still exists", name)
		}
	}
	if got := inUse(repo, "a", tree); got != "" {
		t.Errorf("tree still busy after its session closed: %q", got)
	}
	if err := ClearHostSession(tree); err != nil {
		t.Errorf("second ClearHostSession: %v", err)
	}
}

func TestClearActivityKeepsLeaseAndHost(t *testing.T) {
	repo := newRepo(t)
	tree := create(t, repo, "a", "")
	bin, _ := fakeHost(t, "")
	recordHerdr(t, tree, bin)
	hostedWorkerStamps(t, tree)

	if err := ClearActivity(tree); err != nil {
		t.Fatalf("ClearActivity: %v", err)
	}
	activity, err := markerPath(tree, activityFile)
	if err != nil {
		t.Fatal(err)
	}
	if exists(activity) {
		t.Errorf("activity stamp still exists")
	}
	lease, err := markerPath(tree, leaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(lease) {
		t.Errorf("lease was removed")
	}
	if _, ok := readHostSession(tree); !ok {
		t.Errorf("host session was removed")
	}
	if err := ClearActivity(tree); err != nil {
		t.Errorf("second ClearActivity: %v", err)
	}
}
