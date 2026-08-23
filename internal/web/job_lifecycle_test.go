package web

import (
	"testing"
	"time"
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
