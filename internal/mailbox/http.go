package mailbox

import (
	"fmt"
	"net/http"
	"time"
)

// Version is the reported server version (Server response header and the
// WebSocket handshake's server header). It is a var so release builds can
// stamp it via -ldflags "-X github.com/dougsko/wormhole-mailbox/internal/mailbox.Version=$(git describe --tags)".
var Version = "0.8.0"

// NewHandler builds the HTTP site: plain-text root at "/" (WebSocket upgrades
// at "/" are also accepted), WebSocket at "/v1" (and "/v1/" for clients that
// append a trailing slash).
// If logRequests is set, requests are logged (mirroring twisted's Site.log).
// pingInterval/timeout follow autobahn's autoPingInterval/autoPingTimeout
// (in seconds), with zero meaning "disabled".
func NewHandler(server *Server, logRequests bool, pingInterval, pingTimeout float64) http.Handler {
	ws := NewWebSocketServer(server,
		time.Duration(pingInterval*float64(time.Second)),
		time.Duration(pingTimeout*float64(time.Second)),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Some clients are configured with just the server root. If this is
		// a WebSocket upgrade, treat it as a connection to /v1 instead of
		// returning the plain-text body with a 200 status.
		if r.URL.Path == "/" && isWebSocketUpgrade(r) {
			ws.ServeHTTP(w, r)
			return
		}
		RootHandler(w, r)
	})
	mux.Handle("/v1", ws)
	// Be liberal in what we accept: clients configured with a trailing
	// slash would otherwise fall through to the plain-text root handler and
	// the WebSocket handshake would fail with "got 200".
	mux.Handle("/v1/{$}", ws)

	var handler http.Handler = mux
	if logRequests {
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("%s %s %s\n", r.RemoteAddr, r.Method, r.URL.Path)
			inner.ServeHTTP(w, r)
		})
	}

	// add the Server header ("Magic Wormhole Mailbox <version>"); it also
	// appears in the WebSocket upgrade response
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "Magic Wormhole Mailbox "+Version)
		handler.ServeHTTP(w, r)
	})
}
