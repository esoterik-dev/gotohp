package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// preparedFile is one source file that a hash worker has hashed, and staged
// when StageLocal is set, before an upload worker takes it.
type preparedFile struct {
	source string      // path the user selected
	upload string      // file the upload reads: the staged copy or the source
	hash   []byte      // SHA-1 of upload
	info   os.FileInfo // source info when it was hashed or staged
	// checked is set once the remote duplicate check has run; remoteKey is
	// then the existing library item, or empty when the file is new.
	checked   bool
	remoteKey string
}

// remoteHashFinder is the part of *Api the duplicate check uses.
type remoteHashFinder interface {
	FindRemoteMediaByHash(ctx context.Context, hash []byte) (string, error)
}

// checkRemoteDuplicate looks up f's hash in the library.
func checkRemoteDuplicate(ctx context.Context, api remoteHashFinder, f *preparedFile, workerID int, reporter UploadReporter) error {
	reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "checking",
		FilePath: f.source,
		FileName: filepath.Base(f.source),
		Message:  "Checking if file exists in library...",
	})
	key, err := api.FindRemoteMediaByHash(ctx, f.hash)
	if err != nil {
		// Uploading without a completed duplicate check risks a duplicate item.
		return fmt.Errorf("remote duplicate check failed; upload deferred: %w", err)
	}
	f.checked, f.remoteKey = true, key
	return nil
}

// unchanged fails if the source was replaced or modified since it was
// prepared, so a stale hash is never uploaded and a changed file is never
// deleted.
func (f preparedFile) unchanged() error {
	info, err := os.Stat(f.source)
	if err != nil {
		return err
	}
	if !sameFileVersion(f.info, info) {
		return fmt.Errorf("file changed after hashing: %s", f.source)
	}
	return nil
}

// uploadable fails if uploading f would send bytes that do not match its
// hash. A staged copy is stable, so it can be uploaded even if the source
// changed since; only deleting that source is refused (see unchanged). An
// unstaged file was hashed from the source itself, so any change makes the
// hash stale.
func (f preparedFile) uploadable() error {
	if f.upload != f.source {
		return nil
	}
	return f.unchanged()
}

// preparedUpload is one work item ready for the network stage. files follow
// uploadWorkPaths order. cleanup must run once the item is finished.
type preparedUpload struct {
	item      UploadWorkItem
	files     []preparedFile
	staged    *stagedFiles
	err       error
	cachedKey string // media key from the state file; the item was not hashed
}

func (p preparedUpload) cleanup() { p.staged.cleanup() }

// prepareUpload stages (with StageLocal) and hashes every file of item.
func prepareUpload(ctx context.Context, item UploadWorkItem, opts UploadOptions, workerID int, reporter UploadReporter) preparedUpload {
	p := preparedUpload{item: item}
	paths := uploadWorkPaths(item)
	names := make([]string, len(paths))
	for i, path := range paths {
		names[i] = filepath.Base(path)
	}
	status := func(status, message string) {
		reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   status,
			FilePath: uploadWorkPrimaryPath(item),
			FileName: strings.Join(names, " + "),
			Message:  message,
		})
	}

	reads := paths
	var infos []os.FileInfo
	if opts.StageLocal {
		status("staging", "Copying to local temporary storage...")
		staged, err := stageFilesLocally(ctx, paths)
		if err != nil {
			p.err = fmt.Errorf("stage locally: %w", err)
			return p
		}
		p.staged, reads, infos = staged, staged.paths, staged.infos
	}

	status("hashing", "Hashing...")
	for i, path := range paths {
		var info os.FileInfo
		if infos != nil {
			info = infos[i]
		} else {
			var err error
			if info, err = os.Stat(path); err != nil {
				p.err = err
				return p
			}
		}
		hash, err := CalculateSHA1(ctx, reads[i])
		if err != nil {
			p.err = err
			return p
		}
		file := preparedFile{source: path, upload: reads[i], hash: hash, info: info}
		if p.staged == nil {
			// The hash was read from the source itself; reject a change mid-read.
			if err := file.unchanged(); err != nil {
				p.err = err
				return p
			}
		}
		p.files = append(p.files, file)
	}
	return p
}

// prepareUploads hashes items on opts.HashThreads workers. With
// opts.UploadThreads 0 there is no upload pool: every item is passed to
// finish on its hash worker, which uploads it. With a non-nil
// finder, the same workers also run the duplicate check for single files, and
// a file already in the library is passed to finish on that worker instead
// of being queued. The returned queue holds at most opts.UploadThreads
// prepared items, so hashing runs only a little ahead of uploading. Every
// started item yields exactly one preparedUpload or finish call, including
// after cancellation. Closing stop, or canceling ctx, stops starting new
// items; canceling ctx also aborts hashing in progress.
// Items the state file already confirms skip hashing and go to finish.
func prepareUploads(ctx context.Context, stop <-chan struct{}, items []UploadWorkItem, opts UploadOptions, state *uploadState, finder remoteHashFinder, reporter UploadReporter, finish func(workerID int, p preparedUpload)) <-chan preparedUpload {
	jobs := make(chan UploadWorkItem)
	// With --stage-local every queued item holds a temporary copy, so the
	// queue is bounded and hash workers wait for upload workers. Otherwise a
	// queued item is only paths and a hash: hash workers hand it off and keep
	// checking ahead, so duplicates never wait behind new files.
	bounded := opts.StageLocal
	ready := make(chan preparedUpload, opts.UploadThreads)
	queued := ready // what hash workers send to
	if !bounded {
		ready = make(chan preparedUpload)
		queued = make(chan preparedUpload)
	}
	go func() {
		defer close(jobs)
		for _, item := range items {
			select {
			case <-stop:
				return
			default:
			}
			if ctx.Err() != nil {
				return
			}
			select {
			case jobs <- item:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for i := range min(opts.HashThreads, len(items)) {
		workerID := opts.UploadThreads + i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if key := state.lookup(item, opts); key != "" {
					finish(workerID, preparedUpload{item: item, cachedKey: key})
					continue
				}
				prepared := prepareUpload(ctx, item, opts, workerID, reporter)
				if finder != nil && prepared.err == nil && item.Kind == UploadWorkSingle && !opts.ForceUpload {
					prepared.err = checkRemoteDuplicate(ctx, finder, &prepared.files[0], workerID, reporter)
				}
				// Finish on the hash worker whatever needs no upload worker: a
				// duplicate, a failure, or every item when there is one pool.
				known := prepared.err == nil && item.Kind == UploadWorkSingle && prepared.files[0].remoteKey != ""
				if known || prepared.err != nil || opts.UploadThreads == 0 {
					finish(workerID, prepared)
					reporter.ThreadStatus(ThreadStatus{WorkerID: workerID, Status: "idle", Message: "Waiting for next file..."})
					continue
				}
				if bounded {
					// Waiting here is the point: it caps the temporary copies.
					reporter.ThreadStatus(ThreadStatus{
						WorkerID: workerID,
						Status:   "queued",
						FilePath: uploadWorkPrimaryPath(item),
						FileName: filepath.Base(uploadWorkPrimaryPath(item)),
						Message:  "Waiting for an upload worker...",
					})
				}
				queued <- prepared
				reporter.ThreadStatus(ThreadStatus{WorkerID: workerID, Status: "idle", Message: "Waiting for next file..."})
			}
			reporter.ThreadStatus(ThreadStatus{WorkerID: workerID, Status: "idle", Message: "Finished"})
		}()
	}
	go func() { wg.Wait(); close(queued) }()
	if !bounded {
		var report func(int)
		if qr, ok := reporter.(QueueReporter); ok {
			report = qr.QueueLength
		}
		go unboundedQueue(queued, ready, report)
	}
	return ready
}

// unboundedQueue forwards everything from in to out in order, buffering as
// much as needed, so senders on in never wait for receivers on out. out is
// closed after in is closed and drained. report, if set, gets the number of
// buffered items whenever it changes.
func unboundedQueue(in <-chan preparedUpload, out chan<- preparedUpload, report func(int)) {
	defer close(out)
	var pending []preparedUpload
	reported := 0
	for in != nil || len(pending) > 0 {
		if report != nil && len(pending) != reported {
			reported = len(pending)
			report(reported)
		}
		var send chan<- preparedUpload
		var next preparedUpload
		if len(pending) > 0 {
			send, next = out, pending[0]
		}
		select {
		case p, ok := <-in:
			if !ok {
				in = nil
				continue
			}
			pending = append(pending, p)
		case send <- next:
			pending[0] = preparedUpload{}
			pending = pending[1:]
		}
	}
	if report != nil && reported != 0 {
		report(0)
	}
}
