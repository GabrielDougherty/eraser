package web

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/email"
	"github.com/eraser-privacy/eraser/internal/history"
)

// recordingSender stands in for a real SMTP sender and remembers every
// recipient it was asked to send to, so a test can assert on what the job
// tried to do rather than on what it reported afterwards.
type recordingSender struct {
	mu   sync.Mutex
	sent []string

	// job, when set, is sampled on each Send so a test can assert on the
	// progress the UI would actually display mid-run. Job.Complete() forces
	// Progress to 100 at the end, so the final value proves nothing.
	job          *Job
	progressSeen []int
}

func (r *recordingSender) Send(_ context.Context, msg email.Message) email.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, msg.To)
	if r.job != nil {
		progress, _ := r.job.ToJSON()["progress"].(int)
		r.progressSeen = append(r.progressSeen, progress)
	}
	return email.Result{Success: true, MessageID: "test-message-id"}
}

func (r *recordingSender) Name() string { return "recording" }

func (r *recordingSender) recipients() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

// TestProcessSendJobSkipsBrokersWithNoEmail pins the bulk web-UI send path
// to the behavior the CLI (cmd_send.go) and the single-broker web send
// (handleAPISendOne) already had: a broker with no address on file is
// skipped, not attempted.
//
// Before the fix, this loop passed an empty To straight to the sender,
// which failed with "invalid email format: mail: no address" and wrote a
// failed history record. That inflated the failure count and, worse, put
// brokers that can never be emailed into the Retry Failed queue, where
// each retry re-failed indefinitely. One real run produced 81 such records.
func TestProcessSendJobSkipsBrokersWithNoEmail(t *testing.T) {
	cfg := testConfig()
	// Keep the inter-send delay negligible; the default is 2s per broker.
	cfg.Options.RateLimitMs = 1
	cfg.Options.Template = "generic"
	s := newTestServer(t, cfg)
	s.jobPersistence = NewJobPersistence(t.TempDir())

	toSend := []BrokerWithStatus{
		{Broker: broker.Broker{ID: "has-email", Name: "Has Email", Email: "privacy@example.com"}},
		{Broker: broker.Broker{ID: "no-email", Name: "No Email", Email: ""}},
		{Broker: broker.Broker{ID: "blank-email", Name: "Blank Email", Email: "   "}},
		{Broker: broker.Broker{ID: "also-has-email", Name: "Also Has Email", Email: "legal@example.com"}},
	}

	sender := &recordingSender{}
	job := s.jobManager.Create(len(toSend), "default")
	sender.job = job
	s.processSendJob(job, toSend, sender)

	got := sender.recipients()
	want := []string{"privacy@example.com", "legal@example.com"}
	if len(got) != len(want) {
		t.Fatalf("expected %d sends (empty-email brokers skipped), got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("send %d: expected %q, got %q", i, want[i], got[i])
		}
	}

	// The skipped brokers must not be counted as failures - they belong in
	// the manual follow-up queue, not the failure stats.
	progress := job.ToJSON()
	if f := progress["failed"]; f != 0 {
		t.Errorf("expected 0 failures, got %v", f)
	}
	if sent := progress["sent"]; sent != 2 {
		t.Errorf("expected 2 sent, got %v", sent)
	}
	if skipped := progress["skipped"]; skipped != 2 {
		t.Errorf("expected 2 skipped, got %v", skipped)
	}
	// Skipped brokers must advance the progress bar as they are passed.
	// Sampled at the final send, 3 of 4 brokers are done (1 sent, 2
	// skipped), so the UI should read 75%. Counting only sent+failed shows
	// 25% and leaves the bar lagging for the rest of the run - on a real
	// 763-broker run with 81 skips it trails by ~11 points throughout,
	// before Complete() snaps it to 100.
	if len(sender.progressSeen) != 2 {
		t.Fatalf("expected 2 progress samples, got %v", sender.progressSeen)
	}
	if last := sender.progressSeen[1]; last != 75 {
		t.Errorf("expected 75%% progress at the final send (1 sent + 2 skipped of 4), got %d%%", last)
	}
}

// TestProcessSendJobDailyCapSurvivesResume pins the daily send cap to the
// rolling 24-hour history rather than the current run's counter.
//
// A job that hits the cap pauses and persists its remaining brokers; the
// server resumes it automatically on the next start. Because the cap used
// to be checked against `sent` - which starts at 0 in every invocation of
// processSendJob - each restart handed the job a fresh full allowance. In
// practice that meant repeated `serve` restarts on one day sent far past
// the configured limit and toward the provider's hard cap.
func TestProcessSendJobDailyCapSurvivesResume(t *testing.T) {
	store, err := history.NewStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Simulate a previous run today that already used up the whole cap.
	const dailyLimit = 3
	for i := 0; i < dailyLimit; i++ {
		rec := &history.Record{
			ProfileID:  "default",
			BrokerID:   fmt.Sprintf("earlier-%d", i),
			BrokerName: "Earlier Broker",
			Email:      "privacy@example.com",
			Template:   "generic",
			Status:     history.StatusSent,
			SentAt:     time.Now().Add(-time.Hour),
		}
		if err := store.Add(rec); err != nil {
			t.Fatalf("seeding history: %v", err)
		}
	}

	cfg := testConfig()
	cfg.Options.RateLimitMs = 1
	cfg.Options.Template = "generic"
	cfg.Options.DailySendLimit = dailyLimit

	s := newTestServerWithHistory(t, cfg, store)
	s.jobPersistence = NewJobPersistence(t.TempDir())

	toSend := []BrokerWithStatus{
		{Broker: broker.Broker{ID: "next-1", Name: "Next One", Email: "a@example.com"}},
		{Broker: broker.Broker{ID: "next-2", Name: "Next Two", Email: "b@example.com"}},
	}

	sender := &recordingSender{}
	job := s.jobManager.Create(len(toSend), "default")
	s.processSendJob(job, toSend, sender)

	if got := sender.recipients(); len(got) != 0 {
		t.Errorf("cap already spent in the last 24h, expected 0 sends on resume, got %d: %v", len(got), got)
	}
	if status := job.GetStatus(); status != JobStatusPaused {
		t.Errorf("expected the job to pause at the cap, got status %q", status)
	}
}

// TestProcessSendJobResumePreservesTallies pins the persisted counters
// across a resume that sends nothing.
//
// processSendJob counts in per-run variables that start at zero, and used
// to hand those straight to saveJobProgress. A resumed job that paused
// immediately - the normal case once the daily cap is enforced against
// history - therefore wrote sent:0 over the totals accumulated by earlier
// runs. Observed on a real pending_job.json: 31 sent became 0.
func TestProcessSendJobResumePreservesTallies(t *testing.T) {
	store, err := history.NewStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.NewStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Spend the whole cap, so the resumed job pauses before sending.
	const dailyLimit = 2
	for i := 0; i < dailyLimit; i++ {
		if err := store.Add(&history.Record{
			ProfileID: "default", BrokerID: fmt.Sprintf("earlier-%d", i),
			BrokerName: "Earlier", Email: "privacy@example.com",
			Template: "generic", Status: history.StatusSent,
			SentAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("seeding history: %v", err)
		}
	}

	cfg := testConfig()
	cfg.Options.RateLimitMs = 1
	cfg.Options.Template = "generic"
	cfg.Options.DailySendLimit = dailyLimit

	s := newTestServerWithHistory(t, cfg, store)
	s.jobPersistence = NewJobPersistence(t.TempDir())

	toSend := []BrokerWithStatus{
		{Broker: broker.Broker{ID: "next-1", Name: "Next One", Email: "a@example.com"}},
	}

	// Stand in for a job resumed from disk with earlier runs' tallies.
	job := s.jobManager.Create(10, "default")
	job.Restore(31, 2, 5)

	s.processSendJob(job, toSend, &recordingSender{})

	state, err := s.jobPersistence.Load()
	if err != nil {
		t.Fatalf("loading persisted job state: %v", err)
	}
	if state.Sent != 31 {
		t.Errorf("resume that sent nothing must not clear the tally: expected sent 31, got %d", state.Sent)
	}
	if state.Failed != 2 {
		t.Errorf("expected failed 2 to survive the resume, got %d", state.Failed)
	}
	if state.Skipped != 5 {
		t.Errorf("expected skipped 5 to survive the resume, got %d", state.Skipped)
	}
}
