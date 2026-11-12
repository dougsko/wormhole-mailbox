package mailbox

import (
	crand "crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// SidedMessage is a message in a mailbox, carrying the identity of the side
// that added it.
type SidedMessage struct {
	Side     string
	Phase    string
	Body     string          // hex-encoded, opaque to the server
	ServerRx int64           // seconds since epoch (INTEGER column in the DB)
	MsgID    json.RawMessage // client-provided, may be nil
}

// CrowdedError is returned when a third side tries to claim a nameplate or
// open a mailbox.
var CrowdedError = errors.New("crowded")

// ReclaimedError is returned when a side tries to re-claim a nameplate it
// previously released.
var ReclaimedError = errors.New("reclaimed")

var nameplateRe = regexp.MustCompile(`^\d+$`)

// CheckValidNameplate validates a nameplate id: at most 40 digits.
func CheckValidNameplate(n string) error {
	if len(n) > 40 {
		return fmt.Errorf("nameplate %.50q .. is too long, %d>40", n, len(n))
	}
	if !nameplateRe.MatchString(n) {
		return fmt.Errorf("nameplate %.50q has non-digits", n)
	}
	return nil
}

func generateMailboxID() string {
	buf := make([]byte, 8)
	if _, err := crand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
}

// Listener is a subscription to messages in a mailbox. send receives each
// message (old and new); stop is called when the mailbox is deleted.
type Listener struct {
	send func(sm SidedMessage)
	stop func()
}

// Mailbox is a two-sided store-and-forward queue with subscriptions.
type Mailbox struct {
	app       *AppNamespace
	db        *sql.DB
	usageDB   *sql.DB
	appID     string
	mailboxID string
	mu        sync.Mutex
	listeners map[int64]*Listener
}

// AddListener registers a listener and returns any messages that have
// already been stored.
func (m *Mailbox) AddListener(handle int64, send func(SidedMessage), stop func()) []SidedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listeners == nil {
		m.listeners = make(map[int64]*Listener)
	}
	m.listeners[handle] = &Listener{send: send, stop: stop}
	return m.getMessages()
}

// RemoveListener deregisters a listener.
func (m *Mailbox) RemoveListener(handle int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.listeners, handle)
}

// HasListeners reports whether any listeners are registered.
func (m *Mailbox) HasListeners() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.listeners) > 0
}

// CountListeners returns the number of registered listeners.
func (m *Mailbox) CountListeners() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.listeners)
}

// BroadcastMessage sends a message to all listeners.
func (m *Mailbox) BroadcastMessage(sm SidedMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.listeners {
		l.send(sm)
	}
}

func (m *Mailbox) getMessages() []SidedMessage {
	rows, err := m.db.Query("SELECT side, phase, body, server_rx, msg_id FROM `messages`"+
		" WHERE `app_id`=? AND `mailbox_id`=? ORDER BY `server_rx` ASC",
		m.appID, m.mailboxID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []SidedMessage
	for rows.Next() {
		var sm SidedMessage
		var msgID sql.NullString
		if err := rows.Scan(&sm.Side, &sm.Phase, &sm.Body, &sm.ServerRx, &msgID); err != nil {
			continue
		}
		if msgID.Valid {
			sm.MsgID = json.RawMessage(msgID.String)
		}
		out = append(out, sm)
	}
	return out
}

// Open records that a side opened the mailbox.
func (m *Mailbox) Open(side string, when int64) error {
	already := m.db.QueryRow("SELECT 1 FROM `mailbox_sides` WHERE `mailbox_id`=? AND `side`=?",
		m.mailboxID, side)
	var one int
	if err := already.Scan(&one); err == sql.ErrNoRows {
		_, err := m.db.Exec("INSERT INTO `mailbox_sides` (`mailbox_id`, `opened`, `side`, `added`) VALUES (?,?,?,?)",
			m.mailboxID, true, side, when)
		if err != nil {
			return err
		}
	}
	return m.touch(when)
}

func (m *Mailbox) touch(when int64) error {
	_, err := m.db.Exec("UPDATE `mailboxes` SET `updated`=? WHERE `id`=?", when, m.mailboxID)
	return err
}

// AddMessage stores a message and broadcasts it to listeners.
func (m *Mailbox) AddMessage(sm SidedMessage) error {
	if err := m.addMessage(sm); err != nil {
		return err
	}
	m.BroadcastMessage(sm)
	return nil
}

func (m *Mailbox) addMessage(sm SidedMessage) error {
	if _, err := m.db.Exec("INSERT INTO `messages`"+
		" (`app_id`, `mailbox_id`, `side`, `phase`, `body`, `server_rx`, `msg_id`)"+
		" VALUES (?,?,?,?,?,?,?)",
		m.appID, m.mailboxID, sm.Side, sm.Phase, sm.Body, sm.ServerRx, rawMsgID(sm.MsgID)); err != nil {
		return err
	}
	return m.touch(sm.ServerRx)
}

func rawMsgID(id json.RawMessage) interface{} {
	if len(id) == 0 {
		return nil
	}
	return string(id)
}

// Close records that a side closed the mailbox. When the last side closes,
// the mailbox (and its nameplate, if any) are deleted and any usage
// summary is written.
func (m *Mailbox) Close(side, mood string, when int64) error {
	db := m.db
	row := db.QueryRow("SELECT `for_nameplate` FROM `mailboxes` WHERE `app_id`=? AND `id`=?",
		m.appID, m.mailboxID)
	var forNameplate bool
	if err := row.Scan(&forNameplate); err != nil {
		return nil // mailbox already gone
	}

	res, err := db.Exec("UPDATE `mailbox_sides` SET `opened`=?, `mood`=? WHERE `mailbox_id`=? AND `side`=?",
		false, mood, m.mailboxID, side)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	// any sides still open?
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM `mailbox_sides` WHERE `mailbox_id`=? AND `opened`=?",
		m.mailboxID, true).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	// no sides left open: delete and summarize
	sideRows, err := selectSideRows(db, m.mailboxID)
	if err != nil {
		return err
	}
	// Release any remaining nameplate claims for this mailbox. (The Python
	// reference deletes `nameplate_sides WHERE side=?` here, which leaks the
	// partner's claim rows and can violate the foreign key; deleting rows
	// for the whole nameplate is the intended behavior and cannot leak.)
	if _, err := db.Exec("DELETE FROM `nameplate_sides` WHERE `nameplates_id` IN"+
		" (SELECT `id` FROM `nameplates` WHERE `mailbox_id`=?)", m.mailboxID); err != nil {
		fmt.Printf("warning: deleting nameplate_sides failed: %v\n", err)
	}
	if _, err := db.Exec("DELETE FROM `nameplates` WHERE `mailbox_id`=?", m.mailboxID); err != nil {
		fmt.Printf("warning: deleting nameplates failed: %v\n", err)
	}
	if _, err := db.Exec("DELETE FROM `messages` WHERE `mailbox_id`=?", m.mailboxID); err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM `mailbox_sides` WHERE `mailbox_id`=?", m.mailboxID); err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM `mailboxes` WHERE `id`=?", m.mailboxID); err != nil {
		return err
	}
	if m.usageDB != nil {
		m.app.summarizeMailboxAndStore(m.usageDB, forNameplate, sideRows, when, false)
		m.usageDB.Exec("SELECT 1") // keep symmetry with Python's commit
	}
	// shut down any lingering listeners
	m.mu.Lock()
	for _, l := range m.listeners {
		l.stop()
	}
	m.listeners = map[int64]*Listener{}
	m.mu.Unlock()
	m.app.freeMailbox(m.mailboxID)
	return nil
}

type sideRow struct {
	Added int64
	Mood  sql.NullString
}

func selectSideRows(db *sql.DB, mailboxID string) ([]sideRow, error) {
	rows, err := db.Query("SELECT `added`, `mood` FROM `mailbox_sides` WHERE `mailbox_id`=?", mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sideRow
	for rows.Next() {
		var sr sideRow
		if err := rows.Scan(&sr.Added, &sr.Mood); err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	return out, nil
}

func shutdownMailbox(m *Mailbox) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.listeners {
		l.stop()
	}
	m.listeners = map[int64]*Listener{}
}

// AppNamespace is a per-app_id namespace of nameplates and mailboxes.
type AppNamespace struct {
	mu          sync.Mutex
	db          *sql.DB
	usageDB     *sql.DB
	blurUsage   *int
	logRequests bool
	appID       string
	mailboxes   map[string]*Mailbox
	allowList   bool
}

// GetNameplateIDs returns the sorted list of allocated numeric nameplates,
// or nil if listing is disallowed.
func (a *AppNamespace) GetNameplateIDs() []string {
	if !a.allowList {
		return nil
	}
	rows, err := a.db.Query("SELECT DISTINCT `name` FROM `nameplates` WHERE `app_id`=?", a.appID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			ids = append(ids, name)
		}
	}
	sort.Strings(ids)
	return ids
}

func (a *AppNamespace) findAvailableNameplateID() (string, error) {
	claimed := map[string]bool{}
	rows, err := a.db.Query("SELECT DISTINCT `name` FROM `nameplates` WHERE `app_id`=?", a.appID)
	if err == nil {
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil {
				claimed[name] = true
			}
		}
		rows.Close()
	}
	for size := 1; size <= 3; size++ {
		start := 1
		for i := 0; i < size-1; i++ {
			start *= 10
		}
		end := start * 10
		var avail []string
		for n := start; n < end; n++ {
			id := fmt.Sprintf("%d", n)
			if !claimed[id] {
				avail = append(avail, id)
			}
		}
		if len(avail) > 0 {
			return avail[rand.IntN(len(avail))], nil
		}
	}
	// all small ids claimed; try random larger ones for a while
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("%d", rand.IntN(1000*1000-1000)+1000)
		if !claimed[id] {
			return id, nil
		}
	}
	return "", errors.New("unable to find a free nameplate-id")
}

// AllocateNameplate picks an available nameplate, claims it, and returns it.
func (a *AppNamespace) AllocateNameplate(side string, when int64) (string, error) {
	id, err := a.findAvailableNameplateID()
	if err != nil {
		return "", err
	}
	_, err = a.ClaimNameplate(id, side, when)
	return id, err
}

// ClaimNameplate claims a nameplate for a side, creating a mailbox if needed.
func (a *AppNamespace) ClaimNameplate(name, side string, when int64) (string, error) {
	if err := CheckValidNameplate(name); err != nil {
		return "", err
	}
	db := a.db
	row := db.QueryRow("SELECT `id`, `mailbox_id` FROM `nameplates` WHERE `app_id`=? AND `name`=?",
		a.appID, name)
	var npid int64
	var mailboxID string
	if err := row.Scan(&npid, &mailboxID); err == sql.ErrNoRows {
		if a.logRequests {
			fmt.Printf("creating nameplate#%s for app_id %s\n", name, a.appID)
		}
		mailboxID = generateMailboxID()
		if err := a.addMailbox(mailboxID, true, side, when); err != nil {
			return "", err
		}
		res, err := db.Exec("INSERT INTO `nameplates` (`app_id`, `name`, `mailbox_id`) VALUES (?,?,?)",
			a.appID, name, mailboxID)
		if err != nil {
			return "", err
		}
		npid, _ = res.LastInsertId()
	} else if err != nil {
		return "", err
	}

	row = db.QueryRow("SELECT `claimed` FROM `nameplate_sides` WHERE `nameplates_id`=? AND `side`=?",
		npid, side)
	var claimed bool
	if err := row.Scan(&claimed); err == sql.ErrNoRows {
		_, err = db.Exec("INSERT INTO `nameplate_sides` (`nameplates_id`, `claimed`, `side`, `added`) VALUES (?,?,?,?)",
			npid, true, side, when)
		if err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if !claimed {
		return "", ReclaimedError
	}

	_, err := a.OpenMailbox(mailboxID, side, when)
	if err != nil {
		return "", err
	}
	var sides int
	if err := db.QueryRow("SELECT COUNT(*) FROM `nameplate_sides` WHERE `nameplates_id`=?", npid).Scan(&sides); err == nil {
		if sides > 2 {
			return "", CrowdedError
		}
	}
	return mailboxID, nil
}

// ReleaseNameplate releases a side's claim on a nameplate. When no side
// claims it anymore, the nameplate is deallocated.
func (a *AppNamespace) ReleaseNameplate(name, side string, when int64) error {
	db := a.db
	row := db.QueryRow("SELECT `id` FROM `nameplates` WHERE `app_id`=? AND `name`=?",
		a.appID, name)
	var npid int64
	if err := row.Scan(&npid); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := db.Exec("UPDATE `nameplate_sides` SET `claimed`=? WHERE `nameplates_id`=? AND `side`=?",
		false, npid, side); err != nil {
		return err
	}
	var claims int
	if err := db.QueryRow("SELECT COUNT(*) FROM `nameplate_sides` WHERE `nameplates_id`=? AND `claimed`=?",
		npid, true).Scan(&claims); err != nil {
		return err
	}
	if claims > 0 {
		return nil
	}
	sideRows, err := selectNameplateSideRows(db, npid)
	if err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM `nameplate_sides` WHERE `nameplates_id`=?", npid); err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM `nameplates` WHERE `id`=?", npid); err != nil {
		return err
	}
	if a.usageDB != nil {
		a.summarizeNameplateAndStore(a.usageDB, sideRows, when, false)
		a.usageDB.Exec("SELECT 1")
	}
	return nil
}

func selectNameplateSideRows(db *sql.DB, npid int64) ([]sideRow, error) {
	rows, err := db.Query("SELECT `added`, NULL AS `mood` FROM `nameplate_sides` WHERE `nameplates_id`=?", npid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sideRow
	for rows.Next() {
		var sr sideRow
		if err := rows.Scan(&sr.Added, &sr.Mood); err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	return out, nil
}

func (a *AppNamespace) summarizeNameplateAndStore(usageDB *sql.DB, sideRows []sideRow, deleteTime int64, pruned bool) {
	u := summarizeUsage(sideRows, deleteTime, pruned, false)
	usageDB.Exec("INSERT INTO `nameplates` (`app_id`, `started`, `total_time`, `waiting_time`, `result`)"+
		" VALUES (?,?,?,?,?)", a.appID, u.started, u.totalTime, u.waitingTime, u.result)
}

func (a *AppNamespace) summarizeMailboxAndStore(usageDB *sql.DB, forNameplate bool, sideRows []sideRow, deleteTime int64, pruned bool) {
	u := summarizeUsage(sideRows, deleteTime, pruned, true)
	usageDB.Exec("INSERT INTO `mailboxes` (`app_id`, `for_nameplate`, `started`, `total_time`, `waiting_time`, `result`)"+
		" VALUES (?,?,?,?,?,?)", a.appID, forNameplate, u.started, u.totalTime, u.waitingTime, u.result)
}

type usageSummary struct {
	started     int64
	waitingTime *int64
	totalTime   int64
	result      string
}

func summarizeUsage(sideRows []sideRow, deleteTime int64, pruned, isMailbox bool) usageSummary {
	times := make([]int64, 0, len(sideRows))
	for _, sr := range sideRows {
		times = append(times, sr.Added)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	started := int64(0)
	if len(times) > 0 {
		started = times[0]
	}
	var waitingTime *int64
	if len(times) > 1 {
		w := times[1] - times[0]
		waitingTime = &w
	}
	totalTime := deleteTime - started

	var result string
	if !isMailbox {
		result = "lonely"
		if len(times) == 2 {
			result = "happy"
		}
		if pruned {
			result = "pruney"
		}
		if len(times) > 2 {
			result = "crowded"
		}
		return usageSummary{started, waitingTime, totalTime, result}
	}

	numSides := len(times)
	switch {
	case numSides == 0:
		result = "quiet"
	case numSides == 1:
		result = "lonely"
	default:
		result = "happy"
	}
	moods := map[string]bool{}
	for _, sr := range sideRows {
		if sr.Mood.Valid {
			moods[sr.Mood.String] = true
		}
	}
	if moods["lonely"] {
		result = "lonely"
	}
	if moods["errory"] {
		result = "errory"
	}
	if moods["scary"] {
		result = "scary"
	}
	if pruned {
		result = "pruney"
	}
	if numSides > 2 {
		result = "crowded"
	}
	return usageSummary{started, waitingTime, totalTime, result}
}

func (a *AppNamespace) addMailbox(mailboxID string, forNameplate bool, side string, when int64) error {
	var exists int
	err := a.db.QueryRow("SELECT 1 FROM `mailboxes` WHERE `app_id`=? AND `id`=?", a.appID, mailboxID).Scan(&exists)
	if err == sql.ErrNoRows {
		_, err = a.db.Exec("INSERT INTO `mailboxes` (`app_id`, `id`, `for_nameplate`, `updated`) VALUES (?,?,?,?)",
			a.appID, mailboxID, forNameplate, when)
		return err
	}
	return err
}

// OpenMailbox returns the Mailbox for mailboxID, creating it if needed.
// It raises CrowdedError if too many sides have opened the mailbox.
func (a *AppNamespace) OpenMailbox(mailboxID, side string, when int64) (*Mailbox, error) {
	if err := a.addMailbox(mailboxID, false, side, when); err != nil {
		return nil, err
	}
	mailbox, ok := a.mailboxes[mailboxID]
	if !ok {
		if a.logRequests {
			fmt.Printf("spawning #%s for app_id %s\n", mailboxID, a.appID)
		}
		mailbox = &Mailbox{
			app:       a,
			db:        a.db,
			usageDB:   a.usageDB,
			appID:     a.appID,
			mailboxID: mailboxID,
			listeners: map[int64]*Listener{},
		}
		a.mailboxes[mailboxID] = mailbox
	}
	if err := mailbox.Open(side, when); err != nil {
		return nil, err
	}
	var rows int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM `mailbox_sides` WHERE `mailbox_id`=?", mailboxID).Scan(&rows); err == nil {
		if rows > 2 {
			return nil, CrowdedError
		}
	}
	return mailbox, nil
}

func (a *AppNamespace) freeMailbox(mailboxID string) {
	delete(a.mailboxes, mailboxID)
}

func (a *AppNamespace) shutdown() {
	for _, m := range a.mailboxes {
		shutdownMailbox(m)
	}
}

// CountListeners returns the total number of live listeners across mailboxes.
func (a *AppNamespace) CountListeners() int {
	total := 0
	for _, m := range a.mailboxes {
		total += m.CountListeners()
	}
	return total
}

// Prune deletes idle channels and their nameplates. Returns true if any
// mailbox still has listeners.
func (a *AppNamespace) Prune(now, old int64) (bool, error) {
	fmt.Printf(" prune begins (%s)\n", a.appID)
	db := a.db
	modified := false

	for _, mailbox := range a.mailboxes {
		if mailbox.HasListeners() {
			fmt.Printf("touch %s because listeners\n", mailbox.mailboxID)
			mailbox.touch(now)
		}
	}

	oldMailboxes := map[string]bool{}
	rows, err := db.Query("SELECT `id`, `updated` FROM `mailboxes` WHERE `app_id`=?", a.appID)
	if err != nil {
		return false, err
	}
	type mailboxRow struct {
		id      string
		updated int64
	}
	var mrows []mailboxRow
	for rows.Next() {
		var r mailboxRow
		if err := rows.Scan(&r.id, &r.updated); err != nil {
			rows.Close()
			return false, err
		}
		mrows = append(mrows, r)
	}
	rows.Close()
	for _, r := range mrows {
		if r.updated > old {
			continue
		}
		oldMailboxes[r.id] = true
	}

	rows, err = db.Query("SELECT `id`, `mailbox_id` FROM `nameplates` WHERE `app_id`=?", a.appID)
	if err != nil {
		return false, err
	}
	type nameplateRow struct {
		id        int64
		mailboxID string
	}
	var nrows []nameplateRow
	for rows.Next() {
		var r nameplateRow
		if err := rows.Scan(&r.id, &r.mailboxID); err != nil {
			rows.Close()
			return false, err
		}
		nrows = append(nrows, r)
	}
	rows.Close()
	var oldNameplates []nameplateRow
	for _, r := range nrows {
		if oldMailboxes[r.mailboxID] {
			oldNameplates = append(oldNameplates, r)
		}
	}

	for _, np := range oldNameplates {
		fmt.Printf("  deleting nameplate with dbid %d\n", np.id)
		sideRows, err := selectNameplateSideRows(db, np.id)
		if err == nil && a.usageDB != nil {
			a.summarizeNameplateAndStore(a.usageDB, sideRows, now, true)
		}
		if _, err := db.Exec("DELETE FROM `nameplate_sides` WHERE `nameplates_id`=?", np.id); err != nil {
			return false, err
		}
		if _, err := db.Exec("DELETE FROM `nameplates` WHERE `id`=?", np.id); err != nil {
			return false, err
		}
		modified = true
	}

	for mailboxID := range oldMailboxes {
		fmt.Printf("  deleting mailbox %s\n", mailboxID)
		if old, ok := a.mailboxes[mailboxID]; ok {
			shutdownMailbox(old)
			a.freeMailbox(mailboxID)
		}
		var forNameplate bool
		if err := db.QueryRow("SELECT `for_nameplate` FROM `mailboxes` WHERE `id`=?", mailboxID).Scan(&forNameplate); err != nil {
			continue
		}
		sideRows, err := selectSideRows(db, mailboxID)
		if err == nil && a.usageDB != nil {
			a.summarizeMailboxAndStore(a.usageDB, forNameplate, sideRows, now, true)
		}
		for _, stmt := range []string{
			"DELETE FROM `messages` WHERE `mailbox_id`=?",
			"DELETE FROM `mailbox_sides` WHERE `mailbox_id`=?",
			"DELETE FROM `mailboxes` WHERE `id`=?",
		} {
			if _, err := db.Exec(stmt, mailboxID); err != nil {
				return false, err
			}
		}
		modified = true
	}

	if modified && a.usageDB != nil {
		a.usageDB.Exec("SELECT 1")
	}
	fmt.Printf(" prune complete, in_use=%v\n", len(a.mailboxes) > 0)
	return len(a.mailboxes) > 0, nil
}

// Server is the top-level mailbox server.
type Server struct {
	mu          sync.Mutex
	db          *sql.DB
	usageDB     *sql.DB
	allowList   bool
	welcome     map[string]interface{}
	blurUsage   *int
	logRequests bool
	apps        map[string]*AppNamespace
	listenerSeq atomic.Int64
}

// NewServer builds a Server. welcome keys are included in the welcome message.
func NewServer(db *sql.DB, allowList bool, welcome map[string]interface{}, blurUsage *int, usageDB *sql.DB) *Server {
	return &Server{
		db:          db,
		usageDB:     usageDB,
		allowList:   allowList,
		welcome:     welcome,
		blurUsage:   blurUsage,
		logRequests: blurUsage == nil,
		apps:        map[string]*AppNamespace{},
	}
}

// GetWelcome returns a copy of the welcome dict for the welcome message.
func (s *Server) GetWelcome() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := map[string]interface{}{}
	for k, v := range s.welcome {
		copy[k] = v
	}
	return copy
}

// GetLogRequests reports whether request logging is enabled.
func (s *Server) GetLogRequests() bool { return s.logRequests }

// GetApp returns the (cached) AppNamespace for appID.
func (s *Server) GetApp(appID string) *AppNamespace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getAppLocked(appID)
}

// getAppLocked is GetApp without taking s.mu (caller holds it).
func (s *Server) getAppLocked(appID string) *AppNamespace {
	app, ok := s.apps[appID]
	if !ok {
		if s.logRequests {
			fmt.Printf("spawning app_id %s\n", appID)
		}
		app = &AppNamespace{
			db:          s.db,
			usageDB:     s.usageDB,
			blurUsage:   s.blurUsage,
			logRequests: s.logRequests,
			appID:       appID,
			mailboxes:   map[string]*Mailbox{},
			allowList:   s.allowList,
		}
		s.apps[appID] = app
	}
	return app
}

// NextListenerHandle returns a unique listener handle. It uses an atomic
// counter so it can be called while the server lock is held.
func (s *Server) NextListenerHandle() int64 {
	return s.listenerSeq.Add(1)
}

func (s *Server) getAllApps() ([]string, error) {
	apps := map[string]bool{}
	for _, q := range []string{
		"SELECT DISTINCT `app_id` FROM `nameplates`",
		"SELECT DISTINCT `app_id` FROM `mailboxes`",
		"SELECT DISTINCT `app_id` FROM `messages`",
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var appID string
			if err := rows.Scan(&appID); err == nil {
				apps[appID] = true
			}
		}
		rows.Close()
	}
	out := make([]string, 0, len(apps))
	for appID := range apps {
		out = append(out, appID)
	}
	sort.Strings(out)
	return out, nil
}

// PruneAllApps prunes idle channels in every app. It is serialized with all
// client commands, so it is safe to run from a timer while clients are connected.
func (s *Server) PruneAllApps(now, old int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Printf("beginning app prune\n")
	appIDs, err := s.getAllApps()
	if err != nil {
		return err
	}
	for _, appID := range appIDs {
		fmt.Printf(" app prune checking %q\n", appID)
		app := s.getAppLocked(appID)
		inUse, err := app.Prune(now, old)
		if err != nil {
			fmt.Printf(" error during prune of app %q: %v\n", appID, err)
			continue
		}
		if !inUse {
			delete(s.apps, appID)
		}
	}
	fmt.Printf("app prune ends, %d apps\n", len(s.apps))
	return nil
}

// ClearConnections removes all connection records from the channel database.
// Called at startup.
func (s *Server) ClearConnections() error {
	if _, err := s.db.Exec("DELETE FROM `connection_messages`"); err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM `connections`"); err != nil {
		return err
	}
	return nil
}

// Shutdown forcibly boots any connected clients.
func (s *Server) Shutdown() {
	for _, app := range s.apps {
		app.shutdown()
	}
}

// CountListeners returns the total number of live listeners across all apps.
func (s *Server) CountListeners() int {
	total := 0
	for _, app := range s.apps {
		total += app.CountListeners()
	}
	return total
}

// logClientVersion records the client's implementation and version in the
// usage database, if one is configured. It mirrors AppNamespace.log_client_version.
func (a *AppNamespace) logClientVersion(serverRx int64, side, implementation, version string) {
	if a.blurUsage != nil {
		b := int64(*a.blurUsage)
		if b > 0 {
			serverRx = b * (serverRx / b)
		}
	}
	if a.usageDB == nil {
		return
	}
	a.usageDB.Exec("INSERT INTO `client_versions`"+
		" (`app_id`, `side`, `connect_time`, `implementation`, `version`)"+
		" VALUES(?,?,?,?,?)",
		a.appID, side, serverRx, nullable(implementation), nullable(version))
	a.usageDB.Exec("SELECT 1")
}

func nullable(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
