// Package appcore wires configuration, logging, the engine and the MCP server
// manager together. Both the GUI and the headless serve command start from it.
package appcore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/engine"
	"github.com/972579/open-image-mcp/internal/imageapi"
	"github.com/972579/open-image-mcp/internal/logx"
	"github.com/972579/open-image-mcp/internal/mcpserver"
	"github.com/972579/open-image-mcp/internal/storage"
)

// Core holds the long-lived services. Its methods are safe for concurrent use.
type Core struct {
	Opts    config.LoadOptions
	Engine  *engine.Engine
	Manager *mcpserver.Manager
	Ring    *logx.Ring
	Log     *slog.Logger

	mu        sync.Mutex
	loaded    *config.Loaded
	logCloser io.Closer
}

// Open loads the configuration and builds every service. The MCP server is
// not started.
//
// opts are the startup flags and environment; echoLogs also writes log lines
// to stderr (used by the serve command). It returns an error when the config
// file exists but cannot be parsed.
func Open(opts config.LoadOptions, echoLogs bool) (*Core, error) {
	loaded, err := config.Load(opts)
	if err != nil {
		return nil, err
	}
	ring := logx.NewRing(500)
	log, closer := logx.Setup(config.LogDir(), ring, echoLogs)
	if err := loaded.Effective.Validate(); err != nil {
		log.Warn("configuration has problems", "detail", err.Error())
	}
	eff := loaded.Effective
	store := storage.New(config.HistoryPath())
	eng := engine.New(&eff, imageapi.New(nil), store, log)
	c := &Core{
		Opts: opts, Engine: eng, Manager: mcpserver.NewManager(eng, log),
		Ring: ring, Log: log, loaded: loaded, logCloser: closer,
	}
	log.Info("configuration loaded", "path", loaded.Path, "exists", loaded.Exists, "providers", len(eff.Providers))
	return c, nil
}

// Loaded returns the current layered configuration. Callers must not modify it.
func (c *Core) Loaded() *config.Loaded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaded
}

// SaveFile validates an edited file layer, writes it to disk (0600) and makes
// the result effective in the engine at once. The running MCP server keeps
// serving; only host/port/path/token changes need a restart, which
// NeedsRestart reports.
//
// file is the edited file layer. It returns the validation or write error and
// leaves the previous configuration in place on failure.
func (c *Core) SaveFile(file config.Config) error {
	c.mu.Lock()
	prev := c.loaded
	c.mu.Unlock()
	next := config.Resolve(file, prev, c.Opts)
	if err := next.Effective.Validate(); err != nil {
		return err
	}
	if err := config.Save(prev.Path, next.File); err != nil {
		return err
	}
	eff := next.Effective
	c.Engine.Reload(&eff)
	c.mu.Lock()
	c.loaded = next
	c.mu.Unlock()
	c.Log.Info("configuration saved", "path", prev.Path, "providers", len(eff.Providers))
	return nil
}

// NeedsRestart reports whether the running server's listen settings differ
// from the effective configuration.
func (c *Core) NeedsRestart() bool {
	st := c.Manager.Status()
	if st.State != mcpserver.StateRunning {
		return false
	}
	want := c.Loaded().Effective.Server
	have := st.Server
	return want.Host != have.Host || want.Port != have.Port || want.Path != have.Path || want.AuthToken != have.AuthToken
}

// StartServer starts the MCP server with the effective server settings.
func (c *Core) StartServer() error {
	return c.Manager.Start(c.Loaded().Effective.Server)
}

// RestartServer restarts the MCP server with the effective server settings.
func (c *Core) RestartServer() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Manager.Restart(ctx, c.Loaded().Effective.Server)
}

// Close stops the server, cancels the engine's background image jobs (work
// still running is lost; images already saved stay in the gallery) and closes
// the log file. There is no return value.
func (c *Core) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := c.Manager.Stop(ctx); err != nil {
		c.Log.Warn("stop server", "error", err.Error())
	}
	c.Engine.Close()
	if c.logCloser != nil {
		_ = c.logCloser.Close()
	}
}

// Describe returns a one-line summary for the serve command's banner.
func (c *Core) Describe() string {
	eff := c.Loaded().Effective
	return fmt.Sprintf("config=%s providers=%d default_model=%s output_dir=%s", c.Loaded().Path, len(eff.Providers), eff.Defaults.Model, eff.Defaults.OutputDir)
}
