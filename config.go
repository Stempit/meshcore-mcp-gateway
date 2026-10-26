package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/stempit/meshcore-kit/fit"
	"gopkg.in/yaml.v3"
)

// config is the bridge's own config.yaml — a subset of the family format:
// the MCP section (the xiaozhi.me access point), the Connection section
// (transport) and the Shrink keys that shape voice sends. Unknown sections
// are ignored, so a combined config.yaml shared with other family apps keeps
// working.
type config struct {
	MCP struct {
		// EndpointURL is the agent's MCP access point from the xiaozhi.me
		// console — the bridge dials OUT to it over WebSocket, so no inbound
		// ports are needed.
		EndpointURL string `yaml:"endpoint_url"`
	} `yaml:"MCP"`

	Connection struct {
		// Type selects the transport: "bt" (Bluetooth LE, default) or "serial" (USB/UART).
		Type string `yaml:"type"`
		// SerialPort is the OS device path used when Type is "serial".
		SerialPort string `yaml:"serial_port"`
		// SerialBaud sets the baud rate for the serial transport (default 115200).
		SerialBaud int `yaml:"serial_baud"`
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
// without the endpoint URL there is nothing to dial.
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
