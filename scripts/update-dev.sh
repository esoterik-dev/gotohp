#!/usr/bin/env bash
# Rebuild dev as origin/main plus the fork owner's open PRs, then build the
# CLI from it. See scripts/README.md.
set -euo pipefail
root=$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)
cd "$root"
background=false
build=true
for arg in "$@"; do
  case $arg in
    --background) background=true ;;
    --no-build) build=false ;;
    *) echo "usage: $0 [--background] [--no-build]" >&2; exit 2 ;;
  esac
done

# Untracked files count as source changes unless ignored, for example
# through .git/info/exclude for personal notes.
clean_checkout() {
  git diff --quiet && git diff --cached --quiet || return 1
  [[ -z $(git ls-files --others --exclude-standard) ]]
}
ensure_dev() {
  if [[ $(git branch --show-current) != dev ]] || ! clean_checkout; then
    echo 'Checkout changed during sync; stopped without replacing the build.' >&2; exit 1
  fi
}
if ! clean_checkout; then
  $background && exit 0
  echo 'Source changes present; commit them on a PR branch before syncing dev.' >&2; exit 1
fi
if $background && [[ $(git branch --show-current) != dev ]]; then exit 0; fi
lock=$(git rev-parse --git-path dev-build.lock)
if ! mkdir "$lock" 2>/dev/null; then
  $background && exit 0
  echo "Another dev sync is running (or stale lock: $lock)." >&2; exit 1
fi
tmp=''
trap '[[ -z "$tmp" ]] || rm -f "$tmp" "$tmp.rev"; rmdir "$lock"' EXIT

# Use the repository origin points at; gh's default repo may be upstream.
origin_url=$(git remote get-url origin)
repo=$(sed -E 's#^(git@github\.com:|ssh://git@github\.com/|https://github\.com/)##; s#\.git$##; s#/$##' <<<"$origin_url")
if [[ ! $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "origin is not a GitHub repository: $origin_url" >&2; exit 1
fi
owner=${repo%%/*}

git fetch --prune origin
# Open owner PRs whose base is main, or (for stacked PRs) another included PR.
declare -A included=()
prs=$(gh pr list --repo "$repo" --author "$owner" --state open --limit 1000 \
  --json number,baseRefName,headRefName --jq '.[] | "\(.number) \(.baseRefName) \(.headRefName)"')
changed=true
while $changed; do
  changed=false
  while read -r number base head; do
    [[ -z ${number:-} || -n ${included[$head]:-} ]] && continue
    if [[ $base == main || -n ${included[$base]:-} ]]; then
      included[$head]=$number
      changed=true
    fi
  done <<<"$prs"
done
numbers=$(printf '%s\n' "${included[@]}" | sort -n)
pr_refs=()
for number in $numbers; do
  git fetch origin "+refs/pull/$number/head:refs/remotes/origin/pr/$number"
  pr_refs+=("refs/remotes/origin/pr/$number")
done

if git show-ref --verify --quiet refs/heads/dev; then
  # dev is generated. Refuse to rebuild over work that exists nowhere else.
  orphaned=$(git rev-list --no-merges dev --not origin/main "${pr_refs[@]}")
  if [[ -n $orphaned ]]; then
    echo "dev has commits that are not in main or any open PR; move them to a PR branch first:" >&2
    git log --oneline --no-walk $orphaned >&2; exit 1
  fi
  previous=$(git rev-parse dev)
  $background || git switch --quiet dev
else
  previous=''
fi
[[ $(git branch --show-current) == dev ]] || git switch --quiet -c dev origin/main
git reset --quiet --hard origin/main
for number in $numbers; do
  ensure_dev
  # rerere replays conflict resolutions recorded on earlier runs.
  if ! git -c rerere.enabled=true -c rerere.autoUpdate=true merge --quiet --no-edit "refs/remotes/origin/pr/$number"; then
    if [[ -z $(git diff --name-only --diff-filter=U) ]]; then
      git commit --quiet --no-edit
    else
      git merge --abort
      [[ -z $previous ]] || git reset --quiet --hard "$previous"
      echo "PR #$number conflicts with the PRs before it; fix the PR (or rebase the stack) and rerun." >&2; exit 1
    fi
  fi
done
ensure_dev
revision=$(git rev-parse HEAD)
if $build && [[ ! -x bin/gotohp-cli || ! -f bin/gotohp-cli.rev || $(<bin/gotohp-cli.rev) != "$revision" ]]; then
  go test -tags cli ./core ./internal/cli
  mkdir -p bin
  tmp=$(mktemp "$root/bin/.gotohp-cli.XXXXXX")
  CGO_ENABLED=0 go build -tags cli -trimpath -o "$tmp" .
  ensure_dev
  [[ $(git rev-parse HEAD) == "$revision" ]] || { echo 'dev moved during build; retry.' >&2; exit 1; }
  chmod +x "$tmp"
  printf '%s\n' "$revision" > "$tmp.rev"
  mv -f "$tmp" bin/gotohp-cli
  mv -f "$tmp.rev" bin/gotohp-cli.rev
  tmp=''
fi
ensure_dev
remote=$(git rev-parse --quiet --verify refs/remotes/origin/dev || true)
if ! git push --quiet --force-with-lease="dev:$remote" origin dev; then
  echo "dev $revision is built locally, but another sync (likely the GitHub workflow) updated origin/dev first. Rerun to resync." >&2
  exit 1
fi
if $build; then
  printf 'dev %s → %s/bin/gotohp-cli\n' "$revision" "$root"
else
  printf 'dev %s pushed\n' "$revision"
fi
