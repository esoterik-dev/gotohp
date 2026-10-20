package cli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"app/core"
)

// defaultRunLogPath is where a run's debug log goes without --log-file:
// $XDG_STATE_HOME/gotohp/logs (default ~/.local/state/gotohp/logs), or the
// user cache directory on Windows.
func defaultRunLogPath(now time.Time) (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" && runtime.GOOS == "windows" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = cache
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	name := fmt.Sprintf("upload-%s-%d.log", now.Format("20060102-150405"), os.Getpid())
	return filepath.Join(dir, "gotohp", "logs", name), nil
}

// openRunLog creates the run's debug log: JSON lines at debug level.
func openRunLog(path string) (*os.File, slog.Handler, error) {
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

	mu   sync.Mutex
	last map[int]core.ThreadStatus
}

func newLoggingReporter(inner core.UploadReporter, log *slog.Logger) *loggingReporter {
	return &loggingReporter{inner: inner, log: log, last: make(map[int]core.ThreadStatus)}
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
	r.mu.Lock()
	prev, seen := r.last[s.WorkerID]
	changed := !seen || prev.Status != s.Status || prev.FilePath != s.FilePath || prev.Attempt != s.Attempt
	r.last[s.WorkerID] = s
	r.mu.Unlock()
	if changed {
		r.log.Debug("worker", "worker", s.WorkerID, "status", s.Status, "file", s.FilePath,
			"message", s.Message, "attempt", s.Attempt, "bytes_total", s.BytesTotal)
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

func (r *loggingReporter) AlbumProgress(s core.AlbumStatus) { r.inner.AlbumProgress(s) }

func (r *loggingReporter) AlbumComplete(s core.AlbumStatus) {
	r.log.Info("album complete", "album", fmt.Sprintf("%+v", s))
	r.inner.AlbumComplete(s)
}

func (r *loggingReporter) AlbumError(e core.AlbumError) {
	r.log.Warn("album failed", "album", e.AlbumName, "error", e.Error)
	r.inner.AlbumError(e)
}
