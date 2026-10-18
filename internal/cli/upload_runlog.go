package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"app/core"
)

// addRunLogFlags adds the flags openRunLog reads.
func addRunLogFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("log-file", "", "write the run's debug log to PATH (default: a new gotohp-"+cmd.Name()+"-<time>-<pid>.log in the current directory)")
	f.Bool("no-log-file", false, "write no debug log file")
	cmd.MarkFlagsMutuallyExclusive("log-file", "no-log-file")
}

// defaultRunLogPath is where a run's debug log goes without --log-file: a
// new file in the current directory, named by command, start time and PID.
func defaultRunLogPath(command string, now time.Time) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("gotohp-%s-%s-%d.log", command, now.Format("20060102-150405"), os.Getpid())
	return filepath.Join(dir, name), nil
}

// runLog is a command's debug log: JSON lines at debug level, plus the
// stack trace of a crash (see openRunLog). Its own logger writes only to the
// file, so the command's events stay off stderr; core gets the combined one.
type runLog struct {
	*slog.Logger
	core *slog.Logger // stderr as the stderr logger decides, plus the file
	path string       // "" when there is no log file
	file *os.File
}

// openRunLog opens the debug log that --log-file and --no-log-file select.
// A crash
// (any goroutine's panic or a fatal runtime error) appends its plain-text
// stack trace to the file, after the JSON lines. If the file cannot be
// created the command continues without it, with a warning on stderr.
func openRunLog(f *pflag.FlagSet, command string, stderr *slog.Logger) *runLog {
	l := &runLog{Logger: slog.New(slog.DiscardHandler), core: stderr}
	if off, _ := f.GetBool("no-log-file"); off {
		return l
	}
	path, _ := f.GetString("log-file")
	if path == "" {
		var err error
		if path, err = defaultRunLogPath(command, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: no debug log for this run: %v\n", err)
			return l
		}
	}
	file, handler, err := createRunLog(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: no debug log for this run: %v\n", err)
		return l
	}
	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: crashes will not be recorded in the debug log: %v\n", err)
	}
	l.Logger = slog.New(handler)
	l.core = slog.New(slog.NewMultiHandler(stderr.Handler(), handler))
	l.path, l.file = path, file
	return l
}

// close stops crash recording and closes the file.
func (l *runLog) close() {
	if l.file == nil {
		return
	}
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	_ = l.file.Close()
}

// printPath tells the user where the log is.
func (l *runLog) printPath() {
	if l.path != "" {
		fmt.Fprintf(os.Stderr, "Debug log: %s\n", l.path)
	}
}

// warnings returns a writer that passes w's text through and also records
// each write in the log, for warnings printed before or outside the TUI.
func (l *runLog) warnings(w io.Writer) io.Writer { return logWriter{w: w, log: l.Logger} }

type logWriter struct {
	w   io.Writer
	log *slog.Logger
}

func (lw logWriter) Write(p []byte) (int, error) {
	if text := strings.TrimSpace(string(p)); text != "" {
		lw.log.Warn("stderr", "text", text)
	}
	return lw.w.Write(p)
}

// createRunLog creates the debug log file: JSON lines at debug level.
func createRunLog(path string) (*os.File, slog.Handler, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, err
	}
	return file, slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}), nil
}

// logRunStart records what the run was asked to do. Credentials are never
// part of UploadOptions, so nothing secret is logged.
func logRunStart(log *slog.Logger, version string, paths []string, opts core.UploadOptions) {
	log.Info("upload started",
		"version", version, "pid", os.Getpid(), "paths", paths,
		"account", opts.Api.Account, "saver", opts.Api.Saver, "use_quota", opts.Api.UseQuota,
		"threads", opts.Threads, "hash_threads", opts.HashThreads, "upload_threads", opts.UploadThreads,
		"recursive", opts.Recursive, "force", opts.ForceUpload, "delete", opts.DeleteFromHost,
		"stage_local", opts.StageLocal, "pair_live_photos", opts.PairLivePhotos,
		"update_existing_to_live", opts.UpdateExistingPhotosToLive,
		"state_file", opts.StateFile, "update_state", opts.UpdateState, "revalidate_state", opts.RevalidateState,
		"album", opts.AlbumName, "album_auto", opts.AlbumAutoMode)
}

// loggingReporter forwards every event to inner and records it in the log.
// Worker statuses are logged when a worker's stage, file or attempt changes,
// not on every byte-progress update, so the log shows how long each stage
// took on each worker without flooding.
type loggingReporter struct {
	inner core.UploadReporter
	log   *slog.Logger

	mu    sync.Mutex
	last  map[int]core.ThreadStatus
	since map[int]time.Time // when each worker's current stage began
}

func newLoggingReporter(inner core.UploadReporter, log *slog.Logger) *loggingReporter {
	return &loggingReporter{inner: inner, log: log, last: make(map[int]core.ThreadStatus), since: make(map[int]time.Time)}
}

func (r *loggingReporter) UploadStart(s core.UploadBatchStart) {
	r.log.Info("batch", "total", s.Total)
	r.inner.UploadStart(s)
}

func (r *loggingReporter) UploadStop() {
	r.log.Info("upload stopped")
	r.inner.UploadStop()
}

func (r *loggingReporter) TotalBytes(n int64) {
	r.log.Debug("total bytes", "bytes", n)
	r.inner.TotalBytes(n)
}

func (r *loggingReporter) TotalBytesDelta(n int64) { r.inner.TotalBytesDelta(n) }

func (r *loggingReporter) Warning(w core.PreflightWarning) {
	r.log.Warn("warning", "code", w.Code, "message", w.Message, "paths", w.Paths)
	r.inner.Warning(w)
}

func (r *loggingReporter) ThreadStatus(s core.ThreadStatus) {
	now := time.Now()
	r.mu.Lock()
	prev, seen := r.last[s.WorkerID]
	changed := !seen || prev.Status != s.Status || prev.FilePath != s.FilePath || prev.Attempt != s.Attempt
	r.last[s.WorkerID] = s
	started := r.since[s.WorkerID]
	if changed {
		r.since[s.WorkerID] = now
	}
	r.mu.Unlock()
	if changed {
		attrs := []any{"worker", s.WorkerID, "status", s.Status, "file", s.FilePath,
			"message", s.Message, "attempt", s.Attempt, "bytes_total", s.BytesTotal}
		if seen {
			// How long the stage that just ended took, e.g. hashing one file.
			attrs = append(attrs, "previous_status", prev.Status, "previous_file", prev.FilePath,
				"previous_ms", now.Sub(started).Milliseconds())
			if prev.BytesTotal > 0 {
				attrs = append(attrs, "previous_bytes", prev.BytesTotal)
			}
		}
		r.log.Debug("worker", attrs...)
	}
	r.inner.ThreadStatus(s)
}

func (r *loggingReporter) FileResult(res core.FileUploadResult) {
	switch {
	case res.IsError:
		r.log.Warn("file failed", "path", res.Path, "paths", res.Paths, "error", res.ErrorMessage)
	case res.Skipped:
		r.log.Info("file skipped", "path", res.Path, "code", res.SkipCode, "media_key", res.MediaKey)
	default:
		r.log.Info("file uploaded", "path", res.Path, "paths", res.Paths, "media_key", res.MediaKey)
	}
	r.inner.FileResult(res)
}

// QueueLength forwards to inner when it wants queue lengths.
func (r *loggingReporter) QueueLength(n int) {
	if qr, ok := r.inner.(core.QueueReporter); ok {
		qr.QueueLength(n)
	}
}

func (r *loggingReporter) AlbumProgress(s core.AlbumStatus) { r.inner.AlbumProgress(s) }

func (r *loggingReporter) AlbumComplete(s core.AlbumStatus) {
	r.log.Info("album complete", "album", fmt.Sprintf("%+v", s))
	r.inner.AlbumComplete(s)
}

func (r *loggingReporter) AlbumError(e core.AlbumError) {
	r.log.Warn("album failed", "album", e.AlbumName, "error", e.Error)
	r.inner.AlbumError(e)
}
