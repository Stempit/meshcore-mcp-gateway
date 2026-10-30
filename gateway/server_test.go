package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-kit/companion"
	"github.com/stempit/meshcore-kit/fit"
)

// voiceFit mirrors the default voice policy — split into [i/N] parts, the
// 140-byte limit, the ellipsis — that the bridge resolves from its Shrink
// config when nothing is overridden.
func voiceFit() fit.Options {
	return fit.Options{MaxBytes: 140, Ellipsis: "…", Variant: fit.Split}
}

func newTestServer(mesh Mesh) *Server {
	return NewServer(mesh, ServerOptions{Fit: voiceFit()}, testLogger())
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initializeFrame is the JSON-RPC initialize a client sends first.
func initializeFrame(client string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":%q,"version":"test"}}}`, client))
}

// fakeMesh records calls and serves canned data.
type fakeMesh struct {
	msgs        []companion.Message
	dmErr       error
	chErr       error
	queries     []int64
	dmCalls     [][3]string // to, text, ack("0"/"1")
	chCalls     [][3]string
	searches    []string
	searchNames []string // canned SearchContacts result
}

func (f *fakeMesh) SearchContacts(query string) []string {
	f.searches = append(f.searches, query)
	return f.searchNames
}

func (f *fakeMesh) MessagesSince(after int64) ([]companion.Message, int64) {
	f.queries = append(f.queries, after)
	var out []companion.Message
	for _, m := range f.msgs {
		if m.ID > after {
			out = append(out, m)
		}
	}
	if out == nil {
		out = []companion.Message{}
	}
	return out, f.lastAssigned()
}

func (f *fakeMesh) lastAssigned() int64 {
	if len(f.msgs) == 0 {
		return 0
	}
	return f.msgs[len(f.msgs)-1].ID
}

func (f *fakeMesh) SendDM(to, text string, ack bool) error {
	f.dmCalls = append(f.dmCalls, [3]string{to, text, ackFlag(ack)})
	return f.dmErr
}

func (f *fakeMesh) SendChannel(to, text string, ack bool) error {
	f.chCalls = append(f.chCalls, [3]string{to, text, ackFlag(ack)})
	return f.chErr
}

func ackFlag(ack bool) string {
	if ack {
		return "1"
	}
	return "0"
}

func (f *fakeMesh) Contacts() []string { return []string{"Короед", "Wire"} }

func (f *fakeMesh) Channels() []ChannelRef {
	return []ChannelRef{{Index: 0, Name: "public"}, {Index: 1, Name: "private"}}
}

func TestProtocolInitializeEchoesVersion(t *testing.T) {
	s := newTestServer(&fakeMesh{})
	conn := s.NewConn()
	out := conn.Handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))

	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if resp.ID != 1 || resp.Result.ProtocolVersion != "2025-06-18" {
		t.Errorf("unexpected initialize response: %s", out)
	}
}

func TestProtocolNotificationGetsNoResponse(t *testing.T) {
	s := newTestServer(&fakeMesh{})
	conn := s.NewConn()
	if out := conn.Handle([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); out != nil {
		t.Errorf("notification produced a response: %s", out)
	}
}

func TestToolsListAdvertisesAllTools(t *testing.T) {
	s := newTestServer(&fakeMesh{})
	conn := s.NewConn()
	out := conn.Handle([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Result.Tools) != 5 {
		t.Fatalf("tools/list returned %d tools, want 5", len(resp.Result.Tools))
	}
	for _, want := range []string{"mesh_get_new_messages", "mesh_send_message", "mesh_search_contacts", "mesh_list_contacts", "mesh_list_channels"} {
		if !hasTool(resp.Result.Tools, want) {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}
}

func hasTool(tools []map[string]any, name string) bool {
	for _, tl := range tools {
		if tl["name"] == name {
			return true
		}
	}
	return false
}

func twelveMessages() []companion.Message {
	var msgs []companion.Message
	for i := 1; i <= 12; i++ {
		msgs = append(msgs, companion.Message{ID: int64(i), Type: "dm", Sender: "S", Text: strings.Repeat("x", i) + "|" + string(rune('0'+i))})
	}
	return msgs
}

func TestToolGetMessagesPagesThroughCursor(t *testing.T) {
	mesh := &fakeMesh{}
	s := newTestServer(mesh)
	conn := s.NewConn() // cursor primed at "now" — messages arriving afterwards are new
	mesh.msgs = twelveMessages()

	// First call: capped at 10, cursor advances to the last SHOWN message.
	first := resultText(t, callTool(t, conn, "mesh_get_new_messages", nil))
	if !contains(first, "10 new mesh") || !contains(first, "2 more") {
		t.Errorf("first call should show 10 and announce 2 more:\n%s", first)
	}
	// Second call: pages through the rest.
	second := resultText(t, callTool(t, conn, "mesh_get_new_messages", nil))
	if !contains(second, "2 new mesh") {
		t.Errorf("second call should show the remaining 2:\n%s", second)
	}
	// Third: everything read.
	third := resultText(t, callTool(t, conn, "mesh_get_new_messages", nil))
	if third != "No new mesh messages." {
		t.Errorf("third call = %q, want no new messages", third)
	}
	// The first query is the MaxInt64 "now" probe from NewConn.
	wantQueries := []int64{math.MaxInt64, 0, 10, 12}
	if len(mesh.queries) != len(wantQueries) {
		t.Fatalf("gateway queries = %v, want %v", mesh.queries, wantQueries)
	}
	for i, want := range wantQueries {
		if mesh.queries[i] != want {
			t.Errorf("query %d = %d, want %d", i, mesh.queries[i], want)
		}
	}
}

// TestConnCursorsAreIndependent checks that two clients reading do not
// consume each other's messages: each Conn has its own cursor.
func TestConnCursorsAreIndependent(t *testing.T) {
	mesh := &fakeMesh{}
	s := newTestServer(mesh)
	a := s.NewConn()
	b := s.NewConn()
	a.Handle(initializeFrame("agent-a"))
	b.Handle(initializeFrame("agent-b"))
	mesh.msgs = twelveMessages()

	// a reads everything; b must still see the full batch afterwards.
	if got := resultText(t, callTool(t, a, "mesh_get_new_messages", nil)); !contains(got, "10 new mesh") {
		t.Errorf("first client first call = %q", got)
	}
	if got := resultText(t, callTool(t, a, "mesh_get_new_messages", nil)); !contains(got, "2 new mesh") {
		t.Errorf("first client second call = %q", got)
	}
	if got := resultText(t, callTool(t, b, "mesh_get_new_messages", nil)); !contains(got, "10 new mesh") {
		t.Errorf("second client should have its own cursor, got %q", got)
	}
}

// TestConnCursorResumesAcrossReconnect checks that a client that sends
// initialize again (WebSocket reconnect, new HTTP session) resumes its saved
// cursor instead of replaying or losing messages, while an unknown client
// still starts at "now".
func TestConnCursorResumesAcrossReconnect(t *testing.T) {
	mesh := &fakeMesh{}
	s := newTestServer(mesh)
	first := s.NewConn()
	first.Handle(initializeFrame("hermes"))
	mesh.msgs = []companion.Message{
		{ID: 1, Type: "dm", Sender: "S", Text: "one"},
		{ID: 2, Type: "dm", Sender: "S", Text: "two"},
	}
	if got := resultText(t, callTool(t, first, "mesh_get_new_messages", nil)); !contains(got, "2 new mesh") {
		t.Fatalf("first read = %q", got)
	}

	mesh.msgs = append(mesh.msgs, companion.Message{ID: 3, Type: "dm", Sender: "S", Text: "three"})

	// Reconnect as the same client: cursor resumes at 2 → only message 3.
	resumed := s.NewConn() // starts at "now" (3)…
	resumed.Handle(initializeFrame("hermes"))
	if got := resultText(t, callTool(t, resumed, "mesh_get_new_messages", nil)); !contains(got, "three") || !contains(got, "1 new mesh") {
		t.Errorf("resumed client should see only message 3, got %q", got)
	}

	// A client never seen before starts at "now": nothing replayed.
	fresh := s.NewConn()
	fresh.Handle(initializeFrame("nanobot"))
	if got := resultText(t, callTool(t, fresh, "mesh_get_new_messages", nil)); got != "No new mesh messages." {
		t.Errorf("fresh client should start at now, got %q", got)
	}
}

func TestToolGetMessagesFormatsDMAndChannel(t *testing.T) {
	mesh := &fakeMesh{}
	s := newTestServer(mesh)
	conn := s.NewConn() // conn first: messages seeded afterwards are "new"
	mesh.msgs = []companion.Message{
		{ID: 1, Type: "dm", Sender: "Короед", Text: "hello"},
		{ID: 2, Type: "channel", Channel: "public", Sender: "Wire", Text: "hi all"},
	}

	text := resultText(t, callTool(t, conn, "mesh_get_new_messages", nil))
	for _, want := range []string{`Private from Короед: "hello"`, `Channel public from Wire: "hi all"`} {
		if !contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestToolGetMessagesTruncatesPerOutputSetting(t *testing.T) {
	mesh := &fakeMesh{}
	s := NewServer(mesh, ServerOptions{Fit: voiceFit(), MaxMessageRunes: 40}, testLogger())
	conn := s.NewConn()
	mesh.msgs = []companion.Message{
		{ID: 1, Type: "dm", Sender: "S", Text: strings.Repeat("x", 500)},
	}
	text := resultText(t, callTool(t, conn, "mesh_get_new_messages", nil))
	if !contains(text, strings.Repeat("x", 40)+"…") || contains(text, strings.Repeat("x", 41)) {
		t.Errorf("message should be truncated to 40 runes + ellipsis:\n%s", text)
	}
}

func TestToolSendMapsOutcomes(t *testing.T) {
	t.Run("sent", func(t *testing.T) {
		mesh := &fakeMesh{}
		s := newTestServer(mesh)
		conn := s.NewConn()
		out := callTool(t, conn, "mesh_send_message", map[string]any{"type": "dm", "to": "Короед", "text": "hi"})
		if tr := resultTool(t, out); tr.IsError || !contains(resultText(t, out), "sent to Короед") {
			t.Errorf("unexpected result: %+v", out)
		}
		if len(mesh.dmCalls) != 1 || mesh.dmCalls[0][0] != "Короед" || mesh.dmCalls[0][1] != "hi" || mesh.dmCalls[0][2] != "0" {
			t.Errorf("unexpected dm call: %v", mesh.dmCalls)
		}
	})

	t.Run("unknown recipient hints at directories", func(t *testing.T) {
		mesh := &fakeMesh{dmErr: ErrUnknownRecipient}
		s := newTestServer(mesh)
		conn := s.NewConn()
		out := callTool(t, conn, "mesh_send_message", map[string]any{"type": "dm", "to": "ghost", "text": "hi"})
		if tr := resultTool(t, out); !tr.IsError || !contains(resultText(t, out), "not found") || !contains(resultText(t, out), "mesh_list_contacts") {
			t.Errorf("unexpected result: %+v", out)
		}
	})

	t.Run("ambiguous name asks the user to disambiguate", func(t *testing.T) {
		mesh := &fakeMesh{dmErr: &meshcore.AmbiguousMatchError{Names: []string{"Yorick Hamlet", "Yorick 💀"}}}
		s := newTestServer(mesh)
		conn := s.NewConn()
		out := callTool(t, conn, "mesh_send_message", map[string]any{"type": "dm", "to": "Yorick", "text": "hi"})
		text := resultText(t, out)
		if tr := resultTool(t, out); !tr.IsError || !contains(text, "Several recipients match") ||
			!contains(text, "Yorick Hamlet") || !contains(text, "full name") {
			t.Errorf("unexpected result: %+v", out)
		}
	})

	t.Run("no confirmation is not a hard failure", func(t *testing.T) {
		mesh := &fakeMesh{chErr: meshcore.ErrNoConfirmation}
		s := newTestServer(mesh)
		conn := s.NewConn()
		out := callTool(t, conn, "mesh_send_message", map[string]any{"type": "channel", "to": "public", "text": "hi"})
		if tr := resultTool(t, out); !tr.IsError || !contains(resultText(t, out), "may still arrive") {
			t.Errorf("unexpected result: %+v", out)
		}
	})

	t.Run("device rejection", func(t *testing.T) {
		mesh := &fakeMesh{dmErr: meshcore.ErrDeviceRejected}
		s := newTestServer(mesh)
		conn := s.NewConn()
		out := callTool(t, conn, "mesh_send_message", map[string]any{"type": "dm", "to": "Короед", "text": "hi"})
		if tr := resultTool(t, out); !tr.IsError || !contains(resultText(t, out), "rejected") {
			t.Errorf("unexpected result: %+v", out)
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		mesh := &fakeMesh{}
		s := newTestServer(mesh)
		conn := s.NewConn()
		for _, args := range []map[string]any{
			{"type": "dm", "to": "", "text": "hi"},
			{"type": "dm", "to": "x", "text": " "},
			{"type": "sms", "to": "x", "text": "hi"},
		} {
			out := callTool(t, conn, "mesh_send_message", args)
			if tr := resultTool(t, out); !tr.IsError {
				t.Errorf("args %v should fail validation: %+v", args, out)
			}
		}
		if len(mesh.dmCalls)+len(mesh.chCalls) != 0 {
			t.Errorf("invalid args reached the radio: %v %v", mesh.dmCalls, mesh.chCalls)
		}
	})
}

func TestToolSendShrink(t *testing.T) {
	// Long text must be split into [i/N] parts, one radio send each.
	t.Run("long message splits into parts", func(t *testing.T) {
		mesh := &fakeMesh{}
		s := newTestServer(mesh)
		conn := s.NewConn()
		long := strings.Repeat("это длинное сообщение для рации ", 10) // ~320 bytes
		out := callTool(t, conn, "mesh_send_message", map[string]any{"type": "dm", "to": "Короед", "text": long})
		text := resultText(t, out)

		if len(mesh.dmCalls) < 2 {
			t.Fatalf("long message sent as %d parts, want ≥2: %q", len(mesh.dmCalls), text)
		}
		want := len(mesh.dmCalls)
		for i, call := range mesh.dmCalls {
			if !strings.HasSuffix(call[1], fmt.Sprintf(" [%d/%d]", i+1, want)) {
				t.Errorf("part %d missing series marker: %q", i+1, call[1])
			}
			if len(call[1]) > 140 {
				t.Errorf("part %d exceeds the 140-byte mesh limit: %d bytes", i+1, len(call[1]))
			}
		}
		if !contains(text, fmt.Sprintf("in %d parts", want)) {
			t.Errorf("result should announce the part count: %q", text)
		}
	})

	// Short messages go out as a single unchanged send.
	t.Run("short message is a single send", func(t *testing.T) {
		mesh := &fakeMesh{}
		s := newTestServer(mesh)
		conn := s.NewConn()
		callTool(t, conn, "mesh_send_message", map[string]any{"type": "dm", "to": "Короед", "text": "привет"})
		if len(mesh.dmCalls) != 1 || mesh.dmCalls[0][1] != "привет" {
			t.Errorf("unexpected sends: %v", mesh.dmCalls)
		}
	})

	// CyrToLat squeezes Cyrillic homoglyphs before the fit (р→p here).
	t.Run("cyr_to_lat squeeze", func(t *testing.T) {
		mesh := &fakeMesh{}
		o := voiceFit()
		o.CyrToLat = true
		s := NewServer(mesh, ServerOptions{Fit: o}, testLogger())
		conn := s.NewConn()
		callTool(t, conn, "mesh_send_message", map[string]any{"type": "dm", "to": "Короед", "text": "привет"})
		// Homoglyphs: Cyrillic р→Latin p, Cyrillic е→Latin e.
		if len(mesh.dmCalls) != 1 || mesh.dmCalls[0][1] != "пpивeт" {
			t.Errorf("expected homoglyph squeeze, got: %v", mesh.dmCalls)
		}
	})
}

func TestToolListContactsAndChannels(t *testing.T) {
	s := newTestServer(&fakeMesh{})
	conn := s.NewConn()

	if text := resultText(t, callTool(t, conn, "mesh_list_contacts", nil)); !contains(text, "Короед") || !contains(text, "Wire") {
		t.Errorf("mesh_list_contacts = %q", text)
	}
	if text := resultText(t, callTool(t, conn, "mesh_list_channels", nil)); !contains(text, "public (index 0)") {
		t.Errorf("mesh_list_channels = %q", text)
	}
}

func TestToolSearchContacts(t *testing.T) {
	t.Run("matches are listed with the query", func(t *testing.T) {
		mesh := &fakeMesh{searchNames: []string{"Yorick Hamlet", "Yorick 💀"}}
		s := newTestServer(mesh)
		conn := s.NewConn()
		text := resultText(t, callTool(t, conn, "mesh_search_contacts", map[string]any{"query": "yorick"}))
		if !contains(text, `2 contacts match "yorick"`) || !contains(text, "Yorick Hamlet") || !contains(text, "Yorick 💀") {
			t.Errorf("unexpected result: %q", text)
		}
		if len(mesh.searches) != 1 || mesh.searches[0] != "yorick" {
			t.Errorf("unexpected searches: %v", mesh.searches)
		}
	})

	t.Run("singular wording for one match", func(t *testing.T) {
		mesh := &fakeMesh{searchNames: []string{"Yorick 💀"}}
		s := newTestServer(mesh)
		conn := s.NewConn()
		text := resultText(t, callTool(t, conn, "mesh_search_contacts", map[string]any{"query": "yorick"}))
		if !contains(text, `1 contact matches "yorick"`) {
			t.Errorf("unexpected result: %q", text)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		s := newTestServer(&fakeMesh{})
		conn := s.NewConn()
		text := resultText(t, callTool(t, conn, "mesh_search_contacts", map[string]any{"query": "ghost"}))
		if !contains(text, `No contacts match "ghost"`) {
			t.Errorf("unexpected result: %q", text)
		}
	})

	t.Run("empty query is an error result", func(t *testing.T) {
		mesh := &fakeMesh{}
		s := newTestServer(mesh)
		conn := s.NewConn()
		out := callTool(t, conn, "mesh_search_contacts", map[string]any{"query": "   "})
		if tr := resultTool(t, out); !tr.IsError {
			t.Errorf("empty query should be isError=true: %+v", out)
		}
		if len(mesh.searches) != 0 {
			t.Errorf("empty query reached the mesh: %v", mesh.searches)
		}
	})
}

func TestUnknownToolIsErrorResult(t *testing.T) {
	s := newTestServer(&fakeMesh{})
	conn := s.NewConn()
	out := callTool(t, conn, "no_such_tool", nil)
	if tr := resultTool(t, out); !tr.IsError {
		t.Errorf("unknown tool should be isError=true: %+v", out)
	}
}

// streamingMesh hands out a fresh message on every read so cursor advancement
// never runs dry — enough to keep parallel reads and re-initializes churning.
type streamingMesh struct {
	fakeMesh
	n atomic.Int64
}

func (m *streamingMesh) MessagesSince(after int64) ([]companion.Message, int64) {
	id := m.n.Add(1)
	return []companion.Message{{ID: id, Type: "dm", Sender: "S", Text: "x"}}, id
}

// TestHandleIsSafeForConcurrentUse exercises the documented contract: a Conn
// may serve a re-initialize and tool reads in parallel. Run with -race: this
// is the regression test for the unguarded Conn.client access.
func TestHandleIsSafeForConcurrentUse(t *testing.T) {
	s := newTestServer(&streamingMesh{fakeMesh: fakeMesh{}})
	conn := s.NewConn()
	call := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"mesh_get_new_messages"}}`)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				conn.Handle(initializeFrame("hermes"))
				conn.Handle(call)
			}
		}()
	}
	wg.Wait()
}

// callTool runs a tools/call through Conn.Handle and returns its raw result JSON.
func callTool(t *testing.T, c *Conn, name string, args map[string]any) []byte {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out := c.Handle(raw)
	if out == nil {
		t.Fatal("tools/call produced no response")
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, out)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %s", resp.Error.Message)
	}
	return resp.Result
}

func resultText(t *testing.T, result json.RawMessage) string {
	t.Helper()
	return resultTool(t, result).Content[0].Text
}

func resultTool(t *testing.T, result json.RawMessage) toolResult {
	t.Helper()
	var tr toolResult
	if err := json.Unmarshal(result, &tr); err != nil {
		t.Fatalf("unmarshal tool result: %v (%s)", err, result)
	}
	return tr
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
