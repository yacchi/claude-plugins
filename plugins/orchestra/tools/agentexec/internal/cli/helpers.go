package cli

import (
	"io"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// loadConfig resolves the config layers and annotates executor availability.
// A bad layer is exit 1 with the layer's error (Python printed it verbatim).
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

// writeJSON prints v as Python's json.dumps(v, indent=2) plus a newline.
func writeJSON(w io.Writer, v any) error {
	b, err := jsonx.MarshalIndent(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// writeJSONLine prints v as one line (Python's json.dumps(v)) plus a newline.
func writeJSONLine(w io.Writer, v any) error {
	b, err := jsonx.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
