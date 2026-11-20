package mailbox

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

// StatsSnapshot is a point-in-time snapshot of server statistics.
// Live figures come from the channel DB (connections, mailboxes, nameplates,
// messages are deleted as they retire); historical figures come from the
// usage DB (rows are appended when a nameplate/mailbox is retired).
type StatsSnapshot struct {
	Timestamp int64 `json:"timestamp"`

	// Live state (channel DB)
	ActiveConnections int `json:"active_connections"`
	ActiveMailboxes   int `json:"active_mailboxes"`
	ActiveNameplates  int `json:"active_nameplates"`
	ActiveMessages    int `json:"active_messages"`

	// Historical aggregates (usage DB)
	RetiredNameplates int            `json:"retired_nameplates"`
	RetiredMailboxes  int            `json:"retired_mailboxes"`
	ClientVersions    []ClientCount  `json:"client_versions"`
	ClientImpls       []ClientCount  `json:"client_implementations"`
	NameplateResults  map[string]int `json:"nameplate_results"`
	MailboxResults    map[string]int `json:"mailbox_results"`

	// Live "current" row (usage DB), may be nil if usage tracking is off.
	Current *CurrentStats `json:"current"`

	// Activity time series (usage DB), buckets of retired nameplates+mailboxes.
	Activity []ActivityBucket `json:"activity"`
}

// CurrentStats represents the single "current" row in the usage database.
type CurrentStats struct {
	Rebooted             int64 `json:"rebooted"`
	Updated              int64 `json:"updated"`
	BlurTime             *int  `json:"blur_time"`
	ConnectionsWebSocket int   `json:"connections_websocket"`
}

// ClientCount is one entry in a top-N breakdown.
type ClientCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ActivityBucket is one time bucket of retired nameplates + mailboxes.
type ActivityBucket struct {
	Bucket     string `json:"bucket"`
	Nameplates int    `json:"nameplates"`
	Mailboxes  int    `json:"mailboxes"`
}

// jsonResponse is the envelope for all dashboard API replies.
type jsonResponse struct {
	Success bool        `json:"success"`
	Error   string      `json:"error,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

// serveJSON writes v as a JSON response.
func serveJSON(w http.ResponseWriter, v interface{}, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// getStatsSnapshot gathers a full snapshot. Caller must hold s.mu.
func getStatsSnapshot(s *Server) *StatsSnapshot {
	snap := &StatsSnapshot{
		Timestamp:        time.Now().Unix(),
		NameplateResults: map[string]int{},
		MailboxResults:   map[string]int{},
	}

	// ---- Live state from the channel DB ----
	snap.ActiveConnections = scalarInt(s.db, "SELECT COUNT(*) FROM `connections`")
	snap.ActiveMailboxes = scalarInt(s.db, "SELECT COUNT(*) FROM `mailboxes`")
	snap.ActiveNameplates = scalarInt(s.db, "SELECT COUNT(*) FROM `nameplates`")
	snap.ActiveMessages = scalarInt(s.db, "SELECT COUNT(*) FROM `messages`")

	// ---- Historical aggregates from the usage DB ----
	if s.usageDB != nil {
		snap.RetiredNameplates = scalarInt(s.usageDB, "SELECT COUNT(*) FROM `nameplates`")
		snap.RetiredMailboxes = scalarInt(s.usageDB, "SELECT COUNT(*) FROM `mailboxes`")
		snap.NameplateResults = groupCount(s.usageDB,
			"SELECT `result`, COUNT(*) FROM `nameplates` GROUP BY `result`")
		snap.MailboxResults = groupCount(s.usageDB,
			"SELECT `result`, COUNT(*) FROM `mailboxes` GROUP BY `result`")
		snap.ClientVersions = topCounts(s.usageDB,
			"SELECT `implementation` || ' ' || `version`, COUNT(*) FROM `client_versions` "+
				"WHERE `implementation` IS NOT NULL GROUP BY `implementation`, `version` ORDER BY 2 DESC LIMIT 10")
		snap.ClientImpls = topCounts(s.usageDB,
			"SELECT `implementation`, COUNT(*) FROM `client_versions` "+
				"WHERE `implementation` IS NOT NULL GROUP BY `implementation` ORDER BY 2 DESC LIMIT 10")
		snap.Current = getCurrentStats(s.usageDB)
		snap.Activity = getActivity(s.usageDB, 48)
	}

	return snap
}

// scalarInt runs a single-value COUNT/aggregate query, returning 0 on error.
func scalarInt(db *sql.DB, query string, args ...interface{}) int {
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// groupCount runs a "SELECT key, COUNT(*) ... GROUP BY key" query into a map.
func groupCount(db *sql.DB, query string) map[string]int {
	out := make(map[string]int)
	rows, err := db.Query(query)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var key sql.NullString
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			continue
		}
		if key.Valid && key.String != "" {
			out[key.String] = n
		}
	}
	return out
}

// topCounts runs a "SELECT key, COUNT(*) ... ORDER BY 2 DESC" query into a slice.
func topCounts(db *sql.DB, query string) []ClientCount {
	out := []ClientCount{}
	rows, err := db.Query(query)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name sql.NullString
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			continue
		}
		out = append(out, ClientCount{Name: name.String, Count: n})
	}
	return out
}

// getCurrentStats queries the single "current" row from the usage DB.
func getCurrentStats(db *sql.DB) *CurrentStats {
	var cs CurrentStats
	var blur sql.NullInt64
	err := db.QueryRow("SELECT `rebooted`, `updated`, `blur_time`, `connections_websocket` FROM `current` LIMIT 1").
		Scan(&cs.Rebooted, &cs.Updated, &blur, &cs.ConnectionsWebSocket)
	if err != nil {
		return nil
	}
	if blur.Valid {
		b := int(blur.Int64)
		cs.BlurTime = &b
	}
	return &cs
}

// getActivity builds a per-hour activity series from retired nameplates and
// mailboxes over the last `hours` hours. Requires the usage DB.
func getActivity(db *sql.DB, hours int) []ActivityBucket {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()

	buckets := map[string]*ActivityBucket{}
	add := func(query string, field string) {
		rows, err := db.Query(
			"SELECT strftime('%Y-%m-%dT%H:00', `started`, 'unixepoch') AS b, COUNT(*) "+
				"FROM `"+query+"` WHERE `started` >= ? GROUP BY b ORDER BY b", since)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var bucket sql.NullString
			var n int
			if err := rows.Scan(&bucket, &n); err != nil || !bucket.Valid {
				continue
			}
			b := buckets[bucket.String]
			if b == nil {
				b = &ActivityBucket{Bucket: bucket.String}
				buckets[bucket.String] = b
			}
			if field == "nameplates" {
				b.Nameplates = n
			} else {
				b.Mailboxes = n
			}
		}
	}
	add("nameplates", "nameplates")
	add("mailboxes", "mailboxes")

	out := make([]ActivityBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	// sort chronologically
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Bucket < out[j-1].Bucket; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// getStatsAPI handles GET /api/stats.
func getStatsAPI(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		snap := getStatsSnapshot(s)
		s.mu.Unlock()
		serveJSON(w, jsonResponse{Success: true, Data: snap}, http.StatusOK)
	}
}