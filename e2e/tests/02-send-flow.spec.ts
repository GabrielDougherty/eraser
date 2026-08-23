import { expect, test } from "@playwright/test";
import {
  bodyFor,
  capturedFiles,
  capturedRecipients,
  readCapturedBody,
  readManifest,
} from "../support/captured";
import { FIXTURE, PROFILE, ensureSetupComplete } from "../support/setup";
import { workspaceFromEnv } from "../support/workspace";

const ws = workspaceFromEnv();

const PROFILE_NAME = PROFILE.fullName;
const PROFILE_EMAIL = PROFILE.email;

const SENDABLE = [
  "privacy@alpha.invalid",
  "privacy@bravo.invalid",
  "privacy@charlie.invalid",
  "privacy@foxtrot.invalid",
];

test.describe.configure({ mode: "serial" });

// A no-op in a full run, since the wizard spec is numbered to go first. This
// is what lets this file be run on its own with --grep while debugging.
test.beforeAll(async ({ browser }) => {
  const page = await browser.newPage();
  await ensureSetupComplete(page);
  await page.close();
});

test("the broker fixture is loaded, not the shipped database", async ({ page }) => {
  // Fail fast and loudly. If --brokers didn't take effect the rest of this
  // file would run against 763 real broker addresses - harmless in capture
  // mode, but slow and utterly baffling to debug.
  await page.goto("/brokers");
  await expect(page.getByText(`${FIXTURE.brokers} brokers in database`)).toBeVisible();
  await expect(
    page.getByText(`Showing ${FIXTURE.brokers} of ${FIXTURE.brokers} brokers`),
  ).toBeVisible();
});

test("brokers with no address offer no send button", async ({ page }) => {
  await page.goto("/brokers");

  // Two of the six have no email: one with an opt-out form, one without.
  await expect(
    page.getByRole("row").filter({ hasText: "No email on file" }),
  ).toHaveCount(FIXTURE.noAddress);
  await expect(page.getByRole("row", { name: /Delta Records/ })).toContainText("No email on file");
  await expect(page.getByRole("row", { name: /Echo Append/ })).toContainText("No email on file");
});

test("filters narrow the broker list", async ({ page }) => {
  await page.goto("/brokers");

  await page.check("input[name='missing_email']");
  await expect(
    page.getByText(`Showing ${FIXTURE.noAddress} of ${FIXTURE.brokers} brokers`),
  ).toBeVisible();
  await page.uncheck("input[name='missing_email']");
  await expect(
    page.getByText(`Showing ${FIXTURE.brokers} of ${FIXTURE.brokers} brokers`),
  ).toBeVisible();

  // "Zzz" appears in exactly one fixture name. pressSequentially rather than
  // fill: the search box is wired to htmx's keyup trigger, and fill() sets the
  // value without dispatching key events, so the request would never fire.
  await page.locator("#broker-search").pressSequentially("zzz");
  await expect(page.getByText(`Showing 1 of ${FIXTURE.brokers} brokers`)).toBeVisible();
  // Scoped to the table row: the page renders a parallel set of mobile cards,
  // so a bare text match hits two elements.
  await expect(page.getByRole("row", { name: /Foxtrot Zzz Data/ })).toBeVisible();
});

test("sending to one broker renders the configured template", async ({ page }) => {
  const before = readManifest(ws.captureDir).length;

  await page.goto("/brokers");
  await page.getByRole("row", { name: /Alpha Data/ }).getByRole("button", { name: "Send" }).click();
  await expect(page.locator("#status-e2e-alpha")).toContainText("Sent");

  const captured = readManifest(ws.captureDir);
  expect(captured).toHaveLength(before + 1);
  expect(captured[captured.length - 1].to).toBe("privacy@alpha.invalid");

  // The most valuable assertion in the suite: it proves the template
  // configured at setup (gdpr) actually reached a rendered message addressed
  // to this specific broker. handleAPISendOne once hardcoded "generic"
  // regardless of configuration, and this is what catches that.
  //
  // Assert on the body, never the subject: getSubject ignores its brokerName
  // argument, so every GDPR subject is byte-identical and proves nothing
  // about which broker a message was for.
  const body = bodyFor(ws.captureDir, "privacy@alpha.invalid");
  expect(body).toContain("To Whom It May Concern at Alpha Data");
  expect(body).toContain("Article 17");
  expect(body).toContain(PROFILE_NAME);
  expect(body).toContain(PROFILE_EMAIL);
});

test("send to all skips brokers with no address", async ({ page }) => {
  await page.goto("/brokers");

  const [response] = await Promise.all([
    page.waitForResponse((r) => r.url().includes("/api/send-all") && r.request().method() === "POST"),
    page.getByRole("button", { name: "Send to All" }).click(),
  ]);
  const { job_id: jobId, total } = (await response.json()) as { job_id: string; total: number };

  // Alpha was already sent above and the default filter is status=pending,
  // so this run covers the remaining six.
  expect(total).toBe(6);

  // Terminal state only. Job.Complete() forces progress to 100 and the UI
  // polls every 1.5s, so asserting on intermediate percentages is a flake
  // generator.
  await expect(page.locator("#send-all-progress")).toContainText("Complete!");
  await expect(page.locator("#send-all-progress")).toContainText("Sent 3 emails (1 failed)");

  // The completion banner never mentions skips, so read them off the job.
  // page.request shares the browser's cookies, and a GET needs no CSRF token.
  //
  // The three numbers together are the point: failed and skipped are separate
  // outcomes for separate reasons. Golf has a malformed address that reaches
  // the sender and is rejected; Delta and Echo have no address at all and are
  // never attempted. Counting either as the other would still add up to six.
  const status = await page.request.get(`/api/job/${jobId}/status`);
  const job = (await status.json()) as { sent: number; failed: number; skipped: number };
  expect(job).toMatchObject({ sent: 3, failed: 1, skipped: 2 });
});

test("results are reflected across the UI", async ({ page }) => {
  await page.goto("/brokers");
  // Exact text, not hasText: "Sent" as a substring also matches "Never sent",
  // which would count all six rows and assert nothing.
  await expect(
    page.getByRole("row").filter({ has: page.getByText("Sent", { exact: true }) }),
  ).toHaveCount(4);
  await expect(
    page.getByRole("row").filter({ has: page.getByText("Never sent", { exact: true }) }),
  ).toHaveCount(2);
  await expect(page.locator("#status-e2e-alpha")).toContainText("Sent");
  await expect(page.locator("#status-e2e-delta")).toContainText("Never sent");

  // A rejected address must read as Failed, not as never-attempted. These are
  // different things to a user: one needs a corrected address, the other needs
  // an opt-out form filled in by hand.
  await expect(page.locator("#status-e2e-golf")).toContainText("Failed");
  await expect(
    page.getByRole("row").filter({ has: page.getByText("Failed", { exact: true }) }),
  ).toHaveCount(1);

  await page.goto("/history");
  await expect(page.locator("#sent-count")).toHaveText("4");
  await expect(page.locator("#failed-count")).toHaveText("1");
  // 4 of 5 attempts. The two skipped brokers were never attempted, so they
  // must not drag the rate down.
  await expect(page.locator("#success-rate")).toContainText("80");
});

test("every captured message is well formed and addressed to its own broker", async ({ page }) => {
  await page.goto("/");

  const captured = readManifest(ws.captureDir);

  // One wizard test send, one single send, three from the bulk run.
  expect(captured).toHaveLength(5);
  expect(capturedFiles(ws.captureDir)).toHaveLength(5);

  expect(new Set(capturedRecipients(ws.captureDir))).toEqual(
    new Set([PROFILE_EMAIL, ...SENDABLE]),
  );

  // The rejected address left nothing behind. This is the capture sender's
  // validation parity doing its job: it refuses exactly what SMTP refuses, so
  // a message that could never have been transmitted is not sitting in the
  // directory looking like one that was.
  expect(capturedRecipients(ws.captureDir)).not.toContain("not-an-email");
  expect(captured.every((m) => m.to.trim() !== "")).toBe(true);
  expect(captured.map((m) => m.seq)).toEqual([1, 2, 3, 4, 5]);

  for (const message of captured) {
    // Read by the message's own file, not by recipient: bodyFor returns the
    // first match for an address, so iterating messages while looking them up
    // by recipient would check one file twice and another never as soon as any
    // address repeats - which a re-send test would do immediately.
    const raw = readCapturedBody(ws.captureDir, message.file);
    expect(raw.startsWith("From: ")).toBe(true);
    expect(raw).toContain("MIME-Version: 1.0");
    // Header/body separator.
    expect(raw).toContain("\r\n\r\n");
  }

  // Each broker's message must name that broker. A loop that rendered the
  // first broker's template N times would satisfy every count-based assertion
  // above while sending four copies of the same letter.
  const namesByRecipient: Record<string, string> = {
    "privacy@alpha.invalid": "Alpha Data",
    "privacy@bravo.invalid": "Bravo Marketing",
    "privacy@charlie.invalid": "Charlie Checks",
    "privacy@foxtrot.invalid": "Foxtrot Zzz Data",
  };
  for (const [recipient, brokerName] of Object.entries(namesByRecipient)) {
    expect(bodyFor(ws.captureDir, recipient)).toContain(`To Whom It May Concern at ${brokerName}`);
  }
});
