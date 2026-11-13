package mailbox

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testServerHandle struct {
	server *Server
	db     *sql.DB
	url    string
	dir    string
	close  func()
}

func startTestWebSocketServer(t *testing.T) *testServerHandle {
	t.Helper()
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(db, true, map[string]interface{}{}, nil, nil, nil)
	handler := NewHandler(server, false, 60, 600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler}
	go func() { _ = httpServer.Serve(listener) }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return &testServerHandle{
		server: server,
		db:     db,
		url:    fmt.Sprintf("ws://127.0.0.1:%s/v1", port),
		dir:    dir,
		close: func() {
			_ = httpServer.Close()
			_ = listener.Close()
			server.Shutdown()
			// let websocket handler goroutines settle before closing the DB,
			// then remove the directory ourselves so SQLite's transient
			// journal files can't race TempDir cleanup
			time.Sleep(50 * time.Millisecond)
			_ = db.Close()
			time.Sleep(10 * time.Millisecond)
			_ = os.RemoveAll(dir)
		},
	}
}

type testClient struct {
	t    *testing.T
	conn *websocket.Conn
	buf  []map[string]interface{}
}

func dial(t *testing.T, url string) *testClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	tc := &testClient{t: t, conn: conn}
	// read the welcome frame
	welcome := tc.next()
	if welcome["type"] != "welcome" {
		t.Fatalf("expected welcome, got %v", welcome)
	}
	return tc
}

func (tc *testClient) send(msg map[string]interface{}) {
	tc.t.Helper()
	payload, err := json.Marshal(msg)
	if err != nil {
		tc.t.Fatal(err)
	}
	if err := tc.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		tc.t.Fatal(err)
	}
}

// next reads the next frame from the wire.
func (tc *testClient) next() map[string]interface{} {
	tc.t.Helper()
	_ = tc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := tc.conn.ReadMessage()
	if err != nil {
		tc.t.Fatalf("read frame: %v", err)
	}
	var frame map[string]interface{}
	if err := json.Unmarshal(payload, &frame); err != nil {
		tc.t.Fatal(err)
	}
	return frame
}

// waitForType reads frames until one of type typ arrives. Frames of other
// types are skipped but recorded (useful when debugging; they are never
// consumed out of order because the server emits frames in a strict order).
func (tc *testClient) waitForType(typ string) map[string]interface{} {
	tc.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var skipped []map[string]interface{}
	for time.Now().Before(deadline) {
		frame := tc.next()
		if frame["type"] == typ {
			return frame
		}
		skipped = append(skipped, frame)
	}
	tc.t.Fatalf("timed out waiting for frame of type %q (skipped: %v)", typ, skipped)
	return nil
}

func (tc *testClient) sendCommand(id interface{}, mtype string, kv map[string]interface{}) {
	tc.t.Helper()
	msg := kv
	if msg == nil {
		msg = map[string]interface{}{}
	}
	msg["type"] = mtype
	msg["id"] = id
	tc.send(msg)
}

func (tc *testClient) close() { _ = tc.conn.Close() }

func TestWebSocketHappyPath(t *testing.T) {
	h := startTestWebSocketServer(t)
	defer h.close()

	a := dial(t, h.url)
	defer a.close()
	b := dial(t, h.url)
	defer b.close()

	a.sendCommand(1, "bind", map[string]interface{}{"appid": "app-test", "side": "sideA"})
	aAck := a.waitForType("ack")
	if aAck["id"].(float64) != 1 {
		t.Fatalf("expected ack id 1, got %v", aAck)
	}
	b.sendCommand(1, "bind", map[string]interface{}{"appid": "app-test", "side": "sideB"})
	b.waitForType("ack")

	// claim from both sides
	a.sendCommand(2, "claim", map[string]interface{}{"nameplate": "42"})
	aClaim := a.waitForType("claimed")
	aMailbox, _ := aClaim["mailbox"].(string)
	if aMailbox == "" {
		t.Fatal("expected mailbox id in claimed")
	}
	b.sendCommand(2, "claim", map[string]interface{}{"nameplate": "42"})
	bClaim := b.waitForType("claimed")
	if bClaim["mailbox"].(string) != aMailbox {
		t.Fatal("expected both sides to claim the same mailbox")
	}

	// open mailbox on both sides
	a.sendCommand(3, "open", map[string]interface{}{"mailbox": aMailbox})
	a.waitForType("ack")
	b.sendCommand(3, "open", map[string]interface{}{"mailbox": aMailbox})
	b.waitForType("ack")

	// A adds a message; A and B both receive it (echo semantics)
	// (the message id is the add command's id, as in the reference)
	a.sendCommand(1234, "add", map[string]interface{}{"phase": "0", "body": "deadbeef"})
	msgA := a.waitForType("message")
	if msgA["body"] != "deadbeef" {
		t.Fatalf("wrong body: %v", msgA)
	}
	if msgA["id"].(float64) != 1234 {
		t.Fatalf("wrong msg id: %v", msgA)
	}
	if msgA["side"] != "sideA" {
		t.Fatalf("wrong side: %v", msgA)
	}
	msgB := b.waitForType("message")
	if msgB["body"] != "deadbeef" {
		t.Fatalf("wrong body on side B: %v", msgB)
	}

	// list shows the allocated nameplate
	b.sendCommand(5, "list", nil)
	list := b.waitForType("nameplates")
	nameplates, _ := list["nameplates"].([]interface{})
	if len(nameplates) != 1 {
		t.Fatalf("expected 1 nameplate, got %v", nameplates)
	}

	// ping/pong
	a.sendCommand(6, "ping", map[string]interface{}{"ping": 42})
	pong := a.waitForType("pong")
	if pong["pong"].(float64) != 42 {
		t.Fatalf("wrong pong: %v", pong)
	}

	// both close; mailbox is deleted
	a.sendCommand(7, "close", map[string]interface{}{"mood": "happy"})
	a.waitForType("closed")
	b.sendCommand(7, "close", map[string]interface{}{"mood": "happy"})
	b.waitForType("closed")

	var count int
	_ = h.db.QueryRow("SELECT COUNT(*) FROM `mailboxes`").Scan(&count)
	if count != 0 {
		t.Fatalf("expected mailbox to be deleted after both closes, got %d", count)
	}
}

func TestWebSocketErrors(t *testing.T) {
	h := startTestWebSocketServer(t)
	defer h.close()

	c := dial(t, h.url)
	defer c.close()

	// unknown type before bind -> ack then "must bind first" (reference behavior)
	c.sendCommand(1, "bogus", nil)
	c.waitForType("ack")
	errFrame := c.waitForType("error")
	if errFrame["error"] != "must bind first" {
		t.Fatalf("expected 'must bind first', got %v", errFrame["error"])
	}

	// missing 'type' -> error without ack
	c.send(map[string]interface{}{"x": 1})
	errFrame = c.waitForType("error")
	if errFrame["error"] != "missing 'type'" {
		t.Fatalf("expected \"missing 'type'\", got %v", errFrame["error"])
	}

	// list before bind -> must bind first
	c.sendCommand(2, "list", nil)
	c.waitForType("ack")
	errFrame = c.waitForType("error")
	if errFrame["error"] != "must bind first" {
		t.Fatalf("expected 'must bind first', got %v", errFrame["error"])
	}

	// bind, then allocate twice -> second is rejected
	c.sendCommand(3, "bind", map[string]interface{}{"appid": "app-test", "side": "sideA"})
	c.waitForType("ack")
	c.sendCommand(4, "allocate", nil)
	c.waitForType("ack")
	c.waitForType("allocated")
	c.sendCommand(5, "allocate", nil)
	c.waitForType("ack")
	errFrame = c.waitForType("error")
	if errFrame["error"] != "you already allocated one, don't be greedy" {
		t.Fatalf("unexpected error: %v", errFrame["error"])
	}

	// release without claim -> error
	c.sendCommand(6, "release", nil)
	c.waitForType("ack")
	errFrame = c.waitForType("error")
	if errFrame["error"] != "release without nameplate must follow claim" {
		t.Fatalf("unexpected error: %v", errFrame["error"])
	}

	// claim with invalid nameplate -> error
	c.sendCommand(7, "claim", map[string]interface{}{"nameplate": "abc"})
	c.waitForType("ack")
	errFrame = c.waitForType("error")
	if errFrame["error"] == nil || errFrame["error"] == "" {
		t.Fatalf("expected error for invalid nameplate, got %v", errFrame)
	}

	// unknown type after bind -> ack then "unknown type"
	c.sendCommand(7, "bogus", nil)
	c.waitForType("ack")
	errFrame = c.waitForType("error")
	if errFrame["error"] != "unknown type" {
		t.Fatalf("expected 'unknown type', got %v", errFrame["error"])
	}

	// double claim -> error
	d := dial(t, h.url)
	defer d.close()
	d.sendCommand(1, "bind", map[string]interface{}{"appid": "app-test", "side": "sideD"})
	d.waitForType("ack")
	d.sendCommand(2, "claim", map[string]interface{}{"nameplate": "77"})
	d.waitForType("ack")
	d.waitForType("claimed")
	d.sendCommand(3, "claim", map[string]interface{}{"nameplate": "78"})
	d.waitForType("ack")
	errFrame = d.waitForType("error")
	if errFrame["error"] != "only one claim per connection" {
		t.Fatalf("unexpected error: %v", errFrame["error"])
	}
}

func TestWebSocketCrowded(t *testing.T) {
	h := startTestWebSocketServer(t)
	defer h.close()

	sides := []string{"sideA", "sideB", "sideC"}
	clients := make([]*testClient, 0, 3)
	defer func() {
		for _, cl := range clients {
			cl.close()
		}
	}()

	for i, side := range sides {
		cl := dial(t, h.url)
		cl.sendCommand(1, "bind", map[string]interface{}{"appid": "app-test", "side": side})
		cl.waitForType("ack")
		clients = append(clients, cl)
		_ = i
	}
	for i, cl := range clients {
		cl.sendCommand(2, "claim", map[string]interface{}{"nameplate": "42"})
		cl.waitForType("ack")
		if i < 2 {
			frame := cl.waitForType("claimed")
			if frame["mailbox"] == nil {
				t.Fatalf("side %d should be claimed", i)
			}
		} else {
			frame := cl.waitForType("error")
			if frame["error"] != "crowded" {
				t.Fatalf("expected crowded for third side, got %v", frame["error"])
			}
		}
	}
}

func TestWebSocketMessageReplay(t *testing.T) {
	h := startTestWebSocketServer(t)
	defer h.close()

	a := dial(t, h.url)
	defer a.close()
	a.sendCommand(1, "bind", map[string]interface{}{"appid": "app-test", "side": "sideA"})
	a.waitForType("ack")
	a.sendCommand(2, "claim", map[string]interface{}{"nameplate": "42"})
	a.waitForType("ack")
	aMailbox := a.waitForType("claimed")["mailbox"].(string)

	a.sendCommand(3, "open", map[string]interface{}{"mailbox": aMailbox})
	a.waitForType("ack")
	a.sendCommand(4, "add", map[string]interface{}{"phase": "0", "body": "aaaa", "id": 1})
	a.waitForType("ack")
	a.waitForType("message")
	a.sendCommand(5, "add", map[string]interface{}{"phase": "1", "body": "bbbb", "id": 2})
	a.waitForType("ack")
	a.waitForType("message")

	// B connects later and should get both messages replayed on open
	b := dial(t, h.url)
	defer b.close()
	b.sendCommand(1, "bind", map[string]interface{}{"appid": "app-test", "side": "sideB"})
	b.waitForType("ack")
	b.sendCommand(2, "claim", map[string]interface{}{"nameplate": "42"})
	b.waitForType("ack")
	b.waitForType("claimed")
	b.sendCommand(3, "open", map[string]interface{}{"mailbox": aMailbox})
	b.waitForType("ack")

	m1 := b.waitForType("message")
	m2 := b.waitForType("message")
	if m1["body"] != "aaaa" || m2["body"] != "bbbb" {
		t.Fatalf("wrong replay order: %v, %v", m1["body"], m2["body"])
	}
}

func TestWebSocketDirectResponsesEchoIDAndServerRx(t *testing.T) {
	h := startTestWebSocketServer(t)
	defer h.close()

	c := dial(t, h.url)
	defer c.close()

	checkID := func(frame map[string]interface{}, want float64) {
		t.Helper()
		got, ok := frame["id"].(float64)
		if !ok {
			t.Fatalf("frame %v missing numeric id", frame["type"])
		}
		if got != want {
			t.Fatalf("frame %v id = %v, want %v", frame["type"], got, want)
		}
		srx, ok := frame["server_rx"].(float64)
		if !ok || srx <= 0 {
			t.Fatalf("frame %v server_rx = %v, want positive number", frame["type"], frame["server_rx"])
		}
		if stx, ok := frame["server_tx"].(float64); !ok || stx <= 0 {
			t.Fatalf("frame %v server_tx = %v, want positive number", frame["type"], frame["server_tx"])
		}
	}

	// ping (before bind; ping is allowed pre-bind)
	c.sendCommand(101, "ping", map[string]interface{}{"ping": "hello"})
	checkID(c.waitForType("pong"), 101)

	// list (before bind returns must-bind error; do it after bind)
	c.sendCommand(102, "bind", map[string]interface{}{"appid": "app-spec", "side": "sideA"})
	c.waitForType("ack")
	c.sendCommand(103, "list", nil)
	c.waitForType("ack")
	checkID(c.waitForType("nameplates"), 103)
	// allocate
	c.sendCommand(104, "allocate", nil)
	c.waitForType("ack")
	checkID(c.waitForType("allocated"), 104)
	// claim (same nameplate as allocate; allocate already claimed it, so
	// re-claim by the same side hits the "already claimed" path? No: the
	// allocate path claims for `side`, and the claim here is from the same
	// side, which is idempotent in nameplate_sides (same side row).)
	c.sendCommand(105, "claim", map[string]interface{}{"nameplate": "9"})
	c.waitForType("ack")
	checkID(c.waitForType("claimed"), 105)
	// repeat claim with same side is idempotent on the server; now release
	c.sendCommand(106, "release", map[string]interface{}{"nameplate": "9"})
	c.waitForType("ack")
	checkID(c.waitForType("released"), 106)
	// open + close; close must echo id and carry server_rx
	c.sendCommand(107, "allocate", nil) // second allocate is rejected for the same connection
	c.waitForType("ack")
	c.waitForType("error")

	// mood-less close on a fresh mailbox to exercise "closed" id/server_rx
	c2 := dial(t, h.url)
	defer c2.close()
	c2.sendCommand(201, "bind", map[string]interface{}{"appid": "app-spec", "side": "sideC"})
	c2.waitForType("ack")
	c2.sendCommand(202, "open", map[string]interface{}{"mailbox": "mbx-spec-test"})
	c2.waitForType("ack")
	c2.sendCommand(203, "close", nil)
	c2.waitForType("ack")
	checkID(c2.waitForType("closed"), 203)
}

func TestWebSocketDirectResponseNullID(t *testing.T) {
	h := startTestWebSocketServer(t)
	defer h.close()

	c := dial(t, h.url)
	defer c.close()

	// command without id: direct response must carry JSON null id
	c.send(map[string]interface{}{"type": "ping", "ping": 7})
	pong := c.waitForType("pong")
	if v, present := pong["id"]; !present || v != nil {
		t.Fatalf("expected explicit null id on pong, got %v (present=%v)", v, present)
	}
	if srx, ok := pong["server_rx"].(float64); !ok || srx <= 0 {
		t.Fatalf("pong server_rx = %v, want positive number", pong["server_rx"])
	}
}
