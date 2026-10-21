# Repository instructions

## Pull requests

- Keep each pull request limited to one focused change. Split unrelated work into separate pull requests.
- Use a Conventional Commits title in the form `<type>(<optional-scope>): <description>`.
- Allowed title types are `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, and `revert`.
- Start the description with a lowercase letter. Example: `fix(settings): make Live Photo rows clickable`.

## Docs

- `docs/cli-reference.md` is generated from the CLI's help text. After changing a command or flag, run `go test ./internal/cli -run TestCLIReference -update`; the test fails while it is stale.
- Explain behaviour in `docs/cli-guide.md`; keep the README quick start to copy-paste commands.
