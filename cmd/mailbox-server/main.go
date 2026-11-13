package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dougsko/wormhole-mailbox/internal/mailbox"
)

const (
	defaultPort        = "tcp:4000:interface=::"
	channelExpiryTime  = 11 * time.Minute
	expirationInterval = 5 * time.Minute
)

// wsProtoOptionFlag allows --websocket-protocol-option OPTION=VALUE to be
// given multiple times, matching the Python plugin's repeat-flag semantics
// (and twisted's usage.Options). VALUE is parsed as JSON, like the Python
// implementation's json.loads(value).
type wsProtoOptionFlag map[string]interface{}

func (f wsProtoOptionFlag) String() string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k+"=...")
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (f wsProtoOptionFlag) Set(s string) error {
	eq := strings.Index(s, "=")
	if eq <= 0 {
		return fmt.Errorf("format options as OPTION=VALUE")
	}
	key, valueStr := strings.TrimSpace(s[:eq]), strings.TrimSpace(s[eq+1:])
	var value interface{}
	if err := json.Unmarshal([]byte(valueStr), &value); err != nil {
		return fmt.Errorf("could not parse JSON value for %s: %v", key, err)
	}
	f[key] = value
	return nil
}

// parseEndpoint parses a twisted-style endpoint string into a Go listen
// address. Supported forms:
//
//	tcp:PORT
//	tcp:PORT:interface=ADDR
//	4000
//	[HOST]:PORT / HOST:PORT
func parseEndpoint(ep string) (string, error) {
	ep = strings.TrimSpace(ep)
	if !strings.HasPrefix(ep, "tcp:") {
		// bare port or host:port
		if _, err := strconv.Atoi(ep); err == nil {
			return ":" + ep, nil
		}
		if _, _, err := net.SplitHostPort(ep); err == nil {
			return ep, nil
		}
		return "", fmt.Errorf("unrecognized endpoint %q (want tcp:PORT[:interface=ADDR] or host:port)", ep)
	}
	rest := strings.TrimPrefix(ep, "tcp:")
	parts := strings.Split(rest, ":")
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("unrecognized endpoint %q", ep)
	}
	port, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", fmt.Errorf("unrecognized endpoint %q: bad port", ep)
	}
	iface := ""
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "interface=") {
			iface = strings.TrimPrefix(p, "interface=")
		}
	}
	if iface == "" {
		return fmt.Sprintf(":%d", port), nil
	}
	if strings.Contains(iface, ":") && !strings.HasPrefix(iface, "[") {
		return fmt.Sprintf("[%s]:%d", iface, port), nil
	}
	return fmt.Sprintf("%s:%d", iface, port), nil
}

// wsProtoFloat converts a JSON value for autoPingInterval/autoPingTimeout
// into a float64. JSON numbers decode as float64 (or int/uint/int64).
func wsProtoFloat(key string, v interface{}) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	default:
		return 0, fmt.Errorf("want a number of seconds, got %v (%T)", v, v)
	}
}

func main() {
	var (
		port           string
		channelDB      = flag.String("channel-db", "relay.sqlite", "location for the state database")
		usageDB        = flag.String("usage-db", "", "record usage data (SQLite)")
		addridDB       = flag.String("addrid-db", "", "IP address mapping data (SQLite)")
		generationDur  = flag.Int64("generation-duration", 86400, "lifetime of IP-address tracking table (seconds)")
		blurUsage      = flag.Int("blur-usage", -1, "round logged access times to improve privacy")
		advertiseVer   = flag.String("advertise-version", "", "version to recommend to clients")
		signalError    = flag.String("signal-error", "", "force all clients to fail with a message")
		motd           = flag.String("motd", "", "Send a Message of the Day in the welcome")
		disallowList   = flag.Bool("disallow-list", false, "refuse to send list of allocated nameplates")
		wsProtoOptions = wsProtoOptionFlag{}
	)
	flag.Var(&wsProtoOptions, "websocket-protocol-option", "websocket protocol option OPTION=VALUE (JSON value); may be given multiple times, like the Python plugin")
	// option names and short flags match the Python "twist wormhole-mailbox" plugin
	flag.StringVar(&port, "port", defaultPort, "endpoint to listen on")
	flag.StringVar(&port, "p", defaultPort, "endpoint to listen on (shorthand)")
	flag.Parse()

	usage, err := mailbox.CreateOrUpgradeUsageDB(*usageDB)
	if err != nil {
		log.Fatalf("unable to open usage db %q: %v", *usageDB, err)
	}
	if usage != nil {
		defer usage.Close()
	}

	var addrid *sql.DB
	if *addridDB != "" {
		var err error
		addrid, err = mailbox.CreateOrUpgradeAddridDB(*addridDB)
		if err != nil {
			log.Fatalf("unable to open addrid db %q: %v", *addridDB, err)
		}
	}
	if addrid != nil {
		defer addrid.Close()
	}
	generationDuration := int64(*generationDur)

	listenAddr, err := parseEndpoint(port)
	if err != nil {
		log.Fatalf("bad --port: %v", err)
	}

	// websocket protocol options: autobahn-relevant subset is
	// autoPingInterval/autoPingTimeout, which autobahn interprets in seconds
	// (0 disables pinging). Everything else is accepted for twist-platform
	// parity but ignored with a note, as documented.
	autoPingInterval := 60.0
	autoPingTimeout := 600.0
	for k, v := range wsProtoOptions {
		switch k {
		case "autoPingInterval", "autoPingTimeout":
			n, err := wsProtoFloat(k, v)
			if err != nil {
				log.Fatalf("bad --websocket-protocol-option %s: %v", k, err)
			}
			if k == "autoPingInterval" {
				autoPingInterval = n
			} else {
				autoPingTimeout = n
			}
		default:
			fmt.Printf("note: websocket protocol option %s ignored\n", k)
		}
	}

	var blur *int
	if *blurUsage >= 0 {
		b := *blurUsage
		blur = &b
	}

	mailbox.IncreaseRLimits()

	channel, err := mailbox.CreateOrUpgradeChannelDB(*channelDB)
	if err != nil {
		log.Fatalf("unable to open channel db %q: %v", *channelDB, err)
	}
	defer channel.Close()

	welcome := map[string]interface{}{}
	if *motd != "" {
		welcome["motd"] = *motd
	}
	if *advertiseVer != "" {
		welcome["current_cli_version"] = *advertiseVer
	}
	if *signalError != "" {
		welcome["error"] = *signalError
	}

	server := mailbox.NewServer(channel, !*disallowList, welcome, blur, usage, addrid)

	rebooted := time.Now().Unix()
	if err := server.CheckAddrIDGeneration(rebooted, generationDuration, true); err != nil {
		fmt.Printf("warning: unable to initialize addrid generation: %v\n", err)
	}
	if err := server.ClearConnections(); err != nil {
		fmt.Printf("warning: unable to clear connections table: %v\n", err)
	}

	// periodic pruning and stats, mirroring the Python plugin's 5-minute
	// TimerService. Twisted's TimerService runs its callable immediately at
	// startup, then every interval; we mirror that.
	runMaintenance := func() {
		now := time.Now().Unix()
		old := now - int64(channelExpiryTime/time.Second)
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("error during periodic maintenance: %v\n", r)
			}
		}()
		if err := server.PruneAllApps(now, old); err != nil {
			fmt.Printf("error during prune_all_apps: %v\n", err)
		}
		if err := server.CheckAddrIDGeneration(now, generationDuration, false); err != nil {
			fmt.Printf("error during check_addrid_generation: %v\n", err)
		}
		if err := server.DumpStats(now, rebooted); err != nil {
			fmt.Printf("error during dump_stats: %v\n", err)
		}
	}
	go func() {
		runMaintenance()
		ticker := time.NewTicker(expirationInterval)
		defer ticker.Stop()
		for range ticker.C {
			runMaintenance()
		}
	}()

	handler := mailbox.NewHandler(server, *blurUsage < 0, autoPingInterval, autoPingTimeout)
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("unable to listen on %s: %v", listenAddr, err)
	}
	// print the bound address, and a ready-to-use client URL hint
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		fmt.Printf("websocket listening on ws://%s/v1 (clients on this machine: --relay-url ws://localhost:%d/v1)\n", ln.Addr(), tcp.Port)
	} else {
		fmt.Printf("websocket listening on ws://%s/v1\n", ln.Addr())
	}

	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	// wait for SIGINT/SIGTERM, then shut down
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Printf("shutting down\n")
	server.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}
