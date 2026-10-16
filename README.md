# gotohp

![demo](readme_assets/app_demo.webp)

Unofficial Google Photos Desktop GUI Client

- Unlimited uploads (can be disabled)
- Drag-and-drop file upload interface
- Credential management
- Real-time upload progress tracking
- Configurable upload threads
- Individual files or directories uploads, with optional recursive scanning
- Skips files already present in your account
- CLI mode
- Configurable, persistent upload settings (stored in "%system config path%/gotohp/gotohp.config")  
   You can force local config by creating empty gotohp.config next to executable.

## [Download](https://github.com/xob0t/gotohp/releases/latest)

## CLI quick start

Download the `gotohp-cli` release artifact for your platform; it runs without the GUI. The GUI binary also runs these commands, e.g. `gotohp upload ...`.

```shell
# 1. Add your Google account (how to get the value: see "Sign in" below).
#    "-" reads it from standard input, so it stays out of shell history:
#    paste it, press Enter, then Ctrl+D.
gotohp-cli creds add -
gotohp-cli creds list
gotohp-cli creds set you@gmail.com        # pick the account, if you have several

# 2. Upload a folder (files already in your library are skipped)
gotohp-cli upload --recursive ~/Pictures

# 3. A large or mostly uploaded library: resumable, with a report
gotohp-cli upload --recursive --hash-threads 8 --upload-threads 2 \
  --state-file upload-state.json --update-state \
  --report-file upload-report.json ~/Pictures

# 4. Retry: run the same command again. Files the state file confirms are
#    skipped without being read; failures and files never started are retried.
#    Or retry only the failures listed in a report:
gotohp-cli upload --retry-report upload-report.json --report-file retry-report.json

# 5. A phone, network share or other unstable mount: copy each file locally first
gotohp-cli upload --recursive --stage-local --threads 1 \
  --state-file upload-state.json --update-state /mnt/phone/DCIM

# 6. Apple Live Photos: upload each photo + video as one Live Photo, then verify
gotohp-cli upload --recursive --pair-live-photos --report-file live.json ~/iPhone-export
gotohp-cli verify-live-photos live.json --report-file live-verification.json

# 7. iPhone camera roll on Linux (camera photos and Live Photos only)
scripts/gotohp-iphone-upload.sh --dry-run ~/mnt/iPhone/DCIM
scripts/gotohp-iphone-upload.sh --account you@gmail.com --threads 1 --stage-local ~/mnt/iPhone/DCIM

# 8. Something went wrong? Every run writes a debug log in the current directory
jq -c 'select(.level == "WARN" or .level == "ERROR")' gotohp-upload-*.log
```

When a run ends, the terminal shows a short summary (counts, failed files, report and log paths); pipe or redirect the output to get the full JSON summary instead. Ctrl+C once lets the files in progress finish and writes the report; press it again to stop at once.

More:

- [CLI guide](docs/cli-guide.md): how the options work — workers and threads, rate limits and retries, the state file, reports, Ctrl+C, debug logs, Live Photos.
- [Command reference](docs/cli-reference.md): every command and option with its default (also `gotohp-cli help` and `gotohp-cli upload --help`).
- [Uploading an iPhone camera roll](docs/iphone.md): the iPhone script, what it selects, and its files.

## Sign in

### Option 1 - Embedded setup

1. Open gotohp and click **Add Google account**.
2. Click **Open Google sign-in** and sign in with the account you want to add.
3. Sign in on the Google Embedded Setup page and click **I agree**. The page may keep loading forever; this is expected.
4. Open the browser developer tools. Under Application or Storage, open Cookies for `accounts.google.com`.
5. Copy only the value of the `oauth_token` cookie and paste it into gotohp.
6. Click **Connect account**.

From the CLI, open the same page yourself and run `gotohp-cli creds add "<oauth_token value>"`.

### Option 2 - ReVanced. No root required

<details>
  <summary><strong>Click to expand</strong></summary>

1. Install Google Photos ReVanced on your android device/emulator.
   - Install GmsCore [https://github.com/ReVanced/GmsCore/releases](https://github.com/ReVanced/GmsCore/releases)
   - Install patched apk [https://github.com/RookieEnough/Morphe-AutoBuilds/releases](https://github.com/RookieEnough/Morphe-AutoBuilds/releases) or patch it yourself
2. Connect the device to your PC via ADB.
3. Open the terminal on your PC and execute

   Windows

   ```cmd
   adb logcat | FINDSTR "auth%2Fphotos.native"
   ```

   Linux/Mac

   ```shell
   adb logcat | grep "auth%2Fphotos.native"
   ```

4. If you are already using ReVanced - remove Google Account from GmsCore.
5. Open Google Photos ReVanced on your device and log into your account.
6. One or more identical GmsCore logs should appear in the terminal.
7. Copy text from `androidId=` to the end of the line from any log.
8. That's it! 🎉

</details>

### Option 3 - Official apk. Root required

<details>
  <summary><strong>Click to expand</strong></summary>

1. Get a rooted android device or an emulator.
2. Connect the device to your PC via ADB.
3. Install [HTTP Toolkit](https://httptoolkit.com)
4. In HTTP Toolkit, select Intercept - `Android Device via ADB`. Filter traffic with

   ```text
   contains(https://www.googleapis.com/auth/photos.native)
   ```

   Or if you have an older version of Google Photos, try

   ```text
   contains(www.googleapis.com%2Fauth%2Fplus.photos.readwrite)
   ```

5. Open Google Photos app and login with your account.
6. A single request should appear.  
   Copy request body as text.
7. Add that credential string in gotohp.
8. If gotohp asks for a token binding key, keep the rooted device connected and click `Read from ADB`. gotohp will read the account's `lstBindingKeyAlias` from Android AccountManager and save it into the credential.

#### Troubleshooting

- **No Auth Request Intercepted**
  1. Log out of your Google account.
  2. Log in again.
  3. Try `Android App via Frida` interception method in HTTP Toolkit.
- **Token binding key not found**
  1. Make sure the same Google account is present on the connected device.
  2. Make sure root is available to ADB.

</details>

## Build

CLI only (no Wails or GUI dependencies):

```sh
go build -tags cli -o bin/gotohp-cli .
```

For a fork with an integration branch, `scripts/update-dev.sh` merges `origin/main` and the fork owner's open PRs targeting `main` into `dev`, tests the CLI, and atomically replaces `bin/gotohp-cli` from that branch only. Requires authenticated `gh`, Git, Go, and Bash; configure `origin` and `gh repo set-default` for your fork. It never stages files or resets branches. Tracked edits or merge conflicts stop the update; resolve conflicts on `dev` and rerun. `--background` skips dirty checkouts and branches other than `dev`. A local scheduler can call it periodically. Untracked personal notes remain untouched.

The `Sync dev integration branch` workflow keeps GitHub's `dev` updated when owner PRs change, once the workflow is merged into the default branch. Conflicts fail visibly rather than discard work. Previously integrated closed PRs are not automatically reverted. Local binaries are updated by the script, not by GitHub Actions.

For GUI builds, follow the official [Wails 3 installation guide](https://v3.wails.io/getting-started/installation/).
