package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Save writes the file layer to path atomically (temp file + rename) with mode
// 0600, creating the parent directory with mode 0700. Providers synthesized
// from the environment are skipped, so environment secrets never reach disk.
//
// path is the config file; c is the file layer (not the effective config). It
// returns any I/O or encoding error.
func Save(path string, c Config) error {
	out := c.Clone()
	kept := out.Providers[:0]
	for _, p := range out.Providers {
		if !p.FromEnv {
			kept = append(kept, p)
		}
	}
	out.Providers = kept
	if out.Version == 0 {
		out.Version = 1
	}

	data, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("编码配置失败: %w", err)
	}
	header := "# GenImage MCP 配置文件（由应用自动保存，可手工编辑）\n# api_key 支持 ${ENV_NAME} 引用环境变量\n"
	data = append([]byte(header), data...)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}
	return nil
}

// Mask hides a secret for display: "sk-abcdefgh1234" becomes "sk-****1234".
// Short secrets become "****" and the empty string stays empty. ${ENV}
// references are shown as-is because they hold no secret.
func Mask(secret string) string {
	s := strings.TrimSpace(secret)
	switch {
	case s == "":
		return ""
	case strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}"):
		return s
	case len(s) <= 8:
		return "****"
	}
	prefix := ""
	if i := strings.Index(s, "-"); i > 0 && i <= 4 {
		prefix = s[:i+1]
	}
	return prefix + "****" + s[len(s)-4:]
}

// NewToken returns a random 32-hex-character bearer token for the MCP server.
func NewToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "gim_" + hex.EncodeToString(b[:])
}
