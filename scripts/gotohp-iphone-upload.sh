#!/usr/bin/env bash

set -u

usage() {
    cat <<'EOF'
Usage: gotohp-iphone-upload [--dry-run] [--account EMAIL] [--threads N] [--report-file PATH] [--state-file PATH] [--stage-local] PATH

Select Apple/iPhone camera images and Live Photo videos below PATH (usually a
mounted iPhone's DCIM folder), then upload them with gotohp-cli, pairing each
Live Photo. PNG, GIF, AAE, and non-iPhone JPG/JPEG files are excluded.

Ordinary iPhone videos are NOT uploaded: a MOV file is selected only if it is
the video half of a Live Photo (it carries a Live Photo content identifier).
Upload other videos separately.

Images whose EXIF data cannot be read, and videos that cannot be read, are
excluded too; they are counted and listed as unreadable, not as non-camera.

The selected files are passed to gotohp by their real paths, so reports and
the state file name the files themselves. Confirmed uploads are recorded in
the state file; running the same command again skips them (by path, size and
modification time) and retries only the rest, including files a canceled run
never started. To retry one folder, pass it as PATH, e.g. .../DCIM/101APPLE.

Next to the report it writes REPORT.selection.tsv (every file found, selected
or skipped, with the reason) and REPORT.log (gotohp's debug log).

Requires bash 4+, ImageMagick (identify), realpath, and gotohp-cli in PATH (or
set GOTOHP_CLI to its path or name). ffprobe (from ffmpeg) is strongly
recommended: without it every MOV is read in full to find Live Photo videos.

Options:
  -n, --dry-run    Print the selected files without uploading
  -a, --account EMAIL
                    Google account to upload to (default: the active one,
                    see gotohp-cli creds list); always printed before the run
  -t, --threads N  Upload worker count (default: 3)
      --report-file PATH
                    Save gotohp's final JSON report to PATH
                    (default: gotohp-iphone-<time>-<pid>.json in the
                    current directory)
      --state-file PATH
                    Record confirmed uploads in PATH and skip them on later runs
                    (default: ~/.local/state/gotohp/iphone-upload-state.json)
      --stage-local  Copy selected media to local temporary storage before upload
  -h, --help       Show this help
EOF
}

die() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

threads=3
account=''
dry_run=0
root_arg=''
report_file=''
state_file=''
stage_local=0

while (($# > 0)); do
    case "$1" in
        -n|--dry-run)
            dry_run=1
            shift
            ;;
        -a|--account)
            (($# >= 2)) || die "--account requires a value"
            account=$2
            shift 2
            ;;
        -t|--threads)
            (($# >= 2)) || die "--threads requires a value"
            threads=$2
            shift 2
            ;;
        --report-file)
            (($# >= 2)) || die "--report-file requires a value"
            report_file=$2
            shift 2
            ;;
        --state-file)
            (($# >= 2)) || die "--state-file requires a value"
            state_file=$2
            shift 2
            ;;
        --stage-local)
            stage_local=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        --)
            shift
            break
            ;;
        -*)
            die "unknown option: $1"
            ;;
        *)
            [[ -z "$root_arg" ]] || die "only one PATH is supported"
            root_arg=$1
            shift
            ;;
    esac
done

if [[ -z "$root_arg" && $# -gt 0 ]]; then
    root_arg=$1
    shift
fi

[[ -n "$root_arg" ]] || { usage >&2; exit 2; }
[[ $# -eq 0 ]] || die "only one PATH is supported"
[[ "$threads" =~ ^[1-9][0-9]*$ ]] || die "threads must be a positive integer"

command -v realpath >/dev/null 2>&1 || die "realpath is required"
command -v identify >/dev/null 2>&1 || die "ImageMagick identify is required"
# ffprobe reads only a video's metadata; without it, each MOV is read in
# full (strings + rg), which over ifuse means every ordinary video's bytes.
mov_check=ffprobe
slow_mov_warning=''
if ! command -v ffprobe >/dev/null 2>&1; then
    command -v strings >/dev/null 2>&1 || die "ffprobe, or strings and ripgrep (rg), is required"
    command -v rg >/dev/null 2>&1 || die "ffprobe, or strings and ripgrep (rg), is required"
    mov_check='strings+rg (whole file)'
    slow_mov_warning="ffprobe is not installed, so every MOV is read in full to find Live Photo videos. Over a phone mount (roughly 30-40 MB/s) a library with tens of GB of videos takes many minutes. Install ffmpeg, which provides ffprobe."
    printf 'Warning: %s\n' "$slow_mov_warning" >&2
fi
upload_tool=$(command -v -- "${GOTOHP_CLI:-gotohp-cli}") ||
    die "${GOTOHP_CLI:-gotohp-cli} is not in PATH (set GOTOHP_CLI to the gotohp CLI)"

# Name the account up front: a run must never land in another library just
# because a different account happens to be active.
if [[ -z "$account" ]]; then
    account=$("$upload_tool" creds list 2>/dev/null | sed -n 's/^ *\* *\([^ ]*@[^ ]*\) *$/\1/p' | head -n 1)
    [[ -n "$account" ]] || die "cannot tell which account is active; pass --account EMAIL (see: $upload_tool creds list)"
    printf 'Account: %s (the active account; choose another with --account)\n' "$account"
else
    # Fail now, not file by file after scanning the whole phone.
    "$upload_tool" creds list 2>/dev/null | grep -o '[^ ]*@[^ ]*' | grep -Fqx -- "$account" ||
        die "no stored account $account (see: $upload_tool creds list)"
    printf 'Account: %s\n' "$account"
fi

root=$(realpath -- "$root_arg") || die "cannot resolve PATH: $root_arg"
[[ -e "$root" ]] || die "PATH does not exist: $root"

image_files=()
video_files=()
skipped_png=0
skipped_gif=0
skipped_aae=0
skipped_other=0
# "reason<TAB>path" for every file not selected, for the selection record.
skipped_files=()

collect_candidate() {
    local file=$1
    local extension=${file##*.}
    extension=${extension,,}

    case "$extension" in
        heic|heif|jpg|jpeg)
            image_files+=("$file")
            ;;
        mov)
            video_files+=("$file")
            ;;
        png)
            ((skipped_png++))
            skipped_files+=("screenshot-png"$'\t'"$file")
            ;;
        gif)
            ((skipped_gif++))
            skipped_files+=("gif"$'\t'"$file")
            ;;
        aae)
            ((skipped_aae++))
            skipped_files+=("aae-sidecar"$'\t'"$file")
            ;;
        *)
            ((skipped_other++))
            skipped_files+=("unsupported-type"$'\t'"$file")
            ;;
    esac
}

if [[ -d "$root" ]]; then
    while IFS= read -r -d '' file; do
        collect_candidate "$file"
    done < <(find "$root" -type f -print0)
else
    collect_candidate "$root"
fi

selected_images=()
unreadable_images=()
unreadable_videos=()
batch_size=64
declare -A identified=()

for ((offset = 0; offset < ${#image_files[@]}; offset += batch_size)); do
    batch=( "${image_files[@]:offset:batch_size}" )
    # Split from the right: the file name itself may contain '|'.
    while IFS= read -r line; do
        model=${line##*|}
        rest=${line%|*}
        make=${rest##*|}
        file=${rest%|*}
        [[ -n "$file" && "$rest" == *'|'* ]] || continue
        # identify prints one line per frame; count each file once.
        [[ -z "${identified[$file]+present}" ]] || continue
        identified["$file"]=1
        if [[ "$make" == 'Apple' && "$model" == iPhone* ]]; then
            selected_images+=( "$file" )
        fi
    done < <(
        # -ping reads the attributes without decoding the image.
        identify -ping -format '%i|%[exif:Make]|%[exif:Model]\n' \
            "${batch[@]}" 2>/dev/null
    )
    # identify prints nothing for a file it cannot read.
    for file in "${batch[@]}"; do
        [[ -n "${identified[$file]+present}" ]] || unreadable_images+=( "$file" )
    done
done

selected_videos=()
for file in "${video_files[@]}"; do
    if command -v ffprobe >/dev/null 2>&1; then
        # The same QuickTime tag gotohp pairs Live Photos by.
        if identifier=$(ffprobe -v error -show_entries \
            format_tags=com.apple.quicktime.content.identifier \
            -of default=nw=1:nk=1 -- "$file" 2>/dev/null); then
            [[ -z "$identifier" ]] || selected_videos+=( "$file" )
        else
            unreadable_videos+=( "$file" )
        fi
    elif strings -a -- "$file" 2>/dev/null |
        rg -q 'com\.apple\.quicktime\.content\.identifier'; then
        selected_videos+=( "$file" )
    elif ((PIPESTATUS[0] != 0)); then
        unreadable_videos+=( "$file" )
    fi
done

selected=( "${selected_images[@]}" "${selected_videos[@]}" )

skipped_non_camera=$((
    ${#image_files[@]} - ${#selected_images[@]} - ${#unreadable_images[@]}
))
skipped_non_live_video=$((
    ${#video_files[@]} - ${#selected_videos[@]} - ${#unreadable_videos[@]}
))
skipped_unreadable=$((${#unreadable_images[@]} + ${#unreadable_videos[@]}))
skipped_total=$((
    skipped_png + skipped_gif + skipped_aae + skipped_other +
    skipped_non_camera + skipped_non_live_video + skipped_unreadable
))

print_summary() {
    printf 'Selected:\n'
    printf '  Apple camera images: %d\n' "${#selected_images[@]}"
    printf '  Live Photo videos: %d\n' "${#selected_videos[@]}"
    printf 'Skipped:\n'
    printf '  PNG/screenshots: %d\n' "$skipped_png"
    printf '  GIF files: %d\n' "$skipped_gif"
    printf '  AAE edit sidecars: %d\n' "$skipped_aae"
    printf '  Non-camera image files: %d\n' "$skipped_non_camera"
    printf '  Non-Live-Photo MOV files (ordinary videos): %d\n' "$skipped_non_live_video"
    printf '  Unreadable images: %d\n' "${#unreadable_images[@]}"
    printf '  Unreadable videos: %d\n' "${#unreadable_videos[@]}"
    printf '  Other unsupported files: %d\n' "$skipped_other"
    printf '  Total skipped: %d\n' "$skipped_total"
}
print_summary

if ((skipped_unreadable > 0)); then
    printf 'Unreadable files (excluded; check the mount and retry):\n' >&2
    printf '  %s\n' "${unreadable_images[@]}" "${unreadable_videos[@]}" >&2
fi

((${#selected[@]} > 0)) || die "no matching camera media found"

if ((dry_run)); then
    printf '%s\n' "${selected[@]}"
    [[ -z "$slow_mov_warning" ]] || printf 'Warning: %s\n' "$slow_mov_warning" >&2
    exit 0
fi

if [[ -z "$report_file" ]]; then
    # Like gotohp's own debug log: a new file in the current directory.
    report_file="$PWD/gotohp-iphone-$(date +%Y%m%d-%H%M%S)-$$.json"
else
    mkdir -p -- "$(dirname -- "$report_file")" || die "could not create directory for: $report_file"
fi

printf 'JSON report: %s\n' "$report_file"

# Record what was selected and why everything else was not, next to the
# report, so a missing photo can be explained after the run.
selection_file="${report_file%.json}.selection.tsv"
{
    declare -A chosen=() unreadable=()
    for file in "${selected[@]}"; do chosen["$file"]=1; done
    for file in "${unreadable_images[@]}" "${unreadable_videos[@]}"; do unreadable["$file"]=1; done
    printf '# gotohp-iphone-upload selection, %s\n' "$(date --iso-8601=seconds)"
    printf '# root: %s\n' "$root"
    printf '# account: %s\n' "$account"
    printf '# mov-check: %s\n' "$mov_check"
    [[ -z "$slow_mov_warning" ]] || printf '# warning: %s\n' "$slow_mov_warning"
    print_summary | sed 's/^/# /'
    printf '# status\treason\tpath\n'
    for file in "${selected_images[@]}"; do printf 'selected\tapple-camera-image\t%s\n' "$file"; done
    for file in "${selected_videos[@]}"; do printf 'selected\tlive-photo-video\t%s\n' "$file"; done
    for file in "${unreadable_images[@]}"; do printf 'skipped\tunreadable-image\t%s\n' "$file"; done
    for file in "${unreadable_videos[@]}"; do printf 'skipped\tunreadable-video\t%s\n' "$file"; done
    for file in "${image_files[@]}"; do
        [[ -n "${chosen[$file]+x}" || -n "${unreadable[$file]+x}" ]] || printf 'skipped\tnot-iphone-camera\t%s\n' "$file"
    done
    for file in "${video_files[@]}"; do
        [[ -n "${chosen[$file]+x}" || -n "${unreadable[$file]+x}" ]] || printf 'skipped\tnot-live-photo-video\t%s\n' "$file"
    done
    for entry in "${skipped_files[@]}"; do printf 'skipped\t%s\n' "$entry"; done
} > "$selection_file" || die "could not write selection record: $selection_file"
printf 'Selection record: %s\n' "$selection_file"

state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/gotohp"
if [[ -z "$state_file" ]]; then
    state_file="$state_dir/iphone-upload-state.json"
fi
mkdir -p -- "$(dirname -- "$state_file")" || die "could not create state directory for: $state_file"
printf 'State file: %s\n' "$state_file"

printf 'Starting gotohp upload to %s with %d thread(s)...\n' "$account" "$threads"
upload_args=(
    upload
    --account "$account"
    --pair-live-photos
    --threads "$threads"
    --report-file "$report_file"
    --log-file "${report_file%.json}.log"
    --state-file "$state_file"
    --update-state
)
if ((stage_local)); then
    upload_args+=(--stage-local)
fi
upload_args+=(-- "${selected[@]}")
# Fail clearly instead of with exec's "Argument list too long".
arg_bytes=0
for arg in "${upload_args[@]}"; do
    arg_bytes=$((arg_bytes + ${#arg} + 1))
done
if ((arg_bytes > 1536 * 1024)); then
    die "too many files for one command line ($arg_bytes bytes of paths); run once per folder, e.g. $root/100APPLE"
fi
status=0
"$upload_tool" "${upload_args[@]}" || status=$?
# Repeat it: gotohp's progress view covers what was printed before it.
[[ -z "$slow_mov_warning" ]] || printf 'Warning: %s\n' "$slow_mov_warning" >&2
exit "$status"
