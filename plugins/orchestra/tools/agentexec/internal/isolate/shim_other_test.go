//go:build !windows

package isolate

import "testing"

// shimScriptEnv is unset off Windows, where a shebang shim runs as is.
const shimScriptEnv = "AGENTEXEC_TEST_SHIM_SCRIPT"

func runShimScript(string) int { return 127 }

func shimExe(t *testing.T, _ string) {
	t.Helper()
	t.Fatal("shimExe is for Windows only")
}
