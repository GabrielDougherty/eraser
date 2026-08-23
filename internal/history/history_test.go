package history

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "history.db")
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func addRecord(t *testing.T, s *Store, brokerID string, status Status, sentAt time.Time) {
	t.Helper()
	rec := &Record{
		BrokerID:   brokerID,
		BrokerName: brokerID,
		Email:      brokerID + "@example.com",
		Template:   "gdpr",
		Status:     status,
		SentAt:     sentAt,
	}
	if err := s.Add(rec); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestCountSentSince(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))
	addRecord(t, s, "broker-b", StatusSent, now.Add(-30*time.Hour)) // outside 24h window
	addRecord(t, s, "broker-c", StatusFailed, now.Add(-1*time.Hour))

	count, err := s.CountSentSince("", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountSentSince: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 sent in last 24h, got %d", count)
	}
}

func TestLastSuccessfulSendTimes(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	// Two sends for the same broker - should return the more recent one.
	addRecord(t, s, "broker-a", StatusSent, now.Add(-40*24*time.Hour))
	addRecord(t, s, "broker-a", StatusSent, now.Add(-2*24*time.Hour))
	addRecord(t, s, "broker-b", StatusFailed, now.Add(-1*time.Hour))

	times, err := s.LastSuccessfulSendTimes("")
	if err != nil {
		t.Fatalf("LastSuccessfulSendTimes: %v", err)
	}

	if _, ok := times["broker-b"]; ok {
		t.Errorf("broker-b never had a successful send, should not appear")
	}

	got, ok := times["broker-a"]
	if !ok {
		t.Fatalf("expected broker-a in results")
	}
	wantApprox := now.Add(-2 * 24 * time.Hour)
	if got.Before(wantApprox.Add(-time.Minute)) || got.After(wantApprox.Add(time.Minute)) {
		t.Errorf("expected broker-a's last send near %v, got %v", wantApprox, got)
	}
}

// TestGetAllBrokerStatusesScansAggregateTime guards against a regression
// where GetAllBrokerStatuses scanned its MAX(sent_at) column into
// sql.NullTime directly. Aggregate functions lose the driver's native
// time.Time conversion (confirmed against modernc.org/sqlite - a raw
// `sent_at` column scans fine, but `MAX(sent_at)` comes back as a plain
// string), so that scan errored on every call, and the error was silently
// discarded by getBrokersWithStatus's `brokerStatuses, _ =
// s.historyStore.GetAllBrokerStatuses(...)`, making every broker show as
// "never sent" on the web UI's Brokers page regardless of real history.
// LastSuccessfulSendTimes already had the right fix (parseSQLiteTime) -
// this test would have caught that GetAllBrokerStatuses didn't.
func TestGetAllBrokerStatusesScansAggregateTime(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))
	addRecord(t, s, "broker-b", StatusFailed, now.Add(-2*time.Hour))

	statuses, err := s.GetAllBrokerStatuses("")
	if err != nil {
		t.Fatalf("GetAllBrokerStatuses: %v", err)
	}

	a, ok := statuses["broker-a"]
	if !ok {
		t.Fatalf("expected broker-a in results")
	}
	if a.Status != StatusSent {
		t.Errorf("broker-a: expected status %q, got %q", StatusSent, a.Status)
	}
	if a.LastSent.IsZero() {
		t.Errorf("broker-a: expected a non-zero LastSent, got zero value")
	}
	wantApprox := now.Add(-1 * time.Hour)
	if a.LastSent.Before(wantApprox.Add(-time.Minute)) || a.LastSent.After(wantApprox.Add(time.Minute)) {
		t.Errorf("broker-a: expected LastSent near %v, got %v", wantApprox, a.LastSent)
	}

	b, ok := statuses["broker-b"]
	if !ok {
		t.Fatalf("expected broker-b in results")
	}
	if b.Status != StatusFailed {
		t.Errorf("broker-b: expected status %q, got %q", StatusFailed, b.Status)
	}
}

func TestMarkFailedRemovesFromResendCooldown(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))

	n, err := s.MarkFailed("", "broker-a", "bounced - manually confirmed")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row updated, got %d", n)
	}

	times, err := s.LastSuccessfulSendTimes("")
	if err != nil {
		t.Fatalf("LastSuccessfulSendTimes: %v", err)
	}
	if _, ok := times["broker-a"]; ok {
		t.Errorf("broker-a was just marked failed, should no longer count as a successful send")
	}
}

func TestMarkFailedNoRecordReturnsZero(t *testing.T) {
	s := newTestStore(t)

	n, err := s.MarkFailed("", "never-sent", "bounced")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows updated for a broker with no sent record, got %d", n)
	}
}

func TestMarkFailedOnlyTouchesMostRecentSentRecord(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "broker-a", StatusSent, now.Add(-40*24*time.Hour))
	addRecord(t, s, "broker-a", StatusSent, now.Add(-1*time.Hour))

	if _, err := s.MarkFailed("", "broker-a", "bounced"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	records, err := s.GetRecentRequests("", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	sentCount, failedCount := 0, 0
	for _, r := range records {
		if r.BrokerID != "broker-a" {
			continue
		}
		switch r.Status {
		case StatusSent:
			sentCount++
		case StatusFailed:
			failedCount++
		}
	}
	if sentCount != 1 || failedCount != 1 {
		t.Errorf("expected exactly one older 'sent' record to survive and one 'failed', got sent=%d failed=%d", sentCount, failedCount)
	}
}

// Regression coverage for digisamroc/eraser#3: broker_responses.email_body
// was referenced by AddBrokerResponse's INSERT (and UpdateBrokerResponseBody,
// GetAllBrokerResponses, GetBrokerResponses) but missing from the CREATE
// TABLE, so `eraser monitor` failed on every classified reply with "table
// broker_responses has no column named email_body" on any database created
// before the migrate() fix. This exercises the exact path that broke.
func TestAddBrokerResponse_EmailBodyColumn(t *testing.T) {
	s := newTestStore(t)

	resp := &BrokerResponse{
		BrokerID:     "broker-a",
		BrokerName:   "Broker A",
		ResponseType: "pending",
		EmailFrom:    "privacy@broker-a.example.com",
		EmailSubject: "Re: Personal Data Removal Request",
		EmailBody:    "We have received your request and will respond within 30 days.",
		Confidence:   0.9,
	}
	if err := s.AddBrokerResponse(resp); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	if resp.ID == 0 {
		t.Fatal("AddBrokerResponse did not set an ID")
	}

	all, err := s.GetAllBrokerResponses()
	if err != nil {
		t.Fatalf("GetAllBrokerResponses: %v", err)
	}
	if len(all) != 1 || all[0].EmailBody != resp.EmailBody {
		t.Fatalf("expected the stored email_body to round-trip, got %+v", all)
	}

	if err := s.UpdateBrokerResponseBody(resp.ID, resp.ProfileID, "updated body"); err != nil {
		t.Fatalf("UpdateBrokerResponseBody: %v", err)
	}
}

// ==================== Multi-Profile Tests ====================

func addRecordForProfile(t *testing.T, s *Store, profileID, brokerID string, status Status, sentAt time.Time) {
	t.Helper()
	rec := &Record{
		ProfileID:  profileID,
		BrokerID:   brokerID,
		BrokerName: brokerID,
		Email:      brokerID + "@example.com",
		Template:   "gdpr",
		Status:     status,
		SentAt:     sentAt,
	}
	if err := s.Add(rec); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

func TestProfileIsolation_RecentRequestsAndStats(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordForProfile(t, s, "maris", "broker-a", StatusSent, now)
	addRecordForProfile(t, s, "maris", "broker-b", StatusSent, now)
	addRecordForProfile(t, s, "spouse", "broker-a", StatusSent, now)

	marisRecords, err := s.GetRecentRequests("maris", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(maris): %v", err)
	}
	if len(marisRecords) != 2 {
		t.Errorf("expected 2 records for maris, got %d", len(marisRecords))
	}

	spouseRecords, err := s.GetRecentRequests("spouse", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(spouse): %v", err)
	}
	if len(spouseRecords) != 1 {
		t.Errorf("expected 1 record for spouse, got %d", len(spouseRecords))
	}

	total, sent, _, err := s.GetStats("maris")
	if err != nil {
		t.Fatalf("GetStats(maris): %v", err)
	}
	if total != 2 || sent != 2 {
		t.Errorf("expected maris stats total=2 sent=2, got total=%d sent=%d", total, sent)
	}

	total, _, _, err = s.GetStats("spouse")
	if err != nil {
		t.Fatalf("GetStats(spouse): %v", err)
	}
	if total != 1 {
		t.Errorf("expected spouse stats total=1, got total=%d", total)
	}
}

func TestProfileIsolation_EmptyProfileIDDefaultsConsistently(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	// A record added with no ProfileID (as every pre-multi-profile caller
	// does) should be readable back under both "" and DefaultProfileID -
	// normalizeProfileID has to make those equivalent everywhere.
	addRecordForProfile(t, s, "", "broker-a", StatusSent, now)

	byEmpty, err := s.GetRecentRequests("", 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(\"\"): %v", err)
	}
	if len(byEmpty) != 1 {
		t.Fatalf("expected 1 record via empty profileID, got %d", len(byEmpty))
	}
	if byEmpty[0].ProfileID != DefaultProfileID {
		t.Errorf("expected stored record to be normalized to %q, got %q", DefaultProfileID, byEmpty[0].ProfileID)
	}

	byDefault, err := s.GetRecentRequests(DefaultProfileID, 10)
	if err != nil {
		t.Fatalf("GetRecentRequests(DefaultProfileID): %v", err)
	}
	if len(byDefault) != 1 {
		t.Errorf("expected 1 record via DefaultProfileID, got %d", len(byDefault))
	}
}

func TestMigrationBackfillsExistingRowsToDefaultProfile(t *testing.T) {
	// Simulate a pre-multi-profile database: create the removal_requests
	// table without a profile_id column (as it existed before this
	// feature), insert a row the old way, then run migrate() and confirm
	// the ALTER TABLE ... DEFAULT backfill makes the row show up under
	// DefaultProfileID rather than vanishing behind the new filter.
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE removal_requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			broker_id TEXT NOT NULL,
			broker_name TEXT NOT NULL,
			email TEXT NOT NULL,
			template TEXT NOT NULL,
			status TEXT NOT NULL,
			message_id TEXT,
			error TEXT,
			sent_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`)
	if err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	_, err = db.Exec(`INSERT INTO removal_requests (broker_id, broker_name, email, template, status, sent_at)
		VALUES ('legacy-broker', 'Legacy Broker', 'legacy@example.com', 'gdpr', 'sent', ?)`, time.Now())
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	_ = db.Close()

	// NewStore runs migrate() on open, which should ALTER TABLE ADD COLUMN
	// profile_id with the DefaultProfileID default, backfilling this row.
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore on legacy db: %v", err)
	}
	defer func() { _ = s.Close() }()

	records, err := s.GetRecentRequests(DefaultProfileID, 10)
	if err != nil {
		t.Fatalf("GetRecentRequests: %v", err)
	}
	if len(records) != 1 || records[0].BrokerID != "legacy-broker" {
		t.Fatalf("expected the pre-existing legacy row to be visible under DefaultProfileID, got %+v", records)
	}
}

func TestResolveProfileForBroker(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordForProfile(t, s, "maris", "broker-a", StatusSent, now.Add(-2*time.Hour))
	addRecordForProfile(t, s, "spouse", "broker-b", StatusSent, now.Add(-1*time.Hour))

	got, err := s.ResolveProfileForBroker("broker-a")
	if err != nil {
		t.Fatalf("ResolveProfileForBroker(broker-a): %v", err)
	}
	if got != "maris" {
		t.Errorf("expected broker-a to resolve to maris, got %q", got)
	}

	got, err = s.ResolveProfileForBroker("broker-b")
	if err != nil {
		t.Fatalf("ResolveProfileForBroker(broker-b): %v", err)
	}
	if got != "spouse" {
		t.Errorf("expected broker-b to resolve to spouse, got %q", got)
	}

	// Never emailed by anyone - falls back to DefaultProfileID rather than erroring.
	got, err = s.ResolveProfileForBroker("never-sent")
	if err != nil {
		t.Fatalf("ResolveProfileForBroker(never-sent): %v", err)
	}
	if got != DefaultProfileID {
		t.Errorf("expected never-sent broker to fall back to %q, got %q", DefaultProfileID, got)
	}
}

// ==================== Profile-Scoped Task/Response Isolation ====================
//
// Regression coverage for a fix that added "AND profile_id = ?" to
// GetPendingTaskByID, CompletePendingTask, MarkTaskOpened,
// UpdateBrokerResponseClassification and UpdateBrokerResponseBody, so a
// caller in one profile's session can no longer read or mutate a task/
// response that belongs to another profile just by guessing/reusing its
// numeric ID. Each test below confirms both halves: the cross-profile call
// is a no-op (getter returns nil,nil; updater returns no error but leaves
// the row untouched), and the same call with the correct profile still
// works as before.

func addPendingTaskForProfile(t *testing.T, s *Store, profileID, brokerID string) *PendingTask {
	t.Helper()
	task := &PendingTask{
		ProfileID:  profileID,
		BrokerID:   brokerID,
		BrokerName: brokerID,
		TaskType:   TaskCaptcha,
		FormURL:    "https://" + brokerID + ".example.com/form",
		Notes:      "initial notes",
	}
	if err := s.AddPendingTask(task); err != nil {
		t.Fatalf("AddPendingTask: %v", err)
	}
	return task
}

func TestProfileIsolation_GetPendingTaskByID(t *testing.T) {
	s := newTestStore(t)
	task := addPendingTaskForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: treated as not found, same as a nonexistent ID.
	got, err := s.GetPendingTaskByID(task.ID, "profile-b")
	if err != nil {
		t.Fatalf("GetPendingTaskByID(profile-b): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for a task belonging to another profile, got %+v", got)
	}

	// Correct profile: found, with fields intact.
	got, err = s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID(profile-a): %v", err)
	}
	if got == nil {
		t.Fatal("expected task to be found under its own profile")
	}
	if got.BrokerID != "broker-a" || got.Status != "pending" {
		t.Errorf("unexpected task contents: %+v", got)
	}
}

func TestProfileIsolation_CompletePendingTask(t *testing.T) {
	s := newTestStore(t)
	task := addPendingTaskForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: CompletePendingTask returns no error (SQL UPDATE
	// matching zero rows isn't an error condition in database/sql - Exec
	// only errors on things like a broken connection or bad SQL, not on
	// "the WHERE clause matched nothing"), but the row must be unchanged.
	if err := s.CompletePendingTask(task.ID, "profile-b", "completed"); err != nil {
		t.Fatalf("CompletePendingTask(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got, err := s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected task to still exist under profile-a")
	}
	if got.Status != "pending" {
		t.Errorf("cross-profile CompletePendingTask must not mutate the row - expected status still 'pending', got %q", got.Status)
	}
	if got.CompletedAt.Valid {
		t.Errorf("cross-profile CompletePendingTask must not set completed_at, got %v", got.CompletedAt)
	}

	// Correct profile: the update takes effect.
	if err := s.CompletePendingTask(task.ID, "profile-a", "completed"); err != nil {
		t.Fatalf("CompletePendingTask(profile-a): %v", err)
	}
	got, err = s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil || got.Status != "completed" {
		t.Errorf("expected status 'completed' after same-profile CompletePendingTask, got %+v", got)
	}
	if !got.CompletedAt.Valid {
		t.Errorf("expected completed_at to be set after same-profile CompletePendingTask")
	}
}

func TestProfileIsolation_MarkTaskOpened(t *testing.T) {
	s := newTestStore(t)
	task := addPendingTaskForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: no error, but opened_at must stay unset.
	if err := s.MarkTaskOpened(task.ID, "profile-b"); err != nil {
		t.Fatalf("MarkTaskOpened(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got, err := s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected task to still exist under profile-a")
	}
	if got.OpenedAt.Valid {
		t.Errorf("cross-profile MarkTaskOpened must not set opened_at, got %v", got.OpenedAt)
	}

	// Correct profile: opened_at gets set.
	if err := s.MarkTaskOpened(task.ID, "profile-a"); err != nil {
		t.Fatalf("MarkTaskOpened(profile-a): %v", err)
	}
	got, err = s.GetPendingTaskByID(task.ID, "profile-a")
	if err != nil {
		t.Fatalf("GetPendingTaskByID: %v", err)
	}
	if got == nil || !got.OpenedAt.Valid {
		t.Errorf("expected opened_at to be set after same-profile MarkTaskOpened, got %+v", got)
	}
}

func addBrokerResponseForProfile(t *testing.T, s *Store, profileID, brokerID string) *BrokerResponse {
	t.Helper()
	resp := &BrokerResponse{
		ProfileID:    profileID,
		BrokerID:     brokerID,
		BrokerName:   brokerID,
		ResponseType: "pending",
		EmailFrom:    "privacy@" + brokerID + ".example.com",
		EmailSubject: "Re: Personal Data Removal Request",
		EmailBody:    "original body",
		Confidence:   0.5,
	}
	if err := s.AddBrokerResponse(resp); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}
	return resp
}

// findBrokerResponseByID is a small test helper - there's no production
// GetBrokerResponseByID, so this scans GetAllBrokerResponses (which spans
// every profile, matching what a full-inbox re-scan uses) for the row under
// test.
func findBrokerResponseByID(t *testing.T, s *Store, id int64) *BrokerResponse {
	t.Helper()
	all, err := s.GetAllBrokerResponses()
	if err != nil {
		t.Fatalf("GetAllBrokerResponses: %v", err)
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i]
		}
	}
	return nil
}

func TestProfileIsolation_UpdateBrokerResponseClassification(t *testing.T) {
	s := newTestStore(t)
	resp := addBrokerResponseForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: no error, but the classification fields must stay as inserted.
	if err := s.UpdateBrokerResponseClassification(resp.ID, "profile-b", "success", "https://form.example.com", "https://confirm.example.com", 0.99, true); err != nil {
		t.Fatalf("UpdateBrokerResponseClassification(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got := findBrokerResponseByID(t, s, resp.ID)
	if got == nil {
		t.Fatal("expected response to still exist")
	}
	if got.ResponseType != "pending" || got.FormURL != "" || got.NeedsReview {
		t.Errorf("cross-profile UpdateBrokerResponseClassification must not mutate the row, got %+v", got)
	}

	// Correct profile: the update takes effect.
	if err := s.UpdateBrokerResponseClassification(resp.ID, "profile-a", "success", "https://form.example.com", "https://confirm.example.com", 0.99, true); err != nil {
		t.Fatalf("UpdateBrokerResponseClassification(profile-a): %v", err)
	}
	got = findBrokerResponseByID(t, s, resp.ID)
	if got == nil {
		t.Fatal("expected response to still exist")
	}
	if got.ResponseType != "success" || got.FormURL != "https://form.example.com" || got.ConfirmURL != "https://confirm.example.com" || !got.NeedsReview {
		t.Errorf("expected classification fields updated after same-profile call, got %+v", got)
	}
}

func TestProfileIsolation_UpdateBrokerResponseBody(t *testing.T) {
	s := newTestStore(t)
	resp := addBrokerResponseForProfile(t, s, "profile-a", "broker-a")

	// Wrong profile: no error, but the body must stay as inserted.
	if err := s.UpdateBrokerResponseBody(resp.ID, "profile-b", "attacker-controlled body"); err != nil {
		t.Fatalf("UpdateBrokerResponseBody(profile-b) returned an error for a cross-profile no-op: %v", err)
	}
	got := findBrokerResponseByID(t, s, resp.ID)
	if got == nil {
		t.Fatal("expected response to still exist")
	}
	if got.EmailBody != "original body" {
		t.Errorf("cross-profile UpdateBrokerResponseBody must not mutate the row, got body %q", got.EmailBody)
	}

	// Correct profile: the update takes effect.
	if err := s.UpdateBrokerResponseBody(resp.ID, "profile-a", "updated body"); err != nil {
		t.Fatalf("UpdateBrokerResponseBody(profile-a): %v", err)
	}
	got = findBrokerResponseByID(t, s, resp.ID)
	if got == nil || got.EmailBody != "updated body" {
		t.Errorf("expected body updated after same-profile call, got %+v", got)
	}
}

// ==================== Deletion ====================
//
// These back destructive UI actions - History > "Clear failed", Settings >
// Danger Zone > "Clear All History", and the pipeline's re-scan. Every one of
// them is a DELETE whose safety rests entirely on its WHERE clause, and none
// of them had a test. A missing profile_id predicate would quietly wipe a
// second household member's records; a missing status predicate would take
// successful sends along with the failures.

func TestDeleteByStatusOnlyTouchesThatStatus(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecord(t, s, "sent-one", StatusSent, now)
	addRecord(t, s, "sent-two", StatusSent, now)
	addRecord(t, s, "failed-one", StatusFailed, now)
	addRecord(t, s, "failed-two", StatusFailed, now)

	deleted, err := s.DeleteByStatus("", StatusFailed)
	if err != nil {
		t.Fatalf("DeleteByStatus: %v", err)
	}
	// The count is reported straight back to the user as "Deleted N", so a
	// wrong number is a lie about what just happened to their data.
	if deleted != 2 {
		t.Errorf("reported %d deleted, expected 2", deleted)
	}

	_, sent, failed, err := s.GetStats("")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if sent != 2 {
		t.Errorf("successful sends were destroyed: %d remain, expected 2", sent)
	}
	if failed != 0 {
		t.Errorf("%d failed records survived the delete", failed)
	}
}

func TestDeleteByStatusIsProfileScoped(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordForProfile(t, s, "me", "broker-a", StatusFailed, now)
	addRecordForProfile(t, s, "spouse", "broker-b", StatusFailed, now)
	addRecordForProfile(t, s, "spouse", "broker-c", StatusSent, now)

	deleted, err := s.DeleteByStatus("me", StatusFailed)
	if err != nil {
		t.Fatalf("DeleteByStatus: %v", err)
	}
	if deleted != 1 {
		t.Errorf("reported %d deleted, expected 1", deleted)
	}

	_, sent, failed, err := s.GetStats("spouse")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if sent != 1 || failed != 1 {
		t.Errorf("clearing one profile's failures disturbed another: spouse has %d sent, %d failed", sent, failed)
	}
}

func TestDeleteAllHistoryIsProfileScoped(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordForProfile(t, s, "me", "broker-a", StatusSent, now)
	addRecordForProfile(t, s, "me", "broker-b", StatusFailed, now)
	addRecordForProfile(t, s, "spouse", "broker-c", StatusSent, now)
	addRecordForProfile(t, s, "spouse", "broker-d", StatusFailed, now)

	deleted, err := s.DeleteAllHistory("me")
	if err != nil {
		t.Fatalf("DeleteAllHistory: %v", err)
	}
	if deleted != 2 {
		t.Errorf("reported %d deleted, expected 2", deleted)
	}

	total, _, _, err := s.GetStats("me")
	if err != nil {
		t.Fatalf("GetStats(me): %v", err)
	}
	if total != 0 {
		t.Errorf("%d of this profile's records survived", total)
	}

	// The whole point of the profile_id predicate.
	spouseTotal, spouseSent, spouseFailed, err := s.GetStats("spouse")
	if err != nil {
		t.Fatalf("GetStats(spouse): %v", err)
	}
	if spouseTotal != 2 || spouseSent != 1 || spouseFailed != 1 {
		t.Errorf("another profile's history was destroyed: %d records (%d sent, %d failed)", spouseTotal, spouseSent, spouseFailed)
	}
}

func TestDeleteAllHistoryLeavesBrokerResponsesAlone(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	addRecordForProfile(t, s, "me", "broker-a", StatusSent, now)
	if err := s.AddBrokerResponse(&BrokerResponse{
		ProfileID: "me", BrokerID: "broker-a", BrokerName: "Broker A",
		ResponseType: "success", EmailSubject: "Done", ReceivedAt: now,
	}); err != nil {
		t.Fatalf("AddBrokerResponse: %v", err)
	}

	if _, err := s.DeleteAllHistory("me"); err != nil {
		t.Fatalf("DeleteAllHistory: %v", err)
	}

	// Two separate tables with separate meanings: what you sent, and what
	// came back. Clearing send history must not silently discard evidence
	// that a broker confirmed a deletion.
	responses, err := s.GetBrokerResponses("me", "", false, 10)
	if err != nil {
		t.Fatalf("GetBrokerResponses: %v", err)
	}
	if len(responses) != 1 {
		t.Errorf("clearing send history also removed %d broker response(s)", 1-len(responses))
	}
}

func TestDeleteOnEmptyHistoryReportsZero(t *testing.T) {
	s := newTestStore(t)

	deleted, err := s.DeleteByStatus("", StatusFailed)
	if err != nil {
		t.Fatalf("DeleteByStatus on empty history: %v", err)
	}
	if deleted != 0 {
		t.Errorf("reported %d deleted from an empty history", deleted)
	}

	deleted, err = s.DeleteAllHistory("")
	if err != nil {
		t.Fatalf("DeleteAllHistory on empty history: %v", err)
	}
	if deleted != 0 {
		t.Errorf("reported %d deleted from an empty history", deleted)
	}
}

// TestClearBrokerResponsesIsDeliberatelyGlobal pins behaviour that looks like
// a profile-scoping bug and is not. Responses are classified out of one
// shared inbox, so a full re-scan discards everything and re-derives it -
// including other profiles' responses. Documented here so nobody "fixes" it
// into a per-profile delete, which would leave the other profiles' responses
// stale after a re-scan instead of rebuilt.
func TestClearBrokerResponsesIsDeliberatelyGlobal(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	for _, profile := range []string{"me", "spouse"} {
		if err := s.AddBrokerResponse(&BrokerResponse{
			ProfileID: profile, BrokerID: "broker-a", BrokerName: "Broker A",
			ResponseType: "success", EmailSubject: "Done", ReceivedAt: now,
		}); err != nil {
			t.Fatalf("AddBrokerResponse(%s): %v", profile, err)
		}
	}

	if err := s.ClearBrokerResponses(); err != nil {
		t.Fatalf("ClearBrokerResponses: %v", err)
	}

	all, err := s.GetAllBrokerResponses()
	if err != nil {
		t.Fatalf("GetAllBrokerResponses: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected a global clear, %d response(s) remain", len(all))
	}
}

// ==================== Data directory isolation ====================

// TestDBPathFor pins the contract that makes --config mean "a separate
// instance of everything". The end-to-end suite depends on it entirely: each
// run points --config at a scratch directory and expects its own history.db
// there, rather than reaching into the real ~/.eraser and reading - or
// worse, writing - a person's actual send history.
func TestDBPathFor(t *testing.T) {
	t.Run("lives beside the config file", func(t *testing.T) {
		got := DBPathFor(filepath.Join("/tmp", "scratch", "config.yaml"))
		want := filepath.Join("/tmp", "scratch", "history.db")
		if got != want {
			t.Errorf("DBPathFor = %q, want %q", got, want)
		}
	})

	t.Run("two config paths never share a database", func(t *testing.T) {
		a := DBPathFor(filepath.Join("/tmp", "run-a", "config.yaml"))
		b := DBPathFor(filepath.Join("/tmp", "run-b", "config.yaml"))
		if a == b {
			t.Errorf("separate config directories resolved to one database: %q", a)
		}
	})

	t.Run("config file name is irrelevant", func(t *testing.T) {
		// The database is named history.db regardless of what the config is
		// called, so --config alt.yaml in the default directory still shares
		// the default history rather than silently starting a fresh one.
		a := DBPathFor(filepath.Join("/tmp", "same", "config.yaml"))
		b := DBPathFor(filepath.Join("/tmp", "same", "other.yaml"))
		if a != b {
			t.Errorf("same directory produced different databases: %q vs %q", a, b)
		}
	})

	t.Run("empty config path falls back to the default", func(t *testing.T) {
		if got := DBPathFor(""); got != DefaultDBPath() {
			t.Errorf("DBPathFor(\"\") = %q, want DefaultDBPath() %q", got, DefaultDBPath())
		}
	})
}

func TestDefaultDBPath(t *testing.T) {
	got := DefaultDBPath()
	if !strings.HasSuffix(got, "history.db") {
		t.Errorf("DefaultDBPath = %q, expected it to end in history.db", got)
	}
	// Either ~/.eraser/history.db, or the bare fallback when there is no
	// home directory. Both must be relative-free of surprises.
	if got != "eraser_history.db" && !strings.Contains(got, ".eraser") {
		t.Errorf("DefaultDBPath = %q, expected it under .eraser or the documented fallback", got)
	}
}
