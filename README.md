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

## CLI usage

Download the `gotohp-cli` release artifact for your platform. It runs without the GUI.

```shell
gotohp-cli upload /path/to/photos --recursive --threads 5
gotohp-cli upload /path/to/photos --recursive --report-file upload-report.json
gotohp-cli creds add "<oauth_token cookie value>"   # Embedded Setup sign-in, see below
gotohp-cli creds add "androidId=..."                 # or a raw credential string
gotohp-cli creds add - < token.txt                   # or read either from stdin
gotohp-cli creds set user@gmail.com
```

The CLI shares the config file with the GUI, but reads only the stored credentials from it. Everything else (proxy, quality, upload behaviour) is set by flags, so a scripted run is unaffected by preferences changed in the GUI. Pass `--config <path>` to use a different config file and `--account <email>` to upload with a specific stored account.

`--report-file <path>` writes the final JSON summary to a file while keeping the interactive progress UI visible when the command is run in a terminal.

The first Ctrl+C (or SIGTERM) starts no new files and lets in-progress files finish, then writes the report and exits with status 130 (143 for SIGTERM). Albums are still created for the files that finished. A second Ctrl+C aborts in-progress files and quits at once, keeping the results that already arrived.

`--stage-local` copies each upload item to local temporary storage before hashing and uploading. Use it for FUSE, device, or unstable mounts; temporary copies are removed when the item finishes, and on a force quit (second Ctrl+C). Copies go to `$TMPDIR` (default `/tmp`), which is RAM-backed tmpfs on many Linux distributions, so with several threads and large videos point it at a disk, e.g. `TMPDIR=/var/tmp gotohp-cli upload --stage-local ...`. With `--delete`, a source that changed after it was staged is kept.

Requests are not paced by default: workers go as fast as they can. HTTP 429 responses pause the whole run, honoring `Retry-After` or waiting at least one minute when absent, then workers resume spread over about two seconds and retry the same request. A commit that failed is looked up in the library before it is retried or reported, since a 429 or 5xx can arrive after the item was stored. `--retry-429=false` disables 429 retries; `--max-retries 0` disables all request retries. The default is three retries after the initial attempt, independently for each operation. A failed duplicate check fails the file instead of uploading it, so it can be retried without risking a duplicate. `--requests-per-minute N` adds a cap shared by all workers, if you prefer to stay below the quota rather than react to it. Separate CLI processes do not share limits or pauses. The GUI does not pace requests or share pauses, but follows the same retry rules and also fails a file whose duplicate check fails.

Run `gotohp-cli help` or `gotohp-cli upload --help` for all commands and options.

`gotohp-cli upload --retry-report upload-report.json --threads 1` retries only failed, non-skipped paths, without rescanning a directory. Do not supply positional paths with this flag. Repeat the original account, quality, date, album, and pairing/repair flags as needed; a report is not a configuration file. Two-file failures require `--pair-live-photos`. Album-only failures need separate album repair and are not silently treated as successful file retries. Relative paths resolve against the directory the original upload ran in. A canceled run's report can be retried, but items that never started are not in it; rerun the original sources to include them; a report from a run canceled before any file started is rejected for the same reason. Files that no longer exist are skipped with a warning. With nothing left to retry, the command still writes an empty `--report-file` and exits 0. Keep each run's report under a new filename.

The GUI binary runs the same CLI when given a command, e.g. `gotohp upload ...`.

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

Follow official wails3 guide
[https://v3.wails.io/getting-started/installation/](https://v3.wails.io/getting-started/installation/)
