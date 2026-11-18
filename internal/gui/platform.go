package gui

import (
	"os"
	"regexp"
	"runtime"
	"strings"
)

// fileManager names the system file browser in labels: Finder on macOS,
// 资源管理器 (Explorer) on Windows.
var fileManager = func() string {
	if runtime.GOOS == "windows" {
		return "资源管理器"
	}
	return "Finder"
}()

// Button labels that name the file browser, built once at startup.
var (
	labelReveal  = "在 " + fileManager + " 中显示"
	labelOpenDir = "在 " + fileManager + " 中打开"
)

var winEnvRef = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)

// expandClientPath turns a displayed client config path into a real one:
// a leading ~ becomes the home directory and Windows %VAR% references are
// expanded (unknown variables are left as written).
func expandClientPath(p string) string {
	if strings.HasPrefix(p, "~") {
		p = homeDir() + p[1:]
	}
	return winEnvRef.ReplaceAllStringFunc(p, func(ref string) string {
		if v := os.Getenv(strings.Trim(ref, "%")); v != "" {
			return v
		}
		return ref
	})
}
