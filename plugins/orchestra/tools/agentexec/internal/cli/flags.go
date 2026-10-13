package cli

import (
	"strconv"

	"github.com/spf13/cobra"
)

// optString is a value flag that remembers whether, and how often, it was
// given: the Python parsers distinguish "absent" from "empty" and several
// reject a repeated flag ("duplicate option: --x"). Values are kept as text;
// each command converts and reports bad values in its own wording.
type optString struct {
	val   string
	count int
}

func (o *optString) String() string { return o.val }
func (o *optString) Type() string   { return "string" }
func (o *optString) Set(s string) error {
	o.val = s
	o.count++
	return nil
}

// Get is the last value given and whether the flag was given at all.
func (o *optString) Get() (string, bool) { return o.val, o.count > 0 }

// Or is the value, or def when the flag was not given.
func (o *optString) Or(def string) string {
	if o.count == 0 {
		return def
	}
	return o.val
}

func (o *optString) Count() int { return o.count }

// optBool is a presence flag (never takes a value) with an occurrence count.
type optBool struct{ count int }

func (o *optBool) String() string { return strconv.FormatBool(o.count > 0) }
func (o *optBool) Type() string   { return "bool" }
func (o *optBool) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	if b {
		o.count++
	}
	return nil
}
func (o *optBool) On() bool   { return o.count > 0 }
func (o *optBool) Count() int { return o.count }

func strFlag(cmd *cobra.Command, name, usage string) *optString {
	v := &optString{}
	cmd.Flags().Var(v, name, usage)
	return v
}

func shortStrFlag(cmd *cobra.Command, name, short, usage string) *optString {
	v := &optString{}
	cmd.Flags().VarP(v, name, short, usage)
	return v
}

func boolFlag(cmd *cobra.Command, name, usage string) *optBool {
	v := &optBool{}
	cmd.Flags().Var(v, name, usage)
	cmd.Flags().Lookup(name).NoOptDefVal = "true"
	return v
}

// listFlag collects every occurrence in order (a repeatable flag such as
// --prompt-file); values are never split on commas.
func listFlag(cmd *cobra.Command, name, usage string) *[]string {
	var v []string
	cmd.Flags().StringArrayVar(&v, name, nil, usage)
	return &v
}

// format is the --json/--text (or --json/--line/--text) choice of a command.
type format struct {
	flags map[string]*optBool
	order []string
}

func formatFlags(cmd *cobra.Command, names ...string) *format {
	f := &format{flags: map[string]*optBool{}, order: names}
	for _, n := range names {
		f.flags[n] = boolFlag(cmd, n, "output as "+n)
	}
	return f
}

// Given reports whether --name was passed.
func (f *format) Given(name string) bool {
	b, ok := f.flags[name]
	return ok && b.On()
}

// exclusive is the Python "--json and --text are mutually exclusive" check.
func (f *format) exclusive(cmd *cobra.Command) error {
	var on []string
	for _, n := range f.order {
		if f.flags[n].On() {
			on = append(on, "--"+n)
		}
	}
	if len(on) > 1 {
		return exitf(2, "%s: %s and %s are mutually exclusive", errPrefix(cmd), on[0], on[1])
	}
	return nil
}
