package statusfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccount(t *testing.T) {
	tests := []struct {
		name string
		file string // written to $CLAUDE_CONFIG_DIR/.claude.json; "" means no file
		want string
	}{
		{
			name: "organization name wins over email",
			file: `{"oauthAccount":{"organizationName":"Acme","emailAddress":"a@example.com","organizationUuid":"9f1c"}}`,
			want: "Acme",
		},
		{
			name: "blank organization falls back to trimmed email",
			file: `{"oauthAccount":{"organizationName":"  ","emailAddress":" a@example.com "}}`,
			want: "a@example.com",
		},
		{
			name: "empty oauthAccount is unknown",
			file: `{"oauthAccount":{}}`,
			want: "",
		},
		{
			name: "no oauthAccount is unknown",
			file: `{}`,
			want: "",
		},
		{
			name: "invalid JSON is unknown",
			file: `not json`,
			want: "",
		},
		{
			name: "missing file is unknown",
			file: "",
			want: "",
		},
		{
			name: "uuids alone are never the label",
			file: `{"oauthAccount":{"accountUuid":"1111","organizationUuid":"2222"}}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			configDir := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			if tt.file != "" {
				if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), []byte(tt.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := Account(); got != tt.want {
				t.Errorf("Account() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccountFileWithoutConfigDirIsHomeDotClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"a@example.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, want := AccountFile(), filepath.Join(home, ".claude.json"); got != want {
		t.Errorf("AccountFile() = %q, want %q", got, want)
	}
	if got := Account(); got != "a@example.com" {
		t.Errorf("Account() = %q, want %q", got, "a@example.com")
	}
}
