package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/engine"
)

// State is the server's lifecycle phase.
type State string

// Lifecycle phases shown in the GUI.
const (
	StateStopped  State = "stopped"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateError    State = "error"
)

// Status is a snapshot of the server for the GUI.
type Status struct {
	State     State
	URL       string              // full MCP endpoint, e.g. http://127.0.0.1:8765/mcp
	Addr      string              // listening address actually bound
	Server    config.ServerConfig // settings the running server started with
	StartedAt time.Time
	Requests  int64 // HTTP requests to the MCP endpoint
	ToolCalls int64 // tool invocations
	Rejected  int64 // requests refused for a bad or missing token
	LastError string
}

// Manager starts and stops the Streamable HTTP MCP server. It is safe for
// concurrent use. The GUI calls Start/Stop; status changes are reported
// through the OnChange callback.
type Manager struct {
	eng *engine.Engine
	log *slog.Logger

	mu        sync.Mutex
	srv       *http.Server
	done      chan struct{}
	state     State
	addr      string
	running   config.ServerConfig
	startedAt time.Time
	lastErr   string
	onChange  func()

	requests  atomic.Int64
	toolCalls atomic.Int64
	rejected  atomic.Int64
}

// NewManager returns a stopped manager serving eng. log may be nil.
func NewManager(eng *engine.Engine, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{eng: eng, log: log, state: StateStopped}
}

// OnChange sets a callback run after every state change and request, outside
// the manager's lock. The GUI passes a function that calls
// Invalidator.Invalidate.
func (m *Manager) OnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// Start binds the configured address and serves the MCP endpoint in the
// background. Binding happens before Start returns, so a port already in use
// is reported here as a clear error.
//
// sc holds host, port, path and the optional bearer token. A non-loopback host
// without a token is refused. It returns an error if the server is already
// running or the address cannot be bound.
func (m *Manager) Start(sc config.ServerConfig) error {
	m.mu.Lock()
	if m.state == StateRunning || m.state == StateStopping {
		m.mu.Unlock()
		return errors.New("MCP 服务已在运行")
	}
	if !config.IsLoopbackHost(sc.Host) && strings.TrimSpace(sc.AuthToken) == "" {
		m.mu.Unlock()
		return m.fail(fmt.Errorf("监听地址 %s 不是本机回环地址，必须先设置访问 Token", sc.Host))
	}
	path := sc.Path
	if path == "" {
		path = "/mcp"
	}

	srv, err := NewServer(m.eng, func(string) { m.toolCalls.Add(1); m.changed() })
	if err != nil {
		m.mu.Unlock()
		return m.fail(err)
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		MaxRequestBodyBytes: 64 << 20, // edit_image may carry base64 reference images
		Logger:              m.log,
	})
	mux := http.NewServeMux()
	mux.Handle(path, m.authMiddleware(sc.AuthToken, m.countMiddleware(mcpHandler)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","name":"gen-openai-image","version":"` + Version + `"}`))
	})

	addr := net.JoinHostPort(sc.Host, strconv.Itoa(sc.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.mu.Unlock()
		if isAddrInUse(err) {
			return m.fail(fmt.Errorf("端口 %d 已被占用，请在设置中更换端口或关闭占用该端口的程序", sc.Port))
		}
		return m.fail(fmt.Errorf("监听 %s 失败: %w", addr, err))
	}

	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	m.srv, m.done = httpSrv, done
	m.state, m.addr, m.running, m.startedAt, m.lastErr = StateRunning, ln.Addr().String(), sc, time.Now(), ""
	m.running.Path = path
	m.requests.Store(0)
	m.toolCalls.Store(0)
	m.rejected.Store(0)
	m.mu.Unlock()

	m.log.Info("MCP server started", "url", m.Status().URL)
	go func() {
		defer close(done)
		err := httpSrv.Serve(ln)
		m.mu.Lock()
		if m.srv == httpSrv {
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.state, m.lastErr = StateError, err.Error()
			} else {
				m.state = StateStopped
			}
			m.srv = nil
		}
		m.mu.Unlock()
		m.changed()
	}()
	m.changed()
	return nil
}

// Stop shuts the server down gracefully, waiting up to five seconds for
// in-flight requests. Image jobs started by tool calls belong to the engine
// and keep running; their results land in the gallery and can still be
// fetched with get_image_job after the server is started again.
//
// ctx bounds the wait. It returns nil when the server was not running.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	srv, done := m.srv, m.done
	if srv == nil {
		m.mu.Unlock()
		return nil
	}
	m.state = StateStopping
	m.mu.Unlock()
	m.changed()

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := srv.Shutdown(sctx)
	if err != nil {
		_ = srv.Close()
	}
	<-done
	m.mu.Lock()
	m.state, m.srv = StateStopped, nil
	m.mu.Unlock()
	m.log.Info("MCP server stopped")
	m.changed()
	return nil
}

// Restart stops the server if it is running and starts it with sc.
func (m *Manager) Restart(ctx context.Context, sc config.ServerConfig) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return m.Start(sc)
}

// Status returns a snapshot of the server state and counters.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		State: m.state, Addr: m.addr, Server: m.running, StartedAt: m.startedAt,
		Requests: m.requests.Load(), ToolCalls: m.toolCalls.Load(), Rejected: m.rejected.Load(),
		LastError: m.lastErr,
	}
	if m.state == StateRunning || m.state == StateStopping {
		st.URL = EndpointURL(m.running)
	}
	return st
}

// Running reports whether the server is serving.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == StateRunning
}

// EndpointURL builds the MCP endpoint URL a client should use for sc. A
// wildcard listen address (0.0.0.0 / ::) is shown as 127.0.0.1.
func EndpointURL(sc config.ServerConfig) string {
	host := sc.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	path := sc.Path
	if path == "" {
		path = "/mcp"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(sc.Port)) + path
}

// fail records err as the last error, marks the error state and notifies.
func (m *Manager) fail(err error) error {
	m.mu.Lock()
	m.state, m.lastErr = StateError, err.Error()
	m.mu.Unlock()
	m.log.Error("MCP server error", "error", err.Error())
	m.changed()
	return err
}

// changed runs the OnChange callback.
func (m *Manager) changed() {
	m.mu.Lock()
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// authMiddleware requires "Authorization: Bearer <token>" when token is set,
// comparing in constant time. An empty token disables the check (loopback
// only, enforced by Start).
func (m *Manager) authMiddleware(token string, next http.Handler) http.Handler {
	token = strings.TrimSpace(token)
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			m.rejected.Add(1)
			m.log.Warn("rejected MCP request: bad or missing token", "remote", r.RemoteAddr)
			m.changed()
			w.Header().Set("WWW-Authenticate", `Bearer realm="gen-openai-image"`)
			http.Error(w, `{"error":"unauthorized: missing or invalid bearer token"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// countMiddleware counts requests to the MCP endpoint for the status page.
func (m *Manager) countMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests.Add(1)
		m.changed()
		next.ServeHTTP(w, r)
	})
}
