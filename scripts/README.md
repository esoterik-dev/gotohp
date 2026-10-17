# Fork integration scripts

These scripts serve a fork that keeps a `dev` integration branch. They are not
needed to build or use gotohp.

## `update-dev.sh`

Rebuilds `dev` as `origin/main` plus the fork owner's open pull requests, tests
the CLI, atomically replaces `bin/gotohp-cli`, and force-pushes `dev` with a
lease. Requires Bash, Git, Go and an authenticated `gh`.

- The repository comes from `git remote get-url origin`, never from `gh`'s
  default repository, which can point at upstream.
- Only open PRs authored by the fork owner are merged, and only when they
  target `main` or the branch of another included PR (a stacked PR). They are
  merged in PR-number order. Local-only branches, other people's PRs and PRs
  against other bases are not included.
- A manual run switches the checkout to `dev` (creating it from `origin/main`
  if needed) and leaves it there.
- `dev` is regenerated whenever `origin/main`, an included PR head or this
  script changes, so a closed PR drops out of `dev` and out of the local
  binary on the next run. When nothing changed and the binary is current, the
  run stops before touching `dev`; the inputs of the last build are kept in
  `.git/dev-build.inputs`, and `bin/gotohp-cli.rev` names the commit the
  binary was built from.
- The script refuses to run if `dev` holds a non-merge commit made directly on
  `dev`, that is, one that is not in `main`, an open PR, or a PR head that an
  earlier run merged (so a rebased, squash-merged or closed PR is fine).
- Conflicts between PRs belong in the PRs; rebase them as a stack. If a merge
  still conflicts and no recorded `git rerere` resolution applies, the merge
  is aborted, `dev` is restored, and the script names the PR. A failed test or
  build restores `dev` the same way.
- It stops on tracked edits or untracked files, and checks again before every
  `git reset --hard`, so edits made while it runs are never discarded. List
  personal notes that must stay untracked in `.git/info/exclude`.
- `--background` exits quietly on a dirty checkout or a branch other than
  `dev`, for use from a timer. `--no-build` only rebuilds and pushes `dev`.
- Runs are serialised with `flock` on `.git/dev-build.flock`; the lock is
  released when the process dies. In `--background` mode a busy lock skips the
  run with a log line.
- If the GitHub workflow pushes `dev` first, the lease rejects the local push
  and the script says so; a network or authentication failure is reported as
  such. The local binary is still correct; rerun to push.

## `Sync dev integration branch` workflow

Runs `update-dev.sh --no-build` from `main` whenever a PR changes or `main`
moves, once the workflow is on the default branch. It runs no PR code: the
script only merges Git refs. Local binaries are updated by the script, not by
GitHub Actions.
