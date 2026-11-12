// Package mailbox implements the Wormhole Mailbox Server, a port of
// https://github.com/magic-wormhole/magic-wormhole-mailbox-server
// with on-disk compatibility: databases created by either implementation
// can be opened and served by the other.
//
// The SQLite schemas under db-schemas/ are copied verbatim from the
// reference implementation and must not be edited.
package mailbox

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

//go:embed db-schemas
var schemas embed.FS

const (
	channelDBTargetVersion = 2
	usageDBTargetVersion   = 2
	addridDBTargetVersion  = 1
)

// DBError indicates an unrecoverable database problem (unreadable file,
// missing or unsupported schema version, failed foreign key check).
type DBError struct {
	msg string
}

func (e *DBError) Error() string { return e.msg }

func dbErrorf(format string, args ...interface{}) *DBError {
	return &DBError{msg: fmt.Sprintf(format, args...)}
}

func readSchema(name string, version int) (string, error) {
	data, err := schemas.ReadFile(fmt.Sprintf("db-schemas/%s-v%d.sql", name, version))
	if err != nil {
		return "", dbErrorf("unable to read schema %s-v%d: %v", name, version, err)
	}
	return string(data), nil
}

func readUpgrader(name string, newVersion int) (string, error) {
	data, err := schemas.ReadFile(fmt.Sprintf("db-schemas/upgrade-%s-to-v%d.sql", name, newVersion))
	if err != nil {
		return "", dbErrorf("unable to upgrade to v%d", newVersion)
	}
	return string(data), nil
}

// openDBConnection opens the given file (or ":memory:"), enforces foreign
// keys, and verifies the foreign key check is clean.
func openDBConnection(dbfile string) (*sql.DB, error) {
	dsn := dbfile
	if dbfile != ":memory:" {
		dsn = "file:" + dbfile + "?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, dbErrorf("unable to create/open db file %q: %v", dbfile, err)
	}
	// serialize all access; matches the Python implementation's
	// single-threaded reactor semantics and keeps PRAGMA behavior predictable
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, dbErrorf("unable to create/open db file %q: %v", dbfile, err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, dbErrorf("failed to enable foreign keys: %v", err)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		db.Close()
		return nil, dbErrorf("failed foreign key check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		db.Close()
		return nil, dbErrorf("failed foreign key check")
	}
	return db, nil
}

func initializeDB(db *sql.DB, name string, targetVersion int) error {
	schema, err := readSchema(name, targetVersion)
	if err != nil {
		return err
	}
	if _, err := db.Exec(schema); err != nil {
		return dbErrorf("failed to initialize %s schema v%d: %v", name, targetVersion, err)
	}
	if _, err := db.Exec("INSERT INTO version (version) VALUES (?)", targetVersion); err != nil {
		return dbErrorf("failed to set schema version: %v", err)
	}
	return nil
}

func atomicCreateAndInitializeDB(dbfile, name string, targetVersion int) error {
	dir := filepath.Dir(dbfile)
	if dir == "" {
		dir = "."
	}
	fmt.Printf("populating new database with schema %s v%d\n", name, targetVersion)

	tmp, err := os.CreateTemp(dir, filepath.Base(dbfile)+".*")
	if err != nil {
		return dbErrorf("unable to create temporary database near %q: %v", dbfile, err)
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName) // no-op once renamed

	db, err := openDBConnection(tmpName)
	if err != nil {
		return err
	}
	if err := initializeDB(db, name, targetVersion); err != nil {
		db.Close()
		return err
	}
	db.Close()
	if err := os.Rename(tmpName, dbfile); err != nil {
		return dbErrorf("unable to rename %q to %q: %v", tmpName, dbfile, err)
	}
	return nil
}

func dbVersion(db *sql.DB) (int, error) {
	row := db.QueryRow("SELECT version FROM version")
	var version int
	if err := row.Scan(&version); err != nil {
		return 0, dbErrorf("unable to read db version: %v", err)
	}
	return version, nil
}

func backupDB(dbfile string, version int) error {
	backup := fmt.Sprintf("%s-backup-v%d", dbfile, version)
	src, err := os.Open(dbfile)
	if err != nil {
		return dbErrorf("unable to open %q for backup: %v", dbfile, err)
	}
	defer src.Close()
	dst, err := os.Create(backup)
	if err != nil {
		return dbErrorf("unable to create backup %q: %v", backup, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return dbErrorf("unable to write backup %q: %v", backup, err)
	}
	return dst.Close()
}

// getDB opens or creates the database at dbfile, upgrading it to the target
// version if needed. It mirrors database.get_db() in the Python reference.
func getDB(dbfile, name string, targetVersion int) (*sql.DB, error) {
	if dbfile == ":memory:" {
		db, err := openDBConnection(dbfile)
		if err != nil {
			return nil, err
		}
		if err := initializeDB(db, name, targetVersion); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}

	if fileExists(dbfile) {
		db, err := openDBConnection(dbfile)
		if err != nil {
			return nil, err
		}
		version, err := dbVersion(db)
		if err != nil {
			db.Close()
			return nil, err
		}
		if version < targetVersion {
			fmt.Printf(" storing backup of v%d db in %s\n", version, fmt.Sprintf("%s-backup-v%d", dbfile, version))
			if err := backupDB(dbfile, version); err != nil {
				db.Close()
				return nil, err
			}
		}
		for version < targetVersion {
			fmt.Printf(" need to upgrade from %d to %d\n", version, version+1)
			upgrader, err := readUpgrader(name, version+1)
			if err != nil {
				db.Close()
				return nil, err
			}
			fmt.Printf(" executing upgrader v%d->v%d\n", version, version+1)
			if _, err := db.Exec(upgrader); err != nil {
				db.Close()
				return nil, dbErrorf("unable to upgrade %s to version %d (left at v%d): %v",
					dbfile, version+1, version, err)
			}
			version++
		}
		if version != targetVersion {
			db.Close()
			return nil, dbErrorf("unable to handle db version %d", version)
		}
		return db, nil
	}

	if err := atomicCreateAndInitializeDB(dbfile, name, targetVersion); err != nil {
		return nil, err
	}
	return openDBConnection(dbfile)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// CreateOrUpgradeChannelDB opens or creates the channel (state) database.
func CreateOrUpgradeChannelDB(dbfile string) (*sql.DB, error) {
	return getDB(dbfile, "channel", channelDBTargetVersion)
}

// CreateOrUpgradeUsageDB opens or creates the usage database.
func CreateOrUpgradeUsageDB(dbfile string) (*sql.DB, error) {
	if dbfile == "" {
		return nil, nil
	}
	return getDB(dbfile, "usage", usageDBTargetVersion)
}

// CreateOrUpgradeAddridDB opens or creates the address-id database.
func CreateOrUpgradeAddridDB(dbfile string) (*sql.DB, error) {
	return getDB(dbfile, "addrid", addridDBTargetVersion)
}

// OpenExistingDB opens an existing database without upgrading or creating it.
func OpenExistingDB(dbfile string) (*sql.DB, error) {
	if dbfile == ":memory:" {
		return nil, errors.New("OpenExistingDB requires a file path")
	}
	if !fileExists(dbfile) {
		return nil, dbErrorf("no such database %q", dbfile)
	}
	return openDBConnection(dbfile)
}

// IsDBError reports whether err is a *DBError.
func IsDBError(err error) bool {
	var derr *DBError
	return errors.As(err, &derr)
}
