package mailbox

import (
	"fmt"
	"net/http"
	"time"
)

// Version is the reported server version (Server response header and the
// WebSocket handshake's server header).
const Version = "0.8.0"

// NewHandler builds the HTTP site: plain-text root at "/", WebSocket at "/v1".
// If logRequests is set, requests are logged (mirroring twisted's Site.log).
// pingInterval/timeout follow autobahn's autoPingInterval/autoPingTimeout
// (in seconds), with zero meaning "disabled".
func NewHandler(server *Server, logRequests bool, pingInterval, pingTimeout float64) http.Handler {
	ws := NewWebSocketServer(server,
		time.Duration(pingInterval*float64(time.Second)),
		time.Duration(pingTimeout*float64(time.Second)),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/", RootHandler)
	mux.Handle("/v1", ws)

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
