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
		got, ok := Classify(command, nil)
		if !ok || got != op {
			t.Errorf("%q: (%q, %v), want (%q, heavy)", command, got, ok, op)
		}
	}
	cold := []string{
		"git status", "cat package.json", "grep test", "grep -r 'pnpm test' docs",
		`git commit -m "fix the go test"`, "ls -la", "echo build", "npm run dev", "pnpm dev",
		"vite", "tsc --version", "command -v pnpm", "rm -rf node_modules",
		"cat <<EOF > task.txt\npnpm test\nEOF", "",
	}
	for _, command := range cold {
		if op, ok := Classify(command, nil); ok {
			t.Errorf("%q classified heavy (%q)", command, op)
		}
	}
}

func TestClassifyExtraPatterns(t *testing.T) {
	extra := CompileExtra([]string{`^make (check|test)\b`, "(invalid"})
	if _, ok := Classify("FOO=1 make check", extra); !ok {
		t.Error("make check not matched by heavy_extra")
	}
	if _, ok := Classify("make clean", extra); ok {
		t.Error("make clean matched")
	}
}
