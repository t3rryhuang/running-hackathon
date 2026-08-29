#!/usr/bin/env bash
# Deploy runhack to the Pi. OPERATOR-RUN: the deploy credentials (tailnet
# membership and the Pi ssh key) live on the operator's machine by design, so
# this script is never executed from a Devin box. It is idempotent, gated on
# gofmt/vet/tests, verified against /version, and rolls back if the new binary
# does not come up healthy.
#
# Config comes from the environment (see DEPLOY.md); no secret is ever written
# into this file or the repo.
#
#   RUNHACK_HOST      ssh target, e.g. runhack-pi (a Tailscale MagicDNS name)
#   RUNHACK_SSH_KEY   path to the private key (default ~/.ssh/runhack_pi)
#   RUNHACK_USER      remote user (default ubuntu)
#   RUNHACK_DIR       remote install dir (default /opt/runhack)
#   RUNHACK_SERVICE   systemd unit (default runhack)
#   RUNHACK_URL       public health/version URL (default https://runhack.keanuc.net)
#   RUNHACK_BRANCH    branch that may be deployed (default main)
#
# Exit codes: 0 deployed (or already up to date), 1 preflight/gate failure,
# 2 remote failure, 3 unhealthy and rolled back.
set -euo pipefail

HOST="${RUNHACK_HOST:-}"
KEY="${RUNHACK_SSH_KEY:-$HOME/.ssh/runhack_pi}"
USER_="${RUNHACK_USER:-ubuntu}"
DIR="${RUNHACK_DIR:-/opt/runhack}"
SERVICE="${RUNHACK_SERVICE:-runhack}"
URL="${RUNHACK_URL:-https://runhack.keanuc.net}"
BRANCH="${RUNHACK_BRANCH:-main}"
SKIP_TESTS="${RUNHACK_SKIP_TESTS:-}"

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31merror: %s\033[0m\n' "$*" >&2; exit "${2:-1}"; }

cd "$(dirname "$0")/.."

# --- preflight -------------------------------------------------------------
[ -n "$HOST" ] || die "RUNHACK_HOST is not set (see DEPLOY.md)"
[ -f "$KEY" ] || die "no ssh key at $KEY (see DEPLOY.md)"
command -v ssh >/dev/null || die "ssh not installed"
command -v curl >/dev/null || die "curl not installed"

SSH=(ssh -i "$KEY" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new
     -o ConnectTimeout=10 -o BatchMode=yes "$USER_@$HOST")

current_branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$current_branch" = "$BRANCH" ] || die "on branch $current_branch, only $BRANCH deploys"
[ -z "$(git status --porcelain)" ] || die "working tree is dirty; commit or stash first"
git fetch --quiet origin "$BRANCH" || die "cannot reach origin"
SHA="$(git rev-parse HEAD)"
if [ "$SHA" != "$(git rev-parse "origin/$BRANCH")" ]; then
	die "HEAD ($SHA) is not origin/$BRANCH; push first so the deployed commit is reproducible"
fi

say "preflight: $HOST reachable?"
"${SSH[@]}" true || die "cannot ssh to $USER_@$HOST - is Tailscale up on both ends and the key authorised?" 1

# --- gates -----------------------------------------------------------------
if [ -z "$SKIP_TESTS" ]; then
	say "gate: gofmt / vet / tests"
	[ -z "$(gofmt -l .)" ] || die "gofmt would change files"
	go vet ./... || die "go vet failed"
	go test ./... || die "tests failed"
fi

# --- already deployed? -----------------------------------------------------
running="$(curl -fsS --max-time 10 "$URL/version" 2>/dev/null | sed -n 's/.*"commit":"\([^"]*\)".*/\1/p' || true)"
if [ "$running" = "$SHA" ]; then
	say "already running $SHA - nothing to do"
	exit 0
fi
say "deploying $SHA (currently running: ${running:-unknown})"

# --- build -----------------------------------------------------------------
say "build: linux/arm64"
make build >/dev/null || die "arm64 build failed"
SUM="$(sha256sum runhack-arm64 | cut -d' ' -f1)"

# --- ship ------------------------------------------------------------------
RELEASE="$DIR/releases/$SHA"
say "ship: $RELEASE"
"${SSH[@]}" "sudo install -d -o $USER_ -g $USER_ $DIR/releases" || die "cannot create $DIR/releases" 2
scp -i "$KEY" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new \
	runhack-arm64 "$USER_@$HOST:/tmp/runhack-$SHA" || die "scp failed" 2
"${SSH[@]}" "bash -se" <<REMOTE || die "remote install failed" 2
set -euo pipefail
echo "$SUM  /tmp/runhack-$SHA" | sha256sum -c - >/dev/null
mkdir -p "$RELEASE"
mv "/tmp/runhack-$SHA" "$RELEASE/runhack"
chmod +x "$RELEASE/runhack"
# previous is what we roll back to; capture it before the symlink moves.
prev="\$(readlink -f $DIR/current || true)"
echo "\$prev" > $DIR/.previous
ln -sfn "$RELEASE" "$DIR/current.new"
mv -T "$DIR/current.new" "$DIR/current"
sudo systemctl restart $SERVICE
# keep the five most recent releases
ls -1dt $DIR/releases/*/ 2>/dev/null | tail -n +6 | xargs -r rm -rf
REMOTE

# --- verify ----------------------------------------------------------------
say "verify: $URL/healthz and /version"
ok=""
for _ in $(seq 1 15); do
	sleep 2
	body="$(curl -fsS --max-time 10 "$URL/version" 2>/dev/null || true)"
	case "$body" in *"\"commit\":\"$SHA\""*) ok=1; break ;; esac
done
if [ -z "$ok" ]; then
	say "unhealthy or wrong commit - rolling back"
	"${SSH[@]}" "bash -se" <<'ROLLBACK' || die "rollback failed - service may be down" 2
set -euo pipefail
prev="$(cat ${RUNHACK_DIR:-/opt/runhack}/.previous || true)"
if [ -n "$prev" ] && [ -d "$prev" ]; then
	ln -sfn "$prev" ${RUNHACK_DIR:-/opt/runhack}/current.new
	mv -T ${RUNHACK_DIR:-/opt/runhack}/current.new ${RUNHACK_DIR:-/opt/runhack}/current
fi
sudo systemctl restart ${RUNHACK_SERVICE:-runhack}
ROLLBACK
	die "deploy of $SHA failed verification; rolled back to the previous release" 3
fi

health="$(curl -fsS --max-time 10 "$URL/healthz" || true)"
[ "$health" = "ok" ] || die "/healthz did not return ok after deploy" 3
say "deployed $SHA and verified: $body"
