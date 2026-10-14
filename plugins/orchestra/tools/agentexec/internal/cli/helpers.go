package cli

import (
	"io"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// loadConfig resolves the config layers and annotates executor availability.
// A bad layer is exit 1 with the layer's error.
func loadConfig() (*config.Resolved, error) {
	cfg, err := config.Resolve()
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: err.Error()}
	}
	tools.MarkAvailability(cfg)
	return cfg, nil
}

// configOrDefaults is loadConfig for commands that keep working on a broken
// config layer: they fall back to the built-in defaults.
func configOrDefaults() *config.Resolved {
	cfg, err := loadConfig()
	if err != nil {
		return &config.Resolved{Values: config.Defaults(), Root: config.ProjectBase()}
	}
	return cfg
}

// writeJSON prints v as indented JSON plus a newline.
func writeJSON(w io.Writer, v any) error {
	b, err := fsutil.MarshalJSONIndent(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// writeJSONLine prints v as one line of JSON plus a newline.
func writeJSONLine(w io.Writer, v any) error {
	b, err := fsutil.MarshalJSON(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
