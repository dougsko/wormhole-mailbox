package mailbox

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestChannelDBFreshCreate(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Close()

	if v := mustVersion(t, db); v != 2 {
		t.Fatalf("expected version 2, got %d", v)
	}
	for _, table := range []string{"nameplates", "nameplate_sides", "mailboxes", "mailbox_sides", "messages", "addrid_generation", "connections", "connection_messages"} {
		if !tableExists(t, db, table) {
			t.Fatalf("missing table %s", table)
		}
	}
	// file should exist and be non-empty
	info, err := os.Stat(dbfile)
	if err != nil || info.Size() == 0 {
		t.Fatalf("db file missing or empty: %v", err)
	}
}

func TestChannelDBUpgradeFromV1(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")

	// build a v1 database the way the Python one would look
	schema, err := readSchema("channel", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbfile)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("v1 schema: %v", err)
	}
	if _, err := raw.Exec("INSERT INTO version (version) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	// insert rows like the Python server would have (mailbox first, nameplate references it)
	_, err = raw.Exec("INSERT INTO `mailboxes` (`app_id`, `id`, `updated`, `for_nameplate`) VALUES ('noop.app', 'mailboxidABC', 1, ?)", true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec("INSERT INTO `nameplates` (`app_id`, `name`, `mailbox_id`) VALUES ('noop.app', '42', 'mailboxidABC')")
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer db.Close()

	if v := mustVersion(t, db); v != 2 {
		t.Fatalf("expected version 2, got %d", v)
	}
	if !tableExists(t, db, "connections") {
		t.Fatal("upgrade did not create connections table")
	}
	// data preserved
	var name string
	if err := db.QueryRow("SELECT `name` FROM `nameplates` WHERE `app_id`='noop.app'").Scan(&name); err != nil {
		t.Fatalf("row lost in upgrade: %v", err)
	}
	if name != "42" {
		t.Fatalf("expected name 42, got %q", name)
	}
	// backup of v1 stored
	if !fileExists(dbfile + "-backup-v1") {
		t.Fatal("expected backup file relay.sqlite-backup-v1")
	}
}

func TestChannelDBBadFile(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "relay.sqlite")
	if err := os.WriteFile(dbfile, []byte("this is not sqlite"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateOrUpgradeChannelDB(dbfile); err == nil {
		t.Fatal("expected error for bad db file")
	}
}

func mustVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow("SELECT version FROM version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}
