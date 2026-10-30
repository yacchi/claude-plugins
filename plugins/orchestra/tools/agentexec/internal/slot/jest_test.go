package slot

import (
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

func TestCapJestWorkers(t *testing.T) {
	const n = 3
	capped := map[string]string{
		"jest src/a.test.ts":                      "jest src/a.test.ts --maxWorkers=3",
		"jest":                                    "jest --maxWorkers=3",
		"pnpm exec jest && tsc --noEmit":          "pnpm exec jest --maxWorkers=3 && tsc --noEmit",
		"npx jest --ci":                           "npx jest --ci --maxWorkers=3",
		"pnpm jest src":                           "pnpm jest src --maxWorkers=3",
		"yarn jest":                               "yarn jest --maxWorkers=3",
		"bunx jest":                               "bunx jest --maxWorkers=3",
		"npm exec jest -- src":                    "npm exec jest --maxWorkers=3 -- src",
		"./node_modules/.bin/jest x":              "./node_modules/.bin/jest x --maxWorkers=3",
		"FOO=1 timeout 300 jest x":                "FOO=1 timeout 300 jest x --maxWorkers=3",
		"jest a | tee log":                        "jest a --maxWorkers=3 | tee log",
		"jest a 2>&1 | tail":                      "jest a 2>&1 --maxWorkers=3 | tail",
		"jest a\nnpm test":                        "jest a --maxWorkers=3\nnpm test",
		"jest a \\\n  b":                          "jest a \\\n  b --maxWorkers=3",
		"jest --testNamePattern \"a -i b\" x":     "jest --testNamePattern \"a -i b\" x --maxWorkers=3",
		"echo $(jest x)":                          "echo $(jest x --maxWorkers=3)",
		"pnpm exec jest --maxWorkers=2 && jest x": "pnpm exec jest --maxWorkers=2 && jest x --maxWorkers=3",
	}
	for command, want := range capped {
		got, ok := CapJestWorkers(command, n)
		if !ok || got != want {
			t.Errorf("%q: got (%q, %v), want (%q, true)", command, got, ok, want)
		}
	}

	untouched := []string{
		"jest -i", "jest --runInBand", "jest --maxWorkers=50%", "jest --maxWorkers 4", "jest -w=2",
		"jest -w 2", "jest --maxWorkers=\"50%\"", "jest -t \"-i\"",
		"pnpm test", "npm test -- --watch=false", "vitest run", "jest-junit --output x",
		"command -v jest", "echo \"jest x\"", "git commit -m 'jest -i'", "cat <<EOF\njest x\nEOF",
		"", "git status",
	}
	for _, command := range untouched {
		if got, ok := CapJestWorkers(command, n); ok || got != command {
			t.Errorf("%q: got (%q, %v), want it unchanged", command, got, ok)
		}
	}
}

func TestCapJestWorkersOffWhenNoWorkers(t *testing.T) {
	if got, ok := CapJestWorkers("jest x", 0); ok || got != "jest x" {
		t.Errorf("n 0: got (%q, %v), want unchanged", got, ok)
	}
}

func TestJestWorkerCapSharesTheCoresOverTheTestPool(t *testing.T) {
	was := cpuCount
	t.Cleanup(func() { cpuCount = was })
	cpuCount = func() int { return 14 }

	throttle := func(v any) *config.Resolved {
		return &config.Resolved{Values: map[string]any{"throttle": map[string]any{"test_slots": v}}}
	}
	cases := []struct {
		name string
		v    any
		want int
	}{
		{"two slots", int64(2), 7},
		{"one slot", int64(1), 14},
		{"more slots than cores", int64(20), 1},
		{"off", int64(0), 0},
		{"auto (cores 14 -> two slots)", "auto", 7},
	}
	for _, c := range cases {
		if got := JestWorkerCap(throttle(c.v)); got != c.want {
			t.Errorf("%s: cap %d, want %d", c.name, got, c.want)
		}
	}
}
