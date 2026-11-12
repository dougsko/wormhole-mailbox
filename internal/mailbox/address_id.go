package mailbox

import (
	"database/sql"
	"fmt"
)

// AddressID describes a rotating, privacy-preserving mapping from a peer
// address to a counter, valid for `generationDuration` seconds. It lets the
// server count distinct peers over a period without recording addresses
// indefinitely. It mirrors address_id.py in the reference implementation.
type AddressID struct {
	Generation int64
	Counter    int64
}

// AddressIDTracker manages the channel-db's addrid_generation table and the
// separate addrid-db's address_ids table.
type AddressIDTracker struct {
	channelDB *sql.DB
	addridDB  *sql.DB
}

// NewAddressIDTracker builds a tracker (or nil if either DB is nil).
func NewAddressIDTracker(channelDB, addridDB *sql.DB) *AddressIDTracker {
	if channelDB == nil || addridDB == nil {
		return nil
	}
	return &AddressIDTracker{channelDB: channelDB, addridDB: addridDB}
}

// CheckGeneration rotates the address-id generation if due. force simulates
// startup: the first generation is started unconditionally.
func (t *AddressIDTracker) CheckGeneration(now, duration int64, force bool) error {
	row := t.channelDB.QueryRow("SELECT `generation`, `started` FROM `addrid_generation` LIMIT 1")
	var generation int64
	var started int64
	found := true
	if err := row.Scan(&generation, &started); err == sql.ErrNoRows {
		found = false
	} else if err != nil {
		return fmt.Errorf("reading addrid_generation: %w", err)
	}
	if !force && found && started+duration >= now {
		return nil
	}

	nextStarted := now
	if found {
		nextStarted = started + duration
		if nextStarted+duration < now {
			nextStarted = now // don't let the new generation end early
		}
	}
	nextGeneration := int64(1)
	if found {
		nextGeneration = generation + 1
	}

	if _, err := t.channelDB.Exec("DELETE FROM `addrid_generation`"); err != nil {
		return fmt.Errorf("clearing addrid_generation: %w", err)
	}
	if _, err := t.channelDB.Exec("INSERT INTO `addrid_generation` (`generation`, `started`) VALUES (?,?)",
		nextGeneration, nextStarted); err != nil {
		return fmt.Errorf("writing addrid_generation: %w", err)
	}
	if _, err := t.addridDB.Exec("DELETE FROM `address_ids`"); err != nil {
		return fmt.Errorf("clearing address_ids: %w", err)
	}
	return nil
}

// GetID returns the (generation, counter) for an address, allocating it if
// necessary. Counters increase across all addresses in a generation.
func (t *AddressIDTracker) GetID(addrType, addr string) (*AddressID, error) {
	row := t.addridDB.QueryRow("SELECT `generation`, `counter` FROM `address_ids` WHERE `type`=? AND `address`=?",
		addrType, addr)
	id := &AddressID{}
	if err := row.Scan(&id.Generation, &id.Counter); err == nil {
		return id, nil
	} else if err != sql.ErrNoRows {
		return nil, fmt.Errorf("reading address_ids: %w", err)
	}

	row = t.channelDB.QueryRow("SELECT `generation` FROM `addrid_generation` LIMIT 1")
	var generation int64
	if err := row.Scan(&generation); err != nil {
		return nil, fmt.Errorf("reading addrid_generation: %w", err)
	}

	var topCounter int64
	row = t.addridDB.QueryRow("SELECT `counter` FROM `address_ids` WHERE `generation`=? ORDER BY `counter` DESC LIMIT 1",
		generation)
	if err := row.Scan(&topCounter); err == sql.ErrNoRows {
		topCounter = 0
	} else if err != nil {
		return nil, fmt.Errorf("reading address_ids top counter: %w", err)
	}

	id = &AddressID{Generation: generation, Counter: topCounter + 1}
	if _, err := t.addridDB.Exec("INSERT INTO `address_ids` (`generation`, `counter`, `type`, `address`) VALUES (?,?,?,?)",
		id.Generation, id.Counter, addrType, addr); err != nil {
		return nil, fmt.Errorf("inserting address_ids: %w", err)
	}
	return id, nil
}

// GetAddress resolves an address ID back to its address.
func (t *AddressIDTracker) GetAddress(id *AddressID) (addrType, addr string, ok bool) {
	if id == nil {
		return "", "", false
	}
	row := t.addridDB.QueryRow("SELECT `type`, `address` FROM `address_ids` WHERE `generation`=? AND `counter`=?",
		id.Generation, id.Counter)
	if err := row.Scan(&addrType, &addr); err != nil {
		return "", "", false
	}
	return addrType, addr, true
}
