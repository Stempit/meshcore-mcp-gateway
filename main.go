// meshcore-mcp-gateway is a generic MCP gateway to a MeshCore LoRa device.
// It serves the mesh tools (read mesh messages, send, list contacts and
// channels) to any MCP host, over any mix of transports:
//
//   - xiaozhi.me: dials OUT to the console-issued MCP access point over
//     WebSocket (the cloud is the MCP client) — works behind ISP NAT with
//     no port forwarding;
//   - Streamable HTTP: a local listener that generic hosts (nanobot,
//     hermes, …) attach to by URL, with optional bearer auth;
//   - stdio: newline-delimited JSON-RPC on stdin/stdout for hosts that
//     spawn the gateway as a subprocess.
//
// It is a standalone feature with its own device connection (the Connection
// config section) — independent of the bot and the HTTP gateway. Don't run
// it against the same physical device as another family app at the same
// time: a companion device serves one connection.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/stempit/meshcore-kit/companion"
	"github.com/stempit/meshcore-mcp-gateway/gateway"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	forceStdio := flag.Bool("stdio", false, "serve MCP on stdin/stdout (newline-delimited JSON-RPC), in addition to the config transports")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Logs go to stderr: stdout carries MCP frames in stdio mode.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	cfg.validateVoiceFit(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	xiaozhiURL := strings.TrimSpace(cfg.MCP.EndpointURL)
	httpEnabled := cfg.Transports.HTTP.Enabled
	stdioEnabled := *forceStdio || cfg.Transports.Stdio.Enabled
	if xiaozhiURL == "" && !httpEnabled && !stdioEnabled {
		log.Error(`no MCP transport enabled — set MCP.endpoint_url (xiaozhi.me access point), enable Transports.http, or run with -stdio`)
		os.Exit(1)
	}

	// Meshcore client — BLE (default) or USB serial depending on config.
	client, err := companion.Connect(companion.ConnectOptions{
		Type:       cfg.Connection.Type,
		SerialPort: cfg.Connection.SerialPort,
		SerialBaud: cfg.Connection.SerialBaud,
	})
	if err != nil {
		log.Error("Failed to connect to device", "error", err)
		os.Exit(1)
	}
	defer client.Disconnect()

	// Device runtime: startup stored-message sync, live forwarding into the
	// inbox, keepalive/reconnect loop.
	svc := companion.NewService(client, log)
	svc.Start(ctx)

	// Tools backed directly by the device + inbox. Each client's read cursor
	// is primed at "now" on its first connect, so pre-existing backlog is
	// never replayed (see gateway.NewConn).
	mesh := gateway.NewDirectMesh(client, svc.Inbox, gateway.DirectMeshOptions{
		ChannelTransmissions: cfg.Connection.ChannelTransmissions,
	})
	srv := gateway.NewServer(mesh, gateway.ServerOptions{
		Fit:             cfg.voiceFitOptions(),
		MaxMessageRunes: cfg.Output.MaxMessageRunes,
	}, log)

	// Start every enabled transport; the first hard failure cancels the run.
	errCh := make(chan error, 4)
	run := func(name string, fn func() error) {
		go func() {
			if err := fn(); err != nil {
				errCh <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	if xiaozhiURL != "" {
		sess, err := gateway.NewXiaozhiSession(xiaozhiURL, srv, log)
		if err != nil {
			log.Error("Invalid MCP configuration", "error", err)
			os.Exit(1)
		}
		log.Info("xiaozhi transport: dialing access point", "endpoint", sess.EndpointSafe())
		run("xiaozhi transport", func() error { sess.Run(ctx); return nil })
	}

	if httpEnabled {
		t, err := gateway.NewHTTPTransport(srv, gateway.HTTPOptions{
			Listen: cfg.Transports.HTTP.Listen,
			Path:   cfg.Transports.HTTP.Path,
			Token:  cfg.Transports.HTTP.Token,
		}, log)
		if err != nil {
			log.Error("Invalid HTTP transport configuration", "error", err)
			os.Exit(1)
		}
		run("http transport", func() error { return t.Run(ctx) })
	}

	if stdioEnabled {
		t := gateway.NewStdio(srv, os.Stdin, os.Stdout, log)
		run("stdio transport", func() error { return t.Run(ctx) })
	}

	log.Info("mcp gateway running",
		"cursor", svc.Inbox.LastID(),
		"transports", enabledTransports(xiaozhiURL != "", httpEnabled, stdioEnabled))

	select {
	case err := <-errCh:
		log.Error("Transport failed, shutting down", "error", err)
		stop()
		<-ctx.Done()
	case <-ctx.Done():
	}
	log.Info("Shutting down")
}

func enabledTransports(xiaozhi, http, stdio bool) []string {
	var out []string
	if xiaozhi {
		out = append(out, "xiaozhi")
	}
	if http {
		out = append(out, "http")
	}
	if stdio {
		out = append(out, "stdio")
	}
	return out
}
