package mailbox

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// setupLivenessServer starts a websocket test server with the given
// ping interval/timeout and returns its URL.
func setupLivenessServer(t *testing.T, interval, timeout time.Duration) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(db, true, map[string]interface{}{}, nil, nil, nil)
	handler := NewHandler(server, false, interval.Seconds(), timeout.Seconds())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler}
	go func() { _ = httpServer.Serve(listener) }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	cleanup := func() {
		_ = httpServer.Close()
		_ = listener.Close()
		server.Shutdown()
		time.Sleep(20 * time.Millisecond)
		_ = db.Close()
	}
	return fmt.Sprintf("ws://127.0.0.1:%s/v1", port), cleanup
}

// readFrames is the single legal reader goroutine for the client conn.
// While blocked in ReadMessage, gorilla processes server pings inline and
// invokes our ping handler (auto-pong), which is exactly the autobahn
// behavior this test relies on. Data frames are pushed onto frames; a
// terminal error is pushed onto errs and the goroutine exits.
func readFrames(conn *websocket.Conn, frames chan<- map[string]interface{}, errs chan<- error) {
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			errs <- err
			return
		}
		var frame map[string]interface{}
		if err := json.Unmarshal(payload, &frame); err == nil {
			frames <- frame
		}
	}
}

// waitPong drains frames until a pong arrives or the timeout fires.
func waitPong(t *testing.T, frames <-chan map[string]interface{}, errs <-chan error, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case frame := <-frames:
			if frame["type"] == "pong" {
				return
			}
		case err := <-errs:
			t.Fatalf("connection died before pong: %v", err)
		case <-deadline:
			t.Fatalf("no pong within %s", d)
		}
	}
}

func sendPing(t *testing.T, conn *websocket.Conn, n int) {
	t.Helper()
	msg, err := json.Marshal(map[string]interface{}{"type": "ping", "ping": n})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatalf("write ping: %v", err)
	}
}

func TestPongKeepsIdleClientAlive(t *testing.T) {
	// ~50ms pings, 300ms timeout: matches autobahn semantics where a client
	// that answers pings stays connected indefinitely, even with no data.
	const interval = 50 * time.Millisecond
	const timeout = 300 * time.Millisecond

	url, cleanup := setupLivenessServer(t, interval, timeout)
	defer cleanup()

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// autobahn-style clients (python twisted/autobahn, nhooyr) pong server
	// pings automatically; make this explicit rather than depend on
	// gorilla's defaults
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(2*time.Second))
	})

	frames := make(chan map[string]interface{}, 16)
	errs := make(chan error, 1)
	go readFrames(conn, frames, errs)

	// baseline: an active ping/pong round works
	sendPing(t, conn, 1)
	waitPong(t, frames, errs, 2*time.Second)

	// idle window: the server pings us every ~50ms and we pong; after
	// several rounds (700ms > 2x timeout) the server must still consider
	// the connection live. If the liveness fix is broken, the server kills
	// us around the 300ms mark and the read goroutine reports an error.
	time.Sleep(700 * time.Millisecond)

	select {
	case err := <-errs:
		t.Fatalf("server dropped an idle-but-responsive client: %v", err)
	default:
	}

	sendPing(t, conn, 2)
	waitPong(t, frames, errs, 2*time.Second)
}

func TestSilentClientDropped(t *testing.T) {
	// ~50ms pings, 300ms timeout: a client that never reads (and therefore
	// never pongs) must be dropped once autoPingTimeout elapses with no pong.
	const interval = 50 * time.Millisecond
	const timeout = 300 * time.Millisecond

	url, cleanup := setupLivenessServer(t, interval, timeout)
	defer cleanup()

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// drop the default auto-pong behavior: this client never answers pings,
	// so the server must consider it dead ~autoPingTimeout after the last
	// activity and close the connection.
	conn.SetPingHandler(func(appData string) error { return nil })

	// keep reading until the server closes the connection or we give up
	// after 2s. (gorilla panics on reads after a failed read, so return as
	// soon as any error appears; the deadline is the only timeout.)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err = conn.ReadMessage()
		if err == nil {
			continue // e.g. the server's welcome frame
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("server kept an unresponsive client alive for >2s")
		}
		// close frame / EOF / connection reset: the server dropped us
		// during the idle window, as autobahn requires
		return
	}
}
