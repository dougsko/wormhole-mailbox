package mailbox

import (
	"database/sql"
	"fmt"
)

// ConnectionTable tracks live WebSocket connections in the channel database.
// It mirrors connections.py in the reference implementation.
type ConnectionTable struct {
	db *sql.DB
}

// NewConnectionTable builds a table backed by db.
func NewConnectionTable(db *sql.DB) *ConnectionTable {
	return &ConnectionTable{db: db}
}

// Clear removes all connection records. Called at startup.
func (ct *ConnectionTable) Clear() error {
	if _, err := ct.db.Exec("DELETE FROM `connection_messages`"); err != nil {
		return err
	}
	if _, err := ct.db.Exec("DELETE FROM `connections`"); err != nil {
		return err
	}
	return nil
}

// Connection is a live connection record.
type Connection struct {
	db *sql.DB
	id int64
}

// Established inserts a new connection record. addressID may be nil (address
// tracking disabled).
func (ct *ConnectionTable) Established(addressID *AddressID, now int64) *Connection {
	var generation, counter interface{}
	if addressID != nil {
		generation = addressID.Generation
		counter = addressID.Counter
	}
	res, err := ct.db.Exec("INSERT INTO `connections`"+
		" (`addrid_generation`, `addrid_counter`, `connected`, `active`)"+
		" VALUES (?,?,?,?)", generation, counter, now, now)
	if err != nil {
		fmt.Printf("warning: unable to record connection: %v\n", err)
		return &Connection{db: ct.db} // id 0 means "not tracked"
	}
	id, err := res.LastInsertId()
	if err != nil {
		return &Connection{db: ct.db}
	}
	return &Connection{db: ct.db, id: id}
}

// Bound records the side and client version of a connection.
func (c *Connection) Bound(side, implementation, version string) {
	if c.id == 0 {
		return
	}
	c.db.Exec("UPDATE `connections` SET `side`=?, `implementation`=?, `version`=? WHERE `id`=?",
		side, nullable(implementation), nullable(version), c.id)
}

// AddMessage records activity on a connection.
func (c *Connection) AddMessage(now int64, name string) {
	if c.id == 0 {
		return
	}
	c.db.Exec("UPDATE `connections` SET `active`=? WHERE `id`=?", now, c.id)
	c.db.Exec("INSERT INTO `connection_messages` (`id`, `when`, `name`) VALUES (?,?,?)",
		c.id, now, name)
}

// Lost removes the connection record.
func (c *Connection) Lost() {
	if c.id == 0 {
		return
	}
	c.db.Exec("DELETE FROM `connection_messages` WHERE `id`=?", c.id)
	c.db.Exec("DELETE FROM `connections` WHERE `id`=?", c.id)
}
