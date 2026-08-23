# Commands & Configuration

## Common Commands

```bash
# Build the project
go build -o eraser ./cmd/eraser

# Run tests
go test ./...

# CLI
./eraser init                          # Interactive config setup
./eraser send [--dry-run] [--resend] [--ignore-daily-limit]
./eraser list-brokers [--region eu] [--category financial-b2b] [--search kargo] [--missing-email]
./eraser status [--limit 50]
./eraser add-broker
./eraser mark-bounced <broker-id>...   # correct the record when an email actually bounced
./eraser cleanup-bounces               # find + clear bounced broker emails
./eraser monitor                       # IMAP inbox monitoring for broker replies
./eraser pipeline                      # which brokers need manual follow-up
./eraser confirm                       # click confirmation links from broker emails
./eraser fill                          # browser-automate opt-out forms
./eraser serve [--port 3000] [--no-browser]  # web UI
./eraser profile list                  # list configured profiles
./eraser profile add                   # add a second/third named profile
```

### Capture mode (`--capture-dir`)

`--capture-dir <path>` is a global flag that puts the process in **capture
mode**: `send` and the web UI render every message and record it to that
directory instead of connecting to a mail server. Nothing is transmitted.

```bash
./eraser --config /tmp/trial/config.yaml --capture-dir /tmp/trial/captured serve --no-browser
```

Each message is written as a numbered `.eml` file holding exactly the bytes
that would have gone to the SMTP server, alongside a `manifest.jsonl` with one
JSON object per send for programmatic reading. `ERASER_CAPTURE_DIR` sets the
same thing.

Capture mode is louder than `--dry-run` about being on - it prints a startup
warning and the web UI shows a banner on every page - because a run that
silently isn't sending is as bad as one that silently is. It also differs from
`--dry-run` in going through the *whole* send path, so history records, job
progress and pipeline state all update as they would for a real run. That makes
it what you want for trying the tool out or driving it from a test; `--dry-run`
remains the way to preview which brokers would be contacted without touching
history at all.

`options.dry_run: true` in the config file now also suppresses sending in the
web UI, recording to a `dry-run/` directory beside the config. Previously only
the CLI honoured it, so a config with `dry_run: true` still sent for real to
every broker as soon as you pressed "Send to All". An explicit `--capture-dir`
takes precedence, since it was given for that specific run.

Pair it with `--config` and `--brokers` pointed at a scratch directory to get a
completely isolated instance: config, `history.db` and `pending_job.json` all
live next to the config file.

Every command above (except `profile`, `add-broker`, `list-brokers`) accepts a global `--profile <id>` flag. It can be omitted entirely for the common single-profile setup; it's required once more than one profile is configured. See [multi-profile.md](multi-profile.md) for the full model.

## Configuration

User config is stored at `~/.eraser/config.yaml` (see `config.example.yaml` for the full schema). Key sections:

- `profile` - the legacy/primary profile: name/address/email + `additional_emails`/`name_variants`/`previous_addresses`/`additional_phones` for catching records indexed under old identities
- `profiles` - optional list of additional named profiles (see [multi-profile.md](multi-profile.md)); when present, this list is authoritative and `profile` above becomes vestigial unless one entry has `id: default`
- `email` - SMTP only
- `options` - `template`, `rate_limit_ms`, `daily_send_limit`, `regions`, `excluded_brokers`
- `inbox` - IMAP settings, for `monitor`/`pipeline`/the web UI's inbox scan (shared across all profiles - see [multi-profile.md](multi-profile.md#shared-inbox))
- `pipeline` - browser automation settings for `fill`
