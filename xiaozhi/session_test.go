package xiaozhi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestSessionKeepsAliveWithPings runs the session against a local WebSocket
// server with shortened keep-alive periods and verifies the client actually
// sends pings (which keep NAT mappings warm in production).
func TestSessionKeepsAliveWithPings(t *testing.T) {
	oldPing, oldPong := pingPeriod, pongWait
	pingPeriod, pongWait = 40*time.Millisecond, 300*time.Millisecond
	defer func() { pingPeriod, pongWait = oldPing, oldPong }()

	var pings int32
	var once sync.Once
	connected := make(chan struct{})
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Count pings; answer them the way the default handler would.
		conn.SetPingHandler(func(appData string) error {
			atomic.AddInt32(&pings, 1)
			once.Do(func() { close(connected) })
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeWait))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	sess, err := NewSession("ws"+strings.TrimPrefix(srv.URL, "http"), newTestServer(&fakeMesh{}), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sess.Run(ctx)

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw a connection")
	}
	time.Sleep(300 * time.Millisecond) // a few ping periods
	if got := atomic.LoadInt32(&pings); got < 2 {
		t.Errorf("server saw %d pings in 300ms with a %v period, want ≥2", got, pingPeriod)
	}
}
