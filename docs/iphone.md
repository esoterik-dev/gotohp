# Uploading an iPhone camera roll (Linux)

`scripts/gotohp-iphone-upload.sh` uploads the camera photos and Live Photos from a mounted iPhone with `gotohp-cli`. It picks the files, then runs one `gotohp-cli upload --pair-live-photos` over them, so each Live Photo is stored as one linked item.

Use it when you want **only camera media**. To back up everything on the phone (screenshots, saved images, ordinary videos too), run `gotohp-cli upload --recursive --pair-live-photos` on the DCIM folder directly instead; see the [quick start](../README.md#cli-quick-start).

## What it selects

| File | Selected? |
| --- | --- |
| HEIC/HEIF/JPG whose EXIF says `Make: Apple`, `Model: iPhone…` | yes |
| MOV carrying a Live Photo identifier (the video half of a Live Photo) | yes |
| PNG (screenshots), GIF, AAE edit sidecars | no |
| JPG without iPhone camera EXIF (saved, downloaded, messaging apps) | no |
| Ordinary MOV videos, MP4, other types | no — upload these separately |
| Files whose metadata cannot be read | no — listed as unreadable; check the mount and rerun |

Selected photos and videos are then paired by gotohp. A file that cannot be paired safely (its partner is missing, its metadata is unreadable, or several files share one Live Photo identifier) is skipped with a warning rather than uploaded on its own; see [Live Photos](cli-guide.md#live-photos).

## Requirements

- Linux with the iPhone mounted, e.g. with `ifuse` (`libimobiledevice`); the phone must be unlocked and trusted.
- bash 4+, `realpath`, ImageMagick (`identify`).
- `ffprobe` (from ffmpeg), strongly recommended. It reads only each video's metadata. Without it, the script falls back to reading every MOV in full to find the Live Photo marker, which over a phone mount takes many minutes for a large library; the script prints a warning before and after the upload and records it in the selection file.
- `gotohp-cli` in `PATH`, or `GOTOHP_CLI` set to its name or path.

## Commands

```shell
# 1. Mount the phone (example with ifuse) and check it
mkdir -p ~/mnt/iPhone && ifuse ~/mnt/iPhone
ls ~/mnt/iPhone/DCIM

# 2. Preview: counts and the selected files, nothing is uploaded
scripts/gotohp-iphone-upload.sh --dry-run ~/mnt/iPhone/DCIM

# 3. Upload (one thread and local staging suit a phone mount). Name the
#    account, so the run never depends on which one happens to be active.
gotohp-cli creds list
scripts/gotohp-iphone-upload.sh --account you@gmail.com --threads 1 --stage-local ~/mnt/iPhone/DCIM

# 4. Retry: run the same command again. Files already confirmed are skipped
#    without being read; failures and files a canceled run never started are retried.
scripts/gotohp-iphone-upload.sh --account you@gmail.com --threads 1 --stage-local ~/mnt/iPhone/DCIM

# One folder only
scripts/gotohp-iphone-upload.sh --account you@gmail.com --threads 1 --stage-local ~/mnt/iPhone/DCIM/101APPLE

# 5. Check the Live Photos of a run (read-only: hash lookups, no downloads)
gotohp-cli verify-live-photos --account you@gmail.com REPORT.json --report-file REPORT.verify.json
```

## Options

| Option | Meaning |
| --- | --- |
| `-n`, `--dry-run` | Print the counts and the selected files; upload nothing. |
| `-a`, `--account EMAIL` | Google account to upload to (default: the active one, `gotohp-cli creds list`). Printed before the run and recorded in the selection file; an account that is not stored stops the script at once. |
| `-t`, `--threads N` | gotohp workers (default 3). Use 1 on a phone mount. |
| `--report-file PATH` | gotohp's JSON report (default: `gotohp-iphone-<time>-<pid>.json` in the current directory). |
| `--state-file PATH` | Confirmed uploads, reused by later runs (default: `~/.local/state/gotohp/iphone-upload-state.json`). |
| `--stage-local` | Copy each file to local temporary storage before hashing and uploading (`gotohp-cli upload --stage-local`). |

`GOTOHP_CLI` selects the gotohp CLI to run (default `gotohp-cli`).

## Files it writes

Next to the report (`REPORT.json`):

- `REPORT.selection.tsv` — every file found under PATH as `selected` or `skipped`, with the reason (`apple-camera-image`, `live-photo-video`, `screenshot-png`, `gif`, `aae-sidecar`, `not-iphone-camera`, `not-live-photo-video`, `unreadable-image`, `unreadable-video`, `unsupported-type`). Its header has the counts and how MOVs were checked.
- `REPORT.log` — gotohp's [debug log](cli-guide.md#debug-logs) for the run.

Why was a photo not uploaded?

```shell
grep IMG_0123 REPORT.selection.tsv           # skipped by the script, and why
jq '.results[] | select(.path | test("IMG_0123"))' REPORT.json   # gotohp's result
jq -c 'select((.file // .path // "") | test("IMG_0123"))' REPORT.log
```

## Notes

- Files are passed to gotohp by their real paths, so reports and the state file name the files on the phone and later runs match them.
- About 30,000 files fit on one command line; beyond that the script stops and asks you to run once per folder.
- A Live Photo whose still is already in the library on its own is skipped, not uploaded again: its video cannot be attached to it by a normal upload. `gotohp-cli upload --pair-live-photos --update-existing-photos-to-live` repairs such an item.
