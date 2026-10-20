package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeStageSource(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "IMG_0001.HEIC")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	return path
}

func TestStageFilesLocallyCopiesAndCleans(t *testing.T) {
	t.Parallel()

	sourcePath := writeStageSource(t, "stable upload contents")
	staged, err := stageFilesLocally(context.Background(), []string{sourcePath})
	if err != nil {
		t.Fatalf("stage file: %v", err)
	}
	defer staged.cleanup()
	if len(staged.paths) != 1 || staged.paths[0] == sourcePath {
		t.Fatalf("staged paths = %v", staged.paths)
	}
	got, err := os.ReadFile(staged.paths[0])
	if err != nil || string(got) != "stable upload contents" {
		t.Fatalf("staged contents = %q, %v", got, err)
	}
	if err := staged.verifySourcesUnchanged(); err != nil {
		t.Fatalf("unchanged source rejected: %v", err)
	}

	staged.cleanup()
	if _, err := os.Stat(staged.paths[0]); !os.IsNotExist(err) {
		t.Fatalf("staged file still exists after cleanup, stat error: %v", err)
	}
}

func TestStagedSourceChangedAfterStagingIsNotDeleted(t *testing.T) {
	t.Parallel()

	sourcePath := writeStageSource(t, "original")
	staged, err := stageFilesLocally(context.Background(), []string{sourcePath})
	if err != nil {
		t.Fatal(err)
	}
	defer staged.cleanup()

	// Same size, new modification time: a rewrite the size check alone misses.
	if err := os.WriteFile(sourcePath, []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(sourcePath, later, later); err != nil {
		t.Fatal(err)
	}
	if err := removeUploadedSource(sourcePath, staged); err == nil {
		t.Fatal("deleted a source that changed after staging")
	}
	if data, err := os.ReadFile(sourcePath); err != nil || string(data) != "replaced" {
		t.Fatalf("changed source lost: %q %v", data, err)
	}
	pair := LivePhotoPair{PhotoPath: sourcePath, VideoPath: sourcePath + ".mov"}
	if err := removeLivePhotoSources(pair, staged); err == nil {
		t.Fatal("deleted a Live Photo whose source changed after staging")
	}
}

func TestRemoveUploadedSourceWithoutStaging(t *testing.T) {
	t.Parallel()

	sourcePath := writeStageSource(t, "photo")
	if err := removeUploadedSource(sourcePath, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("source not deleted: %v", err)
	}
}

func TestRemoveStagedFilesDeletesLiveCopies(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.jpg")
	if err := os.WriteFile(src, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	staged, err := stageFilesLocally(context.Background(), []string{src})
	if err != nil {
		t.Fatal(err)
	}
	RemoveStagedFiles()
	if _, err := os.Stat(staged.dir); !os.IsNotExist(err) {
		t.Fatalf("staging dir left behind: %v", err)
	}
	staged.cleanup() // still safe afterwards
}
