package clientcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSnippets checks every JSON snippet parses and the token is wired in.
func TestSnippets(t *testing.T) {
	info := Info{Name: "gen-image", URL: "http://127.0.0.1:8765/mcp", Token: "gim_abc"}
	for _, s := range All(info) {
		if s.Language == "json" {
			var v map[string]any
			if err := json.Unmarshal([]byte(s.Body), &v); err != nil {
				t.Errorf("%s: invalid JSON: %v\n%s", s.Client, err, s.Body)
			}
		}
		if !strings.Contains(s.Body, info.URL) {
			t.Errorf("%s: URL missing", s.Client)
		}
	}
	cd := ClaudeDesktop(info)
	if !strings.Contains(cd.Body, `"mcp-remote"`) || !strings.Contains(cd.Body, "Bearer gim_abc") {
		t.Errorf("claude desktop snippet:\n%s", cd.Body)
	}
	cx := Codex(info)
	if !strings.Contains(cx.Body, "[mcp_servers.gen-image]") || !strings.Contains(cx.Body, `bearer_token_env_var = "GENIMAGE_TOKEN"`) {
		t.Errorf("codex snippet:\n%s", cx.Body)
	}
	noTok := Codex(Info{Name: "gen-image", URL: info.URL})
	if strings.Contains(noTok.Body, "bearer") {
		t.Error("codex snippet without token should not mention bearer")
	}
}

// TestWindowsSnippets checks Windows paths and the cmd /c npx launcher.
func TestWindowsSnippets(t *testing.T) {
	info := Info{Name: "gen-image", URL: "http://127.0.0.1:8765/mcp", OS: "windows"}
	cd := ClaudeDesktop(info)
	if cd.File != `%APPDATA%\Claude\claude_desktop_config.json` {
		t.Errorf("windows Claude path = %q", cd.File)
	}
	if !strings.Contains(cd.Body, `"command": "cmd"`) || !strings.Contains(cd.Body, `"/c"`) {
		t.Errorf("windows Claude snippet should launch npx via cmd /c:\n%s", cd.Body)
	}
	if got := Codex(info).File; got != `%USERPROFILE%\.codex\config.toml` {
		t.Errorf("windows Codex path = %q", got)
	}
	mac := ClaudeDesktop(Info{Name: "gen-image", URL: info.URL, OS: "darwin"})
	if !strings.Contains(mac.Body, `"command": "npx"`) || !strings.HasPrefix(mac.File, "~/Library") {
		t.Errorf("mac snippet wrong: %s %s", mac.File, mac.Body)
	}
}
