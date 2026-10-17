package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const uploadStageBufferSize = 1 * 1024 * 1024

const uploadStageAttempts = 3

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

// stagedFiles holds local copies of one upload work item's sources. The
// source file info is captured when each copy is made, so a source that
// changes afterwards is never deleted as if it were the uploaded file.
type stagedFiles struct {
	paths   []string // local copies, in source order
	sources []string
	infos   []os.FileInfo
	dir     string
}

// stageFilesLocally copies source files into a new directory under
// os.TempDir (TMPDIR on Unix). The copies are stable regular files for the
// duration of one upload work item; cleanup removes only that directory.
func stageFilesLocally(ctx context.Context, sourcePaths []string) (*stagedFiles, error) {
	stageDir, err := os.MkdirTemp("", "gotohp-upload-stage-")
	if err != nil {
		return nil, fmt.Errorf("create local upload staging directory: %w", err)
	}
	staged := &stagedFiles{sources: sourcePaths, dir: stageDir}
	for index, sourcePath := range sourcePaths {
		stagedPath := filepath.Join(stageDir, fmt.Sprintf("%d-%s", index, filepath.Base(sourcePath)))
		info, err := stageOneFile(ctx, sourcePath, stagedPath)
		if err != nil {
			staged.cleanup()
			return nil, err
		}
		staged.paths = append(staged.paths, stagedPath)
		staged.infos = append(staged.infos, info)
	}
	return staged, nil
}

func (s *stagedFiles) cleanup() {
	if s != nil {
		_ = os.RemoveAll(s.dir)
	}
}

// verifySourcesUnchanged fails if any source was replaced or modified since
// it was staged. A nil receiver means nothing was staged.
func (s *stagedFiles) verifySourcesUnchanged() error {
	if s == nil {
		return nil
	}
	for i, path := range s.sources {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !sameFileVersion(s.infos[i], info) {
			return fmt.Errorf("refusing to delete %s: it changed after it was staged for upload", path)
		}
	}
	return nil
}

func sameFileVersion(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// stageOneFile copies sourcePath and returns the source info the copy
// matches. It retries when the source's size or modification time changes
// while it is being copied.
func stageOneFile(ctx context.Context, sourcePath, stagedPath string) (os.FileInfo, error) {
	for attempt := 1; ; attempt++ {
		before, err := os.Stat(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("stat source file %s: %w", sourcePath, err)
		}
		if !before.Mode().IsRegular() {
			return nil, fmt.Errorf("source path is not a regular file: %s", sourcePath)
		}
		copied, err := copyFile(ctx, sourcePath, stagedPath)
		if err != nil {
			return nil, err
		}
		after, err := os.Stat(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("stat source file %s: %w", sourcePath, err)
		}
		if copied == before.Size() && sameFileVersion(before, after) {
			if err := os.Chtimes(stagedPath, before.ModTime(), before.ModTime()); err != nil {
				return nil, fmt.Errorf("preserve timestamp for staged file %s: %w", stagedPath, err)
			}
			return before, nil
		}
		if attempt == uploadStageAttempts {
			return nil, fmt.Errorf("source file %s changed while staging", sourcePath)
		}
	}
}

func copyFile(ctx context.Context, sourcePath, stagedPath string) (int64, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return 0, fmt.Errorf("open source file %s: %w", sourcePath, err)
	}
	defer source.Close()
	destination, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create staged file %s: %w", stagedPath, err)
	}
	copied, err := io.CopyBuffer(destination, contextReader{ctx: ctx, r: source}, make([]byte, uploadStageBufferSize))
	closeErr := destination.Close()
	if err != nil {
		return 0, fmt.Errorf("copy %s to local staging: %w", sourcePath, err)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close staged file %s: %w", stagedPath, closeErr)
	}
	return copied, nil
}
