// Package clientcfg renders the MCP configuration snippets users paste into
// Claude Desktop, Codex, Claude Code, Cursor and Cherry Studio, from the
// server's current URL and token.
package clientcfg

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

// Info is what every snippet needs.
type Info struct {
	Name  string // server key in client configs, e.g. "gen-image"
	URL   string // MCP endpoint, e.g. http://127.0.0.1:8765/mcp
	Token string // bearer token, "" when auth is off
	// OS is the platform the snippets are for ("darwin" or "windows"); empty
	// means the one this program runs on. It decides config file locations
	// and how Claude Desktop launches npx.
	OS string
}

// windows reports whether the snippets target Windows.
func (i Info) windows() bool {
	if i.OS == "" {
		return runtime.GOOS == "windows"
	}
	return i.OS == "windows"
}

// configPath returns where a client keeps its config on the target OS:
// macOS paths use ~, Windows paths use %APPDATA% / %USERPROFILE%.
func (i Info) configPath(client string) string {
	if i.windows() {
		switch client {
		case "claude":
			return `%APPDATA%\Claude\claude_desktop_config.json`
		case "codex":
			return `%USERPROFILE%\.codex\config.toml`
		case "cursor":
			return `%USERPROFILE%\.cursor\mcp.json`
		}
	}
	switch client {
	case "claude":
		return "~/Library/Application Support/Claude/claude_desktop_config.json"
	case "codex":
		return "~/.codex/config.toml"
	case "cursor":
		return "~/.cursor/mcp.json"
	}
	return ""
}

// Snippet is one client's configuration.
type Snippet struct {
	Client   string // display name
	File     string // where the client keeps its config ("" for CLI-only)
	Language string // json, toml or shell, for display
	Body     string
	Notes    []string
}

// All returns the snippets for every supported client, in display order.
func All(info Info) []Snippet {
	if info.Name == "" {
		info.Name = "gen-image"
	}
	return []Snippet{ClaudeDesktop(info), Codex(info), ClaudeCode(info), Cursor(info), CherryStudio(info)}
}

// ClaudeDesktop bridges the local HTTP endpoint through mcp-remote, because
// claude_desktop_config.json only launches stdio commands. The token travels
// through an env var so the header value has no spaces in args. On Windows
// npx is a .cmd script, so it is started through "cmd /c".
func ClaudeDesktop(info Info) Snippet {
	args := []string{"-y", "mcp-remote", info.URL}
	if !isLocal(info.URL) {
		args = append(args, "--allow-http")
	}
	command := "npx"
	if info.windows() {
		command, args = "cmd", append([]string{"/c", "npx"}, args...)
	}
	server := struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env,omitempty"`
	}{Command: command, Args: args}
	if info.Token != "" {
		server.Args = append(server.Args, "--header", "Authorization:${GENIMAGE_AUTH}")
		server.Env = map[string]string{"GENIMAGE_AUTH": "Bearer " + info.Token}
	}
	body := map[string]any{"mcpServers": map[string]any{info.Name: server}}
	return Snippet{
		Client:   "Claude Desktop",
		File:     info.configPath("claude"),
		Language: "json",
		Body:     mustJSON(body),
		Notes: []string{
			"需要安装 Node.js（npx 会自动下载 mcp-remote 作为本地 HTTP 桥接）。",
			"把 mcpServers 下的条目合并进已有配置，保存后完全退出并重新打开 Claude Desktop。",
			"Claude Desktop 对单次工具调用有超时限制，较慢的模型可能超时；本服务在生成期间会持续发送进度通知。",
		},
	}
}

// Codex uses Streamable HTTP natively, configured in TOML.
func Codex(info Info) Snippet {
	var b strings.Builder
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlKey(info.Name))
	fmt.Fprintf(&b, "url = %q\n", info.URL)
	if info.Token != "" {
		b.WriteString("bearer_token_env_var = \"GENIMAGE_TOKEN\"\n")
	}
	b.WriteString("tool_timeout_sec = 300\n")
	notes := []string{
		"Codex 的配置是 TOML 格式（不是 JSON），追加到 " + info.configPath("codex") + " 即可。",
		fmt.Sprintf("也可以用命令添加：codex mcp add %s --url %s", info.Name, info.URL),
		"tool_timeout_sec 调大到 300 秒，避免生图较慢时超时。",
	}
	if info.Token != "" {
		if info.windows() {
			notes = append(notes, "启动 Codex 前在 PowerShell 中设置：$env:GENIMAGE_TOKEN=\""+info.Token+"\"")
		} else {
			notes = append(notes, "启动 Codex 前在 shell 中设置：export GENIMAGE_TOKEN="+info.Token)
		}
	}
	return Snippet{Client: "Codex", File: info.configPath("codex"), Language: "toml", Body: b.String(), Notes: notes}
}

// ClaudeCode is a one-line CLI registration.
func ClaudeCode(info Info) Snippet {
	cmd := fmt.Sprintf("claude mcp add --transport http %s %s", info.Name, info.URL)
	if info.Token != "" {
		cmd += fmt.Sprintf(" --header \"Authorization: Bearer %s\"", info.Token)
	}
	return Snippet{
		Client: "Claude Code", Language: "shell", Body: cmd,
		Notes: []string{"在终端执行；加 --scope user 可让所有项目共用。"},
	}
}

// Cursor reads remote servers from ~/.cursor/mcp.json (%USERPROFILE% on Windows).
func Cursor(info Info) Snippet {
	server := map[string]any{"url": info.URL}
	if info.Token != "" {
		server["headers"] = map[string]string{"Authorization": "Bearer " + info.Token}
	}
	return Snippet{
		Client: "Cursor", File: info.configPath("cursor"), Language: "json",
		Body:  mustJSON(map[string]any{"mcpServers": map[string]any{info.Name: server}}),
		Notes: []string{"保存后在 Cursor 设置的 MCP 页面确认服务为绿色。"},
	}
}

// CherryStudio is configured in its UI; the JSON matches its import format.
func CherryStudio(info Info) Snippet {
	server := map[string]any{"type": "streamableHttp", "baseUrl": info.URL, "name": info.Name, "isActive": true}
	if info.Token != "" {
		server["headers"] = map[string]string{"Authorization": "Bearer " + info.Token}
	}
	return Snippet{
		Client: "Cherry Studio", Language: "json",
		Body: mustJSON(map[string]any{"mcpServers": map[string]any{info.Name: server}}),
		Notes: []string{
			"设置 → MCP 服务器 → 添加服务器 → 类型选「可流式传输的 HTTP (streamableHttp)」，填入 URL。",
			"或使用「从 JSON 导入」粘贴上面的内容。",
		},
	}
}

// mustJSON pretty-prints v with two-space indentation and no HTML escaping.
func mustJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	return strings.TrimRight(b.String(), "\n")
}

// isLocal reports whether url points at localhost, where mcp-remote allows
// plain HTTP without --allow-http.
func isLocal(url string) bool {
	return strings.HasPrefix(url, "http://127.0.0.1") || strings.HasPrefix(url, "http://localhost") || strings.HasPrefix(url, "http://[::1]")
}

// tomlKey quotes a TOML table key when it contains characters bare keys forbid.
func tomlKey(k string) string {
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fmt.Sprintf("%q", k)
		}
	}
	return k
}
