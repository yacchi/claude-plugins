package cli

import (
	"errors"
	"math"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// flagValue is the value behind a cobra flag.
type flagValue interface {
	String() string
	Set(string) error
	Type() string
}

// onceValue makes a second occurrence of a single-valued flag a usage error
// instead of silently keeping the last value: `--path a --path b` or
// `--tasks a --tasks b` is far more likely a mistake than an override.
type onceValue struct {
	flagValue
	set bool
}

func (o *onceValue) Set(s string) error {
	if o.set {
		return errors.New("given more than once")
	}
	o.set = true
	return o.flagValue.Set(s)
}

// rejectRepeats guards, with onceValue, every single-valued flag of the
// command args select that args mention. Presence switches and repeatable
// (array) flags are left alone. Call it before executing args.
func rejectRepeats(root *cobra.Command, args []string) {
	cmd, _, err := root.Find(args)
	if err != nil || cmd == nil {
		return
	}
	for _, a := range args {
		if a == "--" {
			return
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := cmd.Flags().Lookup(name)
		if f == nil && !strings.HasPrefix(a, "--") && len(name) == 1 {
			f = cmd.Flags().ShorthandLookup(name)
		}
		if f == nil {
			continue
		}
		if _, done := f.Value.(*onceValue); done {
			continue
		}
		switch f.Value.Type() {
		case "bool", "stringArray", "stringSlice":
			continue
		}
		f.Value = &onceValue{flagValue: f.Value}
	}
}

// format is a command's set of output switches (--json/--text, or
// --json/--line/--text); at most one may be given.
type format map[string]*bool

func formatFlags(cmd *cobra.Command, names ...string) format {
	f := format{}
	for _, n := range names {
		f[n] = cmd.Flags().Bool(n, false, "output as "+n)
	}
	if len(names) > 1 {
		cmd.MarkFlagsMutuallyExclusive(names...)
	}
	return f
}

// is reports whether --name was passed.
func (f format) is(name string) bool {
	p := f[name]
	return p != nil && *p
}

// finiteNonNegative rejects the values of a float flag that make no sense as
// a count of seconds, minutes or days.
func finiteNonNegative(v float64) bool {
	return v >= 0 && !math.IsInf(v, 0)
}

// duration is v units, saturating instead of overflowing for huge values.
func duration(v float64, unit time.Duration) time.Duration {
	limit := float64(math.MaxInt64) / float64(unit)
	if v >= limit {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(v * float64(unit))
}
