package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wormhole-mailbox/internal/mailbox"
)

const (
	defaultPort        = "tcp:4000:interface=::"
	channelExpiryTime  = 11 * time.Minute
	expirationInterval = 5 * time.Minute
)

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

func main() {
	var (
		port           = flag.String("port", defaultPort, "endpoint to listen on")
		channelDB      = flag.String("channel-db", "relay.sqlite", "location for the state database")
		usageDB        = flag.String("usage-db", "", "record usage data (SQLite)")
		addridDB       = flag.String("addrid-db", "", "IP address mapping data (SQLite)")
		generationDur  = flag.Int64("generation-duration", 86400, "lifetime of IP-address tracking table (seconds)")
		blurUsage      = flag.Int("blur-usage", -1, "round logged access times to improve privacy")
		advertiseVer   = flag.String("advertise-version", "", "version to recommend to clients")
		signalError    = flag.String("signal-error", "", "force all clients to fail with a message")
		motd           = flag.String("motd", "", "Send a Message of the Day in the welcome")
		disallowList   = flag.Bool("disallow-list", false, "refuse to send list of allocated nameplates")
		wsProtoOptions = flag.String("websocket-protocol-option", "", "websocket protocol option OPTION=VALUE (JSON value); may be given multiple times, comma-separated")
	)
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

	listenAddr, err := parseEndpoint(*port)
	if err != nil {
		log.Fatalf("bad --port: %v", err)
	}

	// websocket protocol options (subset: autoPingInterval, autoPingTimeout)
	autoPingInterval := 60.0
	autoPingTimeout := 600.0
	if *wsProtoOptions != "" {
		for _, pair := range strings.Split(*wsProtoOptions, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) != 2 {
				log.Fatalf("bad --websocket-protocol-option %q (want OPTION=VALUE)", pair)
			}
			fmt.Printf("note: websocket protocol option %s ignored (only autoPingInterval/autoPingTimeout are used)\n", kv[0])
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
	httpServer := &http.Server{Addr: listenAddr, Handler: handler}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("unable to listen on %s: %v", listenAddr, err)
	}
	fmt.Printf("websocket listening on ws://HOSTNAME%s/v1\n", ln.Addr())

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
