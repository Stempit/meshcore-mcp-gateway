package xiaozhi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Connection keep-alive: nothing flows between voice commands, and an idle
// connection gets killed by NAT mappings or load balancers after a couple of
// minutes (seen as close 1006 "unexpected EOF"). Pings every pingPeriod keep
// the path warm; pongs (and any inbound frame) feed the read deadline, so a
// dead link is noticed within pongWait instead of stalling silently.
var (
	writeWait  = 10 * time.Second
	pongWait   = 90 * time.Second
	pingPeriod = 30 * time.Second
)

// reconnect pacing: doubled after each failed/short-lived connection, reset
// after a connection that stayed up long enough to look healthy.
const (
	backoffStart = 2 * time.Second
	backoffMax   = 60 * time.Second
	stableAfter  = 60 * time.Second
)

// Session manages the outbound WebSocket connection to the xiaozhi.me MCP
// access point: dial, serve JSON-RPC frames until the connection drops, then
// reconnect with exponential backoff until the context is cancelled.
type Session struct {
	endpoint string
	server   *Server
	log      *slog.Logger
}

// NewSession builds a Session for the access-point URL from the xiaozhi.me
// console. The console prints an https:// URL; the https/http schemes are
// normalized to wss/ws for the WebSocket dial.
func NewSession(endpoint string, server *Server, logger *slog.Logger) (*Session, error) {
	endpoint = strings.TrimSpace(endpoint)
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = "wss://" + strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		endpoint = "ws://" + strings.TrimPrefix(endpoint, "http://")
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
		return nil, fmt.Errorf("invalid MCP endpoint URL %q", endpoint)
	}
	return &Session{endpoint: endpoint, server: server, log: logger}, nil
}

// EndpointSafe returns the endpoint URL with any token query parameter
// redacted, for logging.
func (s *Session) EndpointSafe() string {
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return "(unparseable endpoint)"
	}
	u.RawQuery = ""
	return u.String()
}

// Run connects and serves until ctx is cancelled, reconnecting with backoff.
func (s *Session) Run(ctx context.Context) {
	backoff := backoffStart
	for {
		started := time.Now()
		err := s.connectAndServe(ctx)
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("MCP access point connection lost, reconnecting",
			"error", err, "endpoint", s.EndpointSafe(), "retry_in", backoff.Round(time.Second))

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if time.Since(started) >= stableAfter {
			backoff = backoffStart
		} else {
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
		}
	}
}

// connectAndServe dials the access point and pumps frames until an error or
// ctx cancellation. The dial-out direction is what makes this work behind
// NAT: xiaozhi.me never connects to us.
func (s *Session) connectAndServe(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, s.endpoint, nil)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("dial %s: %w (status %d, body %s)", s.EndpointSafe(), err, resp.StatusCode, body)
		}
		return fmt.Errorf("dial %s: %w", s.EndpointSafe(), err)
	}
	defer conn.Close()

	// Closing the conn on shutdown unblocks ReadMessage below.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stopped:
		}
	}()

	// Keep-alive: extend the read deadline on every pong and every inbound
	// frame; a ticker goroutine sends pings via WriteControl, which gorilla
	// allows concurrently with the response WriteMessage calls.
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	pingStop := make(chan struct{})
	defer close(pingStop)
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)) != nil {
					return // dead conn; the read loop will notice and reconnect
				}
			case <-pingStop:
				return
			}
		}
	}()

	s.log.Info("Connected to MCP access point", "endpoint", s.EndpointSafe())

	for {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if out := s.server.HandleMessage(data); out != nil {
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
				return err
			}
		}
	}
}
