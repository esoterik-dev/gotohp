# gotohp-cli guide

How the CLI's options behave and work together. For copy-paste commands see the [quick start](../README.md#cli-quick-start); for every option with its default see the [command reference](cli-reference.md).

- [Config and accounts](#config-and-accounts)
- [Workers and threads](#workers-and-threads)
- [Rate limits and retries](#rate-limits-and-retries)
- [Unstable sources: `--stage-local`](#unstable-sources---stage-local)
- [Resuming large imports: the state file](#resuming-large-imports-the-state-file)
- [Reports and retrying failures](#reports-and-retrying-failures)
- [Ctrl+C](#ctrlc)
- [Debug logs](#debug-logs)
- [Live Photos](#live-photos)
- [Verifying Live Photos](#verifying-live-photos)

## Config and accounts

The CLI shares the config file with the GUI, but reads only the stored credentials from it. Everything else (proxy, quality, upload behaviour) is set by flags, so a scripted run is unaffected by preferences changed in the GUI. Pass `--config <path>` to use a different config file and `--account <email>` to upload with a specific stored account.

The GUI binary runs the same CLI when given a command, e.g. `gotohp upload ...`.

## Workers and threads

`--threads N` runs N workers, each hashing, checking and uploading its own files; the GUI's thread setting works the same way. Setting `--hash-threads` or `--upload-threads` splits the work into two pools instead (the other one defaults to `--threads`):

- **Hash workers** make the `--stage-local` copies, hash each file, and check whether each file or Live Photo pair is already in the library, so a mostly uploaded library is paced by `--hash-threads`. Duplicate checks that several hash workers make at the same time are sent together in one request.
- **Upload workers** receive only new files, and handle upload sessions and commits. With `--update-existing-photos-to-live`, a pair whose still photo is already in the library is queued too, so an upload worker can turn it into a Live Photo.

Without `--stage-local`, hash workers hand new files to the queue and keep checking ahead, so duplicates never wait behind uploads. With `--stage-local` each queued file holds a temporary copy, so the queue is bounded and a hash worker waiting for an upload worker shows `queued`. Use the split for libraries that are mostly uploaded already, e.g. `--hash-threads 8 --upload-threads 2`. Thread counts do not replace API rate limiting.

Without `--stage-local`, a file that changes after it was hashed is rejected; rerun it once the source is stable. With `--stage-local`, the staged copy is uploaded anyway and only `--delete` keeps the changed source. Staging happens on the hash workers, so up to about `--hash-threads` + 2 × `--upload-threads` copies can exist at once (one per hash worker, plus queued and uploading files); with `--threads N` alone, up to N.

## Rate limits and retries

Requests are not paced by default: workers go as fast as they can. HTTP 429 responses pause the whole run, honoring `Retry-After` or waiting at least one minute when absent, then workers resume spread over about two seconds and retry the same request. After a pause, Photos requests run at half the number sent in the last minute (at least 30 a minute; early in a run this is slower than the rate that caused the 429) and speed up again by a fifth every 50 successful requests, so the run does not hit the quota again right away; with fewer than 20 requests in the last minute the 429 is not ours and only the pause applies. The progress view shows such a pause with a countdown. With `--hash-threads` or `--upload-threads` and without `--stage-local`, it also shows how many hashed files are waiting for an upload worker; with `--stage-local` at most one file per upload worker waits, and that is not shown.

A commit that failed is looked up in the library before it is retried or reported, since a 429 or 5xx can arrive after the item was stored. `--retry-429=false` disables 429 retries; `--max-retries 0` disables all request retries. The default is three retries after the initial attempt, independently for each operation. A failed duplicate check fails the file instead of uploading it, so it can be retried without risking a duplicate.

`--requests-per-minute N` adds a cap shared by all workers, if you prefer to stay below the quota rather than react to it. Separate CLI processes do not share limits or pauses. The GUI does not pace requests or share pauses, but follows the same retry rules and also fails a file whose duplicate check fails.

## Unstable sources: `--stage-local`

`--stage-local` copies each upload item to local temporary storage before hashing and uploading. Use it for FUSE, device, or unstable mounts; temporary copies are removed when the item finishes, and on a force quit (second Ctrl+C). Copies go to `$TMPDIR` (default `/tmp`), which is RAM-backed tmpfs on many Linux distributions, so with several threads and large videos point it at a disk, e.g. `TMPDIR=/var/tmp gotohp-cli upload --stage-local ...`. With `--delete`, a source that changed after it was staged is kept.

## Resuming large imports: the state file

To resume large imports, use `gotohp-cli upload /path/to/photos --recursive --state-file upload-state.json --update-state`. Supplying only `--state-file` reads an existing file without modifying it. Confirmations are scoped to the selected account, upload policy, logical media type, and paths; unchanged path/size/mtime records skip the item without rereading or hashing it. Changed files are hashed again. This metadata shortcut assumes timestamps are trustworthy; use `--revalidate-state` to ignore cached confirmations and recheck the server, especially after remote deletions.

The state file is an append-only log: each confirmed item appends and syncs one line, and opening it with `--update-state` compacts it. One writer at a time holds an OS lock on `upload-state.json.lock`; the lock is released when the process exits, even after a crash, so the leftover file is harmless. Failed uploads and incomplete Live Photo skips are not saved. Cache-only hits never delete originals. With `--delete` and `--update-state`, each item's confirmation is saved before its sources are deleted; keep independent backups. If an item is uploaded but its confirmation cannot be saved (for example, with `--stage-local` its source changed after it was copied), it is reported as uploaded with a `state-checkpoint-failed` warning, which does not change the exit status, and its sources are kept. State covers media uploads, not album completion: use an existing album key when resuming album work, rather than repeatedly creating an album by name.

With a state file, rerunning the original command is the simplest retry: confirmed files are skipped without being read, and everything else, including files a canceled run never started, is tried again.

## Reports and retrying failures

When a run ends, a terminal gets a short summary: how many items were uploaded, already in the library, confirmed earlier, skipped and failed; the failed files with their errors and the files skipped for a reason that may need action (up to 20 each); and where the report and the debug log are. When the output is piped or redirected (`| jq`, `> run.json`), it is the full JSON summary instead, as scripts expect.

`--report-file <path>` writes the full JSON summary to a file in either case, while keeping the interactive progress UI visible when the command is run in a terminal. It never replaces the config, the `--retry-report` or `--state-file` files, or a photo or video to upload (with `--disable-filter`, any file it uploads); that is refused before the upload starts. Keep each run's report under a new filename.

`gotohp-cli upload --retry-report upload-report.json --threads 1` retries only failed, non-skipped paths, without rescanning a directory. Do not supply positional paths with this flag. Repeat the original account, quality, date, album, and pairing/repair flags as needed; a report is not a configuration file. Two-file failures require `--pair-live-photos`. Album-only failures need separate album repair and are not silently treated as successful file retries. Relative paths resolve against the directory the original upload ran in. A canceled run's report can be retried, but items that never started are not in it; rerun the original sources to include them; a report from a run canceled before any file started is rejected for the same reason. Files that no longer exist are skipped with a warning. With nothing left to retry, the command still writes an empty `--report-file` and exits 0.

## Ctrl+C

The first Ctrl+C (or SIGTERM) starts no new files and lets in-progress files finish, then writes the report and exits with status 130 (143 for SIGTERM). Albums are still created for the files that finished. A second Ctrl+C aborts in-progress files and quits at once, keeping the results that already arrived. It creates no album, so files that finished are not added to `--album`; the report says so.

Files that never started are left out of the report. Files already hashed but still waiting in the queue for an upload worker count as not started: they are not uploaded and not reported, so rerun the sources (with the state file) to upload them.

## Debug logs

Every upload run also writes a debug log, as JSON lines, to a new `gotohp-upload-<time>-<pid>.log` in the current directory; `--log-file PATH` writes it to PATH instead, and `--no-log-file` turns it off. The log starts before anything else, so it also records the warnings and the error of a run that stops before uploading; its lines wait in memory until the log's path has been checked (see below) and are then written to the file. The run never uploads its own log, even when the current directory is an upload folder. It records the run's options, each worker's stage changes (hashing, checking, uploading, committing) with how long the previous stage took, every request with its status and duration, each batched duplicate check with how many hashes it carried, waits caused by a 429 pause and how long they last, retries with the server's error response, Ctrl+C, and each file's result. Once the file is written, if the program crashes, the crash's stack trace is appended to the log as plain text after the JSON lines. Credentials are not logged. These events go only to the log; stderr without the TUI shows the same messages as before, filtered by `--log-level`. `--log-file` refuses, before anything is written to it, the config, the report, the state file and its lock, a photo or video to upload (with `--disable-filter`, any file it uploads), and a directory; any other existing file, such as an earlier log, is replaced. If the run stops before that check, the log is written only to a new file, never over an existing one. The path is printed at the end of the run and stored as `logFile` in the JSON summary. Logs are never deleted automatically. `verify-live-photos` writes the same kind of log (`gotohp-verify-live-photos-<time>-<pid>.log`), with every pair's result, and takes the same two flags; its `--log-file` refuses the upload report, the config, its `--report-file`, the photos and videos it reads, and a directory.

Useful queries (with `jq`):

```shell
jq -c 'select(.level == "WARN" or .level == "ERROR")' run.log                # why a run paused or failed
jq -r 'select(.msg == "request") | [.duration_ms, (.status // .error), .path] | @tsv' run.log | sort -rn | head   # slowest requests
jq -c 'select((.file // .path // "") | test("IMG_0123"))' run.log            # everything about one file
jq -r 'select(.previous_status == "hashing") | [.previous_ms, .previous_file] | @tsv' run.log | sort -rn | head  # slowest hashing
jq -c 'select(.msg == "hash lookup batch")' run.log                           # batched duplicate checks
```

## Live Photos

With `--pair-live-photos`, a photo and video sharing an Apple Live Photo identifier are uploaded together as one linked Live Photo, in a single commit. A file that may be half of a Live Photo but cannot be paired safely is not uploaded on its own: a still uploaded without its video cannot be linked to it later. A photo or video whose partner is not in the run, a video without the still-image-time marker, and several photos or videos sharing one identifier are skipped with a warning and listed as skipped in the report. A photo or video whose metadata cannot be read (for example on a flaky phone mount) is listed as failed instead, so the run exits with an error and `--retry-report` tries it again. Its partner, if it was in the run, was skipped as incomplete and is not part of that retry; to upload the pair, rerun the original sources, with `--state-file` to skip files already uploaded. `--upload-incomplete-live-photos` uploads such files as single files instead.

A pair whose still is already in the library is skipped. `--update-existing-photos-to-live` attaches the video to such an existing photo instead. `--ignore-apple-metadata` pairs by filename stem rather than by the embedded identifier; a video with neither a Live Photo identifier nor the still-image-time marker is then uploaded as an ordinary video unless a photo shares its filename stem.

For an iPhone camera roll, see [Uploading an iPhone camera roll](iphone.md).

## Verifying Live Photos

For a report containing Apple Live Photo pairs, `gotohp-cli verify-live-photos upload-report.json --threads 1 --report-file verification.json` compares both local hashes with remote metadata, without downloading media. Its `--report-file` must not be the upload report, the config, or one of the photos and videos it reads. Files are read from the paths in the report; if they have moved, `--source-root /path/to/DCIM` finds each by name under the new root and reports names it cannot tell apart as ambiguous. Unresolved pairs return a nonzero exit status, and Ctrl+C writes a partial report. It takes the same `--max-retries`, `--retry-429` and `--requests-per-minute` flags as `upload`, and a 429 pauses all its workers. This checks shared remote keys, not playback; spot-check important pairs in Google Photos.
