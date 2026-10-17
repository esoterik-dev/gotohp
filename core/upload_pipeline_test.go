package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"app/generated"

	"google.golang.org/protobuf/encoding/protowire"
)

// These tests run UploadManager.Upload end to end against fakeLibrary, so
// they check the documented behaviour of a run (README, flag help), not the
// internals of one stage.

// fakeLibrary is an in-memory Google Photos library.
type fakeLibrary struct {
	mu      sync.Mutex
	known   map[string]string // hash -> media key
	lookups int
	uploads int
	// uploadGate, when set, blocks every upload until it is closed or the
	// upload's context is canceled.
	uploadGate chan struct{}
	// uploadStarted, when set, receives once per upload that starts.
	uploadStarted chan string
}

func newFakeLibrary() *fakeLibrary { return &fakeLibrary{known: make(map[string]string)} }

func (f *fakeLibrary) add(t *testing.T, path string) {
	t.Helper()
	hash, err := CalculateSHA1(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.known[string(hash)] = "existing-" + filepath.Base(path)
}

func (f *fakeLibrary) FindRemoteMediaByHash(_ context.Context, hash []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	return f.known[string(hash)], nil
}

func (f *fakeLibrary) UploadFileWithHashAndProgress(ctx context.Context, filePath string, _ []byte, _ int64, _ UploadProgressCallback) (ScottyFinalizeToken, error) {
	f.mu.Lock()
	f.uploads++
	f.mu.Unlock()
	if f.uploadStarted != nil {
		f.uploadStarted <- filePath
	}
	if f.uploadGate != nil {
		select {
		case <-f.uploadGate:
		case <-ctx.Done():
			return ScottyFinalizeToken{}, ctx.Err()
		}
	}
	raw := protowire.AppendTag(nil, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, scottyFinalizeTokenVersion)
	raw = protowire.AppendTag(raw, 2, protowire.BytesType)
	raw = protowire.AppendBytes(raw, []byte("upload"))
	return ScottyFinalizeToken{Raw: raw}, nil
}

func (f *fakeLibrary) CommitUpload(_ context.Context, _ *generated.CommitToken, fileName string, hash []byte, _ int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := "new-" + fileName
	f.known[string(hash)] = key
	return key, nil
}

func (f *fakeLibrary) CommitLivePhoto(context.Context, LivePhotoCreateRequest) (string, error) {
	return "", fmt.Errorf("fakeLibrary: Live Photos not supported")
}

func (f *fakeLibrary) ReconcileLivePhoto(context.Context, LivePhotoReconcileRequest) (string, error) {
	return "", fmt.Errorf("fakeLibrary: Live Photos not supported")
}

func (f *fakeLibrary) livePhotoCommitPolicy() LivePhotoCommitPolicy { return LivePhotoCommitPolicy{} }

// runRecorder records a run's events; done closes on UploadStop.
type runRecorder struct {
	NopReporter
	mu       sync.Mutex
	results  []FileUploadResult
	statuses []ThreadStatus
	resultCh chan FileUploadResult
	done     chan struct{}
}

func newRunRecorder() *runRecorder {
	return &runRecorder{resultCh: make(chan FileUploadResult, 1000), done: make(chan struct{})}
}

func (r *runRecorder) UploadStop() { close(r.done) }

func (r *runRecorder) ThreadStatus(s ThreadStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, s)
}

func (r *runRecorder) FileResult(res FileUploadResult) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
	r.resultCh <- res
}

func (r *runRecorder) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("upload run did not finish")
	}
}

// workersWithStatus returns the worker IDs that ever reported status.
func (r *runRecorder) workersWithStatus(status string) map[int]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := map[int]bool{}
	for _, s := range r.statuses {
		if s.Status == status {
			ids[s.WorkerID] = true
		}
	}
	return ids
}

func writePhotos(t *testing.T, dir string, n int) []string {
	t.Helper()
	paths := make([]string, n)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("IMG_%03d.jpg", i))
		if err := os.WriteFile(paths[i], []byte(paths[i]), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func startRun(lib *fakeLibrary, paths []string, opts UploadOptions) (*UploadManager, *runRecorder) {
	rec := newRunRecorder()
	m := NewUploadManager(rec, nil)
	m.newAPI = func(ApiOptions) (uploadAPI, error) { return lib, nil }
	m.Upload(paths, opts)
	return m, rec
}

func countResults(results []FileUploadResult) (succeeded, skipped, failed int) {
	for _, r := range results {
		switch {
		case r.IsError:
			failed++
		case r.Skipped:
			skipped++
		default:
			succeeded++
		}
	}
	return
}

// README: hash workers check whether each single file is already in the
// library; only new files are queued for the upload workers.
func TestUploadChecksDuplicatesOnHashWorkers(t *testing.T) {
	paths := writePhotos(t, t.TempDir(), 8)
	lib := newFakeLibrary()
	for _, p := range paths[:5] {
		lib.add(t, p)
	}
	_, rec := startRun(lib, paths, UploadOptions{HashThreads: 4, UploadThreads: 2})
	rec.wait(t)

	ok, skipped, failed := countResults(rec.results)
	if ok != 3 || skipped != 5 || failed != 0 {
		t.Fatalf("succeeded=%d skipped=%d failed=%d", ok, skipped, failed)
	}
	for id := range rec.workersWithStatus("checking") {
		if id < 2 {
			t.Errorf("upload worker %d ran a duplicate check", id)
		}
	}
	for id := range rec.workersWithStatus("uploading") {
		if id >= 2 {
			t.Errorf("hash worker %d uploaded a file", id)
		}
	}
	if lib.uploads != 3 {
		t.Fatalf("uploads=%d, want only the 3 new files", lib.uploads)
	}
}

// Duplicates are finished by hash workers, so they do not wait behind a
// slow upload.
func TestUploadDuplicatesDoNotWaitForSlowUploads(t *testing.T) {
	paths := writePhotos(t, t.TempDir(), 12)
	lib := newFakeLibrary()
	for _, p := range paths[1:] {
		lib.add(t, p)
	}
	lib.uploadGate = make(chan struct{})
	_, rec := startRun(lib, paths, UploadOptions{HashThreads: 4, UploadThreads: 1})

	for range len(paths) - 1 {
		select {
		case res := <-rec.resultCh:
			if !res.Skipped || res.SkipCode != "remote-duplicate" {
				t.Fatalf("unexpected result while upload blocked: %+v", res)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("duplicates waited for the blocked upload")
		}
	}
	close(lib.uploadGate)
	rec.wait(t)
	if ok, skipped, failed := countResults(rec.results); ok != 1 || skipped != 11 || failed != 0 {
		t.Fatalf("succeeded=%d skipped=%d failed=%d", ok, skipped, failed)
	}
}

// README: Ctrl+C stops starting new files; files that never started are left
// out of the report.
func TestUploadCancelReportsOnlyStartedItems(t *testing.T) {
	paths := writePhotos(t, t.TempDir(), 60)
	lib := newFakeLibrary()
	lib.uploadGate = make(chan struct{}) // never opened: uploads end only by cancel
	lib.uploadStarted = make(chan string, len(paths))
	m, rec := startRun(lib, paths, UploadOptions{HashThreads: 3, UploadThreads: 1})

	select {
	case <-lib.uploadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("no upload started")
	}
	m.Cancel()
	rec.wait(t)

	// In flight at cancel: one upload, one queued item, one per hash worker.
	if n := len(rec.results); n == 0 || n > 1+1+3 {
		t.Fatalf("%d results after cancel; files that never started must not be reported", n)
	}
	for _, res := range rec.results {
		if !res.IsError {
			t.Fatalf("canceled item reported as %+v", res)
		}
	}
	if lib.uploads != 1 {
		t.Fatalf("uploads=%d after cancel", lib.uploads)
	}
}

// --force uploads even if the file already exists and never checks.
func TestUploadForceSkipsDuplicateCheck(t *testing.T) {
	paths := writePhotos(t, t.TempDir(), 3)
	lib := newFakeLibrary()
	for _, p := range paths {
		lib.add(t, p)
	}
	_, rec := startRun(lib, paths, UploadOptions{HashThreads: 2, UploadThreads: 2, ForceUpload: true})
	rec.wait(t)
	if ok, _, _ := countResults(rec.results); ok != 3 || lib.lookups != 0 {
		t.Fatalf("succeeded=%d lookups=%d", ok, lib.lookups)
	}
}

// --delete removes a source once it is confirmed in the library, whether it
// was uploaded or found as a duplicate on a hash worker.
func TestUploadDeleteRemovesConfirmedSources(t *testing.T) {
	paths := writePhotos(t, t.TempDir(), 4)
	lib := newFakeLibrary()
	lib.add(t, paths[0])
	lib.add(t, paths[1])
	_, rec := startRun(lib, paths, UploadOptions{HashThreads: 2, UploadThreads: 1, DeleteFromHost: true})
	rec.wait(t)
	if _, _, failed := countResults(rec.results); failed != 0 {
		t.Fatalf("results=%+v", rec.results)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not deleted: %v", filepath.Base(p), err)
		}
	}
}

// Every worker ends the run idle, so the CLI's busy count reaches zero.
func TestUploadLeavesEveryWorkerIdle(t *testing.T) {
	paths := writePhotos(t, t.TempDir(), 10)
	lib := newFakeLibrary()
	for _, p := range paths[:6] {
		lib.add(t, p)
	}
	_, rec := startRun(lib, paths, UploadOptions{HashThreads: 3, UploadThreads: 2})
	rec.wait(t)
	last := map[int]string{}
	for _, s := range rec.statuses {
		last[s.WorkerID] = s.Status
	}
	if len(last) != 5 {
		t.Fatalf("workers seen: %v", last)
	}
	for id, status := range last {
		if status != "idle" {
			t.Errorf("worker %d ended as %q", id, status)
		}
	}
}

var _ uploadAPI = (*fakeLibrary)(nil)
