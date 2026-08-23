import { defineConfig } from "@playwright/test";
import { buildBinary } from "./support/build";
import { binaryPath, brokerFixture, createWorkspace, isPrimaryProcess } from "./support/workspace";

// Both happen at config-load time, which is before Playwright launches
// webServer. The workspace is handed to the specs and the server through the
// environment so everything in the run agrees on one directory.
// Only the primary process builds; workers re-load this config and would
// otherwise each shell out to go build for no reason.
if (isPrimaryProcess()) buildBinary();
const workspace = createWorkspace();
const port = Number(process.env.ERASER_E2E_PORT ?? 8099);

// 127.0.0.1 everywhere, never localhost. The server binds IPv4 loopback only,
// so on hosts where localhost resolves to ::1 first the connection is simply
// refused - and session and CSRF cookies are host-scoped, so mixing the two
// names mid-run would silently break the setup wizard.
const baseURL = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: "./tests",
  // One config.yaml, one SQLite history.db, one pending_job.json and a rate
  // limiter keyed on global strings. These specs describe one machine going
  // through one setup and one send; running them concurrently is incoherent,
  // not merely slow.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : [["list"], ["html", { open: "never" }]],
  timeout: 90_000,
  expect: {
    // The server rate-limits sends to one every 2s and the UI polls job status
    // every 1.5s, so a six-broker run takes several seconds to reach a
    // terminal state.
    timeout: 60_000,
  },
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  webServer: {
    command: [
      binaryPath,
      "--config", workspace.configPath,
      "--brokers", brokerFixture,
      "--capture-dir", workspace.captureDir,
      "serve",
      "--port", String(port),
      "--no-browser",
    ].join(" "),
    // /setup returns 200 with no config present; / would 302 away.
    url: `${baseURL}/setup`,
    // Never reuse. A server left over from an earlier run already has a
    // completed config.yaml, so the app is set up and the from-scratch
    // premise collapses - and it would be pointed at the previous run's
    // capture directory, making every capture assertion nonsense.
    reuseExistingServer: false,
    timeout: 30_000,
    stdout: "pipe",
    stderr: "pipe",
    env: {
      ERASER_E2E_WORKSPACE: workspace.dir,
      ERASER_NO_BROWSER: "1",
    },
  },
  // Also exported to the test processes themselves.
  metadata: { workspace: workspace.dir },
});
