package pilogin

import (
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

const (
	codexFiltered = " Select provider to configure:\n> OpenAI Codex\n" +
		" → OpenAI Codex (legacy) • not configured\n"
	otherFiltered = " Select provider to configure:\n> OpenAI Codex\n" +
		" → OpenAI • not configured\n   OpenAI Codex (legacy) • not configured\n"
	deviceScreen = " https://auth.openai.com/codex/device\n Enter code: ABCD-1234\n" +
		" Waiting for authentication...\n"
	wrappedURL = " Open this URL to sign in:\n" +
		" https://auth.example.com/oauth/authorize?response_type=code&client_id=a\n" +
		" bc&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&state=xyz\n" +
		" Cmd+click to open\n" +
		" Waiting for authentication...\n paste the final redirect URL here:\n"
	wrappedExpected = "https://auth.example.com/oauth/authorize?response_type=code&client_id=a" +
		"bc&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&state=xyz"
)

func TestWrappedURLJoinedWithoutSpacesOrCmdClick(t *testing.T) {
	got, ok := ExtractURL(wrappedURL)
	if !ok || got != wrappedExpected {
		t.Errorf("ExtractURL = %q, %v", got, ok)
	}
}

func TestDeviceScreen(t *testing.T) {
	if got, ok := ExtractURL(deviceScreen); !ok || got != "https://auth.openai.com/codex/device" {
		t.Errorf("ExtractURL = %q, %v", got, ok)
	}
	if got, ok := ExtractCode(deviceScreen); !ok || got != "ABCD-1234" {
		t.Errorf("ExtractCode = %q, %v", got, ok)
	}
	if _, ok := ExtractURL(" no link here\n"); ok {
		t.Error("a URL was found in a screen without one")
	}
	if _, ok := ExtractCode(" https://x\n"); ok {
		t.Error("a code was found in a screen without one")
	}
}

func TestHighlightIgnoresAnnotations(t *testing.T) {
	for screen, want := range map[string]string{
		codexFiltered: "OpenAI Codex",
		otherFiltered: "OpenAI",
		" → Browser login (default)\n   Device code login (headless)\n": "Browser login",
		"   a\n ❯ Sign in with an account\n":                            "Sign in with an account",
	} {
		if got, ok := Highlighted(screen); !ok || got != want {
			t.Errorf("Highlighted(%q) = %q, %v; want %q", screen, got, ok, want)
		}
	}
	if _, ok := Highlighted("nothing selected\n"); ok {
		t.Error("a selection was found where there is none")
	}
}

func TestWantsPaste(t *testing.T) {
	if !WantsPaste(wrappedURL) {
		t.Error("the wrapped URL screen asks for a paste")
	}
	if WantsPaste(deviceScreen) {
		t.Error("the device screen does not")
	}
}

func TestScreenTextPrefersTheJSONPayload(t *testing.T) {
	payload, err := jsonx.Decode([]byte(`{"result": {"text": "from payload"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := ScreenText("raw", payload.(*jsonx.Object)); got != "from payload" {
		t.Errorf("ScreenText = %q", got)
	}
	if got := ScreenText("raw", jsonx.NewObject()); got != "raw" {
		t.Errorf("ScreenText = %q", got)
	}
}

func TestTail(t *testing.T) {
	if got := Tail("a  \nb\n\nc   \n", 2); got != "c" {
		t.Errorf("Tail = %q", got)
	}
	if got := Tail("a  \nb\nc", 25); got != "a\nb\nc" {
		t.Errorf("Tail = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"": "''", "plain-1/x.y": "plain-1/x.y", "a b": "'a b'", "it's": `'it'"'"'s'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
