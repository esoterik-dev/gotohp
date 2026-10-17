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

// preparedUpload is one work item ready for the network stage. files follow
// uploadWorkPaths order. cleanup must run once the item is finished.
type preparedUpload struct {
	item   UploadWorkItem
	files  []preparedFile
	staged *stagedFiles
	err    error
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

// prepareUploads hashes items on opts.HashThreads workers. The returned
// queue holds at most opts.UploadThreads prepared items, so hashing runs
// only a little ahead of uploading. Every item yields exactly one
// preparedUpload, including after cancellation.
func prepareUploads(ctx context.Context, items []UploadWorkItem, opts UploadOptions, reporter UploadReporter) <-chan preparedUpload {
	jobs := make(chan UploadWorkItem)
	ready := make(chan preparedUpload, opts.UploadThreads)
	go func() {
		defer close(jobs)
		for _, item := range items {
			jobs <- item
		}
	}()
	var wg sync.WaitGroup
	for i := range min(opts.HashThreads, len(items)) {
		workerID := opts.UploadThreads + i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				prepared := prepareUpload(ctx, item, opts, workerID, reporter)
				reporter.ThreadStatus(ThreadStatus{WorkerID: workerID, Status: "idle", Message: "Waiting for next file..."})
				ready <- prepared
			}
			reporter.ThreadStatus(ThreadStatus{WorkerID: workerID, Status: "idle", Message: "Finished"})
		}()
	}
	go func() { wg.Wait(); close(ready) }()
	return ready
}
