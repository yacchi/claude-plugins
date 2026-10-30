package slot

import "testing"

func TestClassify(t *testing.T) {
	heavy := map[string]string{
		"cd pkg && pnpm test":               "pnpm test",
		"FOO=1 pnpm -r typecheck":           "pnpm typecheck",
		"pnpm --filter x test":              "pnpm test",
		`pnpm --filter "@app/web" run lint`: "pnpm lint",
		"pnpm install --frozen-lockfile":    "pnpm install",
		"npm ci":                            "npm ci",
		"yarn":                              "yarn",
		"bun add zod":                       "bun add",
		"uv sync":                           "uv sync",
		"python -m pip install -r requirements.txt":   "pip install",
		"cargo +nightly build --release":              "cargo build",
		"go mod download":                             "go mod",
		"go test ./... 2>&1 | tail -20":               "go test",
		"bundle install":                              "bundle install",
		"poetry install":                              "poetry install",
		"npx vitest run src":                          "vitest",
		"pnpm exec tsc --noEmit":                      "tsc",
		"./node_modules/.bin/vue-tsc --noEmit":        "vue-tsc",
		"uv run pytest -x":                            "pytest",
		"timeout 300 turbo run test --filter=web":     "turbo test",
		"time nice -n 5 cargo test":                   "cargo test",
		"mise exec go@1.26 -- go build ./...":         "go build",
		"git status && pnpm lint:fix":                 "pnpm lint:fix",
		"(cd web; npx next build)":                    "next build",
		"npm test":                                    "npm test",
		"pnpm vitest run":                             "pnpm vitest",
		"ruff check . && mypy src":                    "ruff",
		"cdk synth --quiet":                           "cdk synth",
		"vite build":                                  "vite build",
		"esbuild src/index.ts --bundle --outdir=dist": "esbuild",
	}
	for command, op := range heavy {
		c := Classify(command, nil)
		if !c.Heavy || c.Op != op {
			t.Errorf("%q: (%q, %v), want (%q, heavy)", command, c.Op, c.Heavy, op)
		}
	}
	cold := []string{
		"git status", "cat package.json", "grep test", "grep -r 'pnpm test' docs",
		`git commit -m "fix the go test"`, "ls -la", "echo build", "npm run dev", "pnpm dev",
		"vite", "tsc --version", "command -v pnpm", "rm -rf node_modules",
		"cat <<EOF > task.txt\npnpm test\nEOF", "",
	}
	for _, command := range cold {
		if c := Classify(command, nil); c.Heavy {
			t.Errorf("%q classified heavy (%q)", command, c.Op)
		}
	}
}

func TestClassifyExtraPatterns(t *testing.T) {
	extra := CompileExtra([]string{`^make (check|test)\b`, "(invalid"})
	if !Classify("FOO=1 make check", extra).Heavy {
		t.Error("make check not matched by heavy_extra")
	}
	if Classify("make clean", extra).Heavy {
		t.Error("make clean matched")
	}
}

// A worker is denied a full run, so a targeted one read as full blocks real
// work, and a full one read as targeted lets the whole suite through.
func TestClassifyScope(t *testing.T) {
	full := []string{
		"go test ./...", "cd plugins/orchestra/tools/agentexec && go test -count=1 ./... 2>&1 | tail",
		"go vet ./...", "pnpm -r test", "pnpm test", "npm test -- --watch=false", "yarn test",
		"pytest", "uv run pytest -x", "cargo test --workspace", "cargo test", "make test",
		"turbo run test", "go test ./internal/x/... && go test ./...",
	}
	for _, command := range full {
		if c := Classify(command, nil); c.Scope != ScopeFull {
			t.Errorf("%q classified %s, want full", command, c.Scope)
		}
	}
	targeted := []string{
		"go test ./internal/x/...", "go test -count=1 ./internal/slot/... ./internal/checks/...",
		"go test -run TestClassify ./...", "pnpm --filter a test", "pnpm --filter=a test",
		"cd packages/a && pnpm test", "npm test -w a", "pytest tests/test_a.py", "pytest -k scope",
		"cargo test -p core", "make test PKG=./x", "turbo run test --filter=web", "npx vitest run src",
		"go build ./...", "tsc --noEmit", "pnpm install", "pnpm -r lint", "git status",
	}
	for _, command := range targeted {
		if c := Classify(command, nil); c.Scope != ScopeTargeted {
			t.Errorf("%q classified %s, want targeted", command, c.Scope)
		}
	}
}

// RunsProgram sees the program a line runs, through the same stripping as
// Classify: pnpm's own runner verbs, launchers, wrappers and env assignments.
func TestRunsProgram(t *testing.T) {
	yes := []string{
		"pnpm install", "pnpm -C packages/a test", "pnpm exec tsc", "FOO=1 pnpm test",
		"cd x && pnpm -v", "npx pnpm install", "npx -y pnpm@9 test", "corepack pnpm install",
		"nice -n 5 pnpm run lint", "time pnpm -r build", "git status; pnpm test",
	}
	for _, command := range yes {
		if !RunsProgram(command, "pnpm") {
			t.Errorf("%q: pnpm not seen", command)
		}
	}
	no := []string{
		"npm install", "npx tsc", "npx tsc pnpm", "corepack enable", "corepack enable pnpm",
		"echo pnpm test", `echo "pnpm test"`, "command -v pnpm", "ls -la", "pnpmx install",
		"cat <<EOF\npnpm install\nEOF", "",
	}
	for _, command := range no {
		if RunsProgram(command, "pnpm") {
			t.Errorf("%q: pnpm seen", command)
		}
	}
}
