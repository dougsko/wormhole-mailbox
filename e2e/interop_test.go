//go:build e2e

// Package e2e contains end-to-end interoperability tests that talk to a
// real mailbox-server binary built from this repository:
//
//   - the Python magic-wormhole client talks to the mailbox-server
//     (both as sender and as receiver), and
//   - the wormhole-william Go library talks to the mailbox-server as well,
//     including cross-direction transfers where one side is the Python
//     client and the other is the Go library.
//
// The transfers cover text messages, files and directories, with the
// received bytes verified by checksum. A local transit relay (run via
// twistd) is started as well so transfers do not depend on direct TCP
// being viable; set WORMHOLE_E2E_NO_TRANSIT=1 to rely on direct TCP only.
// In that mode the Go sender file/directory tests skip (wormhole-william
// requires a reachable transit relay to send); everything else runs.
// If twistd cannot be found, the clients are pointed at a discard port so
// they can never fall back to the public transit relay.
//
// Requirements (tests skip gracefully if missing, unless
// WORMHOLE_E2E_REQUIRE=1 is set, in which case they fail):
//
//   - a Python environment with magic-wormhole installed
//     (or point WORMHOLE_E2E_PYTHON at the 'wormhole' entry script and
//     WORMHOLE_E2E_TWISTD at the 'twistd' entry script)
//	- a checkout of wormhole-william next to this repository
//
// Run from the e2e directory with:
//
//	go test -tags=e2e -v .

package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dougsko/wormhole-william/wormhole"
)

const (
	testTimeout     = 90 * time.Second
	serverReadyWait = 15 * time.Second
)

// ---------- harness ----------

type stack struct {
	relayURL    string // ws://127.0.0.1:PORT/v1
	transitAddr string // 127.0.0.1:PORT (or 127.0.0.1:9 discard in direct-only mode)
	mboxLog     string
	transitLog  string
	// transitDisabled is true when no usable transit relay is available
	// (twistd missing, or WORMHOLE_E2E_NO_TRANSIT=1). In that case transfers
	// go direct-TCP-only, and the Go sender tests are skipped because
	// wormhole-william requires a reachable transit relay for sends.
	transitDisabled bool

	mboxCmd    *exec.Cmd
	transitCmd *exec.Cmd
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForTCP(t *testing.T, addr string, d time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(d)
	var err error
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			return nil
		}
		err = derr
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s: %v", addr, err)
}

// waitForWebSocket confirms the mailbox answers on /v1 (the websocket
// handshake needs an HTTP server that is actually serving, not just a
// socket in listen state).
func waitForWebSocket(t *testing.T, relayURL string, d time.Duration) error {
	t.Helper()
	// relayURL is ws://host/v1; probe with a plain HTTP GET and accept any
	// response (the upgrade will be rejected, but that proves the handler
	// is mounted).
	httpURL := "http://" + strings.TrimPrefix(relayURL, "ws://")
	deadline := time.Now().Add(d)
	var err error
	for time.Now().Before(deadline) {
		resp, derr := http.Get(httpURL) //nolint:gosec // localhost only
		if derr == nil {
			_ = resp.Body.Close()
			return nil
		}
		err = derr
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for mailbox at %s: %v", relayURL, err)
}

var mailboxBinOnce = struct {
	path string
	err  error
	done bool
}{}

func buildMailbox(t *testing.T) string {
	t.Helper()
	if mailboxBinOnce.done {
		if mailboxBinOnce.err != nil {
			t.Fatalf("mailbox-server build failed earlier: %v", mailboxBinOnce.err)
		}
		return mailboxBinOnce.path
	}
	tmp, err := os.MkdirTemp("", "mailbox-server-bin")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "mailbox-server")
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/mailbox-server")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		mailboxBinOnce.err = fmt.Errorf("go build ./cmd/mailbox-server: %v\n%s", err, out)
		t.Fatalf("%v", mailboxBinOnce.err)
	}
	mailboxBinOnce.path = bin
	mailboxBinOnce.done = true
	return bin
}

// pythonTool locates the 'wormhole' entry script; returns "" if absent.
func pythonTool() string {
	if p := os.Getenv("WORMHOLE_E2E_PYTHON"); p != "" {
		return p
	}
	for _, cand := range []string{
		"/tmp/wormhole-venv/bin/wormhole",
		"wormhole", // PATH lookup
	} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	return ""
}

// twistdTool locates twistd, derived from the wormhole script location by
// default. Returns "" if absent.
func twistdTool() string {
	if p := os.Getenv("WORMHOLE_E2E_TWISTD"); p != "" {
		return p
	}
	if wv := pythonTool(); wv != "" {
		cand := filepath.Join(filepath.Dir(wv), "twistd")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	if p, err := exec.LookPath("twistd"); err == nil {
		return p
	}
	return ""
}

func requirePython(t *testing.T) (string, string) {
	t.Helper()
	wormhole := pythonTool()
	if wormhole == "" {
		if os.Getenv("WORMHOLE_E2E_REQUIRE") == "1" {
			t.Fatal("python magic-wormhole client not found; set WORMHOLE_E2E_PYTHON")
		}
		t.Skip("python magic-wormhole client not found; set WORMHOLE_E2E_PYTHON (or WORMHOLE_E2E_REQUIRE=1)")
	}
	// sanity check: the venv must actually have magic-wormhole importable
	out, err := exec.Command(wormhole, "--version").CombinedOutput()
	if err != nil {
		if os.Getenv("WORMHOLE_E2E_REQUIRE") == "1" {
			t.Fatalf("python wormhole client %q not runnable: %v\n%s", wormhole, err, out)
		}
		t.Skipf("python wormhole client %q not runnable: %v", wormhole, err)
	}
	return wormhole, twistdTool()
}

// startStack boots a mailbox-server and (unless disabled) a transit relay
// on 127.0.0.1 with random free ports, using a fresh temporary channel DB.
func startStack(t *testing.T) *stack {
	t.Helper()
	_, tp := requirePython(t)

	mboxPort := freePort(t)
	dir := t.TempDir()
	mboxLog := filepath.Join(dir, "mailbox.log")
	mboxCmd := exec.Command(buildMailbox(t),
		"--port", fmt.Sprintf("tcp:%d:interface=127.0.0.1", mboxPort),
		"--channel-db", filepath.Join(dir, "relay.sqlite"),
	)
	mboxCmd.Dir = dir // keep cwd clean (no pid files etc.)
	mboxCmd.Stdout = mustAppendFile(t, mboxLog)
	mboxCmd.Stderr = mboxCmd.Stdout
	if err := mboxCmd.Start(); err != nil {
		t.Fatal(err)
	}

	s := &stack{
		relayURL: fmt.Sprintf("ws://127.0.0.1:%d/v1", mboxPort),
		mboxLog:  mboxLog,
		mboxCmd:  mboxCmd,
	}
	t.Cleanup(func() {
		if s.mboxCmd.Process != nil {
			_ = s.mboxCmd.Process.Kill()
			_ = s.mboxCmd.Wait()
		}
	})

	if err := waitForWebSocket(t, s.relayURL, serverReadyWait); err != nil {
		t.Fatalf("mailbox did not come up: %v (log: %s)", err, mboxLog)
	}

	// surface server logs when the test fails; they are not on stdout by
	// default because the server logs at info level on every connection
	dumpLogOnFailure := func(name, path string) {
		t.Cleanup(func() {
			if !t.Failed() {
				return
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Logf("could not read %s log %s: %v", name, path, err)
				return
			}
			const tail = 4000
			if len(data) > tail {
				data = data[len(data)-tail:]
			}
			t.Logf("=== %s log (tail) ===\n%s", name, data)
		})
	}
	dumpLogOnFailure("mailbox", mboxLog)

	if os.Getenv("WORMHOLE_E2E_NO_TRANSIT") == "1" || tp == "" {
		// Direct TCP only. Point clients at the discard port instead of
		// leaving TransitRelayAddress empty, which would make them fall
		// back to the public transit.magic-wormhole.io relay.
		//
		// Text transfers are unaffected (they ride the mailbox). Python
		// senders work with a dead relay candidate (direct TCP wins the
		// candidate race). wormhole-william *senders* dial the relay up
		// front and hard-fail without one, so send-side Go tests skip in
		// this mode.
		if tp == "" {
			t.Log("twistd not found: transit relay disabled; Go sender file/directory tests will skip")
		} else {
			t.Log("WORMHOLE_E2E_NO_TRANSIT=1: Go sender file/directory tests will skip")
		}
		s.transitAddr = "127.0.0.1:9"
		s.transitDisabled = true
		return s
	}

	transitPort := freePort(t)
	transitLog := filepath.Join(dir, "transit.log")
	tcmd := exec.Command(tp, "-n", "transitrelay",
		"--port="+fmt.Sprintf("tcp:%d:interface=127.0.0.1", transitPort))
	tcmd.Dir = dir // twistd writes twistd.pid into its cwd
	tcmd.Stdout = mustAppendFile(t, transitLog)
	tcmd.Stderr = tcmd.Stdout
	if err := tcmd.Start(); err != nil {
		t.Fatalf("start transit relay: %v (set WORMHOLE_E2E_NO_TRANSIT=1 to skip it)", err)
	}
	s.transitCmd = tcmd
	s.transitAddr = fmt.Sprintf("127.0.0.1:%d", transitPort)
	s.transitLog = transitLog
	dumpLogOnFailure("transit", transitLog)
	t.Cleanup(func() {
		if s.transitCmd.Process != nil {
			_ = s.transitCmd.Process.Kill()
			_ = s.transitCmd.Wait()
		}
	})
	if err := waitForTCP(t, s.transitAddr, serverReadyWait); err != nil {
		t.Fatalf("transit relay did not come up: %v (log: %s)", err, transitLog)
	}
	return s
}

func mustAppendFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func pythonCmd(ctx context.Context, python string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, python, args...)
	return cmd
}

func (s *stack) pyArgs(extra ...string) []string {
	args := []string{"--relay-url", s.relayURL}
	if s.transitAddr != "" {
		args = append(args, "--transit-helper", "tcp:"+s.transitAddr)
	}
	return append(args, extra...)
}

func goClient(s *stack) *wormhole.Client {
	return &wormhole.Client{
		RendezvousURL:       s.relayURL,
		TransitRelayAddress: s.transitAddr,
		PassPhraseComponentLength: 2,
	}
}

// requireRelayForSend skips tests whose Go-side sender cannot work without
// a reachable transit relay.
func requireRelayForSend(t *testing.T, s *stack) {
	t.Helper()
	if s.transitDisabled {
		t.Skip("wormhole-william requires a reachable transit relay to send; install magic-wormhole-transit-relay (twistd) or unset WORMHOLE_E2E_NO_TRANSIT")
	}
}

// ---------- payload fixtures ----------

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// makeDirectory builds a nested tree inside root/<dirname> and returns
// (root, dirname, map[relpath]sha256hex).
func makeDirectory(t *testing.T, dirname string) (string, string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, dirname)
	m := map[string]string{}
	put := func(rel string, data []byte) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data)
		m[filepath.ToSlash(filepath.Join(dirname, rel))] = hex.EncodeToString(h[:])
	}
	put("a.json", []byte(`{"hello":"world","n":12345}`+"\n"))
	put("sub/b.txt", []byte("hello world\n"))
	blob := make([]byte, 256*1024)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	put("blob.bin", blob)
	put("sub/deep/c.dat", []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff})
	return root, dirname, m
}

// walkChecksums walks dir and returns map[relpath]sha256hex, relativized
// against the parent of dir (so paths start with "<dirname>/").
func walkChecksums(t *testing.T, dir string) map[string]string {
	t.Helper()
	base := filepath.Dir(dir)
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		sum, err := func() (string, error) {
			f, err := os.Open(path)
			if err != nil {
				return "", err
			}
			defer f.Close()
			h := sha256.New()
			_, err = io.Copy(h, f)
			return hex.EncodeToString(h.Sum(nil)), err
		}()
		if err != nil {
			t.Fatal(err)
		}
		m[filepath.ToSlash(rel)] = sum
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func compareTrees(t *testing.T, want, got map[string]string) {
	t.Helper()
	for _, k := range sortedKeys(want) {
		g, ok := got[k]
		if !ok {
			t.Errorf("missing file in received tree: %s", k)
			continue
		}
		if g != want[k] {
			t.Errorf("checksum mismatch for %s", k)
		}
	}
	for _, k := range sortedKeys(got) {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected file in received tree: %s", k)
		}
	}
}

// ---------- Go -> Python ----------

func TestTextGoToPython(t *testing.T) {
	s := startStack(t)
	py, _ := requirePython(t)

	payload := "interop text payload 👍 — wormhole-william -> magic-wormhole"
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	code, results, err := goClient(s).SendText(ctx, payload)
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}

	outDir := t.TempDir()
	cmd := pythonCmd(ctx, py, s.pyArgs("receive", "--accept-file", code)...)
	cmd.Dir = outDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python receive failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if got := strings.TrimSuffix(stdout.String(), "\n"); got != payload {
		t.Fatalf("text mismatch: got %q, want %q", got, payload)
	}
	res := <-results
	if !res.OK {
		t.Fatalf("send result: %v", res.Error)
	}
}

func TestFileGoToPython(t *testing.T) {
	s := startStack(t)
	requireRelayForSend(t, s)
	py, _ := requirePython(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "payload.bin")
	want := make([]byte, 3*1024*1024+12345)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	code, results, err := goClient(s).SendFile(ctx, "payload.bin", f)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	outDir := t.TempDir()
	cmd := pythonCmd(ctx, py, s.pyArgs("receive", "--accept-file", code)...)
	cmd.Dir = outDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python receive failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	got := filepath.Join(outDir, "payload.bin")
	gotSum := sha256File(t, got)
	wantSum := sha256.Sum256(want)
	if gotSum != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("file checksum mismatch for received payload.bin")
	}
	res := <-results
	if !res.OK {
		t.Fatalf("send result: %v", res.Error)
	}
}

func TestDirectoryGoToPython(t *testing.T) {
	s := startStack(t)
	requireRelayForSend(t, s)
	py, _ := requirePython(t)

	root, dirname, want := makeDirectory(t, "tree")

	// collect directory entries the way the CLI does: paths prefixed with
	// the top-level directory name
	var entries []wormhole.DirectoryEntry
	walkErr := filepath.Walk(filepath.Join(root, dirname), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		mode := info.Mode()
		entries = append(entries, wormhole.DirectoryEntry{
			Path: filepath.ToSlash(relPath),
			Mode: mode,
			Reader: func() (io.ReadCloser, error) {
				return os.Open(path)
			},
		})
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(entries) == 0 {
		t.Fatal("no entries collected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	code, results, err := goClient(s).SendDirectory(ctx, dirname, entries)
	if err != nil {
		t.Fatalf("SendDirectory: %v", err)
	}

	outDir := t.TempDir()
	cmd := pythonCmd(ctx, py, s.pyArgs("receive", "--accept-file", code)...)
	cmd.Dir = outDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python receive failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	got := walkChecksums(t, filepath.Join(outDir, dirname))
	compareTrees(t, want, got)
	res := <-results
	if !res.OK {
		t.Fatalf("send result: %v", res.Error)
	}
}

// ---------- Python -> Go ----------

func TestTextPythonToGo(t *testing.T) {
	s := startStack(t)
	py, _ := requirePython(t)

	payload := "interop text payload 👍 — magic-wormhole -> wormhole-william"
	code := "42-e2e-text"

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sendCmd := pythonCmd(ctx, py, s.pyArgs("send", "--no-qr", "--code", code, "--text", payload)...)
	var sendOut bytes.Buffer
	sendCmd.Stdout, sendCmd.Stderr = &sendOut, &sendOut
	if err := sendCmd.Start(); err != nil {
		t.Fatal(err)
	}

	msg, err := goClient(s).Receive(ctx, code)
	if err != nil {
		_ = sendCmd.Process.Kill()
		_ = sendCmd.Wait()
		t.Fatalf("Receive: %v\npython output: %s", err, sendOut.String())
	}
	if msg.Type != wormhole.TransferText {
		t.Fatalf("expected text transfer, got %v", msg.Type)
	}
	got, err := io.ReadAll(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("text mismatch: got %q, want %q", got, payload)
	}
	if err := sendCmd.Wait(); err != nil {
		t.Fatalf("python send failed: %v\noutput: %s", err, sendOut.String())
	}
}

func TestFilePythonToGo(t *testing.T) {
	s := startStack(t)
	py, _ := requirePython(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "payload.bin")
	want := make([]byte, 3*1024*1024+12345)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	code := "43-e2e-file"

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sendCmd := pythonCmd(ctx, py, s.pyArgs("send", "--no-qr", "--code", code, src)...)
	var sendOut bytes.Buffer
	sendCmd.Stdout, sendCmd.Stderr = &sendOut, &sendOut
	if err := sendCmd.Start(); err != nil {
		t.Fatal(err)
	}

	msg, err := goClient(s).Receive(ctx, code)
	if err != nil {
		_ = sendCmd.Process.Kill()
		_ = sendCmd.Wait()
		t.Fatalf("Receive: %v\npython output: %s", err, sendOut.String())
	}
	if msg.Type != wormhole.TransferFile {
		t.Fatalf("expected file transfer, got %v", msg.Type)
	}
	if msg.Name != "payload.bin" {
		t.Fatalf("offer file name: got %q, want payload.bin", msg.Name)
	}
	got, err := io.ReadAll(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(want))
	}
	wantSum := sha256.Sum256(want)
	gotSum := sha256.Sum256(got)
	if wantSum != gotSum {
		t.Fatal("file checksum mismatch")
	}
	if err := sendCmd.Wait(); err != nil {
		t.Fatalf("python send failed: %v\noutput: %s", err, sendOut.String())
	}
}

func TestDirectoryPythonToGo(t *testing.T) {
	s := startStack(t)
	py, _ := requirePython(t)

	root, dirname, want := makeDirectory(t, "tree")
	code := "44-e2e-dir"

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// send the directory by path; cwd=root so the advertised name is the
	// bare directory name
	sendCmd := pythonCmd(ctx, py, s.pyArgs("send", "--no-qr", "--code", code, dirname)...)
	sendCmd.Dir = root
	var sendOut bytes.Buffer
	sendCmd.Stdout, sendCmd.Stderr = &sendOut, &sendOut
	if err := sendCmd.Start(); err != nil {
		t.Fatal(err)
	}

	msg, err := goClient(s).Receive(ctx, code)
	if err != nil {
		_ = sendCmd.Process.Kill()
		_ = sendCmd.Wait()
		t.Fatalf("Receive: %v\npython output: %s", err, sendOut.String())
	}
	if msg.Type != wormhole.TransferDirectory {
		t.Fatalf("expected directory transfer, got %v", msg.Type)
	}
	payload, err := io.ReadAll(msg)
	if err != nil {
		t.Fatal(err)
	}

	destRoot := t.TempDir()
	extractZipCanonical(t, payload, destRoot, dirname)
	got := walkChecksums(t, filepath.Join(destRoot, dirname))
	compareTrees(t, want, got)

	if err := sendCmd.Wait(); err != nil {
		t.Fatalf("python send failed: %v\noutput: %s", err, sendOut.String())
	}
}

// ---------- controls: Python<->Python and Go<->Go ----------

func TestFilePythonToPython(t *testing.T) {
	s := startStack(t)
	py, _ := requirePython(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "control.bin")
	want := make([]byte, 1024*1024+7)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(want)
	code := "45-e2e-ctl"

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sendCmd := pythonCmd(ctx, py, s.pyArgs("send", "--no-qr", "--code", code, src)...)
	var sendOut bytes.Buffer
	sendCmd.Stdout, sendCmd.Stderr = &sendOut, &sendOut
	if err := sendCmd.Start(); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	recvCmd := pythonCmd(ctx, py, s.pyArgs("receive", "--accept-file", code)...)
	recvCmd.Dir = outDir
	var recvOut bytes.Buffer
	recvCmd.Stdout, recvCmd.Stderr = &recvOut, &recvOut
	if err := recvCmd.Run(); err != nil {
		_ = sendCmd.Process.Kill()
		_ = sendCmd.Wait()
		t.Fatalf("python receive failed: %v\noutput: %s", err, recvOut.String())
	}
	if err := sendCmd.Wait(); err != nil {
		t.Fatalf("python send failed: %v\noutput: %s", err, sendOut.String())
	}
	gotSum := sha256File(t, filepath.Join(outDir, "control.bin"))
	if gotSum != hex.EncodeToString(wantSum[:]) {
		t.Fatal("file checksum mismatch (python -> python control)")
	}
}

func TestFileGoToGo(t *testing.T) {
	s := startStack(t)
	requireRelayForSend(t, s)

	want := make([]byte, 2*1024*1024+99)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	src := bytes.NewReader(want)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	code, results, err := goClient(s).SendFile(ctx, "control.bin", src)
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	type receiverResult struct {
		msg *wormhole.IncomingMessage
		err error
	}
	goResult := make(chan receiverResult, 1)
	go func() {
		msg, err := goClient(s).Receive(ctx, code)
		goResult <- receiverResult{msg, err}
	}()

	rr := <-goResult
	if rr.err != nil {
		t.Fatalf("Receive: %v", rr.err)
	}
	if rr.msg.Type != wormhole.TransferFile {
		t.Fatalf("expected file transfer, got %v", rr.msg.Type)
	}
	got, err := io.ReadAll(rr.msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("file checksum mismatch (go -> go control)")
	}
	res := <-results
	if !res.OK {
		t.Fatalf("send result: %v", res.Error)
	}
}

// ---------- helpers ----------

// extractZipCanonical unpacks a sender-produced zip into destRoot/dirname,
// stripping a single leading "<dirname>/" path component if present (Go
// senders produce entries relative to the directory; Python senders include
// the directory name). Paths that would escape destRoot are rejected.
func extractZipCanonical(t *testing.T, payload []byte, destRoot, dirname string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("payload is not a zip archive: %v", err)
	}
	prefix := dirname + "/"
	for _, zf := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(zf.Name), "/")
		name = strings.TrimPrefix(name, prefix)
		if name == "" {
			continue // the directory entry itself
		}
		if strings.HasPrefix(name, "/") || name == ".." || strings.Contains(name, "/../") || strings.HasPrefix(name, "../") {
			t.Fatalf("unsafe path in zip: %q", zf.Name)
		}
		dest := filepath.Join(destRoot, dirname, filepath.FromSlash(name))
		absDest, err := filepath.Abs(dest)
		if err != nil {
			t.Fatal(err)
		}
		base, err := filepath.Abs(filepath.Join(destRoot, dirname))
		if err != nil {
			t.Fatal(err)
		}
		if absDest != base && !strings.HasPrefix(absDest, base+string(os.PathSeparator)) {
			t.Fatalf("path escapes target directory: %q", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		r, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(dest)
		if err != nil {
			r.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(f, r)
		r.Close()
		f.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
	}
}

// ensure compile-time use of json import even if unused in future edits
