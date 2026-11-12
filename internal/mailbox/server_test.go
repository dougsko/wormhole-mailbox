package mailbox

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dbfile := filepath.Join(t.TempDir(), "relay.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewServer(db, true, nil, nil, nil)
}

func TestClaimCrowdedAndRelease(t *testing.T) {
	s := testServer(t)
	app := s.GetApp("app1")
	now := time.Now().Unix()

	// side A claims 42
	mbA, err := app.ClaimNameplate("42", "sideA", now)
	if err != nil {
		t.Fatal(err)
	}
	// side B claims 42 (same mailbox)
	mbB, err := app.ClaimNameplate("42", "sideB", now)
	if err != nil {
		t.Fatal(err)
	}
	if mbA != mbB {
		t.Fatalf("expected same mailbox, got %q and %q", mbA, mbB)
	}
	// third side claims 42: crowded
	if _, err := app.ClaimNameplate("42", "sideC", now); !errors.Is(err, CrowdedError) {
		t.Fatalf("expected CrowdedError, got %v", err)
	}

	// releasing all three sides (the failed crowded claim also leaves a
	// claimed side row, matching the reference) deallocates the nameplate
	if err := app.ReleaseNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	if err := app.ReleaseNameplate("42", "sideB", now); err != nil {
		t.Fatal(err)
	}
	if err := app.ReleaseNameplate("42", "sideC", now); err != nil {
		t.Fatal(err)
	}
	ids := app.GetNameplateIDs()
	if len(ids) != 0 {
		t.Fatalf("expected no nameplates after release, got %v", ids)
	}

	// A can claim again after full release... but A's old side row is gone,
	// so this must succeed (ReclaimedError only applies to released-then-reclaimed rows)
	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatalf("re-claim of released nameplate should work after both sides released: %v", err)
	}
}

func TestReclaimedError(t *testing.T) {
	s := testServer(t)
	app := s.GetApp("app1")
	now := time.Now().Unix()

	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	// release only A, leaving B's claim? B never claimed; A is the only side.
	// Release deletes everything (no remaining claims) so reclaim works.
	// Instead: keep A claimed, release B (nonexistent), then release A, then A re-claims.
	if err := app.ReleaseNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	// after full release the nameplate row and sides are gone, so reclaim works;
	// to hit ReclaimedError we need a released row to still exist: claim by A and B,
	// release A only (B still claims), then A re-claims -> ReclaimedError
	if _, err := app.ClaimNameplate("42", "sideB", now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	if err := app.ReleaseNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ClaimNameplate("42", "sideA", now); !errors.Is(err, ReclaimedError) {
		t.Fatalf("expected ReclaimedError, got %v", err)
	}
}

func TestInvalidNameplate(t *testing.T) {
	s := testServer(t)
	app := s.GetApp("app1")
	now := time.Now().Unix()
	for _, bad := range []string{"", "abc", "12a34", string(make([]byte, 41))} {
		if _, err := app.ClaimNameplate(bad, "sideA", now); err == nil {
			t.Fatalf("expected error for nameplate %q", bad)
		}
	}
}

func TestAddReplayAndClose(t *testing.T) {
	s := testServer(t)
	app := s.GetApp("app1")
	now := time.Now().Unix()

	mbID, err := app.ClaimNameplate("42", "sideA", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.ClaimNameplate("42", "sideB", now); err != nil {
		t.Fatal(err)
	}
	mb, err := app.OpenMailbox(mbID, "sideA", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := mb.AddMessage(SidedMessage{Side: "sideA", Phase: "0", Body: "deadbeef", ServerRx: now}); err != nil {
		t.Fatal(err)
	}

	// replay: opening as sideB returns the stored message
	mbB, err := app.OpenMailbox(mbID, "sideB", now)
	if err != nil {
		t.Fatal(err)
	}
	replayed := mbB.AddListener(99, func(SidedMessage) {}, func() {})
	if len(replayed) != 1 {
		t.Fatalf("expected 1 replayed message, got %d", len(replayed))
	}
	if replayed[0].Body != "deadbeef" || replayed[0].Phase != "0" {
		t.Fatalf("wrong replayed message: %+v", replayed[0])
	}

	// close both sides with mood=happy; mailbox is deleted entirely
	if err := mb.Close("sideA", "happy", now); err != nil {
		t.Fatal(err)
	}
	if err := mbB.Close("sideB", "happy", now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `mailboxes`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected mailboxes table to be empty after close, got %d", count)
	}
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `messages`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected messages table to be empty after close, got %d", count)
	}
}

func TestPruneExpiresIdleChannels(t *testing.T) {
	s := testServer(t)
	app := s.GetApp("app1")
	now := time.Now().Unix()

	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	// backdate the mailbox far into the past
	if _, err := app.db.Exec("UPDATE `mailboxes` SET `updated` = ?", now-10000); err != nil {
		t.Fatal(err)
	}
	inUse, err := app.Prune(now, now-660) // 11 minutes
	if err != nil {
		t.Fatal(err)
	}
	if inUse {
		t.Fatal("expected no apps in use after prune")
	}
	var count int
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `mailboxes`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected mailbox to be pruned, got %d rows", count)
	}
	if err := app.db.QueryRow("SELECT COUNT(*) FROM `nameplates`").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected nameplate to be pruned, got %d rows", count)
	}
}

func TestPruneKeepsActiveChannels(t *testing.T) {
	s := testServer(t)
	app := s.GetApp("app1")
	now := time.Now().Unix()

	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	inUse, err := app.Prune(now, now-660)
	if err != nil {
		t.Fatal(err)
	}
	if !inUse {
		t.Fatal("expected app to still be in use (fresh channel)")
	}
}

func TestUsageSummaryHappy(t *testing.T) {
	dbfile := filepath.Join(t.TempDir(), "relay.sqlite")
	db, err := CreateOrUpgradeChannelDB(dbfile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	usageFile := filepath.Join(t.TempDir(), "usage.sqlite")
	usageDB, err := CreateOrUpgradeUsageDB(usageFile)
	if err != nil {
		t.Fatal(err)
	}
	defer usageDB.Close()

	s := NewServer(db, true, nil, nil, usageDB)
	app := s.GetApp("app1")
	now := time.Now().Unix()

	if _, err := app.ClaimNameplate("42", "sideA", now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ClaimNameplate("42", "sideB", now); err != nil {
		t.Fatal(err)
	}
	mbID := "" // find the mailbox id
	if err := app.db.QueryRow("SELECT `mailbox_id` FROM `nameplates` WHERE `name`='42'").Scan(&mbID); err != nil {
		t.Fatal(err)
	}
	mb, err := app.OpenMailbox(mbID, "sideA", now)
	if err != nil {
		t.Fatal(err)
	}
	mbB, err := app.OpenMailbox(mbID, "sideB", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := mb.Close("sideA", "happy", now); err != nil {
		t.Fatal(err)
	}
	if err := mbB.Close("sideB", "happy", now); err != nil {
		t.Fatal(err)
	}
	var result string
	var totalTime int64
	if err := usageDB.QueryRow("SELECT `result`, `total_time` FROM `mailboxes` WHERE `app_id`='app1'").Scan(&result, &totalTime); err != nil {
		t.Fatal(err)
	}
	if result != "happy" {
		t.Fatalf("expected happy usage result, got %q", result)
	}
	if totalTime < 0 {
		t.Fatalf("expected non-negative total_time, got %d", totalTime)
	}
}

func TestNameplateAllocationPrefersSmallNumbers(t *testing.T) {
	s := testServer(t)
	app := s.GetApp("app1")
	now := time.Now().Unix()
	id, err := app.AllocateNameplate("sideA", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 1 {
		t.Fatalf("expected a 1-digit nameplate, got %q", id)
	}
}
