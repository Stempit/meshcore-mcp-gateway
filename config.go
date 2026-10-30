package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/stempit/meshcore-kit/fit"
	"gopkg.in/yaml.v3"
)

// config is the gateway's own config.yaml — a subset of the family format:
// the MCP section (the xiaozhi.me access point), the Transports section
// (local listeners for generic MCP hosts), the Connection section
// (transport to the radio) and the Shrink keys that shape sends. Unknown
// sections are ignored, so a combined config.yaml shared with other family
// apps keeps working.
type config struct {
	MCP struct {
		// EndpointURL is the agent's MCP access point from the xiaozhi.me
		// console — the bridge dials OUT to it over WebSocket, so no inbound
		// ports are needed. Optional: the gateway runs with any combination
		// of the transports below.
		EndpointURL string `yaml:"endpoint_url"`
	} `yaml:"MCP"`

	Transports struct {
		HTTP struct {
			// Enabled turns on the Streamable HTTP listener that generic
			// MCP hosts (nanobot, hermes, …) attach to.
			Enabled bool `yaml:"enabled"`
			// Listen is the host:port to bind. Default 127.0.0.1:8788.
			Listen string `yaml:"listen"`
			// Path is the MCP endpoint path. Default /mcp.
			Path string `yaml:"path"`
			// Token is the bearer token hosts must present. Required when
			// Listen is not a loopback address.
			Token string `yaml:"token"`
		} `yaml:"http"`
		Stdio struct {
			// Enabled serves newline-delimited JSON-RPC on stdin/stdout —
			// for hosts that spawn the gateway as a subprocess. The -stdio
			// flag forces this on regardless.
			Enabled bool `yaml:"enabled"`
		} `yaml:"stdio"`
	} `yaml:"Transports"`

	Output struct {
		// MaxMessageRunes caps each message quoted by mesh_get_new_messages
		// (default 200). Raise it for text agents that read full messages.
		MaxMessageRunes int `yaml:"max_message_runes"`
	} `yaml:"Output"`

	Connection struct {
		// Type selects the transport: "bt" (Bluetooth LE, default), "serial"
		// (USB/UART) or "tcp" (alias "wifi": Wi-Fi/Ethernet companion firmware).
		Type string `yaml:"type"`
		// SerialPort is the OS device path used when Type is "serial".
		SerialPort string `yaml:"serial_port"`
		// SerialBaud sets the baud rate for the serial transport (default 115200).
		SerialBaud int `yaml:"serial_baud"`
		// TCPAddress is the device address used when Type is "tcp" — the Wi-Fi
		// or Ethernet companion firmware serves the same protocol on TCP.
		// Either "host:port" or a bare host, which gets the default port 5000.
		TCPAddress string `yaml:"tcp_address"`
		// DeviceName selects the BLE device by exact advertised name (e.g.
		// "MeshCore-AB12") when Type is "bt". Empty scans any MeshCore device
		// and picks the strongest signal. The portable identifier on macOS,
		// where the address is a per-machine CoreBluetooth UUID.
		DeviceName string `yaml:"device_name"`
		// DeviceAddress selects the BLE device by platform address when Type
		// is "bt" (MAC on Linux/Windows, CoreBluetooth UUID on macOS).
		// Combine with DeviceName or use alone.
		DeviceAddress string `yaml:"device_address"`
		// ChannelTransmissions is the MAXIMUM number of times each channel
		// message is transmitted (deduplicated by receivers). 0 or unset
		// defaults to 3.
		ChannelTransmissions int `yaml:"channel_transmissions"`
	} `yaml:"Connection"`

	Shrink struct {
		MaxBytes  int               `yaml:"max_bytes"`  // hard mesh limit; default 140
		Ellipsis  string            `yaml:"ellipsis"`   // appended by truncate
		CyrToLat  bool              `yaml:"cyr_to_lat"` // lossless Cyrillic→Latin squeeze
		Overrides map[string]string `yaml:"overrides"`  // per-path variant, e.g. xiaozhi: truncate
	} `yaml:"Shrink"`
}

// loadConfig reads and parses the config file. A missing file is an error:
// without a config there is nothing to dial or listen on.
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var c config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &c, nil
}

// voiceFitOptions resolves the voice-send policy. The "xiaozhi" override wins
// when set to a known variant; otherwise the built-in default is Split — a
// voice assistant reading a truncated reply loses its ending. Mirrors the
// resolution meshcore-go-bot's shrink layer applies to the "xiaozhi" path.
func (c *config) voiceFitOptions() fit.Options {
	v := fit.Split
	if o, ok := c.Shrink.Overrides["xiaozhi"]; ok {
		switch fit.Variant(o) {
		case fit.Truncate, fit.Split, fit.Skip:
			v = fit.Variant(o)
		case fit.LLM:
			// The voice bridge ships no LLM compressor; splitting is the
			// closest behavior.
			v = fit.Split
		}
	}
	return fit.Options{
		MaxBytes: c.Shrink.MaxBytes,
		Ellipsis: c.Shrink.Ellipsis,
		CyrToLat: c.Shrink.CyrToLat,
		Variant:  v,
	}
}

// validateVoiceFit warns about an "xiaozhi" override that names no known
// variant — it is ignored at runtime.
func (c *config) validateVoiceFit(log *slog.Logger) {
	switch fit.Variant(c.Shrink.Overrides["xiaozhi"]) {
	case "", fit.Truncate, fit.Split, fit.LLM, fit.Skip:
	default:
		log.Warn("shrink: unknown xiaozhi override variant, using split",
			"value", c.Shrink.Overrides["xiaozhi"])
	}
}
