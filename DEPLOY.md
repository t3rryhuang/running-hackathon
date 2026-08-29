# Deploying CheckIn

## The real path

Devin's box is **not** on the tailnet and holds **no** SSH credentials for the
Pi. That is deliberate: deploy credentials stay operator-side. So the deploy is
two-party, and every step is verifiable:

1. Devin runs the gates and pushes: `scripts/release.sh`
   (gofmt → `go vet` → `go test` → arm64 build → `git push origin main`).
   It prints the exact request line.
2. Devin posts `OPS REQUEST: deploy main @ <sha>`.
3. The operator pulls that sha, cross-compiles arm64, copies the binary to the
   Pi, restarts the unit and confirms health. Turnaround is ~2-3 minutes.
4. Either side verifies with `scripts/check-deploy.sh`, which compares
   `GET /version` against `origin/main` over plain HTTPS — no Pi access needed.

Anything that claims deployment without step 4 passing is a claim, not a fact.

## Verifying and detecting drift

The binary stamps its commit at build time (`-ldflags -X main.commit=…`, set by
the Makefile) and reports it:

```bash
curl -s https://runhack.keanuc.net/version
# {"commit":"bfec842…","dirty":false,"built_at":"2026-08-29T14:12:03Z",
#  "go_version":"go1.23.4","backend":"postgres","started_at":"…","uptime_seconds":812}

scripts/check-deploy.sh     # 0 in sync, 1 drift (lists the missing commits), 2 unreachable
```

`backend` is the one field worth watching after a config change: it says
`sqlite` if `DATABASE_URL` was not exported to the process, which is the failure
mode that otherwise looks completely healthy while quietly using the wrong
database. `/version` carries no secrets — no DSN, no credentials, no user data.

## The Pi

| Thing | Where |
| --- | --- |
| Binary | `/opt/runhack/runhack-arm64` |
| Environment | `/opt/runhack/.env`, loaded by systemd `EnvironmentFile=` |
| Service | `systemctl {status,restart} runhack` |
| Reverse proxy | Caddy vhost `runhack.keanuc.net` → `127.0.0.1:8090` |
| Database | Postgres 16 in docker container `runhack-pg`, `127.0.0.1:5433`, persistent volume, restart policy on |
| Logs | `journalctl -u runhack -f` |

Environment handling:

- `DATABASE_URL` must be **exported** to the process. With systemd,
  `EnvironmentFile=/opt/runhack/.env` does that; a value only present in an
  interactive shell does not. If it is missing the service still boots — on
  SQLite — so check the boot log line or `/version`'s `backend`.
- `TWILIO_AUTH_TOKEN` being set turns on inbound signature verification,
  computed over the public HTTPS URL. Caddy must forward `Host` and
  `X-Forwarded-Proto` (its defaults do) or `/sms` will 403 legitimate traffic.
- `ELEVENLABS_WEBHOOK_SECRET` must be exported too. Post-call transcript
  deliveries to `/webhooks/elevenlabs` are rejected without it, silently as far
  as the dashboard is concerned — it will simply show no calls. Check with
  `journalctl -u runhack | grep transcript`. Rejections now log a reason and the
  first 220 bytes of the body, so a refused delivery is diagnosable.
- `ELEVENLABS_API_KEY` does double duty: outbound calls, and the transcript
  backfill that runs at boot and every 10 minutes against
  `GET /v1/convai/conversations`. Without it the dashboard only ever shows calls
  whose webhook delivery arrived; with it, calls that predate the webhook (or
  whose delivery was lost — provider retries are off) are reconciled in.
- Sign-in codes go out over Twilio, so `/login` only works on a box where
  Twilio is configured. Without it a code is generated and never delivered.
- Secrets live only in `/opt/runhack/.env` (operator-owned, mode 600). Nothing
  in this repo, including these scripts, ever contains one.

Migrations run at boot, in order, inside a transaction, and are idempotent, so a
deploy needs no manual DDL. There is no automatic SQLite→Postgres data
migration: the first Postgres boot starts empty.

## Optional: the automated script (operator machine)

`scripts/deploy.sh` does the whole thing in one gated, idempotent command from a
machine that *does* have tailnet access and the Pi key. It is not usable from a
Devin box, and it is not required — the two-party flow above is the supported
path. It refuses to run unless the tree is clean, `HEAD == origin/main`, and
gofmt/vet/tests pass; it is a no-op when `/version` already reports the target
commit; and it rolls back if the new build does not report the expected commit
within 30 seconds.

```bash
export RUNHACK_HOST=runhack-pi            # Tailscale MagicDNS name
export RUNHACK_SSH_KEY=~/.ssh/runhack_pi  # operator-owned key, never in the repo
export RUNHACK_USER=ubuntu
make deploy
```

It keeps releases at `/opt/runhack/releases/<sha>/runhack` with
`/opt/runhack/current` as a symlink, and rolls back by moving the symlink. Using
it therefore requires the unit's `ExecStart` to point at
`/opt/runhack/current/runhack` instead of `/opt/runhack/runhack-arm64` — a
one-line change, and the only reason not to adopt it is that it is one more
thing to keep in sync during a hackathon.

| Variable | Default | Meaning |
| --- | --- | --- |
| `RUNHACK_HOST` | — | ssh target (required) |
| `RUNHACK_SSH_KEY` | `~/.ssh/runhack_pi` | private key path |
| `RUNHACK_USER` | `ubuntu` | remote user |
| `RUNHACK_DIR` | `/opt/runhack` | install root |
| `RUNHACK_SERVICE` | `runhack` | systemd unit |
| `RUNHACK_URL` | `https://runhack.keanuc.net` | health/version URL |
| `RUNHACK_BRANCH` | `main` | only branch allowed to deploy |

Exit codes: `0` deployed or already current, `1` preflight/gate failure, `2`
remote failure, `3` unhealthy and rolled back.

Tailscale assumptions: the operator machine and the Pi are on the same tailnet,
the Pi's SSH is reachable over it (nothing is exposed publicly except Caddy on
443), and MagicDNS resolves `RUNHACK_HOST`. If Tailscale is down the script
fails in preflight rather than half-deploying.

## Rollback without the script

```bash
ssh <pi> 'sudo cp /opt/runhack/runhack-arm64.prev /opt/runhack/runhack-arm64 \
  && sudo systemctl restart runhack'
curl -s https://runhack.keanuc.net/version
```

Keep the previous binary as `.prev` before overwriting; it is the whole
deployment, so that single file is the rollback.
