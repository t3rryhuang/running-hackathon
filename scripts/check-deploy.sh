#!/usr/bin/env bash
# Report whether the running deployment matches the repo. Needs nothing but
# HTTPS, so it works from any box - including one with no access to the Pi.
#
#   RUNHACK_URL     default https://runhack.keanuc.net
#   RUNHACK_BRANCH  default main
#
# Exit codes: 0 in sync, 1 drift (running commit != origin/branch), 2 the
# service is unreachable or too old to report /version.
set -euo pipefail

URL="${RUNHACK_URL:-https://runhack.keanuc.net}"
BRANCH="${RUNHACK_BRANCH:-main}"

cd "$(dirname "$0")/.."
git fetch --quiet origin "$BRANCH" 2>/dev/null || true
want="$(git rev-parse --verify --quiet "origin/$BRANCH" || true)"
if [ -z "$want" ]; then
	# No tracking ref (a fresh clone, or a push made straight to a URL): the
	# local commit is the best statement of what should be running.
	BRANCH="local HEAD"
	want="$(git rev-parse HEAD)"
fi

body="$(curl -fsS --max-time 10 "$URL/version" 2>/dev/null || true)"
if [ -z "$body" ]; then
	echo "unreachable: $URL/version returned nothing (service down, or running a build older than /version)" >&2
	exit 2
fi

field() { printf '%s' "$body" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
have="$(field commit)"
built="$(field built_at)"
backend="$(field backend)"
started="$(field started_at)"

echo "running : ${have:-unknown} (built $built, backend $backend, up since $started)"
echo "expected: $want ($BRANCH)"

if [ "$have" = "$want" ]; then
	echo "in sync"
	exit 0
fi
echo "DRIFT: the deployment is not running $BRANCH" >&2
if git cat-file -e "$have^{commit}" 2>/dev/null; then
	echo "behind by:" >&2
	git --no-pager log --oneline "$have..$want" >&2
fi
exit 1
