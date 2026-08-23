import { expect, test } from "@playwright/test";
import { readManifest } from "../support/captured";
import { type SeededServer, sleep, startSeededServer } from "../support/seeded-server";

/**
 * Auto-resume on startup.
 *
 * Restarting `serve` picks up a partially-finished send two seconds after
 * boot. That path caused a real incident: a run that was three-quarters done
 * displayed "14/763", looked exactly like a restart from the top of the
 * broker list, and was cancelled. Nothing had been re-sent, but the display
 * gave no way to know that.
 *
 * These tests own the server process, because the resume is a startup event
 * and Playwright's managed server starts once, before any test. They are also
 * the only place the trigger itself is covered - the Go tests exercise the
 * resume logic, but not the fact that starting the binary sets it off.
 */
test.describe.configure({ mode: "serial" });

// Fixture ids. Foxtrot is sendable but deliberately absent from every
// remaining list, so it can prove the resume didn't reach past what it was
// given.
const REMAINING = ["e2e-alpha", "e2e-bravo", "e2e-charlie"];
const FIXTURE_TOTAL = 7;

let server: SeededServer | undefined;

test.afterEach(async () => {
  // In afterEach rather than at the end of each test body: a failed
  // assertion aborts the body, and a leaked process would hold the port for
  // everything after it.
  await server?.stop();
  server = undefined;
});

/**
 * Waits for the resumed job to appear and returns it. The resume fires at
 * T+2s, and a job is only visible via /api/job/active while it is running -
 * once it finishes or pauses, that endpoint reports nothing and the id is
 * unrecoverable. So the id has to be caught here, in flight.
 */
async function catchRunningJob(baseURL: string) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const res = await fetch(`${baseURL}/api/job/active`);
    const body = (await res.json()) as { job: null | Record<string, unknown> };
    if (body.job) return body.job;
    await sleep(100);
  }
  throw new Error("no job became active within 20s - the resume never started");
}

async function jobStatus(baseURL: string, id: string) {
  const res = await fetch(`${baseURL}/api/job/${id}/status`);
  return (await res.json()) as {
    id: string; status: string; sent: number; failed: number;
    skipped: number; total: number; progress: number; error: string;
  };
}

async function waitForTerminalStatus(baseURL: string, id: string) {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    const job = await jobStatus(baseURL, id);
    if (job.status !== "running") return job;
    await sleep(100);
  }
  throw new Error("job never reached a terminal state");
}

test("a resumed job reports the work already done", async () => {
  // The state actually found on disk after the counter-zeroing bug: four of
  // seven brokers dealt with, but sent recorded as 0.
  server = await startSeededServer({
    pending: { total: FIXTURE_TOTAL, sent: 0, remaining: REMAINING },
  });

  const job = await catchRunningJob(server.baseURL);

  // Four brokers were already handled. Before the fix this reported 0 sent
  // and 0% - indistinguishable from starting over, which is what made the
  // real run look like it was about to re-email everyone.
  const carried = FIXTURE_TOTAL - REMAINING.length;
  expect(job.sent as number).toBeGreaterThanOrEqual(carried);
  expect(job.progress as number).toBeGreaterThanOrEqual(
    Math.floor((carried / FIXTURE_TOTAL) * 100),
  );
  expect(job.total).toBe(FIXTURE_TOTAL);
});

test("a resumed job contacts only the brokers that were left", async () => {
  server = await startSeededServer({
    pending: { total: FIXTURE_TOTAL, sent: 0, remaining: REMAINING },
    // Keep the run brief; the point here is what goes out, not the pacing.
    rateLimitMs: 50,
  });

  const job = await catchRunningJob(server.baseURL);
  const finished = await waitForTerminalStatus(server.baseURL, job.id as string);

  expect(finished.status).toBe("completed");
  // Four carried over plus the three just sent.
  expect(finished.sent).toBe(FIXTURE_TOTAL);

  // The answer to "did it email everyone again", read off the wire rather
  // than off a counter: exactly the three remaining brokers got a message.
  const captured = readManifest(server.captureDir);
  expect(captured).toHaveLength(REMAINING.length);
  expect(new Set(captured.map((m) => m.to))).toEqual(
    new Set(["privacy@alpha.invalid", "privacy@bravo.invalid", "privacy@charlie.invalid"]),
  );
  // Foxtrot is sendable and was not in the remaining list. A resume that
  // rebuilt its work from the broker database instead of the persisted list
  // would have reached it.
  expect(captured.map((m) => m.to)).not.toContain("privacy@foxtrot.invalid");
});

test("a resumed job stops at the daily limit", async () => {
  // No history seeding needed: the cap trips on this run's own sends. It
  // sends one, then finds itself at the limit on the next broker.
  server = await startSeededServer({
    pending: { total: FIXTURE_TOTAL, sent: 0, remaining: ["e2e-alpha", "e2e-bravo"] },
    dailySendLimit: 1,
  });

  const job = await catchRunningJob(server.baseURL);
  const finished = await waitForTerminalStatus(server.baseURL, job.id as string);

  expect(finished.status).toBe("paused");
  expect(finished.error).toContain("Daily limit of 1");
  // Exactly one message, and the second broker untouched - the cap stopped
  // the run rather than merely being reported.
  expect(readManifest(server.captureDir)).toHaveLength(1);

  // Worth recording, because it is a real gap rather than an oversight in
  // this test: a paused job is invisible to the UI. /api/job/active only
  // returns running jobs, and layout.html filters on status === 'running' on
  // top of that. So a restart that resumes and immediately pauses shows the
  // user nothing at all - no banner, no indicator, no explanation. Asserted
  // here so a future change that surfaces it is a deliberate one.
  const active = await fetch(`${server.baseURL}/api/job/active`);
  expect(((await active.json()) as { job: unknown }).job).toBeNull();
});

test("a pending job for unknown brokers is discarded rather than resumed", async () => {
  // Broker ids are resolved against the database at resume time. A pending
  // job naming brokers that no longer exist must not hang around forever
  // trying, and must certainly not fall back to sending to everyone.
  server = await startSeededServer({
    pending: { total: FIXTURE_TOTAL, sent: 0, remaining: ["no-such-broker", "also-gone"] },
  });

  // Give the resume its 2s delay plus room to act.
  await sleep(5_000);

  const active = await fetch(`${server.baseURL}/api/job/active`);
  expect(((await active.json()) as { job: unknown }).job).toBeNull();
  expect(readManifest(server.captureDir)).toHaveLength(0);
});
