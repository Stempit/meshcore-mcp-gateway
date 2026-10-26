package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// startHTTP builds a token-authenticated transport on a random loopback port
// and waits until it is accepting connections.
func startHTTP(t *testing.T, token string) *HTTPTransport {
	t.Helper()
	tr, err := NewHTTPTransport(newTestServer(&fakeMesh{}), HTTPOptions{Listen: "127.0.0.1:0", Token: token}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = tr.Run(t.Context()) }()
	deadline := time.Now().Add(2 * time.Second)
	for tr.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("HTTP transport never started listening")
		}
		time.Sleep(time.Millisecond)
	}
	return tr
}

// post executes a prepared request against the transport.
func post(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func postFrame(t *testing.T, url, session, token string, frame any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set(sessionHdr, session)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return post(t, req)
}

func rpcFrame(id int, method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

// rpcResult decodes the JSON-RPC result of a successful response.
func rpcResult(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %s", body.Error.Message)
	}
	return body.Result
}

// initialize opens a session and returns the Mcp-Session-Id.
func initialize(t *testing.T, tr *HTTPTransport, client, token string) string {
	t.Helper()
	resp := postFrame(t, tr.url(), "", token, rpcFrame(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": client, "version": "test"},
	}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status %d, body %s", resp.StatusCode, readBody(t, resp))
	}
	sid := resp.Header.Get(sessionHdr)
	if sid == "" {
		t.Fatal("initialize response carries no Mcp-Session-Id")
	}
	return sid
}

func (tr *HTTPTransport) url() string { return "http://" + tr.Addr() + tr.path }

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestHTTPSessionLifecycle(t *testing.T) {
	tr := startHTTP(t, "")
	sid := initialize(t, tr, "hermes", "")

	// tools/list over the session advertises all five tools.
	resp := postFrame(t, tr.url(), sid, "", rpcFrame(2, "tools/list", map[string]any{}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status %d", resp.StatusCode)
	}
	tools, _ := rpcResult(t, resp)["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tools/list returned %d tools, want 5", len(tools))
	}

	// A tool call round-trips as application/json.
	resp = postFrame(t, tr.url(), sid, "", rpcFrame(3, "tools/call", map[string]any{
		"name": "mesh_list_contacts", "arguments": map[string]any{},
	}))
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("tools/call content type %q", ct)
	}
	if text, _ := rpcResult(t, resp)["content"].([]any); len(text) == 0 {
		t.Errorf("tools/call returned no content")
	}

	// A notification is acknowledged with no body.
	resp = postFrame(t, tr.url(), sid, "", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification status %d, want 202", resp.StatusCode)
	}

	// DELETE closes the session; the next call on it is a 404.
	req, err := http.NewRequest(http.MethodDelete, tr.url(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(sessionHdr, sid)
	resp = post(t, req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status %d, want 204", resp.StatusCode)
	}
	resp = postFrame(t, tr.url(), sid, "", rpcFrame(4, "tools/list", map[string]any{}))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("post-DELETE request status %d, want 404", resp.StatusCode)
	}
}

func TestHTTPSessionIsRequired(t *testing.T) {
	tr := startHTTP(t, "")

	// tools/list without initialize first is rejected…
	resp := postFrame(t, tr.url(), "", "", rpcFrame(1, "tools/list", map[string]any{}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("session-less tools/list status %d, want 400", resp.StatusCode)
	}
	// …and an unknown session id is a 404 (re-initialize).
	resp = postFrame(t, tr.url(), "stale-session", "", rpcFrame(2, "tools/list", map[string]any{}))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown-session tools/list status %d, want 404", resp.StatusCode)
	}
}

func TestHTTPBearerAuth(t *testing.T) {
	tr := startHTTP(t, "sekrit")

	resp := postFrame(t, tr.url(), "", "", rpcFrame(1, "initialize", map[string]any{}))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token initialize status %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 must carry WWW-Authenticate")
	}

	resp = postFrame(t, tr.url(), "", "wrong", rpcFrame(1, "initialize", map[string]any{}))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token initialize status %d, want 401", resp.StatusCode)
	}

	sid := initialize(t, tr, "nanobot", "sekrit") // correct token via helper
	resp = postFrame(t, tr.url(), sid, "sekrit", rpcFrame(2, "tools/list", map[string]any{}))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct-token tools/list status %d", resp.StatusCode)
	}
}

func TestHTTPMethodRouting(t *testing.T) {
	tr := startHTTP(t, "")

	resp, err := http.Get(tr.url())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "POST, DELETE" {
		t.Errorf("GET Allow header %q", allow)
	}
}

func TestHTTPTransportRequiresTokenOffLoopback(t *testing.T) {
	srv := newTestServer(&fakeMesh{})
	for _, listen := range []string{"0.0.0.0:8788", "192.168.1.10:8788", "[::]:8788"} {
		if _, err := NewHTTPTransport(srv, HTTPOptions{Listen: listen}, testLogger()); err == nil {
			t.Errorf("listen %q without a token must be rejected", listen)
		}
	}
	if _, err := NewHTTPTransport(srv, HTTPOptions{Listen: "0.0.0.0:8788", Token: "x"}, testLogger()); err != nil {
		t.Errorf("listen %q with a token must be accepted: %v", "0.0.0.0:8788", err)
	}
	for _, listen := range []string{"127.0.0.1:8788", "localhost:8788", "[::1]:8788"} {
		if _, err := NewHTTPTransport(srv, HTTPOptions{Listen: listen}, testLogger()); err != nil {
			t.Errorf("loopback listen %q must be accepted without a token: %v", listen, err)
		}
	}
	if _, err := NewHTTPTransport(srv, HTTPOptions{Listen: "no-port"}, testLogger()); err == nil {
		t.Error("listen without a port must be rejected")
	}
}
