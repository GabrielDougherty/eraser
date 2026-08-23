import { spawn, type ChildProcess } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { binaryPath, brokerFixture, e2eRoot } from "./workspace";

/**
 * Stands up an independently configured `eraser serve` with state already on
 * disk, so tests can exercise what the server does *at startup*.
 *
 * The main suite's server is managed by Playwright and starts once, before
 * any test - there is no way to restart it mid-run with a pending job
 * waiting. Auto-resume fires two seconds after boot and is invisible
 * afterwards (a finished job cannot be looked up without its id), so a test
 * has to own the process lifecycle to catch it.
 *
 * Everything the resume needs is a plain file: `serve` never calls
 * Validate(), and the pending-job loader does no validation at all.
 */

export interface PendingJobSeed {
  /** Broker count the original run started with. */
  total: number;
  /** Persisted sent counter. Zero reproduces the corrupted state found on disk. */
  sent?: number;
  failed?: number;
  skipped?: number;
  /** Broker ids still to process. Must match ids in the fixture exactly - an
   *  unknown id is silently dropped rather than reported. */
  remaining: string[];
}

export interface SeededServerOptions {
  pending: PendingJobSeed;
  /** Written into config.yaml's options block. */
  dailySendLimit?: number;
  rateLimitMs?: number;
}

export interface SeededServer {
  baseURL: string;
  workspace: string;
  captureDir: string;
  stop(): Promise<void>;
}

const port = Number(process.env.ERASER_E2E_RESUME_PORT ?? 8100);

let instance = 0;

export async function startSeededServer(opts: SeededServerOptions): Promise<SeededServer> {
  instance += 1;
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const workspace = path.join(e2eRoot, ".artifacts", `${stamp}-resume-${instance}`);
  const captureDir = path.join(workspace, "captured");
  mkdirSync(workspace, { recursive: true });

  writeConfig(workspace, opts);
  writePendingJob(workspace, opts.pending);

  // 127.0.0.1 for the same reason as the main server: it binds IPv4 loopback
  // only, and cookies are host-scoped.
  const baseURL = `http://127.0.0.1:${port}`;

  const child = spawn(
    binaryPath,
    [
      "--config", path.join(workspace, "config.yaml"),
      "--brokers", brokerFixture,
      "--capture-dir", captureDir,
      "serve",
      "--port", String(port),
      "--no-browser",
    ],
    { stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, ERASER_NO_BROWSER: "1" } },
  );

  // Keep output for diagnosis; a resume that declines to start says why on
  // stdout ("Cannot resume job: ...") and nothing else would reveal it.
  const log: string[] = [];
  child.stdout?.on("data", (d) => log.push(String(d)));
  child.stderr?.on("data", (d) => log.push(String(d)));

  const stop = makeStopper(child);

  try {
    await waitForReady(baseURL, child, log);
  } catch (err) {
    await stop();
    throw err;
  }

  return { baseURL, workspace, captureDir, stop };
}

function writeConfig(workspace: string, opts: SeededServerOptions): void {
  // Minimal but complete enough for the resume to get past its guards:
  // Email.Provider must be non-empty, and a first name keeps `/` off the
  // setup wizard. Everything else takes config.Load's defaults.
  const lines = [
    "profile:",
    "    first_name: Resume",
    "    last_name: Tester",
    "    email: resume@example.com",
    "email:",
    "    provider: smtp",
    "    from: resume@example.com",
    "options:",
    "    template: gdpr",
  ];
  if (opts.rateLimitMs !== undefined) lines.push(`    rate_limit_ms: ${opts.rateLimitMs}`);
  if (opts.dailySendLimit !== undefined) lines.push(`    daily_send_limit: ${opts.dailySendLimit}`);

  // 0600 only to keep config.Load's permissions warning off stderr; it is a
  // warning, not a requirement.
  writeFileSync(path.join(workspace, "config.yaml"), lines.join("\n") + "\n", { mode: 0o600 });
}

function writePendingJob(workspace: string, pending: PendingJobSeed): void {
  const state = {
    id: "seeded-resume-job",
    profile_id: "default",
    status: "paused",
    sent: pending.sent ?? 0,
    failed: pending.failed ?? 0,
    skipped: pending.skipped ?? 0,
    total: pending.total,
    started_at: new Date().toISOString(),
    remaining_brokers: pending.remaining,
    search: "",
    category: "",
    region: "",
    status_filter: "",
  };
  writeFileSync(path.join(workspace, "pending_job.json"), JSON.stringify(state, null, 2), { mode: 0o600 });
}

async function waitForReady(baseURL: string, child: ChildProcess, log: string[]): Promise<void> {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`server exited with code ${child.exitCode}:\n${log.join("")}`);
    }
    try {
      const res = await fetch(`${baseURL}/setup`);
      if (res.ok) return;
    } catch {
      // not listening yet
    }
    await sleep(100);
  }
  throw new Error(`server did not become ready within 20s:\n${log.join("")}`);
}

function makeStopper(child: ChildProcess): () => Promise<void> {
  let stopped = false;
  return async () => {
    if (stopped) return;
    stopped = true;
    if (child.exitCode !== null) return;

    const exited = new Promise<void>((resolve) => child.once("exit", () => resolve()));
    child.kill("SIGTERM");
    // Don't let a wedged process hold the port for the next test.
    const timer = setTimeout(() => child.kill("SIGKILL"), 5_000);
    await exited;
    clearTimeout(timer);
  };
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
