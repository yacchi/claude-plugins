package isolate

import (
	"encoding/json"
	"os"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/hostref"
)

// Hosted sessions. A host (herdr, Orca) that opens a Claude session in a
// tree records it in the tree's git dir and clears it when it closes the
// session itself; every deletion of the tree closes a session still
// recorded first, so no removal path leaves a live worker (and its peer /
// Remote Control entry) in a directory that is gone. Closing never blocks
// or fails a removal.

// hostSessionFile: JSON hostref.Ref of the session a host opened in the
// tree.
const hostSessionFile = "orchestra-host.json"

// hostCloseTimeout bounds closing one recorded session: a few local socket
// calls, kept short because the SessionEnd hook removes every tree of a
// session within 20 s.
var hostCloseTimeout = 5 * time.Second

// WriteHostSession records ref as the hosted session living in wt.
func WriteHostSession(wt string, ref hostref.Ref) error {
	p, err := markerPath(wt, hostSessionFile)
	if err != nil {
		return err
	}
	b, err := fsutil.MarshalJSON(ref)
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// ClearHostSession forgets the hosted session of wt (its host closed it).
func ClearHostSession(wt string) error { return removeMarkers(wt, hostSessionFile) }

func readHostSession(wt string) (hostref.Ref, bool) {
	p, err := markerPath(wt, hostSessionFile)
	if err != nil {
		return hostref.Ref{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return hostref.Ref{}, false
	}
	var ref hostref.Ref
	if json.Unmarshal(b, &ref) != nil || ref.Host == "" {
		return hostref.Ref{}, false
	}
	return ref, true
}

// closeHostSession closes the hosted session recorded in the tree at path,
// right before the tree is deleted. "" when there was none or it closed;
// otherwise why not, for the removal to report (the record then stays, so
// a removal that fails after this can retry).
func closeHostSession(path string) string {
	ref, ok := readHostSession(path)
	if !ok {
		return ""
	}
	if warning := hostref.Close(ref, hostCloseTimeout); warning != "" {
		return warning
	}
	_ = ClearHostSession(path)
	return ""
}
