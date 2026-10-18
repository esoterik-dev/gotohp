package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerCounts(t *testing.T) {
	for _, tc := range []struct {
		opts         UploadOptions
		hash, upload int
	}{
		{UploadOptions{Threads: 8}, 8, 0}, // one pool, as before the split
		{UploadOptions{Threads: 8, HashThreads: 6, UploadThreads: 2}, 6, 2},
		{UploadOptions{Threads: 8, HashThreads: 4}, 4, 8},
		{UploadOptions{Threads: 8, UploadThreads: 2}, 8, 2},
		{UploadOptions{}, 1, 0},
	} {
		got := tc.opts.normalized()
		if again := got.normalized(); again != got {
			t.Fatalf("normalized is not idempotent: %+v then %+v", got, again)
		}
		if got.HashThreads != tc.hash || got.UploadThreads != tc.upload {
			t.Fatalf("%+v", got)
		}
	}
}

func TestPreparedFileRejectsChangedSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, []byte("photo"), 0600); err != nil {
		t.Fatal(err)
	}
	item := UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}
	p := prepareUpload(context.Background(), item, UploadOptions{}, 0, NopReporter{})
	if p.err != nil || len(p.files) != 1 || len(p.files[0].hash) != 20 {
		t.Fatalf("%+v", p)
	}
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uploadSingleFile(context.Background(), &Api{}, p.files[0], UploadOptions{}, 0, NopReporter{}); err == nil {
		t.Fatal("uploaded with a stale hash")
	}
}

func TestCancelledPreparationStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := make([]UploadWorkItem, 40)
	for i := range items {
		items[i] = UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: "missing.jpg"}}
	}
	ready := prepareUploads(ctx, nil, items, UploadOptions{HashThreads: 8, UploadThreads: 2, StageLocal: true}, nil, nil, NopReporter{}, nil)
	if cap(ready) != 2 { // staged copies keep the queue bounded
		t.Fatalf("queue capacity=%d", cap(ready))
	}
	for p := range ready {
		// Items never started must not be reported as failures.
		t.Fatalf("canceled run started %+v", p.item)
	}
}

// Without --stage-local, hash workers never wait for the upload side: all
// items are hashed and handed off even if nothing reads the queue yet.
func TestHashWorkersDoNotWaitForUploadWorkers(t *testing.T) {
	items := make([]UploadWorkItem, 40)
	dir := t.TempDir()
	for i := range items {
		path := filepath.Join(dir, fmt.Sprintf("%02d.jpg", i))
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
		items[i] = UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}
	}
	reporter := &finishRecorder{}
	ready := prepareUploads(context.Background(), nil, items, UploadOptions{HashThreads: 4, UploadThreads: 1}, nil, nil, reporter, nil)
	deadline := time.Now().Add(5 * time.Second)
	for reporter.finished.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatal("hash workers waited for the upload side")
		}
		time.Sleep(10 * time.Millisecond)
	}
	count := 0
	for range ready {
		count++
	}
	if count != len(items) {
		t.Fatalf("got %d items", count)
	}
}

// finishRecorder counts hash workers that reported they are finished.
type finishRecorder struct {
	NopReporter
	finished atomic.Int32
}

func (r *finishRecorder) ThreadStatus(s ThreadStatus) {
	if s.Status == "idle" && s.Message == "Finished" {
		r.finished.Add(1)
	}
}

type statusRecorder struct {
	NopReporter
	mu   sync.Mutex
	last map[int]string
}

func (r *statusRecorder) ThreadStatus(s ThreadStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[s.WorkerID] = s.Status
}

func TestHashWorkersReturnToIdle(t *testing.T) {
	dir := t.TempDir()
	items := make([]UploadWorkItem, 5)
	for i := range items {
		path := filepath.Join(dir, string(rune('a'+i))+".jpg")
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
		items[i] = UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}
	}
	reporter := &statusRecorder{last: make(map[int]string)}
	for p := range prepareUploads(context.Background(), nil, items, UploadOptions{HashThreads: 3, UploadThreads: 2}, nil, nil, reporter, nil) {
		if p.err != nil {
			t.Fatal(p.err)
		}
	}
	for id := 2; id < 5; id++ {
		if status := reporter.last[id]; status != "idle" {
			t.Fatalf("hash worker %d left as %q", id, status)
		}
	}
}

func TestPreparationReportsHashErrors(t *testing.T) {
	item := UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: filepath.Join(t.TempDir(), "missing.jpg")}}
	if p := prepareUpload(context.Background(), item, UploadOptions{}, 0, NopReporter{}); !errors.Is(p.err, os.ErrNotExist) {
		t.Fatalf("err=%v", p.err)
	}
}

// hashFinder reports every hash in known as already in the library.
type hashFinder struct {
	mu    sync.Mutex
	known map[string]string
	calls int
}

func (f *hashFinder) FindRemoteMediaByHash(_ context.Context, hash []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.known[string(hash)], nil
}

func TestHashWorkersFinishDuplicates(t *testing.T) {
	dir := t.TempDir()
	items := make([]UploadWorkItem, 6)
	finder := &hashFinder{known: make(map[string]string)}
	for i := range items {
		path := filepath.Join(dir, string(rune('a'+i))+".jpg")
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
		items[i] = UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}
		if i%2 == 0 {
			hash, err := CalculateSHA1(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			finder.known[string(hash)] = "key-" + path
		}
	}
	var mu sync.Mutex
	finished := map[int]bool{}
	finish := func(workerID int, p preparedUpload) {
		mu.Lock()
		defer mu.Unlock()
		if workerID < 2 {
			t.Errorf("duplicate finished on upload worker id %d", workerID)
		}
		if p.files[0].remoteKey != "key-"+p.files[0].source {
			t.Errorf("remoteKey=%q", p.files[0].remoteKey)
		}
		finished[len(finished)] = true
	}
	queued := 0
	for p := range prepareUploads(context.Background(), nil, items, UploadOptions{HashThreads: 3, UploadThreads: 2}, nil, finder, NopReporter{}, finish) {
		queued++
		if p.err != nil || !p.files[0].checked || p.files[0].remoteKey != "" {
			t.Fatalf("queued %+v", p.files)
		}
	}
	if len(finished) != 3 || queued != 3 || finder.calls != 6 {
		t.Fatalf("finished=%d queued=%d calls=%d", len(finished), queued, finder.calls)
	}
}

func TestForceUploadSkipsDuplicateCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jpg")
	if err := os.WriteFile(path, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	finder := &hashFinder{}
	items := []UploadWorkItem{{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}}
	for p := range prepareUploads(context.Background(), nil, items, UploadOptions{ForceUpload: true, HashThreads: 1, UploadThreads: 1}, nil, finder, NopReporter{}, nil) {
		if p.err != nil || p.files[0].checked {
			t.Fatalf("%+v", p)
		}
	}
	if finder.calls != 0 {
		t.Fatalf("calls=%d", finder.calls)
	}
}

// Live Photo pairs are checked on hash workers: a pair whose still is in the
// library is finished there (skipped, no upload worker), a new pair is
// queued with both keys known, and with --update-existing-photos-to-live a
// pair with a known still is queued for reconciling.
func TestHashWorkersCheckLivePhotoPairs(t *testing.T) {
	dir := t.TempDir()
	pairs := make([]LivePhotoPair, 2)
	for i := range pairs {
		pairs[i] = LivePhotoPair{
			PhotoPath: filepath.Join(dir, fmt.Sprintf("IMG_%d.HEIC", i)),
			VideoPath: filepath.Join(dir, fmt.Sprintf("IMG_%d.MOV", i)),
		}
		for _, p := range []string{pairs[i].PhotoPath, pairs[i].VideoPath} {
			if err := os.WriteFile(p, []byte(p), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	finder := &hashFinder{known: make(map[string]string)}
	still, err := CalculateSHA1(context.Background(), pairs[0].PhotoPath)
	if err != nil {
		t.Fatal(err)
	}
	finder.known[string(still)] = "existing-still"
	items := []UploadWorkItem{
		{Kind: UploadWorkLivePhoto, LivePhoto: &pairs[0]},
		{Kind: UploadWorkLivePhoto, LivePhoto: &pairs[1]},
	}
	instantRechecks(t)
	for _, update := range []bool{false, true} {
		var mu sync.Mutex
		finished := 0
		finish := func(_ int, p preparedUpload) {
			mu.Lock()
			defer mu.Unlock()
			finished++
			if p.item.LivePhoto != &pairs[0] || p.files[0].remoteKey != "existing-still" {
				t.Errorf("finished %+v", p.item)
			}
		}
		opts := UploadOptions{HashThreads: 2, UploadThreads: 1, UpdateExistingPhotosToLive: update}
		var queued []preparedUpload
		for p := range prepareUploads(context.Background(), nil, items, opts, nil, finder, NopReporter{}, finish) {
			queued = append(queued, p)
			if !p.files[0].checked || !p.files[1].checked {
				t.Fatalf("queued pair not checked: %+v", p.files)
			}
		}
		if wantFinished := map[bool]int{false: 1, true: 0}[update]; finished != wantFinished || len(queued) != 2-wantFinished {
			t.Fatalf("update=%v: finished=%d queued=%d", update, finished, len(queued))
		}
	}
}

// A pair finished on a hash worker is reported as skipped without a single
// further request: lookupAPI has no upload or commit methods.
func TestCheckedLivePhotoSkipNeedsNoRequests(t *testing.T) {
	dir := t.TempDir()
	pair := LivePhotoPair{PhotoPath: filepath.Join(dir, "IMG.HEIC"), VideoPath: filepath.Join(dir, "IMG.MOV")}
	for _, p := range []string{pair.PhotoPath, pair.VideoPath} {
		if err := os.WriteFile(p, []byte(p), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prepared := prepareUpload(context.Background(), UploadWorkItem{Kind: UploadWorkLivePhoto, LivePhoto: &pair}, UploadOptions{}, 0, NopReporter{})
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
	photo, video := prepared.files[0], prepared.files[1]
	photo.checked, photo.remoteKey = true, "still"
	video.checked = true
	api := lookupAPI{lookup: func([]byte) (string, error) {
		t.Fatal("looked up again")
		return "", nil
	}}
	_, skipped, err := uploadLivePhotoWithCallback(context.Background(), api, pair, photo, video, LivePhotoUploadOptions{}, 0, NopReporter{})
	if err != nil || !skipped {
		t.Fatalf("skipped=%v err=%v", skipped, err)
	}
}
