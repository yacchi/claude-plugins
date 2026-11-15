package integrate

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/procx"
)

// shimScriptEnv names the script a copy of this test binary runs through
// sh instead of testing (shimExe): how a shebang shim runs on Windows.
const shimScriptEnv = "AGENTEXEC_TEST_SHIM_SCRIPT"

// runShimScript runs script with this process's arguments through sh and
// returns its exit code.
func runShimScript(script string) int {
	sh := procx.ShellPath()
	cmd := exec.Command(sh)
	// Every argument quoted: the MSYS runtime brace- and glob-expands an
	// unquoted one (HEAD^{commit} would reach git as HEAD^commit).
	parts := []string{quoteArg(sh), quoteArg(script)}
	for _, a := range os.Args[1:] {
		parts = append(parts, quoteArg(a))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join(parts, " ")}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		return 127
	}
}

// quoteArg is s double-quoted for a Windows command line (the
// CommandLineToArgvW rules), quoted even when it needs no quoting.
func quoteArg(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteByte('"')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteByte(c)
		}
		slashes = 0
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// shimExe makes script+".exe" a copy of this test binary that runs script
// through sh with its arguments (see TestMain). A copy, not a hard link: a
// link to the running binary could not be deleted by the test's cleanup.
func shimExe(t *testing.T, script string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(script + ".exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shimScriptEnv, script)
}
