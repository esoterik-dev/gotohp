package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// FilesDroppedEvent is emitted when files are dropped on any drop zone
type FilesDroppedEvent struct {
	Files    []string `json:"files"`
	DropZone string   `json:"dropZone"`
}

// StartUploadEvent is received from frontend to start upload
type StartUploadEvent struct {
	Files []string `json:"files"`
}

type UploadManager struct {
	mu sync.Mutex
	wg sync.WaitGroup
	// stop ends the run gracefully: nothing new starts and in-flight items
	// finish. cancel also aborts in-flight items; it implies stop.
	stop     chan struct{}
	stopped  bool
	cancel   chan struct{}
	canceled bool
	running  bool
	reporter UploadReporter
	logger   *slog.Logger
	// apiOptions is captured from the options of the run in progress.
	apiOptions ApiOptions
	// newAPI creates the clients an upload run uses; tests swap in a fake.
	newAPI func(ApiOptions) (uploadAPI, error)
}

// NewUploadManager creates a manager that reports progress to reporter. A nil
// reporter or logger discards output.
func NewUploadManager(reporter UploadReporter, logger *slog.Logger) *UploadManager {
	if reporter == nil {
		reporter = NopReporter{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &UploadManager{
		reporter: reporter,
		logger:   logger,
		newAPI:   newUploadAPI,
	}
}

func (m *UploadManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Stop lets in-flight items finish but starts no new ones. Albums are still
// created for the items that finished.
func (m *UploadManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *UploadManager) stopLocked() {
	if m.stop != nil && !m.stopped {
		close(m.stop)
		m.stopped = true
	}
}

// Cancel aborts in-flight items as well as stopping the run.
func (m *UploadManager) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	if m.cancel != nil && !m.canceled {
		close(m.cancel)
		m.canceled = true
		// Don't set to nil - readers still need to detect closure via select
	}
}

// isCancelled checks if cancellation has been requested
func (m *UploadManager) isCancelled() bool {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel == nil {
		return false
	}
	select {
	case <-cancel:
		return true
	default:
		return false
	}
}

// isStopped reports whether the run should start no new work.
func (m *UploadManager) isStopped() bool {
	select {
	case <-m.getStopChan():
		return true
	default:
		return false
	}
}

func (m *UploadManager) getStopChan() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stop
}

// getCancelChan returns the cancel channel safely
func (m *UploadManager) getCancelChan() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancel
}

type UploadBatchStart struct {
	Total      int   `json:"Total"`
	TotalBytes int64 `json:"TotalBytes"`
}

type FileUploadResult struct {
	MediaKey     string   `json:"MediaKey"`
	IsError      bool     `json:"IsError"`
	IsLivePhoto  bool     `json:"IsLivePhoto"`
	Skipped      bool     `json:"Skipped"`
	SkipCode     string   `json:"SkipCode"`
	SkipReason   string   `json:"SkipReason"`
	Error        error    `json:"-"`
	ErrorMessage string   `json:"ErrorMessage"`
	Path         string   `json:"Path"`
	Paths        []string `json:"Paths"`
}

type ThreadStatus struct {
	WorkerID      int    `json:"WorkerID"`
	Status        string `json:"Status"` // "idle", "staging", "hashing", "checking", "queued", "uploading", "finalizing", "completed", "skipped", "error"
	FilePath      string `json:"FilePath"`
	FileName      string `json:"FileName"`
	Message       string `json:"Message"`
	BytesUploaded int64  `json:"BytesUploaded"`
	BytesTotal    int64  `json:"BytesTotal"`
	Attempt       int    `json:"Attempt"` // Current attempt number (1-based), 0 if not applicable
}

// Upload starts an upload run in the background. Progress is delivered through
// the manager's UploadReporter; UploadStop marks the end of the run.
func (m *UploadManager) Upload(paths []string, opts UploadOptions) {
	opts = opts.normalized()
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.stop = make(chan struct{})
	m.stopped = false
	m.cancel = make(chan struct{})
	m.canceled = false
	m.apiOptions = opts.Api
	m.mu.Unlock()

	// Make preflight visible immediately so a long directory or metadata scan can
	// be cancelled from the UI.
	m.reporter.UploadStart(UploadBatchStart{})

	targetPaths, err := filterGooglePhotosFilesWithCancel(paths, opts, m.isStopped)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			m.finish()
			return
		}
		m.reporter.FileResult(FileUploadResult{
			IsError:      true,
			Error:        err,
			ErrorMessage: err.Error(),
		})
		m.finish()
		return
	}
	workItems, preflightWarnings := ClassifyUploadWork(targetPaths, LivePhotoClassificationOptions{
		Enabled:             opts.PairLivePhotos,
		SkipIncomplete:      opts.SkipIncompleteLivePhotos,
		IgnoreAppleMetadata: opts.IgnoreAppleMetadata,
		Cancelled:           m.isStopped,
	}, nil)
	if m.isStopped() {
		m.finish()
		return
	}
	m.reportPreflight(len(workItems), preflightWarnings)

	if len(workItems) == 0 {
		m.finish()
		return
	}

	state, err := openUploadState(opts)
	if err != nil {
		// One run-level failure, not one per file: no file was tried, and a
		// per-file report would make --retry-report re-queue the whole import.
		m.reporter.FileResult(FileUploadResult{IsError: true, Error: err, ErrorMessage: err.Error()})
		m.finish()
		return
	}
	checkApi, apiErr := m.newAPI(opts.Api)
	if err := apiErr; err != nil {
		state.close()
		for _, item := range workItems {
			path := uploadWorkPrimaryPath(item)
			m.reporter.FileResult(FileUploadResult{
				IsError:      true,
				Error:        err,
				ErrorMessage: err.Error(),
				IsLivePhoto:  item.Kind == UploadWorkLivePhoto,
				Path:         path,
				Paths:        uploadWorkPaths(item),
			})
		}
		m.finish()
		return
	}

	// Calculate total bytes asynchronously and emit update when complete
	go func() {
		var totalBytes int64
		for _, item := range workItems {
			for _, path := range uploadWorkPaths(item) {
				if info, err := os.Stat(path); err == nil {
					totalBytes += info.Size()
				}
			}
		}
		m.reporter.TotalBytes(totalBytes)
	}()

	// Hash workers prepare items ahead of the network workers. Canceling the
	// run cancels hashing; already prepared items still get a result.
	prepareCtx, cancelPrepare := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.getCancelChan():
			cancelPrepare()
		case <-prepareCtx.Done():
		}
	}()
	// Don't start more network workers than items to process
	numWorkers := min(opts.UploadThreads, len(workItems))
	results := make(chan FileUploadResult, numWorkers)

	// Hash workers also check for duplicates and finish items already in the
	// library, so only real uploads wait for a network worker. They finish
	// before workChan closes, so before the network workers and results do.
	// Hash workers share checkApi for duplicate checks, which do not change
	// it. *Api is not safe for concurrent uploads, so in one pool, where hash
	// workers also upload, each creates its own client like an upload worker.
	finishAPI := func(int) (uploadAPI, error) { return checkApi, nil }
	if opts.UploadThreads == 0 {
		// Hash worker IDs are 0..HashThreads-1 here, and each worker only
		// touches its own entry.
		clients := make([]uploadAPI, opts.HashThreads)
		clientErrs := make([]error, opts.HashThreads)
		finishAPI = func(workerID int) (uploadAPI, error) {
			if clients[workerID] == nil && clientErrs[workerID] == nil {
				clients[workerID], clientErrs[workerID] = m.newAPI(opts.Api)
			}
			return clients[workerID], clientErrs[workerID]
		}
	}
	// Hash workers' duplicate checks that arrive together share one request
	// when the API supports it.
	var finder remoteHashFinder = checkApi
	if batcher, ok := checkApi.(batchHashFinder); ok && opts.HashThreads > 1 {
		finder = newHashBatcher(prepareCtx, batcher, min(opts.HashThreads, hashBatchMax), hashBatchWait)
	}
	workChan := prepareUploads(prepareCtx, m.getStopChan(), workItems, opts, state, finder, m.reporter, func(workerID int, p preparedUpload) {
		api, apiErr := finishAPI(workerID)
		m.finishPrepared(workerID, api, apiErr, p, results, opts, state)
	})

	// Start workers
	for i := range numWorkers {
		m.wg.Add(1)
		go m.runWorker(i, workChan, results, opts, state)
	}
	if numWorkers == 0 {
		// One pool: hash workers finish every item themselves. workChan
		// closes once they are done, and results must stay open until then.
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			for range workChan {
			}
		}()
	}

	// Handle results, wait for completion, and create album if configured
	go func() {
		defer cancelPrepare()
		// Collect successful uploads with path -> mediaKey mapping for AUTO mode
		successfulUploads := make(map[string]string) // path -> mediaKey

		// Wait for all workers to finish in a separate goroutine, then close results
		go func() {
			m.wg.Wait()
			close(results)
		}()

		// Process all results (this blocks until results channel is closed)
		for result := range results {
			m.reporter.FileResult(result)
			if result.IsError {
				m.logger.Error("upload error", "path", result.Path, "error", result.Error)
			} else {
				m.logger.Info("upload success", "path", result.Path)
				if result.MediaKey != "" {
					successfulUploads[result.Path] = result.MediaKey
				}
			}
		}

		m.logger.Info("upload complete", "successful", len(successfulUploads),
			"albumName", opts.AlbumName, "albumAutoMode", opts.AlbumAutoMode)

		state.close()
		if len(successfulUploads) > 0 {
			m.handleAlbumCreation(successfulUploads, opts.AlbumName, opts.AlbumAutoMode)
		}

		m.finish()
	}()
}

// finish signals the end of a run and releases the manager for the next one.
func (m *UploadManager) finish() {
	m.reporter.UploadStop()
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

func (m *UploadManager) reportPreflight(uploadItemCount int, warnings []PreflightWarning) {
	preflightResults := 0
	for _, warning := range warnings {
		if warning.Skipped || warning.Failed {
			preflightResults++
		}
	}

	// Start before emitting warnings/results so the frontend resets its previous
	// batch while retaining every event from this preflight.
	m.reporter.UploadStart(UploadBatchStart{
		Total:      uploadItemCount + preflightResults,
		TotalBytes: 0,
	})
	for _, warning := range warnings {
		m.reporter.Warning(warning)
		primaryPath := ""
		if len(warning.Paths) > 0 {
			primaryPath = warning.Paths[0]
		}
		if warning.Failed {
			m.reporter.FileResult(FileUploadResult{
				IsError:      true,
				Error:        errors.New(warning.Message),
				ErrorMessage: warning.Message,
				Path:         primaryPath,
				Paths:        warning.Paths,
			})
			continue
		}
		if !warning.Skipped {
			continue
		}
		m.reporter.FileResult(FileUploadResult{
			IsLivePhoto: true,
			Skipped:     true,
			SkipCode:    warning.Code,
			SkipReason:  warning.Message,
			Path:        primaryPath,
			Paths:       warning.Paths,
		})
	}
}

// handleAlbumCreation handles album creation based on config (manual name/key or AUTO mode)
func (m *UploadManager) handleAlbumCreation(uploads map[string]string, albumName string, albumAutoMode bool) {
	// Check if cancelled before starting album creation
	if m.isCancelled() {
		m.logger.Info("Upload cancelled, skipping album creation")
		return
	}

	m.logger.Info(fmt.Sprintf("handleAlbumCreation called with %d uploads", len(uploads)))

	// Create API once for all album operations
	api, err := NewApi(m.apiOptions)
	if err != nil {
		m.logger.Error(fmt.Sprintf("failed to create API for album creation: %v", err))
		m.reporter.AlbumError(AlbumError{
			AlbumName: albumName,
			Error:     fmt.Sprintf("failed to initialize API: %v", err),
		})
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-m.getCancelChan():
			cancel()
		case <-ctx.Done():
		}
	}()
	albumManager := NewAlbumManager(ctx, api, m.reporter, m.logger)

	// Check if AUTO mode is enabled
	if albumAutoMode {
		m.logger.Info("AUTO mode enabled, creating albums from directories")
		m.createAlbumsFromDirectories(albumManager, uploads)
		return
	}

	// Manual mode: use AlbumName if set
	if albumName == "" {
		m.logger.Info("No album name set and AUTO mode disabled, skipping album creation")
		return
	}

	m.logger.Info(fmt.Sprintf("Creating album with name/key: '%s'", albumName))

	mediaKeys := make([]string, 0, len(uploads))
	for _, mediaKey := range uploads {
		mediaKeys = append(mediaKeys, mediaKey)
	}

	m.logger.Info(fmt.Sprintf("Adding %d media keys to album '%s'", len(mediaKeys), albumName))

	albumKeys, err := albumManager.AddToAlbum(mediaKeys, albumName)
	if err != nil {
		m.logger.Error(fmt.Sprintf("failed to create album '%s': %v", albumName, err))
		m.reporter.AlbumError(AlbumError{
			AlbumName: albumName,
			Error:     err.Error(),
		})
		return
	}
	m.logger.Info(fmt.Sprintf("created album '%s' with %d items, album keys: %v", albumName, len(mediaKeys), albumKeys))
}

// createAlbumsFromDirectories creates albums based on parent directory names (AUTO mode)
func (m *UploadManager) createAlbumsFromDirectories(albumManager *AlbumManager, uploads map[string]string) {
	// Group media keys by parent directory
	mediaKeysByDir := make(map[string][]string)

	for filePath, mediaKey := range uploads {
		parentDir := filepath.Dir(filePath)
		mediaKeysByDir[parentDir] = append(mediaKeysByDir[parentDir], mediaKey)
	}

	// Create an album for each directory
	for dirPath, mediaKeys := range mediaKeysByDir {
		albumName := filepath.Base(dirPath)
		if albumName == "" || albumName == "." {
			albumName = "Uploads"
		}

		albumKeys, err := albumManager.AddToAlbum(mediaKeys, albumName)
		if err != nil {
			m.logger.Error(fmt.Sprintf("failed to create album '%s': %v", albumName, err))
			m.reporter.AlbumError(AlbumError{
				AlbumName: albumName,
				Error:     err.Error(),
			})
			continue
		}
		m.logger.Info(fmt.Sprintf("created album '%s' with %d items, album keys: %v", albumName, len(mediaKeys), albumKeys))
	}
}

// supportedFormats is a map of file extensions supported by Google Photos (O(1) lookup)
var supportedFormats = map[string]bool{
	// Photo formats
	"avif": true, "bmp": true, "gif": true, "heic": true, "heif": true, "ico": true,
	"jpg": true, "jpeg": true, "png": true, "tif": true, "tiff": true, "webp": true,
	"cr2": true, "cr3": true, "nef": true, "arw": true, "orf": true,
	"raf": true, "rw2": true, "pef": true, "sr2": true, "dng": true,
	// Video formats
	"3gp": true, "3g2": true, "asf": true, "avi": true, "divx": true,
	"m2t": true, "m2ts": true, "m4v": true, "mkv": true, "mmv": true,
	"mod": true, "mov": true, "mp4": true, "mpg": true, "mpeg": true,
	"mts": true, "tod": true, "wmv": true, "ts": true, "webm": true,
}

// isSupportedByGooglePhotos checks if a file extension is supported by Google Photos
func isSupportedByGooglePhotos(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		return false
	}
	// Remove the dot and check map
	return supportedFormats[ext[1:]]
}

func scanDirectoryForFiles(path string, recursive bool, excludePattern string, cancelled func() bool) ([]string, error) {
	var files []string

	if cancelled != nil && cancelled() {
		return nil, context.Canceled
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if cancelled != nil && cancelled() {
			return nil, context.Canceled
		}
		fullPath := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			if excludePattern != "" && entry.Name() == excludePattern {
				continue
			}
			if recursive {
				subFiles, err := scanDirectoryForFiles(fullPath, recursive, excludePattern, cancelled)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return nil, err
					}
					continue
				}
				files = append(files, subFiles...)
			}
		} else {
			files = append(files, fullPath)
		}
	}

	return files, nil
}

// FilterGooglePhotosFiles expands paths into the files that would be uploaded
// under opts: directories are scanned, unsupported files dropped, duplicates removed.
func FilterGooglePhotosFiles(paths []string, opts UploadOptions) ([]string, error) {
	return filterGooglePhotosFilesWithCancel(paths, opts, nil)
}

func filterGooglePhotosFilesWithCancel(paths []string, opts UploadOptions, cancelled func() bool) ([]string, error) {
	var supportedFiles []string
	type seenUploadFile struct {
		canonicalPath string
		info          os.FileInfo
	}
	seenFiles := make(map[string][]seenUploadFile)
	var skipped []seenUploadFile
	for _, path := range opts.SkipFiles {
		info, _ := os.Stat(path)
		skipped = append(skipped, seenUploadFile{canonicalPath: canonicalUploadPath(path), info: info})
	}
	appendFile := func(path string, info os.FileInfo) {
		if !opts.DisableUnsupportedFilesFilter && !isSupportedByGooglePhotos(path) {
			return
		}
		canonicalPath := canonicalUploadPath(path)
		if info == nil {
			info, _ = os.Stat(path)
		}
		for _, skip := range skipped {
			if canonicalPath == skip.canonicalPath ||
				(info != nil && skip.info != nil && os.SameFile(info, skip.info)) {
				return
			}
		}
		bucketKey := canonicalPath
		if runtime.GOOS == "windows" {
			bucketKey = strings.ToLower(bucketKey)
		}
		for _, seen := range seenFiles[bucketKey] {
			if canonicalPath == seen.canonicalPath ||
				(info != nil && seen.info != nil && os.SameFile(info, seen.info)) {
				return
			}
		}
		seenFiles[bucketKey] = append(seenFiles[bucketKey], seenUploadFile{
			canonicalPath: canonicalPath,
			info:          info,
		})
		supportedFiles = append(supportedFiles, path)
	}

	for _, path := range paths {
		if cancelled != nil && cancelled() {
			return nil, context.Canceled
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("error accessing path %s: %w", path, err)
		}

		if fileInfo.IsDir() {
			files, err := scanDirectoryForFiles(path, opts.Recursive, opts.ExcludePattern, cancelled)
			if err != nil {
				return nil, fmt.Errorf("error scanning directory %s: %w", path, err)
			}

			for _, file := range files {
				if cancelled != nil && cancelled() {
					return nil, context.Canceled
				}
				appendFile(file, nil)
			}
		} else {
			appendFile(path, fileInfo)
		}
	}

	return supportedFiles, nil
}

func canonicalUploadPath(path string) string {
	canonicalPath, err := filepath.Abs(path)
	if err != nil {
		canonicalPath = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(canonicalPath); err == nil {
		canonicalPath = resolved
	}
	return filepath.Clean(canonicalPath)
}

// UploadFile uploads a single file and returns its media key.
func UploadFile(ctx context.Context, api *Api, filePath string, opts UploadOptions, workerID int, reporter UploadReporter) (string, error) {
	if reporter == nil {
		reporter = NopReporter{}
	}
	prepared := prepareUpload(ctx, UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: filePath}}, opts, workerID, reporter)
	defer prepared.cleanup()
	if prepared.err != nil {
		return "", fmt.Errorf("error preparing file: %w", prepared.err)
	}
	mediaKey, _, err := uploadSingleFile(ctx, api, prepared.files[0], opts, workerID, reporter)
	return mediaKey, err
}

// uploadSingleFile uploads one file that a hash worker has prepared. The bool
// is true when the file already exists remotely; the returned media key is
// then the existing item's key.
func uploadSingleFile(ctx context.Context, api uploadAPI, file preparedFile, opts UploadOptions, workerID int, reporter UploadReporter) (string, bool, error) {
	filePath := file.source
	fileName := filepath.Base(filePath)
	mediakey := ""
	if err := file.uploadable(); err != nil {
		return "", false, err
	}

	// Determine the timestamp to use for the upload.
	// Default to file mtime, but allow filename-based timestamp to take precedence if enabled.
	uploadTimestamp := file.info.ModTime().Unix()
	if opts.SetDateFromFilename {
		if t, ok := parseTimestampFromFilename(filePath); ok {
			uploadTimestamp = t.Unix()
		}
	}
	sha1_hash_bytes := file.hash

	// Stage 2: Checking if exists in library (usually done by the hash worker)
	if !opts.ForceUpload {
		if !file.checked {
			if err := checkRemoteDuplicate(ctx, api, &file, workerID, reporter); err != nil {
				return "", false, err
			}
		}
		mediakey = file.remoteKey
		if len(mediakey) > 0 {
			reporter.ThreadStatus(ThreadStatus{
				WorkerID: workerID,
				Status:   "completed",
				FilePath: filePath,
				FileName: fileName,
				Message:  "Already in library",
			})
			if opts.DeleteFromHost {
				if err := removeUploadedSource(file); err != nil {
					return mediakey, true, fmt.Errorf("file exists in library but failed to delete local copy: %w", err)
				}
			}
			return mediakey, true, nil
		}
	}

	// Stage 3: Uploading
	fileSize := file.info.Size()
	reporter.ThreadStatus(ThreadStatus{
		WorkerID:      workerID,
		Status:        "uploading",
		FilePath:      filePath,
		FileName:      fileName,
		Message:       "Uploading...",
		BytesUploaded: 0,
		BytesTotal:    fileSize,
	})

	// Create progress callback for upload
	progressCallback := func(bytesUploaded, bytesTotal int64, attempt int) {
		message := "Uploading..."
		if attempt > 1 {
			message = fmt.Sprintf("Retrying... (attempt %d)", attempt)
		}
		reporter.ThreadStatus(ThreadStatus{
			WorkerID:      workerID,
			Status:        "uploading",
			FilePath:      filePath,
			FileName:      fileName,
			Message:       message,
			BytesUploaded: bytesUploaded,
			BytesTotal:    bytesTotal,
			Attempt:       attempt,
		})
	}

	finalizeToken, err := api.UploadFileWithHashAndProgress(ctx, file.upload, sha1_hash_bytes, fileSize, progressCallback)
	if err != nil {
		return "", false, fmt.Errorf("error uploading file: %w", err)
	}
	commitToken, err := finalizeToken.legacyCommitToken()
	if err != nil {
		return "", false, fmt.Errorf("error decoding upload finalize token: %w", err)
	}

	// Stage 4: Finalizing
	reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "finalizing",
		FilePath: filePath,
		FileName: fileName,
		Message:  "Committing upload...",
	})

	mediaKey, err := api.CommitUpload(ctx, commitToken, fileName, sha1_hash_bytes, uploadTimestamp)
	if err != nil {
		return "", false, fmt.Errorf("error committing file: %w", err)
	}

	if len(mediaKey) == 0 {
		return "", false, fmt.Errorf("media key not received")
	}

	if opts.DeleteFromHost {
		if err := removeUploadedSource(file); err != nil {
			return mediaKey, false, fmt.Errorf("uploaded successfully but failed to delete file: %w", err)
		}
	}

	return mediaKey, false, nil
}

// removeUploadedSource deletes an uploaded file, refusing if it changed after
// it was hashed or staged.
func removeUploadedSource(file preparedFile) error {
	if err := file.unchanged(); err != nil {
		return fmt.Errorf("refusing to delete: %w", err)
	}
	return os.Remove(file.source)
}

func (m *UploadManager) runWorker(workerID int, workChan <-chan preparedUpload, results chan<- FileUploadResult, opts UploadOptions, state *uploadState) {
	defer m.wg.Done()

	// Emit idle status initially
	m.reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "idle",
		Message:  "Waiting for files...",
	})

	// Create API client once per worker for connection reuse
	api, apiErr := m.newAPI(opts.Api)

	// Drain the queue even after Stop or Cancel so every prepared item is
	// cleaned up. Items still waiting for an upload worker then were never
	// uploaded: they are dropped without a result, like files never started.
	stop := m.getStopChan()
	for prepared := range workChan {
		select {
		case <-stop:
			prepared.cleanup()
			continue
		default:
		}
		m.finishPrepared(workerID, api, apiErr, prepared, results, opts, state)

		// Mark as idle after completing file
		m.reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   "idle",
			Message:  "Waiting for next file...",
		})
	}

	// Final idle status when no more work
	m.reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "idle",
		Message:  "Finished",
	})
}

// finishPrepared uploads one prepared item, or reports it when preparation
// failed or found it in the library, and sends its result. apiErr is the
// error from creating api, if any.
func (m *UploadManager) finishPrepared(workerID int, api uploadAPI, apiErr error, prepared preparedUpload, results chan<- FileUploadResult, opts UploadOptions, state *uploadState) {
	cancel := m.getCancelChan()
	item := prepared.item
	failure := prepared.err
	if failure == nil {
		failure = apiErr
	}
	// An item already found in the library, or confirmed by the state file,
	// needs no request, so a cancel does not turn its known result into a
	// failure.
	known := failure == nil && (prepared.cachedKey != "" || prepared.knownResult(opts))
	select {
	case <-cancel:
		if !known {
			failure = context.Canceled
		}
	default:
	}
	if failure != nil {
		results <- FileUploadResult{IsError: true, IsLivePhoto: item.Kind == UploadWorkLivePhoto, Error: failure, ErrorMessage: failure.Error(), Path: uploadWorkPrimaryPath(item), Paths: uploadWorkPaths(item)}
		prepared.cleanup()
		return
	}
	if prepared.cachedKey != "" {
		// Cache-only hits never delete originals: nothing was checked this run.
		results <- FileUploadResult{IsLivePhoto: item.Kind == UploadWorkLivePhoto, Path: uploadWorkPrimaryPath(item), Paths: uploadWorkPaths(item), Skipped: true, SkipCode: "state-cache", SkipReason: "Previously confirmed for this account and upload policy", MediaKey: prepared.cachedKey}
		return
	}
	ctx, cancelUpload := context.WithCancel(context.Background())
	go func() {
		select {
		case <-cancel:
			cancelUpload()
		case <-ctx.Done():
			// Upload completed or context cancelled
		}
	}()

	path := uploadWorkPrimaryPath(item)
	paths := uploadWorkPaths(item)
	isLivePhoto := item.Kind == UploadWorkLivePhoto
	mediaKey, skipped, err := uploadPreparedItem(ctx, api, prepared, opts, state, workerID, m.reporter)
	skipCode, skipReason := remoteSkip(isLivePhoto)
	if err != nil && mediaKey != "" {
		// The item is in the library; only deleting the local copy, or saving
		// its checkpoint, failed.
		code, problem := "local-cleanup-failed", "local cleanup failed"
		if errors.Is(err, errStateCheckpoint) {
			code, problem = "state-checkpoint-failed", "state checkpoint failed"
		}
		result := FileUploadResult{IsLivePhoto: isLivePhoto, Path: path, Paths: paths, MediaKey: mediaKey}
		message := fmt.Sprintf("Uploaded, but %s: %v", problem, err)
		if skipped {
			result.Skipped, result.SkipCode, result.SkipReason = true, skipCode, skipReason
			message = fmt.Sprintf("Already in library, but %s: %v", problem, err)
		}
		results <- result
		m.reporter.Warning(PreflightWarning{
			Paths:   paths,
			Code:    code,
			Message: err.Error(),
		})
		m.reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   "completed",
			FilePath: path,
			FileName: filepath.Base(path),
			Message:  message,
		})
	} else if err != nil {
		results <- FileUploadResult{IsError: true, IsLivePhoto: isLivePhoto, Error: err, ErrorMessage: err.Error(), Path: path, Paths: paths}
		m.reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   "error",
			FilePath: path,
			FileName: filepath.Base(path),
			Message:  fmt.Sprintf("Error: %v", err),
		})
	} else if skipped {
		results <- FileUploadResult{
			IsLivePhoto: isLivePhoto,
			Skipped:     true,
			MediaKey:    mediaKey,
			SkipCode:    skipCode,
			SkipReason:  skipReason,
			Path:        path,
			Paths:       paths,
		}
	} else {
		results <- FileUploadResult{IsLivePhoto: isLivePhoto, Path: path, Paths: paths, MediaKey: mediaKey}
		m.reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   "completed",
			FilePath: path,
			FileName: filepath.Base(path),
			Message:  "Completed",
		})
	}
	cancelUpload()
	prepared.cleanup()
}

// errStateCheckpoint marks an item that is in the library but whose state
// checkpoint could not be saved.
var errStateCheckpoint = errors.New("state checkpoint failed")

// uploadPreparedItem uploads one item and, with a writable state, checkpoints
// it. --delete then waits until the checkpoint is durable. Without a writable
// state, deletion happens inside the upload exactly as before.
func uploadPreparedItem(ctx context.Context, api uploadAPI, prepared preparedUpload, opts UploadOptions, state *uploadState, workerID int, reporter UploadReporter) (string, bool, error) {
	deleteAfterCheckpoint := opts.DeleteFromHost && state.writable()
	if deleteAfterCheckpoint {
		opts.DeleteFromHost = false
	}
	mediaKey, skipped, err := uploadWorkItem(ctx, api, prepared, opts, workerID, reporter)
	if err != nil {
		return mediaKey, skipped, err
	}
	if err := state.save(prepared, mediaKey, skipped); err != nil {
		// The item is in the library, so it is not a failure, but its sources
		// must not be deleted without a checkpoint.
		return mediaKey, skipped, fmt.Errorf("%w: %w", errStateCheckpoint, err)
	}
	if deleteAfterCheckpoint && mediaKey != "" && (!skipped || prepared.item.Kind == UploadWorkSingle) {
		return mediaKey, skipped, removePreparedSources(prepared)
	}
	return mediaKey, skipped, nil
}

// removePreparedSources deletes an item's sources, refusing any that changed
// after hashing.
func removePreparedSources(prepared preparedUpload) error {
	if prepared.item.Kind == UploadWorkLivePhoto {
		return removeLivePhotoSources(*prepared.item.LivePhoto, prepared.files...)
	}
	return removeUploadedSource(prepared.files[0])
}

// remoteSkip describes an item skipped because it is already in the library.
func remoteSkip(isLivePhoto bool) (code, reason string) {
	if isLivePhoto {
		return "remote-live-photo-component-exists", "Skipped because a Live Photo component already exists remotely"
	}
	return "remote-duplicate", "Skipped because this file already exists remotely"
}

func uploadWorkItem(ctx context.Context, api uploadAPI, prepared preparedUpload, opts UploadOptions, workerID int, reporter UploadReporter) (string, bool, error) {
	item := prepared.item
	switch item.Kind {
	case UploadWorkSingle:
		if item.Single == nil || item.LivePhoto != nil || len(prepared.files) != 1 {
			return "", false, fmt.Errorf("invalid single-media work item")
		}
		return uploadSingleFile(ctx, api, prepared.files[0], opts, workerID, reporter)
	case UploadWorkLivePhoto:
		if item.LivePhoto == nil || item.Single != nil || len(prepared.files) != 2 {
			return "", false, fmt.Errorf("invalid Live Photo work item")
		}
		return uploadLivePhotoWithCallback(ctx, api, *item.LivePhoto, prepared.files[0], prepared.files[1], LivePhotoUploadOptions{
			Policy:                     api.livePhotoCommitPolicy(),
			DeleteFromHost:             opts.DeleteFromHost,
			SetDateFromFilename:        opts.SetDateFromFilename,
			UpdateExistingPhotosToLive: opts.UpdateExistingPhotosToLive,
		}, workerID, reporter)
	default:
		return "", false, fmt.Errorf("unsupported upload work kind %q", item.Kind)
	}
}

func uploadWorkPaths(item UploadWorkItem) []string {
	if item.Kind == UploadWorkLivePhoto && item.LivePhoto != nil {
		return []string{item.LivePhoto.PhotoPath, item.LivePhoto.VideoPath}
	}
	if item.Single != nil {
		return []string{item.Single.Path}
	}
	return nil
}

func uploadWorkPrimaryPath(item UploadWorkItem) string {
	paths := uploadWorkPaths(item)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}
