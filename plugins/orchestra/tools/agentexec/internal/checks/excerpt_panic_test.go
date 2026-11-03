package checks

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func goPanicJoin(lines ...string) string { return strings.Join(lines, "\n") }

func TestGoPanicSummary(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "panic with a named test and later package FAIL",
			text: goPanicJoin(
				"--- FAIL: TestLoad (0.00s)",
				"panic: runtime error: index out of range [3] with length 3 [recovered]",
				"\tpanic: runtime error: index out of range [3] with length 3",
				"",
				"goroutine 7 [running]:",
				"testing.tRunner.func1.2({0x1, 0x2})",
				"FAIL\tgithub.com/x/y/internal/cfg\t0.012s",
				"ok  \tgithub.com/x/y/internal/run\t0.010s",
				"FAIL",
			),
			want: []string{"TestLoad panicked; later tests in github.com/x/y/internal/cfg did not run"},
		},
		{
			name: "timeout with no preceding test failure",
			text: goPanicJoin(
				"panic: test timed out after 10m0s",
				"\trunning tests:",
				"\t\tTestSlow (10m0s)",
				"FAIL\tgithub.com/x/y/internal/wave\t600.013s",
			),
			want: []string{"a test panicked; later tests in github.com/x/y/internal/wave did not run"},
		},
		{
			name: "subtest that panicked",
			text: goPanicJoin(
				"--- FAIL: TestA (0.00s)",
				"    --- FAIL: TestA/sub (0.00s)",
				"panic: boom [recovered]",
				"FAIL\texample.com/p\t0.01s",
			),
			want: []string{"TestA/sub panicked; later tests in example.com/p did not run"},
		},
		{
			name: "earlier ordinary failure, then a different test panics",
			text: goPanicJoin(
				"--- FAIL: TestX (0.00s)",
				"    x_test.go:9: want 1",
				"--- FAIL: TestY (0.00s)",
				"panic: nil map [recovered]",
				"FAIL\texample.com/p\t0.01s",
			),
			want: []string{"TestY panicked; later tests in example.com/p did not run"},
		},
		{
			name: "two panicked packages with an ok package between",
			text: goPanicJoin(
				"--- FAIL: TestA (0.00s)",
				"panic: one",
				"FAIL\texample.com/a\t0.01s",
				"ok  \texample.com/b\t0.01s",
				"--- FAIL: TestC (0.00s)",
				"panic: two",
				"FAIL\texample.com/c\t0.02s",
			),
			want: []string{
				"TestA panicked; later tests in example.com/a did not run",
				"TestC panicked; later tests in example.com/c did not run",
			},
		},
		{
			name: "panic printed by a passing package is not a panic",
			text: goPanicJoin(
				"panic: not really",
				"ok  \texample.com/p\t0.01s",
				"--- FAIL: TestZ (0.00s)",
				"FAIL\texample.com/q\t0.01s",
			),
			want: nil,
		},
		{
			name: "a failed test from an ok package does not leak into the next panic",
			text: goPanicJoin(
				"--- FAIL: TestA (0.00s)",
				"ok  \texample.com/p\t0.01s",
				"panic: x",
				"FAIL\texample.com/q\t0.01s",
			),
			want: []string{"a test panicked; later tests in example.com/q did not run"},
		},
		{
			name: "panic cut off before any FAIL package line",
			text: goPanicJoin(
				"--- FAIL: TestA (0.00s)",
				"panic: boom [recovered]",
			),
			want: nil,
		},
		{
			name: "ordinary failure with no panic",
			text: goPanicJoin(
				"--- FAIL: TestX (0.00s)",
				"    x_test.go:9: want 1",
				"FAIL\texample.com/p\t0.01s",
			),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := goPanicSummary(strings.Split(tt.text, "\n"))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("goPanicSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGoPanicExcerptNamesPanicFirst(t *testing.T) {
	text := goPanicJoin(
		"--- FAIL: TestLoad (0.00s)",
		"panic: runtime error: index out of range [3] with length 3 [recovered]",
		"\tpanic: runtime error: index out of range [3] with length 3",
		"FAIL\tgithub.com/x/y/internal/cfg\t0.012s",
		"ok  \tgithub.com/x/y/internal/run\t0.010s",
		"FAIL",
	)
	const summary = "TestLoad panicked; later tests in github.com/x/y/internal/cfg did not run"
	got := FailureExcerpt(text)
	if !strings.HasPrefix(got, summary+"\n") {
		t.Fatalf("FailureExcerpt() does not start with the panic summary:\n%s", got)
	}
}

func TestGoPanicExcerptSummaryCountsAgainstLimit(t *testing.T) {
	const summary = "TestLoad panicked; later tests in github.com/x/y/internal/cfg did not run"
	lines := []string{
		"--- FAIL: TestLoad (0.00s)",
		"panic: boom [recovered]",
		"FAIL\tgithub.com/x/y/internal/cfg\t0.012s",
	}
	filler := make([]string, 0, 1000)
	for range 1000 {
		filler = append(filler, "filler line with no anchor in it")
	}
	text := strings.Join(append(lines, filler...), "\n")
	const limit = 1500
	got := excerptLimit(text, limit)
	if n := utf8.RuneCountInString(got); n > limit {
		t.Fatalf("excerpt is %d runes, want at most %d", n, limit)
	}
	if !strings.HasPrefix(got, summary+"\n") {
		t.Fatalf("excerpt does not start with the panic summary:\n%s", got)
	}
}

func TestGoPanicExcerptWithoutPanicIsUnchanged(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "vitest failure",
			text: "FAIL src/a.test.ts\nboom",
			want: "FAIL src/a.test.ts\nboom",
		},
		{
			name: "go test failure without panic",
			text: goPanicJoin(
				"--- FAIL: TestX (0.00s)",
				"    x_test.go:9: want 1",
				"FAIL\texample.com/p\t0.01s",
			),
			want: goPanicJoin(
				"--- FAIL: TestX (0.00s)",
				"    x_test.go:9: want 1",
				"FAIL\texample.com/p\t0.01s",
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FailureExcerpt(tt.text); got != tt.want {
				t.Fatalf("FailureExcerpt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGoPanicExcerptANSIColourStillMatches(t *testing.T) {
	text := goPanicJoin(
		"\x1b[31m--- FAIL: TestQ (0.00s)\x1b[0m",
		"\x1b[31mpanic: boom\x1b[0m",
		"\x1b[31mFAIL\x1b[0m\texample.com/p\t0.01s",
	)
	const want = "TestQ panicked; later tests in example.com/p did not run"
	if got := FailureExcerpt(text); !strings.HasPrefix(got, want+"\n") {
		t.Fatalf("FailureExcerpt() does not start with %q:\n%s", want, got)
	}
}
