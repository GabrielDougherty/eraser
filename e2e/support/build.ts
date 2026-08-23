import { execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import path from "node:path";
import { binaryPath, repoRoot } from "./workspace";

/**
 * Builds the binary the suite drives.
 *
 * Called at config-load time rather than from globalSetup because Playwright
 * launches webServer *before* globalSetup runs - building there would be too
 * late and the server would fail to start.
 *
 * Deliberately a real `go build` to a stable path rather than `go run`: go run
 * compiles on first invocation, which blows the server readiness timeout on a
 * cold build cache, and it forwards signals poorly enough that Playwright's
 * teardown can leave an orphan holding the port. Building into e2e/.bin keeps
 * Go's build cache warm, so repeat local runs are near-instant.
 *
 * Set ERASER_E2E_BIN to skip this and use an existing binary.
 */
export function buildBinary(): void {
  if (process.env.ERASER_E2E_BIN) {
    console.log(`Using pre-built binary: ${process.env.ERASER_E2E_BIN}`);
    return;
  }

  mkdirSync(path.dirname(binaryPath), { recursive: true });
  console.log("Building eraser binary for e2e...");
  execFileSync("go", ["build", "-o", binaryPath, "./cmd/eraser"], {
    cwd: repoRoot,
    stdio: "inherit",
  });
}
