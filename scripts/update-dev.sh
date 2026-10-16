#!/usr/bin/env bash
# Sync the fork owner's open main-targeted PRs, then build only from dev.
set -euo pipefail
root=$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)
cd "$root"
background=false
if [[ ${1:-} == --background ]]; then background=true; elif [[ $# != 0 ]]; then
  echo "usage: $0 [--background]" >&2; exit 2
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
  $background && exit 0
  echo 'Tracked changes present; commit or stash them before syncing dev.' >&2; exit 1
fi
if $background && [[ $(git branch --show-current) != dev ]]; then exit 0; fi
lock=$(git rev-parse --git-path dev-build.lock)
if ! mkdir "$lock" 2>/dev/null; then
  $background && exit 0
  echo "Another dev sync is running (or stale lock: $lock)." >&2; exit 1
fi
tmp=''
trap '[[ -z "$tmp" ]] || rm -f "$tmp"; rmdir "$lock"' EXIT
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
owner=${repo%%/*}
git fetch origin
if git show-ref --verify --quiet refs/heads/dev; then git switch dev; else git switch -c dev origin/main; fi
git merge --no-edit origin/main
if git show-ref --verify --quiet refs/remotes/origin/dev; then git merge --no-edit origin/dev; fi
numbers=$(gh pr list --repo "$repo" --author "$owner" --base main --state open --limit 1000 --json number --jq '.[].number' | sort -n)
for number in $numbers; do
  git fetch origin "refs/pull/$number/head"
  git merge --no-edit FETCH_HEAD
done
[[ $(git branch --show-current) == dev ]]
revision=$(git rev-parse HEAD)
if [[ ! -x bin/gotohp-cli || ! -f bin/gotohp-cli.rev || $(<bin/gotohp-cli.rev) != "$revision" ]]; then
  go test -tags cli ./core ./internal/cli
  mkdir -p bin
  tmp=$(mktemp "$root/bin/.gotohp-cli.XXXXXX")
  CGO_ENABLED=0 go build -tags cli -trimpath -o "$tmp" .
  chmod +x "$tmp"
  mv -f "$tmp" bin/gotohp-cli
  tmp=''
  printf '%s\n' "$revision" > bin/gotohp-cli.rev
fi
git push origin dev
printf 'dev %s → %s/bin/gotohp-cli\n' "$revision" "$root"
