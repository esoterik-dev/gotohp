# Fork integration scripts

These scripts serve a fork that keeps a `dev` integration branch. They are not
needed to build or use gotohp.

## `update-dev.sh`

Rebuilds `dev` as `origin/main` plus the fork owner's open pull requests, tests
the CLI, atomically replaces `bin/gotohp-cli`, and force-pushes `dev` with a
lease. Requires Bash, Git, Go and an authenticated `gh`.

- The repository comes from `git remote get-url origin`, never from `gh`'s
  default repository, which can point at upstream.
- A PR is included when it targets `main`, or targets the branch of another
  included PR (a stacked PR). They are merged in PR-number order.
- `dev` is regenerated on every run, so a closed PR drops out of `dev` and
  out of the local binary on the next run. The script refuses to run if
  `dev` holds a non-merge commit that is in neither `main` nor an open PR.
- Conflicts between PRs belong in the PRs; rebase them as a stack. If a merge
  still conflicts and no recorded `git rerere` resolution applies, the merge
  is aborted, `dev` is restored, and the script names the PR.
- It stops on tracked edits or untracked files. List personal notes that must
  stay untracked in `.git/info/exclude`.
- `--background` exits quietly on a dirty checkout or a branch other than
  `dev`, for use from a timer. `--no-build` only rebuilds and pushes `dev`.
- If the GitHub workflow pushes `dev` first, the lease rejects the local push
  and the script says so. The local binary is still correct; rerun to resync.

## `Sync dev integration branch` workflow

Runs `update-dev.sh --no-build` from `main` whenever a PR changes or `main`
moves, once the workflow is on the default branch. It runs no PR code: the
script only merges Git refs. Local binaries are updated by the script, not by
GitHub Actions.
