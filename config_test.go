package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stempit/meshcore-kit/fit"
)

// writeConfig puts a YAML file in a temp dir and loads it.
func writeConfig(t *testing.T, body string) (*config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadConfig(path)
}

// A pre-generic-gateway config (MCP endpoint only) must keep working unchanged.
func TestConfigLegacyShape(t *testing.T) {
	c, err := writeConfig(t, `
MCP:
  endpoint_url: "https://api.xiaozhi.me/mcp/?token=XXXX"
Connection:
  type: "bt"
  channel_transmissions: 2
Shrink:
  cyr_to_lat: true
  overrides:
    xiaozhi: truncate
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.MCP.EndpointURL == "" {
		t.Error("endpoint_url not parsed")
	}
	if got := c.voiceFitOptions(); got.Variant != fit.Truncate || !got.CyrToLat {
		t.Errorf("voiceFitOptions = %+v", got)
	}

	// Unknown sections are still ignored.
	if _, err := writeConfig(t, "SomeFamilyApp:\n  whatever: 1\n"+legacyBody()); err != nil {
		t.Errorf("unknown section broke parsing: %v", err)
	}
}

func legacyBody() string {
	return "MCP:\n  endpoint_url: x\n"
}

func TestConfigTransportsAndOutput(t *testing.T) {
	c, err := writeConfig(t, `
Transports:
  http:
    enabled: true
    listen: "0.0.0.0:9999"
    path: /mcp
    token: "sekrit"
  stdio:
    enabled: true
Output:
  max_message_runes: 1000
`)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Transports.HTTP.Enabled || c.Transports.HTTP.Listen != "0.0.0.0:9999" ||
		c.Transports.HTTP.Path != "/mcp" || c.Transports.HTTP.Token != "sekrit" {
		t.Errorf("http transport = %+v", c.Transports.HTTP)
	}
	if !c.Transports.Stdio.Enabled {
		t.Error("stdio transport not enabled")
	}
	if c.Output.MaxMessageRunes != 1000 {
		t.Errorf("max_message_runes = %d", c.Output.MaxMessageRunes)
	}
}

// Defaults: sections absent → everything off, Output cap unset (server picks 200).
func TestConfigDefaults(t *testing.T) {
	c, err := writeConfig(t, legacyBody())
	if err != nil {
		t.Fatal(err)
	}
	if c.Transports.HTTP.Enabled || c.Transports.Stdio.Enabled {
		t.Error("transports must default to off")
	}
	if c.Transports.HTTP.Listen != "" || c.Transports.HTTP.Path != "" {
		t.Errorf("unexpected transport defaults: %+v", c.Transports.HTTP)
	}
	if c.Output.MaxMessageRunes != 0 {
		t.Errorf("max_message_runes default = %d, want 0 (unset)", c.Output.MaxMessageRunes)
	}
}
