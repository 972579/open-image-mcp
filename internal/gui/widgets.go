package gui

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/vivaui/viva/gfx"
	"github.com/vivaui/viva/ui"
)

// scaffold declares a scrolling page with a title, a muted subtitle and the
// body below them.
func scaffold(c ui.C, title, subtitle string, body func()) {
	ui.Scroll(c).Grow(1).FillWidth().Children(func() {
		ui.Column(c).FillWidth().Pad(24).Gap(16).Children(func() {
			ui.Column(c).Gap(4).Children(func() {
				ui.Text(c, title).FontSize(22).Weight(gfx.WeightSemibold)
				if subtitle != "" {
					ui.Paragraph(c, subtitle, 3).Muted()
				}
			})
			body()
		})
	})
}

// card declares a bordered surface with an optional heading.
func card(c ui.C, title string, body func()) ui.Box {
	theme := c.Theme()
	return ui.Column(c).FillWidth().Pad(16).Gap(12).Radius(theme.Metrics.RadiusSurface).
		Background(theme.Card).Border(1, theme.Border).Children(func() {
		if title != "" {
			ui.Text(c, title).Weight(gfx.WeightMedium)
		}
		body()
	})
}

// field declares a labeled form row: the title, the control(s) from body, and
// an optional muted hint under them.
func field(c ui.C, title, hint string, body func()) {
	theme := c.Theme()
	ui.Column(c).FillWidth().Gap(6).Children(func() {
		ui.Text(c, title).FontSize(13).Weight(gfx.WeightMedium)
		body()
		if hint != "" {
			ui.Paragraph(c, hint, 3).Muted().FontSize(theme.Metrics.FontSizeS)
		}
	})
}

// tone is a badge color.
type tone uint8

const (
	toneNeutral tone = iota
	toneAccent
	toneSuccess
	toneDanger
	toneWarning
)

// badge declares a small pill label.
func badge(c ui.C, t tone, text string) ui.Box {
	theme := c.Theme()
	bg, fg := theme.Secondary, theme.SecondaryForeground
	switch t {
	case toneAccent:
		bg, fg = theme.Primary, theme.PrimaryForeground
	case toneSuccess:
		bg, fg = theme.Success.WithAlpha(0.14), theme.Success
	case toneDanger:
		bg, fg = theme.Danger.WithAlpha(0.12), theme.Danger
	case toneWarning:
		bg, fg = theme.Chart[2].WithAlpha(0.16), theme.Chart[2]
	}
	return ui.Text(c, text).FontSize(theme.Metrics.FontSizeS).Weight(gfx.WeightMedium).Color(fg).
		Background(bg).PadX(8).PadY(2).Radius(gfx.RadiusFull).Shrink(0)
}

// codeBlock declares monospace lines on a muted surface. lines should be
// cached by the caller so the page does not split text on every paint.
func codeBlock(c ui.C, lines []string) {
	theme := c.Theme()
	ui.Column(c).FillWidth().Pad(12).Gap(2).Radius(theme.Metrics.RadiusControl).
		Background(theme.Muted).Border(1, theme.Border).Children(func() {
		for _, line := range lines {
			ui.Wrap(c, line).Mono().FontSize(12)
		}
	})
}

// splitLines splits text for codeBlock, showing empty lines as a space so they
// keep their height.
func splitLines(s string) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = " "
		}
	}
	return lines
}

// isImagePath reports whether a path has an image extension the app accepts.
func isImagePath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	}
	return false
}

// shortPath abbreviates the home directory as ~ for display.
func shortPath(p string) string {
	if home := homeDir(); home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// statPath is os.Stat, named for readability at call sites.
func statPath(p string) (os.FileInfo, error) { return os.Stat(p) }

// parentDir returns the nearest existing ancestor directory of p, so revealing
// a config file that does not exist yet never creates folders belonging to
// another application.
func parentDir(p string) string {
	dir := filepath.Dir(p)
	for dir != "/" && dir != "." {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return dir
}
