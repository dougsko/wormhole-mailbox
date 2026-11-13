package mailbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// clientError is a protocol-level error reported to the client as
// {"type": "error", "error": ...}.
type clientError struct {
	msg string
}

func (e *clientError) Error() string { return e.msg }

func errorf(format string, args ...interface{}) *clientError {
	return &clientError{msg: fmt.Sprintf(format, args...)}
}

// wsMsg is a parsed client command. RawMessage preserves the bytes so the
// command can be echoed back verbatim as "orig" on error.
type wsMsg map[string]json.RawMessage

func (m wsMsg) msgType() (string, bool) {
	raw, ok := m["type"]
	if !ok {
		return "", false
	}
	var t string
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", false
	}
	return t, true
}

func (m wsMsg) str(key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func (m wsMsg) idRaw() json.RawMessage {
	if raw, ok := m["id"]; ok {
		return raw
	}
	return nil
}

// peerAddress describes the client address, possibly overridden by
// X-Real-IP/X-Real-Port headers (see e.g. Caddy). Spoofing only hurts the
// spoofer; the value is used for hole-punch assistance and logging.
type peerAddress struct {
	typ  string // "ipv4" or "ipv6"
	host string
	port int
}

func peerAddressFromRequest(r *http.Request) peerAddress {
	if xrIP := r.Header.Get("X-Real-IP"); xrIP != "" {
		port, _ := strconv.Atoi(r.Header.Get("X-Real-Port"))
		typ := "ipv4"
		if strings.Contains(xrIP, ":") {
			typ = "ipv6"
		}
		return peerAddress{typ: typ, host: xrIP, port: port}
	}
	host, portStr, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
		portStr = "0"
	}
	typ := "ipv4"
	if strings.Contains(host, ":") {
		typ = "ipv6"
		if strings.HasPrefix(host, "::ffff:") {
			host = strings.TrimPrefix(host, "::ffff:")
			typ = "ipv4"
		}
	}
	port, _ := strconv.Atoi(portStr)
	return peerAddress{typ: typ, host: host, port: port}
}

func (p peerAddress) yourAddress() map[string]interface{} {
	you := map[string]interface{}{"port": p.port}
	if p.typ == "ipv4" {
		you["ipv4"] = p.host
	} else {
		you["ipv6"] = p.host
	}
	return you
}

// websocketConn is one WebSocket connection to a mailbox, bound to at most
// one appid+side and one mailbox at a time.
type websocketConn struct {
	conn   *websocket.Conn
	server *Server
	peer   peerAddress

	app         *AppNamespace
	side        string
	didAllocate bool
	isListening bool
	didClaim    bool
	nameplateID string
	didRelease  bool
	mailbox     *Mailbox
	mailboxID   string
	didClose    bool
	handle      int64

	writeMu    sync.Mutex
	closed     chan struct{}
	once       sync.Once
	connection *Connection

	pingInterval time.Duration
	pingTimeout  time.Duration
}

// WebSocketServer wires a mailbox Server into an http.Handler for /v1.
type WebSocketServer struct {
	server       *Server
	pingInterval time.Duration
	pingTimeout  time.Duration
	upgrader     websocket.Upgrader
}

// NewWebSocketServer builds the handler. autoPingInterval/autoPingTimeout
// match autobahn's defaults (60s / 600s).
func NewWebSocketServer(server *Server, autoPingInterval, autoPingTimeout time.Duration) *WebSocketServer {
	return &WebSocketServer{
		server:       server,
		pingInterval: autoPingInterval,
		pingTimeout:  autoPingTimeout,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// isWebSocketUpgrade reports whether the request is asking for a WebSocket
// upgrade. The upgrader revalidates the full handshake, so this only needs to
// distinguish likely upgrade requests from ordinary HTTP requests.
func isWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

func headerHasToken(h, token string) bool {
	for _, part := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// RootHandler serves the plain-text "Wormhole Relay\n" root.
func RootHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("Wormhole Relay\n"))
}

func (ws *WebSocketServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer := peerAddressFromRequest(r)
	conn, err := ws.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already wrote the HTTP error
	}
	addressID := ws.server.GetAddressID(peer.typ, peer.host)
	connection := ws.server.ConnectionEstablished(addressID, time.Now().Unix())
	wc := &websocketConn{
		conn:         conn,
		server:       ws.server,
		peer:         peer,
		nameplateID:  "",
		closed:       make(chan struct{}),
		connection:   connection,
		pingInterval: ws.pingInterval,
		pingTimeout:  ws.pingTimeout,
	}
	go wc.run()
}

func (wc *websocketConn) yourAddress() map[string]interface{} {
	return wc.peer.yourAddress()
}

// send encodes a frame and writes it with a server_tx timestamp.
func (wc *websocketConn) send(mtype string, kv map[string]interface{}) error {
	msg := map[string]interface{}{}
	for k, v := range kv {
		msg[k] = v
	}
	msg["type"] = mtype
	msg["server_tx"] = float64(time.Now().UnixNano()) / 1e9
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()
	select {
	case <-wc.closed:
		return errors.New("connection closed")
	default:
	}
	_ = wc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return wc.conn.WriteMessage(websocket.TextMessage, payload)
}

func (wc *websocketConn) sendWelcome() {
	welcome := wc.server.GetWelcome()
	welcome["your-address"] = wc.yourAddress()
	_ = wc.send("welcome", map[string]interface{}{"welcome": welcome})
}

func (wc *websocketConn) sendError(expl string, orig wsMsg) {
	_ = wc.send("error", map[string]interface{}{"error": expl, "orig": orig})
}

func (wc *websocketConn) sendAck(id json.RawMessage) {
	if id == nil {
		id = json.RawMessage("null")
	}
	_ = wc.send("ack", map[string]interface{}{"id": id})
}

func (wc *websocketConn) run() {
	defer wc.conn.Close()
	defer wc.onClose()

	// Liveness semantics follow autobahn's autoPingInterval/autoPingTimeout:
	// drop the connection when no pong is received within autoPingTimeout of
	// the server's ping. The read deadline is refreshed on every received
	// frame (data, ping, or pong) — gorilla does not surface control frames
	// through ReadMessage, so the deadline must be refreshed by the handlers
	// or idle-but-responsive clients are dropped.
	if wc.pingTimeout > 0 {
		wc.conn.SetPongHandler(func(appData string) error {
			_ = wc.conn.SetReadDeadline(time.Now().Add(wc.pingTimeout))
			return nil
		})
		// a client ping is equally good evidence of life; keep gorilla's
		// default ping handler (it replies with a pong) and additionally
		// extend the read deadline
		wc.conn.SetPingHandler(func(appData string) error {
			_ = wc.conn.SetReadDeadline(time.Now().Add(wc.pingTimeout))
			if err := wc.conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second)); err != nil {
				return err
			}
			return nil
		})
	}
	if wc.pingInterval > 0 {
		go wc.pingLoop()
	}
	if wc.pingTimeout > 0 {
		_ = wc.conn.SetReadDeadline(time.Now().Add(wc.pingTimeout))
	}

	wc.sendWelcome()

	for {
		if wc.pingTimeout > 0 {
			_ = wc.conn.SetReadDeadline(time.Now().Add(wc.pingTimeout))
		}
		_, payload, err := wc.conn.ReadMessage()
		if err != nil {
			return
		}
		if wc.pingTimeout > 0 {
			_ = wc.conn.SetReadDeadline(time.Now().Add(wc.pingTimeout))
		}
		wc.handleFrame(payload)
	}
}

func (wc *websocketConn) pingLoop() {
	ticker := time.NewTicker(wc.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-wc.closed:
			return
		case <-ticker.C:
			wc.writeMu.Lock()
			_ = wc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = wc.conn.WriteMessage(websocket.PingMessage, []byte{})
			wc.writeMu.Unlock()
		}
	}
}

func (wc *websocketConn) onClose() {
	wc.once.Do(func() {
		close(wc.closed)
	})
	if wc.connection != nil {
		wc.connection.Lost()
	}
	if wc.app != nil && wc.mailbox != nil && wc.isListening {
		wc.mailbox.RemoveListener(wc.handle)
	}
}

func (wc *websocketConn) handleFrame(payload []byte) {
	var msg wsMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		// unlike a well-formed "missing 'type'" command, this is not JSON
		// we can parse at all; report and stay connected (mirrors autobahn's
		// behavior of dropping the frame and logging)
		_ = wc.send("error", map[string]interface{}{
			"error": "could not parse JSON",
			"orig":  map[string]interface{}{},
		})
		return
	}
	mtype, ok := msg.msgType()
	if !ok {
		wc.sendError("missing 'type'", msg)
		return
	}

	now := time.Now().Unix()
	serverRx := float64(time.Now().UnixNano()) / 1e9

	// ack is sent before the command executes (like the reference). Per the
	// spec the ack carries the command `id` for all commands; note that the
	// mailbox section of server-protocol.md claims there is "no explicit ack
	// to the `open` command" — the two statements conflict; we ack `open`
	// because the transport-section rule ("all commands") is generic, the
	// reference acks it too, and wormhole-william blocks on the ack.
	wc.sendAck(msg.idRaw())
	if wc.connection != nil {
		wc.connection.AddMessage(now, mtype)
	}

	// Serialize command handling with everything else (pruning runs under
	// the same lock), mirroring the Python implementation's single-threaded
	// reactor.
	wc.server.mu.Lock()
	defer wc.server.mu.Unlock()

	var herr error
	switch mtype {
	case "ping":
		herr = wc.handlePing(msg, serverRx)
	case "bind":
		herr = wc.handleBind(msg, now)
	default:
		if wc.app == nil {
			herr = errorf("must bind first")
			break
		}
		switch mtype {
		case "list":
			herr = wc.handleList(msg, serverRx)
		case "allocate":
			herr = wc.handleAllocate(msg, now, serverRx)
		case "claim":
			herr = wc.handleClaim(msg, now, serverRx)
		case "release":
			herr = wc.handleRelease(msg, now, serverRx)
		case "open":
			herr = wc.handleOpen(msg, now)
		case "add":
			herr = wc.handleAdd(msg, now)
		case "close":
			herr = wc.handleClose(msg, now, serverRx)
		default:
			herr = errorf("unknown type")
		}
	}
	if herr != nil {
		var ce *clientError
		if errors.As(herr, &ce) {
			wc.sendError(ce.msg, msg)
			return
		}
		// internal error: report generic message; keep connection state sane
		fmt.Printf("internal error handling %q: %v\n", mtype, herr)
		wc.sendError(fmt.Sprintf("internal error: %v", herr), msg)
	}
}

// directKv builds the key/value set for a direct response to a client
// command. Per the spec (server-protocol.md), the command's `id` is copied
// into the direct response, and direct responses include a `server_rx`
// timestamp recording when the client's command was received (float seconds
// since epoch, matching `server_tx`). When the command has no `id`, the
// response carries JSON null (same convention as the `ack`).
func directKv(msg wsMsg, serverRx float64, kv map[string]interface{}) map[string]interface{} {
	if kv == nil {
		kv = map[string]interface{}{}
	}
	if id := msg.idRaw(); id != nil {
		kv["id"] = id
	} else {
		kv["id"] = nil
	}
	kv["server_rx"] = serverRx
	return kv
}

func (wc *websocketConn) handlePing(msg wsMsg, serverRx float64) error {
	raw, ok := msg["ping"]
	if !ok {
		return errorf("ping requires 'ping'")
	}
	return wc.send("pong", directKv(msg, serverRx, map[string]interface{}{"pong": raw}))
}

func (wc *websocketConn) handleBind(msg wsMsg, serverRx int64) error {
	if wc.app != nil || wc.side != "" {
		return errorf("already bound")
	}
	appid, ok := msg.str("appid")
	if !ok {
		return errorf("bind requires 'appid'")
	}
	side, ok := msg.str("side")
	if !ok {
		return errorf("bind requires 'side'")
	}
	wc.app = wc.server.getAppLocked(appid)
	wc.side = side

	impl, ver := "", ""
	if raw, ok := msg["client_version"]; ok {
		var tuple []json.RawMessage
		if err := json.Unmarshal(raw, &tuple); err == nil && len(tuple) >= 2 {
			impl = strOrNone(tuple[0])
			ver = strOrNone(tuple[1])
		}
	}
	if wc.connection != nil {
		wc.connection.Bound(side, impl, ver)
	}
	wc.app.logClientVersion(serverRx, side, impl, ver)
	if wc.app.logRequests {
		v := 4
		if wc.peer.typ == "ipv6" {
			v = 6
		}
		fmt.Printf("ws client connecting: tcp%d:%s:%d\n", v, wc.peer.host, wc.peer.port)
	}
	return nil
}

func strOrNone(raw json.RawMessage) string {
	if string(raw) == "null" || len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func (wc *websocketConn) handleList(msg wsMsg, serverRx float64) error {
	ids := wc.app.GetNameplateIDs()
	nameplates := make([]map[string]interface{}, 0, len(ids))
	for _, id := range ids {
		nameplates = append(nameplates, map[string]interface{}{"id": id})
	}
	return wc.send("nameplates", directKv(msg, serverRx, map[string]interface{}{"nameplates": nameplates}))
}

func (wc *websocketConn) handleAllocate(msg wsMsg, now int64, serverRx float64) error {
	if wc.didAllocate {
		return errorf("you already allocated one, don't be greedy")
	}
	nameplateID, err := wc.app.AllocateNameplate(wc.side, now)
	if err != nil {
		// out of nameplates, or storage problem
		return fmt.Errorf("%v", err)
	}
	wc.didAllocate = true
	return wc.send("allocated", directKv(msg, serverRx, map[string]interface{}{"nameplate": nameplateID}))
}

func (wc *websocketConn) handleClaim(msg wsMsg, now int64, serverRx float64) error {
	nameplateID, ok := msg.str("nameplate")
	if !ok || nameplateID == "" {
		return errorf("claim requires 'nameplate'")
	}
	if wc.didClaim {
		return errorf("only one claim per connection")
	}
	if err := CheckValidNameplate(nameplateID); err != nil {
		return clientErrorFrom(err)
	}
	wc.didClaim = true
	wc.nameplateID = nameplateID
	mailboxID, err := wc.app.ClaimNameplate(nameplateID, wc.side, now)
	if err != nil {
		if errors.Is(err, CrowdedError) {
			return errorf("crowded")
		}
		if errors.Is(err, ReclaimedError) {
			return errorf("reclaimed")
		}
		return err
	}
	return wc.send("claimed", directKv(msg, serverRx, map[string]interface{}{"mailbox": mailboxID}))
}

func clientErrorFrom(err error) error {
	return &clientError{msg: err.Error()}
}

func (wc *websocketConn) handleRelease(msg wsMsg, now int64, serverRx float64) error {
	if wc.didRelease {
		return errorf("only one release per connection")
	}
	nameplateID := ""
	if raw, ok := msg["nameplate"]; ok {
		var np string
		if err := json.Unmarshal(raw, &np); err == nil {
			nameplateID = np
		}
		if wc.nameplateID != "" && nameplateID != wc.nameplateID {
			return errorf("release and claim must use same nameplate")
		}
	} else {
		if wc.nameplateID == "" {
			return errorf("release without nameplate must follow claim")
		}
		nameplateID = wc.nameplateID
	}
	wc.didRelease = true
	if err := wc.app.ReleaseNameplate(nameplateID, wc.side, now); err != nil {
		return err
	}
	return wc.send("released", directKv(msg, serverRx, nil))
}

func (wc *websocketConn) handleOpen(msg wsMsg, now int64) error {
	if wc.mailbox != nil {
		return errorf("only one open per connection")
	}
	mailboxID, ok := msg.str("mailbox")
	if !ok {
		return errorf("open requires 'mailbox'")
	}
	wc.mailboxID = mailboxID
	mailbox, err := wc.app.OpenMailbox(mailboxID, wc.side, now)
	if err != nil {
		if errors.Is(err, CrowdedError) {
			return errorf("crowded")
		}
		return err
	}
	wc.mailbox = mailbox
	// send old messages first, subscribing to future ones
	wc.handle = wc.server.NextListenerHandle()
	wc.isListening = true
	_send := func(sm SidedMessage) {
		_ = wc.send("message", messageFrame(sm))
	}
	old := mailbox.AddListener(wc.handle, _send, func() {})
	for _, sm := range old {
		_send(sm)
	}
	return nil
}

func messageFrame(sm SidedMessage) map[string]interface{} {
	id := json.RawMessage("null")
	if len(sm.MsgID) > 0 {
		id = sm.MsgID
	}
	return map[string]interface{}{
		"side":      sm.Side,
		"phase":     sm.Phase,
		"body":      sm.Body,
		"server_rx": float64(sm.ServerRx),
		"id":        id,
	}
}

func (wc *websocketConn) handleAdd(msg wsMsg, now int64) error {
	if wc.mailbox == nil {
		return errorf("must open mailbox before adding")
	}
	phase, ok := msg.str("phase")
	if !ok {
		return errorf("missing 'phase'")
	}
	body, ok := msg.str("body")
	if !ok {
		return errorf("missing 'body'")
	}
	var id json.RawMessage
	if raw, ok := msg["id"]; ok {
		id = raw
	}
	sm := SidedMessage{
		Side:     wc.side,
		Phase:    phase,
		Body:     body,
		ServerRx: now,
		MsgID:    id,
	}
	return wc.mailbox.AddMessage(sm)
}

func (wc *websocketConn) handleClose(msg wsMsg, now int64, serverRx float64) error {
	if wc.didClose {
		return errorf("only one close per connection")
	}
	mailboxID := ""
	if raw, ok := msg["mailbox"]; ok {
		var mb string
		if err := json.Unmarshal(raw, &mb); err == nil {
			mailboxID = mb
		}
		if wc.mailboxID != "" && mailboxID != wc.mailboxID {
			return errorf("open and close must use same mailbox")
		}
	} else {
		if wc.mailboxID == "" {
			return errorf("close without mailbox must follow open")
		}
		mailboxID = wc.mailboxID
	}
	var mood *string
	if raw, ok := msg["mood"]; ok {
		var m string
		if err := json.Unmarshal(raw, &m); err == nil {
			mood = &m
		}
	}

	if wc.mailbox == nil {
		mailbox, err := wc.app.OpenMailbox(mailboxID, wc.side, now)
		if err != nil {
			if errors.Is(err, CrowdedError) {
				return errorf("crowded")
			}
			return err
		}
		wc.mailbox = mailbox
	}
	if wc.isListening {
		wc.mailbox.RemoveListener(wc.handle)
		wc.isListening = false
	}
	wc.didClose = true
	if err := wc.mailbox.Close(wc.side, mood, now); err != nil {
		return err
	}
	wc.mailbox = nil
	return wc.send("closed", directKv(msg, serverRx, nil))
}
