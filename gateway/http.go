package gateway

// Streamable HTTP transport (the modern MCP HTTP binding): the host POSTs
// JSON-RPC frames to a single endpoint and reads the response as
// application/json; notifications are answered with 202. Sessions are
// keyed by the Mcp-Session-Id header issued at initialize, so each host
// keeps its own Conn (and its own read cursor) across requests.
//
// There are no server-initiated streams (capabilities declare
// tools.listChanged=false, no resources/prompts), so GET returns 405 and
// no SSE is used. DELETE drops a session. Bearer-token auth is enforced
// whenever a token is configured — and is mandatory for non-loopback
// listeners (checked in NewHTTPTransport).

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sessionHdr    = "Mcp-Session-Id"
	maxRequestLen = 1 << 20 // 1 MiB of JSON-RPC is plenty for these tools
)

// HTTPOptions configures the Streamable HTTP listener.
type HTTPOptions struct {
	// Listen is the host:port to bind. Default "127.0.0.1:8788".
	Listen string
	// Path is the MCP endpoint path. Default "/mcp".
	Path string
	// Token is the required bearer token. Optional on loopback listeners,
	// mandatory otherwise.
	Token string
}

// HTTPTransport serves the MCP tools over Streamable HTTP.
type HTTPTransport struct {
	srv    *Server
	listen string
	path   string
	token  string
	log    *slog.Logger

	mu    sync.Mutex
	conns map[string]*Conn // session id → connection
	addr  string           // resolved bind address (port 0 → real port)
}

// NewHTTPTransport validates the options. A non-loopback Listen without a
// Token is rejected: the tools can send radio traffic, so an open listener
// must never be exposed beyond the loopback.
func NewHTTPTransport(server *Server, opts HTTPOptions, logger *slog.Logger) (*HTTPTransport, error) {
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:8788"
	}
	if opts.Path == "" {
		opts.Path = "/mcp"
	}
	if !strings.HasPrefix(opts.Path, "/") {
		opts.Path = "/" + opts.Path
	}
	host, _, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("http transport: listen must be host:port, got %q", opts.Listen)
	}
	if !isLoopbackHost(host) && opts.Token == "" {
		return nil, fmt.Errorf("http transport: token is required when listening on a non-loopback address %q", opts.Listen)
	}
	return &HTTPTransport{
		srv:    server,
		listen: opts.Listen,
		path:   opts.Path,
		token:  opts.Token,
		conns:  make(map[string]*Conn),
		log:    logger,
	}, nil
}

func isLoopbackHost(host string) bool {
	h := strings.Trim(host, "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasPrefix(h, "127.")
}

// Addr is the resolved bind address — meaningful after Run has bound the
// listener (a configured port of 0 gets replaced by the real port).
func (t *HTTPTransport) Addr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.addr
}

// Run serves until ctx is cancelled. A bind failure is returned immediately.
func (t *HTTPTransport) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(t.path, t.handle)
	httpSrv := &http.Server{Handler: mux} // no read timeout: a tool call may block ~30s

	ln, err := net.Listen("tcp", t.listen)
	if err != nil {
		return fmt.Errorf("http transport: listening on %s: %w", t.listen, err)
	}
	t.mu.Lock()
	t.addr = ln.Addr().String()
	t.mu.Unlock()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	t.log.Info("HTTP transport listening", "addr", t.listen, "path", t.path)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-serveErr:
		if err == http.ErrServerClosed {
			return nil
		}
		return fmt.Errorf("http transport: %w", err)
	}
}

// handle is the single MCP endpoint.
func (t *HTTPTransport) handle(w http.ResponseWriter, r *http.Request) {
	if !t.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="meshcore-mcp-gateway"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPost:
		t.handlePost(w, r)
	case http.MethodDelete:
		sid := r.Header.Get(sessionHdr)
		t.dropSession(sid)
		w.WriteHeader(http.StatusNoContent)
	default:
		// GET would be the server-stream channel; we never push, so it is
		// simply not supported.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (t *HTTPTransport) authorized(r *http.Request) bool {
	if t.token == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	got, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(t.token)) == 1
}

func (t *HTTPTransport) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestLen))
	if err != nil || len(body) == 0 {
		http.Error(w, "unreadable request body", http.StatusBadRequest)
		return
	}

	sid := r.Header.Get(sessionHdr)
	conn, newSID, status, err := t.sessionFor(sid, body)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if newSID != "" {
		w.Header().Set(sessionHdr, newSID)
	}

	out := conn.Handle(body)
	if out == nil { // notification — acknowledged, no body
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// sessionFor resolves the POST to a Conn, plus the HTTP status to fail with.
// Per the Streamable HTTP spec the initialize request arrives without a
// session id and mints one; requests without a session id that are not
// initialize are rejected; unknown or expired session ids are a 404 telling
// the client to re-initialize.
func (t *HTTPTransport) sessionFor(sid string, body []byte) (*Conn, string, int, error) {
	isInit := isInitializeRequest(body)

	t.mu.Lock()
	defer t.mu.Unlock()

	if sid != "" {
		conn, ok := t.conns[sid]
		if !ok {
			return nil, "", http.StatusNotFound, fmt.Errorf("unknown or expired session — initialize again")
		}
		return conn, "", http.StatusOK, nil
	}
	if !isInit {
		return nil, "", http.StatusBadRequest, fmt.Errorf("missing %s header — initialize first", sessionHdr)
	}
	id, err := newSessionID()
	if err != nil {
		return nil, "", http.StatusInternalServerError, fmt.Errorf("generating session id: %w", err)
	}
	conn := t.srv.NewConn()
	t.conns[id] = conn
	return conn, id, http.StatusOK, nil
}

func (t *HTTPTransport) dropSession(sid string) {
	if sid == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, sid)
}

func isInitializeRequest(body []byte) bool {
	var req struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.Method == "initialize"
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
