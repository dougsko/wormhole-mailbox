# E2E interoperability tests

End-to-end tests for `mailbox-server` that exercise **real transfers**
through a freshly built, real `mailbox-server` binary on 127.0.0.1:

| Test | Sender | Receiver | Payload |
|------|--------|----------|---------|
| `TestTextGoToPython` | wormhole-william (Go) | magic-wormhole (Python) | text |
| `TestFileGoToPython` | wormhole-william (Go) | magic-wormhole (Python) | file |
| `TestDirectoryGoToPython` | wormhole-william (Go) | magic-wormhole (Python) | directory |
| `TestTextPythonToGo` | magic-wormhole (Python) | wormhole-william (Go) | text |
| `TestFilePythonToGo` | magic-wormhole (Python) | wormhole-william (Go) | file |
| `TestDirectoryPythonToGo` | magic-wormhole (Python) | wormhole-william (Go) | directory |
| `TestFilePythonToPython` | magic-wormhole (Python) | magic-wormhole (Python) | file (control) |
| `TestFileGoToGo` | wormhole-william (Go) | wormhole-william (Go) | file (control) |

Received bytes are verified by SHA-256. The Go side talks to the mailbox
using the local `wormhole-william` checkout (via a `replace` directive in
`e2e/go.mod`), so uncommitted client changes are exercised too.

Each test boots its own `mailbox-server` (fresh temporary channel database)
and, when available, a local transit relay (`twistd transitrelay`). No
traffic leaves 127.0.0.1 and the public `relay.magic-wormhole.io` /
`transit.magic-wormhole.io` servers are never contacted.

## Requirements

- Go (see `../go.mod` for the version)
- Python with [`magic-wormhole`](https://pypi.org/project/magic-wormhole/)
  and [`magic-wormhole-transit-relay`](https://pypi.org/project/magic-wormhole-transit-relay/) installed, e.g.:

  ```sh
  pip install magic-wormhole magic-wormhole-transit-relay
  ```

- a checkout of `wormhole-william` as a **sibling** of this repository:

  ```
  bin/
    wormhole-mailbox/   # this repository
    wormhole-william/   # github.com/dougsko/wormhole-william
  ```

  If your checkout lives elsewhere, edit the `replace` directive in
  `e2e/go.mod`.

## Running

```sh
cd e2e
go test -tags=e2e -v .
```

The `e2e` build tag keeps the default `go test ./...` fast and free of the
Python requirement.

## Environment variables

| Variable | Meaning |
|----------|---------|
| `WORMHOLE_E2E_PYTHON` | path to the Python `wormhole` entry script (default: `/tmp/wormhole-venv/bin/wormhole`, then `wormhole` from `PATH`) |
| `WORMHOLE_E2E_TWISTD` | path to `twistd` (default: sibling of the `wormhole` script, then `PATH`) |
| `WORMHOLE_E2E_REQUIRE=1` | fail instead of skip when the Python client is missing (used by CI) |
| `WORMHOLE_E2E_NO_TRANSIT=1` | no transit relay: direct TCP only. Go-sender file/directory tests skip (wormhole-william requires a reachable transit relay to send); everything else still runs |

When `twistd` cannot be found, the clients are pointed at a discard port so
they can never fall back to the public transit relay; the Go-sender
file/directory tests are skipped in that mode as well. Text-message tests
always run (they ride the mailbox, not the transit relay).

## CI

The `e2e` job in `.github/workflows/ci.yaml` checks out both repositories,
installs the Python packages and runs the suite with
`WORMHOLE_E2E_REQUIRE=1`.
