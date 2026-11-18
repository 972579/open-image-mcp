// Package logx sets up the application logger: slog text lines written to a
// log file and kept in an in-memory ring for the GUI, with secrets masked.
package logx

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/imageapi"
)

// Ring keeps the most recent log lines in memory. It is safe for concurrent
// use; the GUI reads a snapshot and the version tells it when to re-read.
type Ring struct {
	mu      sync.Mutex
	lines   []string
	max     int
	version uint64
	notify  func()
	partial []byte
}

// NewRing returns a ring holding up to max lines.
func NewRing(max int) *Ring { return &Ring{max: max} }

// OnAppend sets a callback run (outside the lock) after lines are added; the
// GUI passes its Invalidator so the log view repaints.
func (r *Ring) OnAppend(fn func()) {
	r.mu.Lock()
	r.notify = fn
	r.mu.Unlock()
}

// Write implements io.Writer, splitting the input into lines.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.partial = append(r.partial, p...)
	added := false
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			break
		}
		r.lines = append(r.lines, string(r.partial[:i]))
		r.partial = r.partial[i+1:]
		added = true
	}
	if over := len(r.lines) - r.max; over > 0 {
		r.lines = append(r.lines[:0:0], r.lines[over:]...)
	}
	if added {
		r.version++
	}
	notify := r.notify
	r.mu.Unlock()
	if added && notify != nil {
		notify()
	}
	return len(p), nil
}

// Snapshot returns a copy of the lines and the current version.
func (r *Ring) Snapshot() ([]string, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...), r.version
}

// Version returns a counter that grows whenever lines are added.
func (r *Ring) Version() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.version
}

// redactWriter masks API-key-looking text in every line before passing it on.
type redactWriter struct{ w io.Writer }

// Write masks secrets in p and writes the result; it reports len(p) on success
// so callers see their own byte count.
func (rw redactWriter) Write(p []byte) (int, error) {
	if _, err := rw.w.Write([]byte(imageapi.RedactText(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// sensitiveKeys are attribute names whose values are always masked.
var sensitiveKeys = []string{"api_key", "apikey", "authorization", "token", "secret", "password"}

// replaceAttr masks the values of sensitive attributes.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return slog.String(a.Key, config.Mask(a.Value.String()))
		}
	}
	return a
}

// Setup creates the logger. Lines go to <dir>/app.log (rotated to app.log.1
// once it passes 5 MiB at startup), to the ring, and to stderr when echo is
// true (the headless serve command).
//
// dir is the log directory; ring receives lines for the GUI; echo also writes
// to stderr. It returns the logger and a closer for the log file. A log file
// that cannot be opened is skipped rather than failing startup.
func Setup(dir string, ring *Ring, echo bool) (*slog.Logger, io.Closer) {
	var writers []io.Writer
	var closer io.Closer = io.NopCloser(nil)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		path := filepath.Join(dir, "app.log")
		if info, err := os.Stat(path); err == nil && info.Size() > 5<<20 {
			_ = os.Rename(path, path+".1")
		}
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			writers = append(writers, f)
			closer = f
		}
	}
	if ring != nil {
		writers = append(writers, ring)
	}
	if echo {
		writers = append(writers, os.Stderr)
	}
	out := redactWriter{w: io.MultiWriter(writers...)}
	h := slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: replaceAttr})
	return slog.New(h), closer
}
