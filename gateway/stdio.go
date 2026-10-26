package gateway

// Stdio transport: newline-delimited JSON-RPC over stdin/stdout, for MCP
// hosts that launch the gateway as a subprocess (command + args in the
// agent's mcp_servers config). One process run is one MCP connection.
// Nothing but protocol frames may be written to stdout — the app's logs go
// to stderr.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
)

// Stdio serves one MCP connection on the given streams.
type Stdio struct {
	srv *Server
	in  io.Reader
	out io.Writer
	log *slog.Logger
}

// NewStdio builds the transport. In production in/out are os.Stdin/os.Stdout;
// tests pass pipes.
func NewStdio(server *Server, in io.Reader, out io.Writer, logger *slog.Logger) *Stdio {
	return &Stdio{srv: server, in: in, out: out, log: logger}
}

// Run reads newline-delimited JSON-RPC frames until EOF or ctx cancellation
// and writes each response back as one line. The frame loop is sequential —
// fine for a single host, same as the WebSocket session.
func (s *Stdio) Run(ctx context.Context) error {
	client := s.srv.NewConn()
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if out := client.Handle(line); out != nil {
			if _, err := s.out.Write(append(bytes.Clone(out), '\n')); err != nil {
				return fmt.Errorf("writing response: %w", err)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return sc.Err() // nil on clean EOF
}
