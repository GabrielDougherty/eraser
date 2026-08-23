package web

import (
	"testing"
	"time"

	"github.com/eraser-privacy/eraser/internal/broker"
)

// The job lifecycle rules below are what stop a send run going wrong on its
// own: the circuit breaker that halts a job when the mail provider starts
// rejecting credentials, the per-profile active-job lookup, and the eviction
// of finished jobs. None of it is reachable from the end-to-end suite - it
// would need a mail provider to start failing mid-run - so it is only ever
// covered here.

func TestRecordAuthFailureTripsOnTheThird(t *testing.T) {
	job := NewJobManager().Create(10, "default")

	// Two failures are survivable: a provider can reject one message for
	// reasons that have nothing to do with the credentials.
	if job.RecordAuthFailure() {
		t.Error("stopped after 1 auth failure")
	}
	if job.RecordAuthFailure() {
		t.Error("stopped after 2 auth failures")
	}
	// Three in a row means the credentials are the problem, and continuing
	// would hammer the provider with attempts it has already refused.
	if !job.RecordAuthFailure() {
		t.Error("did not stop after 3 consecutive auth failures")
	}
}

// TestResetAuthFailuresRequiresConsecutiveFailures pins the "consecutive"
// part. processSendJob resets the counter on every success, so a run that
// fails, succeeds, fails, succeeds must never trip - otherwise a couple of
// unrelated rejections spread across 700 brokers would abort the whole run.
func TestResetAuthFailuresRequiresConsecutiveFailures(t *testing.T) {
	job := NewJobManager().Create(10, "default")

	for i := 0; i < 5; i++ {
		if job.RecordAuthFailure() {
			t.Fatalf("tripped on failure %d despite a success in between each", i+1)
		}
		job.ResetAuthFailures()
	}
}

func TestStopWithError(t *testing.T) {
	job := NewJobManager().Create(10, "default")
	job.Update(3, 1, "Some Broker", "some-broker")

	job.StopWithError("auth", "Stopped due to repeated authentication failures")

	if got := job.GetStatus(); got != JobStatusCompleted {
		t.Errorf("status = %q, want %q", got, JobStatusCompleted)
	}
	state := job.ToJSON()
	if state["error_type"] != "auth" {
		t.Errorf("error_type = %v, want auth", state["error_type"])
	}
	if state["error"] == "" {
		t.Error("error message was not recorded")
	}
	// The UI shows current_broker while a job runs; leaving the last one set
	// on a stopped job makes it look like work is still in progress.
	if state["current_broker"] != "" || state["current_broker_id"] != "" {
		t.Errorf("current broker survived the stop: %v / %v", state["current_broker"], state["current_broker_id"])
	}
	// Counts must survive - they are the record of what did go out before
	// the job gave up.
	if state["sent"] != 3 || state["failed"] != 1 {
		t.Errorf("tallies lost on stop: sent=%v failed=%v", state["sent"], state["failed"])
	}
}

// TestGetActiveIsProfileScoped covers why GetActive takes a profile at all:
// two profiles can send concurrently, each against its own daily limit, so a
// job running for one must not make the other look busy and block a send.
func TestGetActiveIsProfileScoped(t *testing.T) {
	jm := NewJobManager()
	mine := jm.Create(5, "me")

	if got := jm.GetActive("me"); got == nil || got.ID != mine.ID {
		t.Errorf("GetActive(me) = %v, want the running job", got)
	}
	if got := jm.GetActive("spouse"); got != nil {
		t.Errorf("a job running for one profile reported another as busy: %v", got.ID)
	}
}

func TestGetActiveIgnoresFinishedJobs(t *testing.T) {
	jm := NewJobManager()
	job := jm.Create(5, "me")
	job.Complete()

	if got := jm.GetActive("me"); got != nil {
		t.Errorf("a completed job is still reported as active: %v", got.ID)
	}
}

// TestCleanupEvictsOnlyOldFinishedJobs pins both halves of the eviction rule.
// Jobs live only in memory, so this is what stops a long-running `serve`
// growing without bound - but evicting a *running* job would lose the handle
// the UI polls for progress, and evicting a recent one would make a
// just-finished send unreviewable.
func TestCleanupEvictsOnlyOldFinishedJobs(t *testing.T) {
	jm := NewJobManager()

	old := jm.Create(1, "me")
	old.Complete()
	old.CompletedAt = time.Now().Add(-8 * 24 * time.Hour)

	recent := jm.Create(1, "me")
	recent.Complete()

	running := jm.Create(1, "me")

	jm.Cleanup(7 * 24 * time.Hour)

	if jm.Get(old.ID) != nil {
		t.Error("a job finished 8 days ago was not evicted")
	}
	if jm.Get(recent.ID) == nil {
		t.Error("a just-finished job was evicted; its results are still worth reviewing")
	}
	if jm.Get(running.ID) == nil {
		t.Error("a running job was evicted; the UI polls that handle for progress")
	}
}

func TestFinishedBefore(t *testing.T) {
	jm := NewJobManager()
	cutoff := time.Now().Add(-time.Hour)

	running := jm.Create(1, "me")
	if running.finishedBefore(cutoff) {
		t.Error("a running job reported itself finished")
	}

	done := jm.Create(1, "me")
	done.Complete()
	done.CompletedAt = time.Now().Add(-2 * time.Hour)
	if !done.finishedBefore(cutoff) {
		t.Error("a job finished before the cutoff was not recognised")
	}

	justDone := jm.Create(1, "me")
	justDone.Complete()
	if justDone.finishedBefore(cutoff) {
		t.Error("a job finished after the cutoff was treated as old")
	}
}

// ==================== Resumed-job progress ====================

func TestReconcileTallies(t *testing.T) {
	cases := map[string]struct {
		state                          PersistentJobState
		remaining                      int
		wantSent, wantFailed, wantSkip int
	}{
		// The case that actually happened: an older build zeroed the
		// counters when a resumed job paused immediately, so the file said
		// 0 sent while 581 of 763 brokers had been dealt with. The progress
		// bar read "14/763" mid-run and looked like a fresh start from the
		// top of the broker list - alarming enough to abort a correct run.
		"counters corrupted to zero": {
			PersistentJobState{Total: 763, Sent: 0, Failed: 0, Skipped: 0}, 182,
			581, 0, 0,
		},
		"counters agree with the list": {
			PersistentJobState{Total: 100, Sent: 70, Failed: 5, Skipped: 5}, 20,
			70, 5, 5,
		},
		// A job persisted before the skipped counter existed.
		"no skipped counter recorded": {
			PersistentJobState{Total: 100, Sent: 60, Failed: 0, Skipped: 0}, 20,
			80, 0, 0,
		},
		"nothing done yet": {
			PersistentJobState{Total: 50}, 50,
			0, 0, 0,
		},
		"everything done": {
			PersistentJobState{Total: 50, Sent: 45, Failed: 2, Skipped: 3}, 0,
			45, 2, 3,
		},
		// Failed and skipped exceed what the list says was processed, so the
		// split is unusable. Fall back to the one number that is trustworthy.
		"counters contradict the list": {
			PersistentJobState{Total: 100, Sent: 90, Failed: 30, Skipped: 30}, 90,
			10, 0, 0,
		},
		// A broker removed from the database between runs leaves more
		// remaining entries than the total. Never report negative progress.
		"remaining exceeds total": {
			PersistentJobState{Total: 10, Sent: 3}, 12,
			0, 0, 0,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			state := tc.state
			sent, failed, skipped := reconcileTallies(&state, tc.remaining)
			if sent != tc.wantSent || failed != tc.wantFailed || skipped != tc.wantSkip {
				t.Errorf("got sent=%d failed=%d skipped=%d, want sent=%d failed=%d skipped=%d",
					sent, failed, skipped, tc.wantSent, tc.wantFailed, tc.wantSkip)
			}
		})
	}
}

// TestReconcileTalliesAlwaysAccountsForEveryProcessedBroker is the invariant
// that makes the progress bar honest: whatever the persisted counters say,
// the three outcomes must add up to what the remaining list implies has been
// dealt with. If they don't, the bar under-reports and a resumed run looks
// like it is starting over.
func TestReconcileTalliesAlwaysAccountsForEveryProcessedBroker(t *testing.T) {
	states := []PersistentJobState{
		{Total: 763, Sent: 0, Failed: 0, Skipped: 0},
		{Total: 763, Sent: 450, Failed: 0, Skipped: 81},
		{Total: 100, Sent: 0, Failed: 100, Skipped: 0},
		{Total: 1, Sent: 0},
	}
	for _, state := range states {
		for remaining := 0; remaining <= state.Total; remaining++ {
			s := state
			sent, failed, skipped := reconcileTallies(&s, remaining)
			if got, want := sent+failed+skipped, s.Total-remaining; got != want {
				t.Fatalf("total=%d remaining=%d: outcomes sum to %d, want %d",
					s.Total, remaining, got, want)
			}
			if sent < 0 || failed < 0 || skipped < 0 {
				t.Fatalf("total=%d remaining=%d: negative tally sent=%d failed=%d skipped=%d",
					s.Total, remaining, sent, failed, skipped)
			}
		}
	}
}

// TestResumedJobReportsRealProgress covers the whole path rather than the
// helper: a job restored from a state whose counters were zeroed must still
// show progress against what is genuinely left to do.
func TestResumedJobReportsRealProgress(t *testing.T) {
	state := PersistentJobState{Total: 763, Sent: 0, Failed: 0, Skipped: 0}
	const remaining = 182

	job := NewJobManager().Create(state.Total, "default")
	job.Restore(reconcileTallies(&state, remaining))

	progress := job.ToJSON()
	if got := progress["sent"]; got != 581 {
		t.Errorf("sent = %v, want 581", got)
	}
	// 581 of 763 is 76%. Before this, the same resume displayed 0%.
	if got := progress["progress"]; got != 76 {
		t.Errorf("progress = %v%%, want 76%%", got)
	}
}

// TestResumeJobShowsProgressAgainstWhatIsLeft covers the wiring, not just
// the arithmetic. Testing reconcileTallies alone proves nothing about
// whether resumeJob actually uses it - reverting that single call site
// leaves every helper test passing while the progress bar goes back to
// reading "14/763" on a run that is three-quarters done.
func TestResumeJobShowsProgressAgainstWhatIsLeft(t *testing.T) {
	cfg := captureTestConfig()
	cfg.Options.RateLimitMs = 1
	s, _ := newCaptureTestServer(t, cfg)
	s.jobPersistence = NewJobPersistence(t.TempDir())
	s.brokerDB = &broker.BrokerDatabase{Brokers: []broker.Broker{
		{ID: "left-one", Name: "Left One", Email: "a@example.invalid"},
		{ID: "left-two", Name: "Left Two", Email: "b@example.invalid"},
	}}

	// A job that had dealt with 8 of 10 brokers, whose counters were zeroed
	// by an older build - exactly the state found on disk after the bug.
	s.resumeJob(&PersistentJobState{
		ID:               "resumed",
		ProfileID:        "default",
		Total:            10,
		Sent:             0,
		RemainingBrokers: []string{"left-one", "left-two"},
	})

	var job *Job
	for _, j := range s.jobManager.jobs {
		job = j
	}
	if job == nil {
		t.Fatal("resuming created no job")
	}

	// 8 already done, plus the 2 just sent.
	progress := job.ToJSON()
	if got := progress["sent"]; got != 10 {
		t.Errorf("sent = %v, want 10 (8 carried over + 2 sent now)", got)
	}
	if got := progress["progress"]; got != 100 {
		t.Errorf("progress = %v%%, want 100%%", got)
	}
}
