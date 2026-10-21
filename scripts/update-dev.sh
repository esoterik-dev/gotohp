#!/usr/bin/env bash
# Rebuild dev as origin/main plus the fork owner's open PRs, then build the
# CLI from it. See scripts/README.md.
set -euo pipefail
script=$(realpath "$0")
root=$(git -C "$(dirname "$script")/.." rev-parse --show-toplevel)
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

fail() { echo "$*" >&2; exit 1; }
# Git must not inherit the lock below: detached auto-maintenance would hold it.
git() { command git "$@" 9>&-; }
# Untracked files count as source changes unless ignored, for example
# through .git/info/exclude for personal notes.
clean_checkout() {
  git diff --quiet && git diff --cached --quiet || return 1
  [[ -z $(git ls-files --others --exclude-standard) ]]
}
on_dev_and_clean() { [[ $(git branch --show-current) == dev ]] && clean_checkout; }
ensure_dev() {
  on_dev_and_clean || fail 'Checkout changed during sync; stopped without replacing the build.'
}
# Move dev and the working tree to $1, checking first that no edit made since
# the sync started would be discarded.
reset_dev() {
  on_dev_and_clean || fail "Checkout changed during sync; not resetting dev to $1."
  git reset --quiet --hard "$1"
}

if ! clean_checkout; then
  $background && exit 0
  fail 'Source changes present; commit them on a PR branch before syncing dev.'
fi
if $background && [[ $(git branch --show-current) != dev ]]; then exit 0; fi
# flock is released when the process dies, so a crash cannot leave a stale lock.
lock=$(git rev-parse --git-path dev-build.flock)
exec 9>"$lock"
if ! flock --nonblock 9; then
  $background && { echo "Another dev sync holds $lock; skipping this run." >&2; exit 0; }
  fail "Another dev sync is running (lock: $lock)."
fi

tmp=''
previous=''
restore=false # true while dev differs from $previous and is not yet built
# On failure, put dev back where it was, without discarding anyone's edits.
restore_dev() {
  if [[ -f $(git rev-parse --git-path MERGE_HEAD) ]]; then git merge --abort || true; fi
  if [[ -z $previous ]]; then
    echo "dev did not exist before this run; left at $(git rev-parse --short dev)." >&2
  elif [[ $(git branch --show-current) != dev ]]; then
    git branch --force dev "$previous" && echo "dev restored to $previous." >&2
  elif clean_checkout; then
    git reset --quiet --hard "$previous" && echo "dev restored to $previous." >&2
  else
    echo "dev not restored to $previous: the checkout has uncommitted changes." >&2
  fi
}
on_exit() {
  local status=$?
  [[ -z $tmp ]] || rm -f "$tmp" "$tmp.rev"
  if (( status != 0 )) && $restore; then restore_dev; fi
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Use the repository origin points at; gh's default repo may be upstream.
origin_url=$(git remote get-url origin)
repo=$(sed -E 's#^(git@github\.com:|ssh://git@github\.com/|https://github\.com/)##; s#\.git$##; s#/$##' <<<"$origin_url")
if [[ ! $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  fail "origin is not a GitHub repository: $origin_url"
fi
owner=${repo%%/*}

git fetch --quiet --prune origin
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
# Everything dev is generated from. If it is unchanged, so is dev.
inputs="main $(git rev-parse origin/main)"$'\n'"script $(git hash-object "$script")"
for number in $numbers; do
  git fetch --quiet origin "+refs/pull/$number/head:refs/remotes/origin/pr/$number"
  pr_refs+=("refs/remotes/origin/pr/$number")
  inputs+=$'\n'"pr $number $(git rev-parse "refs/remotes/origin/pr/$number")"
done

state=$(git rev-parse --git-path dev-build.inputs)
regenerate=true
if git show-ref --verify --quiet refs/heads/dev; then
  # dev is generated. Refuse to rebuild over work that exists nowhere else,
  # that is, commits made directly on dev. PR heads merged by earlier runs (the
  # second parents of dev's merges) count as known even after the PR was
  # rebased, squash-merged or closed, and its old commits left every ref.
  mapfile -t merged_heads < <(git rev-list --merges --parents origin/main..dev | cut -d' ' -f3- | tr ' ' '\n')
  mapfile -t orphaned < <(git rev-list --no-merges dev --not origin/main "${pr_refs[@]}" "${merged_heads[@]}")
  if ((${#orphaned[@]})); then
    echo "dev has commits that are not in main or any merged PR; move them to a PR branch first:" >&2
    git log --oneline --no-walk "${orphaned[@]}" >&2; exit 1
  fi
  previous=$(git rev-parse dev)
  $background || git switch --quiet dev
  if [[ -f $state && $(<"$state") == "$previous"$'\n'"$inputs" ]]; then regenerate=false; fi
fi

if $regenerate; then
  [[ $(git branch --show-current) == dev ]] || git switch --quiet -c dev origin/main
  reset_dev origin/main
  restore=true
  for number in $numbers; do
    ensure_dev
    # rerere replays conflict resolutions recorded on earlier runs. --no-ff
    # records every PR head as a merge parent, which the orphan check relies on.
    if ! git -c rerere.enabled=true -c rerere.autoUpdate=true merge --quiet --no-ff --no-edit "refs/remotes/origin/pr/$number"; then
      if [[ -z $(git diff --name-only --diff-filter=U) ]]; then
        git commit --quiet --no-edit
      else
        fail "PR #$number conflicts with the PRs before it; fix the PR (or rebase the stack) and rerun."
      fi
    fi
  done
fi
ensure_dev
revision=$(git rev-parse HEAD)
if $build && [[ ! -x bin/gotohp-cli || ! -f bin/gotohp-cli.rev || $(<bin/gotohp-cli.rev) != "$revision" ]]; then
  go test -tags cli ./core ./internal/cli
  mkdir -p bin
  tmp=$(mktemp "$root/bin/.gotohp-cli.XXXXXX")
  CGO_ENABLED=0 go build -tags cli -trimpath -o "$tmp" .
  ensure_dev
  if [[ $(git rev-parse HEAD) != "$revision" ]]; then
    restore=false # someone else moved dev; leave their commit alone
    fail 'dev moved during build; retry.'
  fi
  chmod +x "$tmp"
  printf '%s\n' "$revision" > "$tmp.rev"
  mv -f "$tmp" bin/gotohp-cli
  mv -f "$tmp.rev" bin/gotohp-cli.rev
  tmp=''
  built=true
else
  built=false
fi
restore=false
printf '%s\n%s\n' "$revision" "$inputs" > "$state"

ensure_dev
remote=$(git rev-parse --quiet --verify refs/remotes/origin/dev || true)
if [[ $remote != "$revision" ]]; then
  if ! git push --quiet --force-with-lease="dev:$remote" origin dev; then
    if ! now=$(git ls-remote origin refs/heads/dev); then
      fail "dev $revision is ready locally, but origin could not be reached to push it (network or authentication; see above). Rerun to retry."
    elif [[ ${now%%[[:space:]]*} != "$remote" ]]; then
      fail "dev $revision is ready locally, but another sync (likely the GitHub workflow) updated origin/dev first. Rerun to resync."
    else
      fail "dev $revision is ready locally, but pushing it failed (see above); origin/dev is unchanged. Rerun to retry."
    fi
  fi
  pushed=true
else
  pushed=false
fi
if $built; then
  printf 'dev %s → %s/bin/gotohp-cli\n' "$revision" "$root"
elif $regenerate || $pushed; then
  printf 'dev %s pushed\n' "$revision"
else
  $background || printf 'dev %s is up to date (main and PRs unchanged)\n' "$revision"
fi
