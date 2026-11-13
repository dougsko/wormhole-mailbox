module github.com/dougsko/wormhole-mailbox/e2e

go 1.26.0

require github.com/dougsko/wormhole-william v1.0.8

require (
	github.com/klauspost/compress v1.20.3 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	nhooyr.io/websocket v1.8.17 // indirect
)

// The E2E suite deliberately tests the wormhole-william library from the
// local checkout so that uncommitted client changes are exercised too.
// The repos are expected to be siblings (see README.md); if they are not,
// point the replace directive at your wormhole-william clone instead.
replace github.com/dougsko/wormhole-william => ../../wormhole-william
