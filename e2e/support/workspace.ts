import { mkdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// path.dirname() on a URL path with a trailing slash strips the wrong
// component, so resolve this file's own directory and walk up from there.
const supportDir = path.dirname(fileURLToPath(import.meta.url));
export const e2eRoot = path.dirname(supportDir);
export const repoRoot = path.dirname(e2eRoot);

/**
 * A workspace is one run's scratch directory. `--config` puts config.yaml,
 * history.db and pending_job.json all alongside each other, so a fresh
 * directory is a completely isolated instance.
 *
 * Every run gets a new one, never a reused one. A leftover pending_job.json
 * would make the server auto-resume a send two seconds after boot - a send
 * path that runs with no HTTP request behind it - and a leftover config.yaml
 * would mean the app is already set up, which quietly invalidates the entire
 * from-scratch premise these tests exist to check.
 */
export interface Workspace {
  dir: string;
  configPath: string;
  captureDir: string;
}

export function createWorkspace(): Workspace {
  // Playwright loads this config once in the main process and again in every
  // worker, so minting a fresh timestamped directory unconditionally would
  // give the server one workspace and each spec a different, empty one - the
  // tests would then read a capture directory nothing ever wrote to. The
  // first call publishes the directory into the environment, which workers
  // inherit when they are spawned, and later calls adopt it.
  const existing = process.env.ERASER_E2E_WORKSPACE;
  if (existing) return workspaceAt(existing);

  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const dir = path.join(e2eRoot, ".artifacts", stamp);
  mkdirSync(dir, { recursive: true });
  process.env.ERASER_E2E_WORKSPACE = dir;
  return workspaceAt(dir);
}

/** True in the process that owns this run, false in spawned workers. */
export function isPrimaryProcess(): boolean {
  return !process.env.ERASER_E2E_WORKSPACE;
}

function workspaceAt(dir: string): Workspace {
  return {
    dir,
    configPath: path.join(dir, "config.yaml"),
    captureDir: path.join(dir, "captured"),
  };
}

/**
 * The workspace is created once when playwright.config.ts loads and handed to
 * the specs through the environment, so every worker and the server itself
 * agree on which directory this run owns.
 */
export function workspaceFromEnv(): Workspace {
  const dir = process.env.ERASER_E2E_WORKSPACE;
  if (!dir) {
    throw new Error("ERASER_E2E_WORKSPACE is not set - run through playwright.config.ts");
  }
  return workspaceAt(dir);
}

/** Absolute path to the broker fixture, independent of the server's cwd. */
export const brokerFixture = path.join(e2eRoot, "fixtures", "brokers.yaml");

/** Where globalSetup puts the compiled binary. */
export const binaryPath = process.env.ERASER_E2E_BIN ?? path.join(e2eRoot, ".bin", "eraser");
