#!/usr/bin/env bash
# Run the gates, push the branch, and print the exact deploy request to hand to
# the operator. This is the path a Devin box actually has: it can push, it
# cannot reach the Pi.
set -euo pipefail

BRANCH="${RUNHACK_BRANCH:-main}"
REMOTE="${RUNHACK_GIT_REMOTE:-origin}"

cd "$(dirname "$0")/.."

[ "$(git rev-parse --abbrev-ref HEAD)" = "$BRANCH" ] || { echo "not on $BRANCH" >&2; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "working tree is dirty; commit first" >&2; exit 1; }

[ -z "$(gofmt -l .)" ] || { echo "gofmt would change files" >&2; exit 1; }
go vet ./...
go test ./...
make build >/dev/null

git push "$REMOTE" "$BRANCH"
sha="$(git rev-parse HEAD)"
echo
echo "OPS REQUEST: deploy $BRANCH @ $sha"
echo "verify afterwards with: scripts/check-deploy.sh"
