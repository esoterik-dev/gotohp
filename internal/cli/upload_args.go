package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"app/core"
)

func errorsAs(err error, target *exitError) bool { return errors.As(err, target) }

func newUploadCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload <path> [<path> ...]",
		Short: "Upload files or directories",
		Long: "Upload files or directories to Google Photos.\n\n" +
			"The config file only supplies credentials. Everything else, including the\n" +
			"proxy and quality settings, is controlled by flags; GUI preferences do not apply.",
		Args: uploadArgs,
		RunE: runUploadCommand,
	}
	f := cmd.Flags()
	f.SortFlags = false
	f.String("account", "", "account email to upload with (default: the selected account)")
	f.String("proxy", "", "proxy URL for all requests")
	f.Bool("use-quota", false, "count uploads against the account's storage quota")
	f.Bool("saver", false, "upload in storage saver quality")
	f.BoolP("recursive", "r", false, "include subdirectories")
	f.IntP("threads", "t", core.DefaultPreferences.UploadThreads, "workers; each hashes, checks and uploads its own files (see --hash-threads)")
	f.Int("hash-threads", 0, "split the work: workers that hash, stage and check for duplicates (default: --threads)")
	f.Int("upload-threads", 0, "split the work: workers that transfer and commit new files (default: --threads)")
	addRequestPolicyFlags(f)
	f.BoolP("force", "f", false, "upload even if the file already exists in the library")
	f.BoolP("delete", "d", false, "delete from host after upload")
	f.Bool("disable-filter", false, "disable file type filtering")
	f.Bool("date-from-filename", false, "set media date from filename (e.g. 20240709_182027.jpg)")
	f.Bool("stage-local", false, "copy each file to TMPDIR before hashing and uploading")
	f.StringP("exclude", "e", "", "exclude directories whose name matches pattern (e.g. @eaDir)")
	f.StringP("album", "a", "", "add uploads to album (creates if needed); 'AUTO' creates albums from folder names")
	f.Bool("pair-live-photos", false, "pair Apple Live Photo files; incomplete pairs are skipped")
	f.Bool("upload-incomplete-live-photos", false, "upload an unmatched Live Photo member as a single file")
	f.Bool("update-existing-photos-to-live", false, "attach matching MOV files to existing photos")
	f.Bool("ignore-apple-metadata", false, "match Live Photo pairs by filename stem instead of Apple metadata")
	f.StringP("log-level", "l", "info", "log level: debug, info, warn, error")
	f.Bool("no-tui", false, "disable the interactive progress UI")
	f.String("report-file", "", "write the final JSON summary to PATH")
	f.String("retry-report", "", "retry only failed, non-skipped files from a JSON report; no positional paths")

	// Accepted for compatibility; skipping incomplete pairs is already the default.
	f.Bool("skip-incomplete-live-photos", false, "")
	_ = f.MarkHidden("skip-incomplete-live-photos")
	cmd.MarkFlagsMutuallyExclusive("skip-incomplete-live-photos", "upload-incomplete-live-photos")
	return cmd
}

// uploadRunSettings holds CLI-only settings that are not part of UploadOptions.
type uploadRunSettings struct {
	logLevel   string
	noTUI      bool
	reportFile string
}

func runUploadCommand(cmd *cobra.Command, paths []string) error {
	retry, _ := cmd.Flags().GetString("retry-report")
	if retry != "" {
		var err error
		pair, _ := cmd.Flags().GetBool("pair-live-photos")
		paths, err = retryReportPaths(retry, pair, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		paths = existingRetryPaths(paths, cmd.ErrOrStderr())
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("invalid upload path %q: %w", path, err)
		}
	}
	f := cmd.Flags()
	if threads, _ := f.GetInt("threads"); threads < 1 {
		return fmt.Errorf("threads must be a positive integer, got %d", threads)
	}

	// Like rclone, the config file holds only the account; every upload option
	// is per-invocation so a scripted run behaves the same regardless of GUI state.
	configPath, _ := f.GetString("config")
	if err := core.LoadConfig(configPath); err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	opts := uploadOptionsFromFlags(f)
	if err := validateUploadOptions(f, opts); err != nil {
		return err
	}
	policy, err := requestPolicyFromFlags(f)
	if err != nil {
		return err
	}
	opts.Api.Policy = policy

	if retry != "" && len(paths) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "No failed non-skipped files to retry")
		// Still write the (empty) summary, so --report-file is never stale.
		model := initialModel()
		model.workingDir, _ = os.Getwd()
		reportFile, _ := f.GetString("report-file")
		return finishUpload(model, cmd.OutOrStdout(), reportFile)
	}

	logLevel, _ := f.GetString("log-level")
	noTUI, _ := f.GetBool("no-tui")
	reportFile, _ := f.GetString("report-file")
	return runUpload(paths, opts, uploadRunSettings{
		logLevel:   logLevel,
		noTUI:      noTUI,
		reportFile: reportFile,
	})
}

// uploadOptionsFromFlags builds run options from defaults plus the parsed flags.
func uploadOptionsFromFlags(f *pflag.FlagSet) core.UploadOptions {
	opts := core.DefaultPreferences.UploadOptions()
	getBool := func(name string) bool { v, _ := f.GetBool(name); return v }

	opts.Api.Account, _ = f.GetString("account")
	opts.Api.Proxy, _ = f.GetString("proxy")
	opts.Api.UseQuota = getBool("use-quota")
	opts.Api.Saver = getBool("saver")

	opts.Recursive = getBool("recursive")
	opts.ForceUpload = getBool("force")
	opts.DeleteFromHost = getBool("delete")
	opts.DisableUnsupportedFilesFilter = getBool("disable-filter")
	opts.SetDateFromFilename = getBool("date-from-filename")
	opts.StageLocal = getBool("stage-local")
	opts.PairLivePhotos = getBool("pair-live-photos")
	opts.SkipIncompleteLivePhotos = !getBool("upload-incomplete-live-photos")
	opts.UpdateExistingPhotosToLive = getBool("update-existing-photos-to-live")
	opts.IgnoreAppleMetadata = getBool("ignore-apple-metadata")
	opts.Threads, _ = f.GetInt("threads")
	opts.HashThreads, _ = f.GetInt("hash-threads")
	opts.UploadThreads, _ = f.GetInt("upload-threads")
	opts.ExcludePattern, _ = f.GetString("exclude")

	album, _ := f.GetString("album")
	if strings.EqualFold(album, "AUTO") {
		opts.AlbumAutoMode = true
		opts.AlbumName = ""
	} else {
		opts.AlbumAutoMode = false
		opts.AlbumName = album
	}
	return opts
}

// addRequestPolicyFlags adds the flags requestPolicyFromFlags reads.
func addRequestPolicyFlags(f *pflag.FlagSet) {
	f.Bool("retry-429", true, "retry HTTP 429 responses (disable with --retry-429=false)")
	f.Int("max-retries", core.DefaultMaxRetries, "retries after the initial attempt for transient request failures (0 disables)")
	f.Int("requests-per-minute", core.DefaultRequestsPerMinute, "cap Photos RPCs per minute across all workers (default 0: no cap; a 429 always pauses the run)")
}

// requestPolicyFromFlags builds the run's shared request policy. The CLI always
// sets one; library callers that pass none keep unpaced default retries.
func requestPolicyFromFlags(f *pflag.FlagSet) (*core.RequestPolicy, error) {
	retry429, _ := f.GetBool("retry-429")
	maxRetries, _ := f.GetInt("max-retries")
	requestsPerMinute, _ := f.GetInt("requests-per-minute")
	return core.NewRequestPolicy(maxRetries, retry429, requestsPerMinute)
}

// validateUploadOptions rejects flags that only make sense with Live Photo
// pairing enabled.
func validateUploadOptions(f *pflag.FlagSet, opts core.UploadOptions) error {
	if err := validateReportFile(f); err != nil {
		return err
	}
	for _, name := range []string{"hash-threads", "upload-threads"} {
		if n, _ := f.GetInt(name); f.Changed(name) && n < 1 {
			return fmt.Errorf("--%s must be positive", name)
		}
	}
	if opts.PairLivePhotos {
		return nil
	}
	for _, name := range []string{
		"update-existing-photos-to-live",
		"ignore-apple-metadata",
		"skip-incomplete-live-photos",
		"upload-incomplete-live-photos",
	} {
		if f.Changed(name) {
			return fmt.Errorf("--%s requires --pair-live-photos", name)
		}
	}
	return nil
}
