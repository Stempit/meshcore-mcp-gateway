package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestStdioServesFrames feeds a full session through the stdio transport and
// checks that every request yields exactly one response line, notifications
// none, and logs never touch stdout.
func TestStdioServesFrames(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"hermes","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"mesh_list_contacts","arguments":{}}}`,
		``,
		`   `,
	}, "\n") + "\n"

	var out bytes.Buffer
	s := NewStdio(newTestServer(&fakeMesh{}), strings.NewReader(in), &out, testLogger())
	if err := s.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout has %d lines, want 3 (no response for the notification): %q", len(lines), out.String())
	}

	var initResp struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("initialize line: %v (%s)", err, lines[0])
	}
	if initResp.Result.ServerInfo.Name != "meshcore-mcp-gateway" {
		t.Errorf("initialize serverInfo = %+v", initResp.Result.ServerInfo)
	}

	var listResp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatalf("tools/list line: %v (%s)", err, lines[1])
	}
	if len(listResp.Result.Tools) != 5 {
		t.Errorf("tools/list returned %d tools, want 5", len(listResp.Result.Tools))
	}

	var callResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &callResp); err != nil {
		t.Fatalf("tools/call line: %v (%s)", err, lines[2])
	}
	if !contains(callResp.Result.Content[0].Text, "Короед") {
		t.Errorf("tools/call text = %q", callResp.Result.Content[0].Text)
	}
}
