# End-to-end tests

Browser tests that drive the real web UI from a completely fresh install:
setup wizard → send → verify results. They exist because nothing in the Go
test suite starts a real listener, so CSRF, session cookies, the middleware
chain and static asset serving are otherwise untested.

## Running

```bash
cd e2e
npm ci
npx playwright install chromium
npm test
```

Other scripts: `npm run test:ui` (Playwright's UI mode, best for debugging),
`npm run report` (last HTML report), `npm run typecheck`.

These are **not** part of `go test ./...`. That suite stays fast and needs no
browser; this one runs on its own and has its own CI job.

## How a run works

`playwright.config.ts` builds the binary into `.bin/`, mints a fresh workspace
under `.artifacts/<timestamp>/`, and starts the server against it:

```
eraser --config <ws>/config.yaml \
       --brokers e2e/fixtures/brokers.yaml \
       --capture-dir <ws>/captured \
       serve --port 8099 --no-browser
```

`--config` places `config.yaml`, `history.db` and `pending_job.json` in one
directory, so each run is a fully isolated instance of the app.

**Every run gets a brand-new workspace, never a reused one.** A leftover
`config.yaml` means the app is already set up, which invalidates the
from-scratch premise; a leftover `pending_job.json` makes the server
auto-resume a send two seconds after boot. For the same reason
`reuseExistingServer` is `false` even locally.

Workspaces are kept after the run — open `.artifacts/<timestamp>/captured/` to
read the actual emails a run produced, which is usually the fastest way to
understand a failure. The last few runs are retained and older ones are pruned
automatically, so this directory doesn't grow without bound.

## Capture mode

`--capture-dir` makes the app record every message instead of transmitting it.
Each message is written as a numbered `.eml` holding exactly the bytes that
would have gone to the SMTP server, plus a line in `manifest.jsonl` for
structured reading. `support/captured.ts` wraps both.

The broker fixture uses `.invalid` addresses (RFC 2606, permanently
unresolvable), so even a catastrophic mis-wiring that reached a real mail
server could not deliver to anyone.

### What these tests do *not* prove

**Capture mode ignores the email configuration entirely.** The suite fills in
SMTP settings during the wizard and asserts they were written to
`config.yaml`, but it never connects to a mail server — so it cannot tell you
that a given SMTP host, port or app password actually works. Nothing here
exercises `SMTPSender`'s network path.

What it does prove is that the message *content* and the surrounding flow are
right: the correct template rendered for the configured profile, addressed to
the right broker, with history, progress and pipeline state updated as they
would be on a real run.

## Conventions worth knowing

- **`127.0.0.1`, never `localhost`.** The server binds IPv4 loopback only; on
  hosts where `localhost` resolves to `::1` first the connection is refused.
  Session and CSRF cookies are host-scoped, so mixing the two names mid-run
  silently breaks the wizard.
- **`workers: 1`, no parallelism.** One `config.yaml`, one SQLite database, one
  pending-job file, and a rate limiter keyed on global strings.
- **Never assert on intermediate progress.** `Job.Complete()` forces progress
  to 100 and the UI polls every 1.5s, so mid-run percentages are a flake
  generator. Assert terminal states only.
- Sends are rate-limited to one every 2s by the wizard's default config, so a
  full send takes several seconds. Timeouts are set accordingly.
