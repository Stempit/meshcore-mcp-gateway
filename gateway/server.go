package gateway

// JSON-RPC 2.0 / Model Context Protocol wire handling, shared by every
// transport (the xiaozhi.me dial-out WebSocket, Streamable HTTP, stdio).
// The client — whichever it is — sends "initialize", then "tools/list" /
// "tools/call"; the gateway responds. Only the handful of MCP methods a
// tool server needs are implemented.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-kit/fit"
)

// rpcRequest is an incoming JSON-RPC request or notification.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcResponse is an outgoing JSON-RPC response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// toolResult is the MCP tools/call result shape (content items + isError).
type toolResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type contentItem struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// Output shaping limits: how much one answer may contain. The defaults keep
// a voice reply short; text agents may raise the per-message rune cap via
// the Output config section.
const (
	maxBatchMessages       = 10 // messages per mesh_get_new_messages call
	defaultMaxMessageRunes = 200
	maxListItems           = 30 // names per contacts listing
)

// Server holds the state shared by every connected MCP client: the radio
// backend, the send-fitting policy and the per-client read cursors. Each
// client connection gets its own Conn from NewConn. It is
// transport-agnostic: a transport feeds frames to Conn.Handle and writes
// back what comes out. The radio side is whatever Mesh implementation it
// was built with; Fit is the resolved fitting policy for sends.
type Server struct {
	mesh Mesh
	log  *slog.Logger
	Fit  fit.Options

	maxMessageRunes int

	// cursorsMu guards cursors, the last message ID each named client has
	// read (keyed by the clientInfo.name sent in initialize). A client that
	// reconnects resumes where it left off; the map is in-memory only, so a
	// restart starts everyone at "now" — the retained backlog is not
	// replayed.
	cursorsMu sync.Mutex
	cursors   map[string]int64
}

// ServerOptions configures a Server.
type ServerOptions struct {
	// Fit is the policy for fitting outgoing sends to the mesh limit
	// (split into [i/N] parts by default).
	Fit fit.Options
	// MaxMessageRunes caps each message quoted by mesh_get_new_messages
	// (default 200). Raise it for text agents that read full messages.
	MaxMessageRunes int
}

// NewServer builds a Server serving the mesh tools through the given radio
// backend. See ServerOptions for the knobs.
func NewServer(mesh Mesh, opts ServerOptions, logger *slog.Logger) *Server {
	if opts.MaxMessageRunes <= 0 {
		opts.MaxMessageRunes = defaultMaxMessageRunes
	}
	return &Server{
		mesh:            mesh,
		Fit:             opts.Fit,
		maxMessageRunes: opts.MaxMessageRunes,
		cursors:         make(map[string]int64),
		log:             logger,
	}
}

// NewConn starts one MCP client connection. Its read cursor begins at "now"
// — the backlog that predates the connection is not replayed — unless the
// client is already known from an earlier connection, in which case its
// initialize resumes the saved cursor.
func (s *Server) NewConn() *Conn {
	return &Conn{srv: s, cursor: s.nowCursor()}
}

// nowCursor returns the newest inbox ID: the "no backlog" starting point.
func (s *Server) nowCursor() int64 {
	_, last := s.mesh.MessagesSince(math.MaxInt64)
	return last
}

func (s *Server) savedCursor(client string) (int64, bool) {
	s.cursorsMu.Lock()
	defer s.cursorsMu.Unlock()
	cur, ok := s.cursors[client]
	return cur, ok
}

func (s *Server) saveCursor(client string, cur int64) {
	s.cursorsMu.Lock()
	defer s.cursorsMu.Unlock()
	if cur > s.cursors[client] {
		s.cursors[client] = cur
	}
}

// Conn is one MCP client connection: its identity (the clientInfo.name from
// initialize) and its private read cursor. Handle is safe for concurrent
// use — an HTTP session may issue tool calls in parallel.
type Conn struct {
	srv    *Server
	client string

	mu     sync.Mutex
	cursor int64
}

// Handle processes one incoming JSON-RPC frame and returns the response
// bytes, or nil for notifications (which get no response).
func (c *Conn) Handle(data []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(data, &req); err != nil {
		c.srv.log.Warn("Unparseable MCP frame", "error", err, "bytes", len(data))
		return nil // no ID to correlate; drop
	}
	if req.Method == "" {
		return nil
	}
	// JSON-RPC notification = absent or null id → never respond.
	if isNotification(req.ID) {
		c.srv.log.Debug("MCP notification", "method", req.Method)
		return nil
	}

	switch req.Method {
	case "initialize":
		return marshalResponse(req.ID, c.handleInitialize(req.Params), nil)
	case "tools/list":
		return marshalResponse(req.ID, map[string]any{"tools": ToolDefinitions()}, nil)
	case "tools/call":
		result := c.handleToolsCall(req.Params)
		return marshalResponse(req.ID, result, nil)
	case "ping":
		return marshalResponse(req.ID, map[string]any{}, nil)
	default:
		c.srv.log.Warn("Unsupported MCP method", "method", req.Method)
		return marshalResponse(req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

func isNotification(id json.RawMessage) bool {
	return len(id) == 0 || string(id) == "null"
}

func marshalResponse(id json.RawMessage, result any, rerr *rpcError) []byte {
	b, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
	if err != nil {
		return nil // cannot happen for these shapes
	}
	return b
}

// handleInitialize echoes the client's requested protocol version, declares
// the tools capability, and adopts the client's identity: a known client
// resumes its saved cursor, a new one keeps the connection-time cursor.
func (c *Conn) handleInitialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ProtocolVersion == "" {
		p.ProtocolVersion = "2024-11-05"
	}
	if name := strings.TrimSpace(p.ClientInfo.Name); name != "" {
		c.setClient(name)
		if saved, ok := c.srv.savedCursor(name); ok {
			c.setCursor(saved)
		}
	}
	return map[string]any{
		"protocolVersion": p.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "meshcore-mcp-gateway", "version": "1.0.0"},
	}
}

// handleToolsCall dispatches a tools/call. Per the MCP spec, tool failures
// (unreachable radio, bad recipient) are SUCCESS results with isError=true —
// the LLM reads them and can tell the user what went wrong.
func (c *Conn) handleToolsCall(params json.RawMessage) any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolResult{Content: []contentItem{{Type: "text", Text: "invalid tools/call params"}}, IsError: true}
	}

	var text string
	var isErr bool
	switch p.Name {
	case "mesh_get_new_messages":
		text, isErr = c.toolGetMessages()
	case "mesh_send_message":
		text, isErr = c.toolSend(p.Arguments)
	case "mesh_search_contacts":
		text, isErr = c.toolSearchContacts(p.Arguments)
	case "mesh_list_contacts":
		text, isErr = c.toolContacts()
	case "mesh_list_channels":
		text, isErr = c.toolChannels()
	default:
		text, isErr = fmt.Sprintf("Unknown tool: %s", p.Name), true
	}
	if isErr {
		c.srv.log.Info("Tool call failed", "tool", p.Name, "result", text)
	}
	return toolResult{Content: []contentItem{{Type: "text", Text: text}}, IsError: isErr}
}

// truncate cuts s to max runes, appending an ellipsis.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func (c *Conn) getCursor() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursor
}

func (c *Conn) setCursor(cur int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor = cur
}

// setClient adopts the client identity under the same mutex as the cursor —
// Handle may run initialize concurrently with tool calls.
func (c *Conn) setClient(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = name
}

// advanceCursor moves the connection cursor (and the saved per-client one)
// forward to last, never backward.
func (c *Conn) advanceCursor(last int64) {
	c.mu.Lock()
	if last > c.cursor {
		c.cursor = last
	}
	client := c.client
	c.mu.Unlock()
	c.srv.saveCursor(client, last)
}

// toolGetMessages reads the next batch of unread messages through the
// connection's inbox cursor and formats them for the reply. The cursor
// advances only through what was returned, so a caller pages through a
// large backlog by asking again.
func (c *Conn) toolGetMessages() (string, bool) {
	after := c.getCursor()

	msgs, _ := c.srv.mesh.MessagesSince(after)
	if len(msgs) == 0 {
		return "No new mesh messages.", false
	}

	shown := msgs
	more := 0
	if len(msgs) > maxBatchMessages {
		shown, more = msgs[:maxBatchMessages], len(msgs)-maxBatchMessages
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d new mesh message(s):", len(shown))
	for i, m := range shown {
		text := truncate(m.Text, c.srv.maxMessageRunes)
		switch m.Type {
		case "dm":
			fmt.Fprintf(&b, "\n%d. Private from %s: %q", i+1, m.Sender, text)
		case "channel":
			name := m.Channel
			if name == "" {
				name = fmt.Sprintf("index %d", m.ChannelIndex)
			}
			fmt.Fprintf(&b, "\n%d. Channel %s from %s: %q", i+1, name, m.Sender, text)
		default:
			fmt.Fprintf(&b, "\n%d. %s: %q", i+1, m.Type, text)
		}
	}
	if more > 0 {
		fmt.Fprintf(&b, "\n(%d more — read the rest by asking again)", more)
	}

	// Advance the cursor only through what was shown, keeping the rest of
	// the backlog for the next call.
	c.advanceCursor(shown[len(shown)-1].ID)
	return b.String(), false
}

// sendRequest is the mesh_send_message arguments.
type sendRequest struct {
	Type string `json:"type"` // "dm" | "channel"
	To   string `json:"to"`   // contact name / hex key prefix / channel name / index
	Text string `json:"text"`
	Ack  bool   `json:"ack"` // true = wait for delivery confirmation instead of sent
}

// toolSend validates arguments, fits the text to the mesh (the server's Fit
// policy: split into [i/N] parts by default, optional Cyrillic→Latin squeeze
// via its CyrToLat option), sends every part, and maps the distinct failure
// modes onto text the agent can act on.
func (c *Conn) toolSend(args json.RawMessage) (string, bool) {
	var req sendRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return "Invalid arguments: " + err.Error(), true
	}
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.To = strings.TrimSpace(req.To)
	if req.To == "" || strings.TrimSpace(req.Text) == "" {
		return "Both recipient (to) and text are required.", true
	}
	if req.Type != "dm" && req.Type != "channel" {
		return `Type must be "dm" or "channel".`, true
	}

	parts := fit.Fit(c.srv.Fit, "", []string{req.Text})
	sent := 0
	var err error
	for _, part := range parts {
		switch req.Type {
		case "dm":
			err = c.srv.mesh.SendDM(req.To, part, req.Ack)
		case "channel":
			err = c.srv.mesh.SendChannel(req.To, part, req.Ack)
		}
		if err != nil {
			break // stop at the first failure; report exactly what got through
		}
		sent++
	}

	if err == nil {
		if len(parts) == 1 {
			return fmt.Sprintf("Message sent to %s.", req.To), false
		}
		return fmt.Sprintf("Message sent to %s in %d parts.", req.To, len(parts)), false
	}
	failure := sendFailureText(req.To, err)
	if sent > 0 {
		return fmt.Sprintf("Only %d of %d parts were sent: %s", sent, len(parts), failure), true
	}
	return failure, true
}

// sendFailureText renders a radio-side send error as a sentence the calling
// agent can relay and act on.
func sendFailureText(to string, err error) string {
	var amb *meshcore.AmbiguousMatchError
	switch {
	case errors.As(err, &amb):
		return fmt.Sprintf("Several recipients match %q: %s. Use the full name.", to, strings.Join(amb.Names, ", "))
	case errors.Is(err, ErrUnknownRecipient):
		return fmt.Sprintf("Recipient %q not found. Use mesh_list_contacts or mesh_list_channels to check names.", to)
	case errors.Is(err, meshcore.ErrNoConfirmation):
		return "Message transmitted but not confirmed by the radio. It may still arrive — don't repeat it right away."
	case errors.Is(err, meshcore.ErrDeviceRejected):
		return fmt.Sprintf("The radio device rejected the message: %v", err)
	default:
		return "Send failed: " + err.Error()
	}
}

func (c *Conn) toolContacts() (string, bool) {
	names := c.srv.mesh.Contacts()
	if len(names) == 0 {
		return "No contacts are known yet.", false
	}
	if len(names) > maxListItems {
		return fmt.Sprintf("%d known contacts, first %d: %s. And %d more.",
			len(names), maxListItems,
			strings.Join(names[:maxListItems], ", "), len(names)-maxListItems), false
	}
	return fmt.Sprintf("Known contacts (%d): %s.", len(names), strings.Join(names, ", ")), false
}

// toolSearchContacts answers mesh_search_contacts: all names matching a
// fragment. With a large contact book this is how the agent finds one
// recipient without listing the whole directory.
func (c *Conn) toolSearchContacts(args json.RawMessage) (string, bool) {
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "Invalid arguments: " + err.Error(), true
	}
	p.Query = strings.TrimSpace(p.Query)
	if p.Query == "" {
		return "Provide a name (or part of it) to search for.", true
	}

	names := c.srv.mesh.SearchContacts(p.Query)
	if len(names) == 0 {
		return fmt.Sprintf("No contacts match %q.", p.Query), false
	}

	noun, verb := "contacts", "match"
	if len(names) == 1 {
		noun, verb = "contact", "matches"
	}
	shown := names
	var more string
	if len(names) > maxListItems {
		shown = names[:maxListItems]
		more = fmt.Sprintf(" And %d more — refine the search.", len(names)-maxListItems)
	}
	return fmt.Sprintf("%d %s %s %q: %s.%s",
		len(names), noun, verb, p.Query, strings.Join(shown, ", "), more), false
}

func (c *Conn) toolChannels() (string, bool) {
	refs := c.srv.mesh.Channels()
	if len(refs) == 0 {
		return "No channels are configured.", false
	}
	parts := make([]string, 0, len(refs))
	for _, ch := range refs {
		parts = append(parts, fmt.Sprintf("%s (index %d)", ch.Name, ch.Index))
	}
	return "Configured channels: " + strings.Join(parts, ", ") + ".", false
}
