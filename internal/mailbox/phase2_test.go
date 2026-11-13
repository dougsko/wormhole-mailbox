package mailbox

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsageCurrentRow(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	usageFile := filepath.Join(dir, "usage.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	usageDB, err := CreateOrUpgradeUsageDB(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	s := NewServer(db, true, nil, nil, usageDB, nil)
	now := time.Now().Unix()

	// two listeners attached to the same mailbox
	app := s.GetApp("app1")
	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ClaimNameplate("42", "sideB", now); err != nil {
		t.Fatal(err)
	}
	mbID := ""
	if err := app.db.QueryRow("SELECT `mailbox_id` FROM `nameplates` WHERE `name`='42'").Scan(&mbID); err != nil {
		t.Fatal(err)
	}
	mb, err := app.OpenMailbox(mbID, "sideA", now)
	if err != nil {
		t.Fatal(err)
	}
	cb := func(SidedMessage) {}
	st := func() {}
	mb.AddListener(1, cb, st)
	mbB, err := app.OpenMailbox(mbID, "sideB", now)
	if err != nil {
		t.Fatal(err)
	}
	mbB.AddListener(2, cb, st)

	if err := s.DumpStats(now, 12345); err != nil {
		t.Fatal(err)
	}
	var rebooted, updated, connections int64
	var blurTime interface{}
	if err := usageDB.QueryRow("SELECT `rebooted`, `updated`, `blur_time`, `connections_websocket` FROM `current`").
		Scan(&rebooted, &updated, &blurTime, &connections); err != nil {
		t.Fatal(err)
	}
	if rebooted != 12345 || updated != now {
		t.Fatalf("wrong current row: rebooted=%d updated=%d", rebooted, updated)
	}
	if connections != 2 {
		t.Fatalf("expected connections_websocket=2, got %d", connections)
	}

	// second dump replaces the single current row
	if err := s.DumpStats(now+5, 12345); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := usageDB.QueryRow("SELECT COUNT(*) FROM `current`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one current row, got %d", count)
	}
}

func TestUsageStartedBlurredOnClose(t *testing.T) {
	blur := 100
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	usageFile := filepath.Join(dir, "usage.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	usageDB, err := CreateOrUpgradeUsageDB(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	s := NewServer(db, true, nil, &blur, usageDB, nil)
	app := s.GetApp("app1")
	base := int64(1794458123)
	now := base

	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ClaimNameplate("42", "sideB", now); err != nil {
		t.Fatal(err)
	}
	mbID := ""
	if err := app.db.QueryRow("SELECT `mailbox_id` FROM `nameplates` WHERE `name`='42'").Scan(&mbID); err != nil {
		t.Fatal(err)
	}
	mbA, err := app.OpenMailbox(mbID, "sideA", now)
	if err != nil {
		t.Fatal(err)
	}
	mbB, err := app.OpenMailbox(mbID, "sideB", now)
	if err != nil {
		t.Fatal(err)
	}
	// release both nameplate claims first (the client sends release before
	// or interleaved with close); once the last side releases, the
	// nameplate is retired and its usage summary is recorded. Note the
	// mailbox close path also deletes the nameplate rows silently, so a
	// release attempted after close records nothing.
	if err := app.ReleaseNameplate("42", "sideA", now+60); err != nil {
		t.Fatal(err)
	}
	if err := app.ReleaseNameplate("42", "sideB", now+60); err != nil {
		t.Fatal(err)
	}

	if err := mbA.Close("sideA", strptr("happy"), now+50); err != nil {
		t.Fatal(err)
	}
	if err := mbB.Close("sideB", strptr("happy"), now+50); err != nil {
		t.Fatal(err)
	}

	var started int64
	var result string
	if err := usageDB.QueryRow("SELECT `started`, `result` FROM `mailboxes` WHERE `app_id`='app1' ORDER BY `rowid` LIMIT 1").Scan(&started, &result); err != nil {
		t.Fatal(err)
	}
	blurred := int64(blur) * (base / int64(blur))
	if started != blurred {
		t.Fatalf("expected mailbox started=%d (blurred), got %d", blurred, started)
	}
	if result != "happy" {
		t.Fatalf("expected happy, got %q", result)
	}
	if err := usageDB.QueryRow("SELECT `started`, `result` FROM `nameplates` WHERE `app_id`='app1' ORDER BY `rowid` LIMIT 1").Scan(&started, &result); err != nil {
		t.Fatal(err)
	}
	if started != blurred {
		t.Fatalf("expected nameplate started=%d (blurred), got %d", blurred, started)
	}
}

func TestClientVersionsRecorded(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	usageFile := filepath.Join(dir, "usage.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	usageDB, err := CreateOrUpgradeUsageDB(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	s := NewServer(db, true, nil, nil, usageDB, nil)
	app := s.GetApp("app1")
	now := time.Now().Unix()
	app.logClientVersion(now, "sideA", "python", "0.24.0")

	var implementation, version string
	var connectTime int64
	if err := usageDB.QueryRow("SELECT `implementation`, `version`, `connect_time` FROM `client_versions` WHERE `side`='sideA'").
		Scan(&implementation, &version, &connectTime); err != nil {
		t.Fatal(err)
	}
	if implementation != "python" || version != "0.24.0" {
		t.Fatalf("wrong client_versions row: %q %q", implementation, version)
	}
	if connectTime != now {
		t.Fatalf("expected connect_time=%d, got %d", now, connectTime)
	}
}

func TestConnectionsLifecycle(t *testing.T) {
	s := testServer(t)
	now := time.Now().Unix()

	conn := s.ConnectionEstablished(nil, now)
	app := s.GetApp("app1")

	// simulate a bind + a few commands
	conn.Bound("sideA", "python", "0.24.0")
	conn.AddMessage(now, "bind")
	conn.AddMessage(now+1, "claim")
	conn.AddMessage(now+2, "open")

	var side, impl, version string
	var active int64
	if err := app.db.QueryRow("SELECT `side`, `implementation`, `version`, `active` FROM `connections`").
		Scan(&side, &impl, &version, &active); err != nil {
		t.Fatal(err)
	}
	if side != "sideA" || impl != "python" || version != "0.24.0" {
		t.Fatalf("wrong connections row: %q %q %q", side, impl, version)
	}
	if active != now+2 {
		t.Fatalf("expected active=%d, got %d", now+2, active)
	}
	var count int
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `connection_messages`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 connection_messages, got %d", count)
	}

	conn.Lost()
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `connections`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected connections table empty after Lost, got %d", count)
	}
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `connection_messages`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected connection_messages table empty after Lost, got %d", count)
	}

	// startup semantics: ClearConnections removes leftover rows
	s.ConnectionEstablished(nil, now)
	if err := s.ClearConnections(); err != nil {
		t.Fatal(err)
	}
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `connections`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected ClearConnections to empty the table, got %d", count)
	}
}

func TestAddrIDTrackerGenerations(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	addridFile := filepath.Join(dir, "addrid.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	addridDB, err := CreateOrUpgradeAddridDB(addridFile)
	if err != nil {
		t.Fatal(err)
	}
	defer addridDB.Close()

	tracker := NewAddressIDTracker(db, addridDB)

	// first force creates generation 1
	if err := tracker.CheckGeneration(1000, 86400, true); err != nil {
		t.Fatal(err)
	}
	id1, err := tracker.GetID("ipv4", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if id1.Generation != 1 || id1.Counter != 1 {
		t.Fatalf("expected (1,1), got %+v", id1)
	}
	// same address gets the same id
	id1again, err := tracker.GetID("ipv4", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if *id1again != *id1 {
		t.Fatalf("expected same id for same address, got %+v vs %+v", id1again, id1)
	}
	// another address bumps the counter
	id2, err := tracker.GetID("ipv6", "::1")
	if err != nil {
		t.Fatal(err)
	}
	if id2.Generation != 1 || id2.Counter != 2 {
		t.Fatalf("expected (1,2), got %+v", id2)
	}
	typ, addr, ok := tracker.GetAddress(id1)
	if !ok || typ != "ipv4" || addr != "1.2.3.4" {
		t.Fatalf("GetAddress roundtrip failed: %q %q %v", typ, addr, ok)
	}

	// not due yet: no rotation
	if err := tracker.CheckGeneration(1000+3600, 86400, false); err != nil {
		t.Fatal(err)
	}
	id3, err := tracker.GetID("ipv4", "9.9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if id3.Generation != 1 {
		t.Fatalf("expected no rotation, got generation %d", id3.Generation)
	}

	// due: rotates to generation 2 and resets counters
	if err := tracker.CheckGeneration(1000+86400+1, 86400, false); err != nil {
		t.Fatal(err)
	}
	id4, err := tracker.GetID("ipv4", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if id4.Generation != 2 {
		t.Fatalf("expected generation 2 after rotation, got %d", id4.Generation)
	}
	if id4.Counter != 1 {
		t.Fatalf("expected counter to reset to 1, got %d", id4.Counter)
	}
	// old address was forgotten (table cleared)
	if _, addr, ok := tracker.GetAddress(id4); ok != true || addr != "1.2.3.4" {
		t.Fatalf("new-generation address lookup failed: %v %v", addr, ok)
	}
}

func TestAddrIDGenerationForcedRestart(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	addridFile := filepath.Join(dir, "addrid.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	addridDB, err := CreateOrUpgradeAddridDB(addridFile)
	if err != nil {
		t.Fatal(err)
	}
	defer addridDB.Close()

	tracker := NewAddressIDTracker(db, addridDB)
	if err := tracker.CheckGeneration(1000, 86400, true); err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.GetID("ipv4", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	// forced restart: generation increments, table cleared
	if err := tracker.CheckGeneration(2000, 86400, true); err != nil {
		t.Fatal(err)
	}
	id, err := tracker.GetID("ipv4", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if id.Generation != 2 || id.Counter != 1 {
		t.Fatalf("expected (2,1) after forced rotation, got %+v", id)
	}
}
