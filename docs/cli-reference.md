# gotohp-cli command reference

Generated from the CLI's own help text; do not edit by hand. Regenerate with `go test ./internal/cli -run TestCLIReference -update`. For how the options work together, see the [CLI guide](cli-guide.md).

Commands:

- [`gotohp-cli creds add`](#gotohp-cli-creds-add) — Add an account from a raw auth string or an Embedded Setup oauth_token cookie
- [`gotohp-cli creds list`](#gotohp-cli-creds-list) — List all credentials
- [`gotohp-cli creds remove`](#gotohp-cli-creds-remove) — Remove a credential by email
- [`gotohp-cli creds set`](#gotohp-cli-creds-set) — Set active credential (supports partial matching)
- [`gotohp-cli upload`](#gotohp-cli-upload) — Upload files or directories
- [`gotohp-cli verify-live-photos`](#gotohp-cli-verify-live-photos) — Verify Live Photo pairs using remote hash responses
- [`gotohp-cli version`](#gotohp-cli-version) — Show version information

## Global options

These work with every command.

| Option | Default | Description |
| --- | --- | --- |
| `-c, --config string` |  | path to config file |

## gotohp-cli creds add

Add an account.

Pass either a raw credential string (androidId=...&Email=...&Token=...) or the
oauth_token cookie value from https://accounts.google.com/EmbeddedSetup. A cookie
is exchanged with Google for a credential, validated, and saved; the account
becomes the selected one.

Pass "-" to read the value from standard input instead, which keeps it out of
shell history and process listings.

```text
gotohp-cli creds add <auth-string | oauth-token> [flags]
```

| Option | Default | Description |
| --- | --- | --- |
| `--proxy string` |  | proxy URL for the sign-in exchange |

## gotohp-cli creds list

List all credentials

```text
gotohp-cli creds list
```

Aliases: `ls`

## gotohp-cli creds remove

Remove a credential by email

```text
gotohp-cli creds remove <email>
```

Aliases: `rm`

## gotohp-cli creds set

Set active credential (supports partial matching)

```text
gotohp-cli creds set <email>
```

Aliases: `select`

## gotohp-cli upload

Upload files or directories to Google Photos.

The config file only supplies credentials. Everything else, including the
proxy and quality settings, is controlled by flags; GUI preferences do not apply.

```text
gotohp-cli upload <path> [<path> ...] [flags]
```

| Option | Default | Description |
| --- | --- | --- |
| `--account string` |  | account email to upload with (default: the selected account) |
| `-a, --album string` |  | add uploads to album (creates if needed); 'AUTO' creates albums from folder names |
| `--date-from-filename` |  | set media date from filename (e.g. 20240709_182027.jpg) |
| `-d, --delete` |  | delete from host after upload |
| `--disable-filter` |  | disable file type filtering |
| `-e, --exclude string` |  | exclude directories whose name matches pattern (e.g. @eaDir) |
| `-f, --force` |  | upload even if the file already exists in the library |
| `--hash-threads int` |  | split the work: workers that hash, stage and check for duplicates (default: --threads) |
| `--ignore-apple-metadata` |  | match Live Photo pairs by filename stem instead of Apple metadata |
| `--log-file string` |  | write the run's debug log to PATH (default: a new gotohp-upload-<time>-<pid>.log in the current directory) |
| `-l, --log-level string` | `info` | stderr log level without the TUI (--no-tui or no terminal): debug, info, warn, error |
| `--max-retries int` | `3` | retries after the initial attempt for transient request failures (0 disables) |
| `--no-log-file` |  | write no debug log file |
| `--no-tui` |  | disable the interactive progress UI |
| `--pair-live-photos` |  | pair Apple Live Photo files; files that cannot be paired safely are skipped |
| `--proxy string` |  | proxy URL for all requests |
| `-r, --recursive` |  | include subdirectories |
| `--report-file string` |  | write the final JSON summary to PATH |
| `--requests-per-minute int` |  | cap Photos RPCs per minute across all workers (default 0: no cap; a 429 always pauses the run) |
| `--retry-429` | `true` | retry HTTP 429 responses (disable with --retry-429=false) |
| `--retry-report string` |  | retry only failed, non-skipped files from a JSON report; no positional paths |
| `--revalidate-state` |  | ignore cached confirmations and recheck remote state |
| `--saver` |  | upload in storage saver quality |
| `--stage-local` |  | copy each file to TMPDIR before hashing and uploading |
| `--state-file string` |  | read confirmed upload state from PATH |
| `-t, --threads int` | `3` | workers; each hashes, checks and uploads its own files (see --hash-threads) |
| `--update-existing-photos-to-live` |  | attach matching MOV files to existing photos |
| `--update-state` |  | checkpoint confirmed uploads to --state-file |
| `--upload-incomplete-live-photos` |  | upload Live Photo files that cannot be paired (unmatched, unreadable or ambiguous) as single files |
| `--upload-threads int` |  | split the work: workers that transfer and commit new files (default: --threads) |
| `--use-quota` |  | count uploads against the account's storage quota |

## gotohp-cli verify-live-photos

Verify Live Photo pairs without downloading media from Google Photos.

Each local HEIF and MOV is hashed, and the authenticated Google Photos
hash endpoint is queried for both hashes. A pair is verified when both
responses share a primary or related remote media key. This is a
metadata check, not playback or downloaded-content verification.

Files are read from the paths in the report. If the files have moved,
--source-root finds each one under a new root by file name, using its
parent folder name to tell apart files with the same name.

```text
gotohp-cli verify-live-photos <upload-report.json> [flags]
```

| Option | Default | Description |
| --- | --- | --- |
| `--account string` |  | account to check (default: the selected account) |
| `--limit int` |  | verify only the first N pairs (0 means all) |
| `--log-file string` |  | write the run's debug log to PATH (default: a new gotohp-verify-live-photos-<time>-<pid>.log in the current directory) |
| `--max-retries int` | `3` | retries after the initial attempt for transient request failures (0 disables) |
| `--no-log-file` |  | write no debug log file |
| `--proxy string` |  | proxy URL for all requests |
| `--report-file string` |  | write the verification JSON report to PATH |
| `--requests-per-minute int` |  | cap Photos RPCs per minute across all workers (default 0: no cap; a 429 always pauses the run) |
| `--retry-429` | `true` | retry HTTP 429 responses (disable with --retry-429=false) |
| `--source-root string` |  | find the report's files under this directory instead of their recorded paths |
| `-t, --threads int` | `1` | number of concurrent verification workers |

## gotohp-cli version

Show version information

```text
gotohp-cli version
```

