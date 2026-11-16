package mailbox

// Benchmarks for the websocket mailbox hot path. These exist to measure the
// effect of decoupling listener broadcasts from the server-wide command
// lock; run with:
//
//	go test ./internal/mailbox -run '^$' -bench=. -benchmem -count=10
//
// Compare runs with benchstat (golang.org/x/perf/cmd/benchstat).
//
// Note on determinism: each benchmark uses a fresh appid per benchmark
// goroutine, so mailboxes never collide and no cross-goroutine contention
// on nameplates occurs beyond SQLite itself (which is deliberately
// serialized, single-connection, matching the reference implementation).

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type benchHarness struct {
	url   string
	close func()
}

func newBenchHarness(b *testing.B) *benchHarness {
	b.Helper()
	dir := b.TempDir()
	dbfile := dir + "/relay.sqlite"
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		b.Fatal(err)
	}
	blur := 3600
	server := NewServer(db, true, map[string]interface{}{}, &blur, nil, nil)
	handler := NewHandler(server, false, 60, 600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler}
	go func() { _ = httpServer.Serve(listener) }()
	return &benchHarness{
		url: "ws://" + listener.Addr().String() + "/v1",
		close: func() {
			_ = httpServer.Close()
			_ = listener.Close()
			time.Sleep(50 * time.Millisecond)
			_ = db.Close()
		},
	}
}

type benchClient struct {
	conn *websocket.Conn
}

var benchSeq atomic.Int64

func newBenchClient(b *testing.B, url string) *benchClient {
	b.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		b.Fatal(err)
	}
	c := &benchClient{conn: conn}
	c.expectType(b, "welcome")
	return c
}

func (c *benchClient) send(b *testing.B, msg map[string]interface{}) {
	b.Helper()
	payload, err := json.Marshal(msg)
	if err != nil {
		b.Fatal(err)
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		b.Fatal(err)
	}
}

func (c *benchClient) command(b *testing.B, id int64, mtype string, kv map[string]interface{}) {
	b.Helper()
	msg := map[string]interface{}{}
	for k, v := range kv {
		msg[k] = v
	}
	msg["type"] = mtype
	msg["id"] = id
	c.send(b, msg)
}

// readFrame reads one frame, skipping nothing.
func (c *benchClient) readFrame(b *testing.B) map[string]interface{} {
	b.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, payload, err := c.conn.ReadMessage()
	if err != nil {
		b.Fatalf("read frame: %v", err)
	}
	var frame map[string]interface{}
	if err := json.Unmarshal(payload, &frame); err != nil {
		b.Fatal(err)
	}
	return frame
}

// expectType reads frames until a frame of type typ arrives (or a deadline
// hits, failing the benchmark).
func (c *benchClient) expectType(b *testing.B, typ string) map[string]interface{} {
	b.Helper()
	for range 50 {
		frame := c.readFrame(b)
		if frame["type"] == typ {
			return frame
		}
	}
	b.Fatalf("did not receive frame of type %q", typ)
	return nil
}

// setupMailboxPairs binds two fresh sides of a fresh app and claims a
// nameplate, returning (sideA, sideB, mailboxID).
func setupMailboxPair(b *testing.B, url, appID string) (a, bb *benchClient, mailbox string) {
	b.Helper()
	a = newBenchClient(b, url)
	bb = newBenchClient(b, url)
	a.command(b, 1, "bind", map[string]interface{}{"appid": appID, "side": "sideA"})
	a.expectType(b, "ack")
	bb.command(b, 1, "bind", map[string]interface{}{"appid": appID, "side": "sideB"})
	bb.expectType(b, "ack")
	a.command(b, 2, "claim", map[string]interface{}{"nameplate": "1"})
	claimed := a.expectType(b, "claimed")
	mailbox, _ = claimed["mailbox"].(string)
	bb.command(b, 2, "claim", map[string]interface{}{"nameplate": "1"})
	bb.expectType(b, "claimed")
	a.command(b, 3, "open", map[string]interface{}{"mailbox": mailbox})
	a.expectType(b, "ack")
	bb.command(b, 3, "open", map[string]interface{}{"mailbox": mailbox})
	bb.expectType(b, "ack")
	return a, bb, mailbox
}

func messageBody(b *testing.B, size int) string {
	// deterministic pseudo-payload: "a" repeated size*2 hex chars is not
	// interesting data, but the mailbox server treats bodies as opaque
	// strings; size only affects bytes on the wire and into SQLite
	return hex.EncodeToString(make([]byte, size))
}

// BenchmarkAddMessageEcho measures the steady-state cost of one message
// being added by side A and received by both sides (the echo semantics of
// the mailbox protocol).
func BenchmarkAddMessageEcho(b *testing.B) {
	h := newBenchHarness(b)
	defer h.close()

	b.RunParallel(func(pb *testing.PB) {
		appID := fmt.Sprintf("bench-echo-%d", benchSeq.Add(1))
		a, c, _ := setupMailboxPair(b, h.url, appID)
		defer a.conn.Close()
		defer c.conn.Close()
		body := messageBody(b, 64)
		id := int64(1000)
		b.ResetTimer()
		for pb.Next() {
			id++
			a.command(b, id, "add", map[string]interface{}{"phase": "0", "body": body})
			a.expectType(b, "message")
			c.expectType(b, "message")
		}
		b.StopTimer()
	})
}

// BenchmarkBindClaimOpen measures the full cost of establishing a complete
// two-sided mailbox session (bind, claim, open, one add), including client
// setup. This captures session-establishment cost and DB page churn.
func BenchmarkBindClaimOpen(b *testing.B) {
	h := newBenchHarness(b)
	defer h.close()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			appID := fmt.Sprintf("bench-setup-%d", benchSeq.Add(1))
			a, c, _ := setupMailboxPair(b, h.url, appID)
			a.command(b, 100, "add", map[string]interface{}{"phase": "0", "body": messageBody(b, 16)})
			a.expectType(b, "message")
			c.expectType(b, "message")
			// close both sides (waits for the acks) so the mailbox is
			// deleted, keeping DB growth flat and the logs quiet
			a.command(b, 101, "close", map[string]interface{}{})
			c.command(b, 101, "close", map[string]interface{}{})
			a.expectType(b, "closed")
			c.expectType(b, "closed")
			a.conn.Close()
			c.conn.Close()
		}
	})
}
