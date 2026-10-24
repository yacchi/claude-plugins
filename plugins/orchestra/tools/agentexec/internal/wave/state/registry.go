package state

import (
	"os"
	"path/filepath"
)

func stopPath(statePath string) string {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		abs = statePath
	}
	return filepath.Join(filepath.Dir(abs), "STOP")
}

// StopRequested reports whether a STOP file sits beside the state file.
func StopRequested(statePath string) bool {
	st, err := os.Stat(stopPath(statePath))
	return err == nil && st.Mode().IsRegular()
}

// RequestStop writes the STOP file (its content is the reason) the runner
// polls between steps.
func RequestStop(statePath, reason string) error {
	p := stopPath(statePath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(reason), 0o644)
}

// ClearStop removes the STOP file; a missing one is fine.
func ClearStop(statePath string) error {
	err := os.Remove(stopPath(statePath))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
