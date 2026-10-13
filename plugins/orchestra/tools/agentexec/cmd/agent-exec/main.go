// Command agent-exec is orchestra's executor wrapper; see internal/cli.
package main

import (
	"os"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
