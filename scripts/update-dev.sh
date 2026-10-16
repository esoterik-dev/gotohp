#!/usr/bin/env bash
# Sync the fork owner's open main-targeted PRs, then build only from dev.
set -euo pipefail
root=$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)
cd "$root"
background=false
if [[ ${1:-} == --background ]]; then background=true; elif [[ $# != 0 ]]; then
  echo "usage: $0 [--background]" >&2; exit 2
fi
clean_checkout() {
  git diff --quiet && git diff --cached --quiet || return 1
  local untracked
  untracked=$(git ls-files --others --exclude-standard | grep -vx -e INSTRUCTIONS.md -e PROGRESS.md || true)
  [[ -z "$untracked" ]]
}
ensure_dev() {
  if [[ $(git branch --show-current) != dev ]] || ! clean_checkout; then
    echo 'Checkout changed during sync; stopped without replacing the build.' >&2; exit 1
  fi
}
if ! clean_checkout; then
  $background && exit 0
  echo 'Source changes present; commit or stash them before syncing dev.' >&2; exit 1
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
if $background; then
  [[ $(git branch --show-current) == dev ]] && clean_checkout || exit 0
else
  if git show-ref --verify --quiet refs/heads/dev; then git switch dev; else git switch -c dev origin/main; fi
fi
ensure_dev
git merge --no-edit origin/main
ensure_dev
if git show-ref --verify --quiet refs/remotes/origin/dev; then git merge --no-edit origin/dev; fi
numbers=$(gh pr list --repo "$repo" --author "$owner" --base main --state open --limit 1000 --json number --jq '.[].number' | sort -n)
for number in $numbers; do
  git fetch origin "refs/pull/$number/head"
  ensure_dev
  git merge --no-edit FETCH_HEAD
done
ensure_dev
revision=$(git rev-parse HEAD)
if [[ ! -x bin/gotohp-cli || ! -f bin/gotohp-cli.rev || $(<bin/gotohp-cli.rev) != "$revision" ]]; then
  go test -tags cli ./core ./internal/cli
  mkdir -p bin
  tmp=$(mktemp "$root/bin/.gotohp-cli.XXXXXX")
  CGO_ENABLED=0 go build -tags cli -trimpath -o "$tmp" .
  ensure_dev
  [[ $(git rev-parse HEAD) == "$revision" ]] || { echo 'dev moved during build; retry.' >&2; exit 1; }
  chmod +x "$tmp"
  mv -f "$tmp" bin/gotohp-cli
  tmp=''
  printf '%s\n' "$revision" > bin/gotohp-cli.rev
fi
ensure_dev
git push origin dev
printf 'dev %s → %s/bin/gotohp-cli\n' "$revision" "$root"
