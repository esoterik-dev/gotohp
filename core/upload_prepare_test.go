package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
	ready := prepareUploads(ctx, nil, items, UploadOptions{HashThreads: 8, UploadThreads: 2}, nil, NopReporter{}, nil)
	if cap(ready) != 2 {
		t.Fatalf("queue capacity=%d", cap(ready))
	}
	for p := range ready {
		// Items never started must not be reported as failures.
		t.Fatalf("canceled run started %+v", p.item)
	}
}

func TestCancelMidRunReportsOnlyStartedItems(t *testing.T) {
	dir := t.TempDir()
	items := make([]UploadWorkItem, 40)
	for i := range items {
		path := filepath.Join(dir, fmt.Sprintf("%02d.jpg", i))
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
		items[i] = UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := prepareUploads(ctx, nil, items, UploadOptions{HashThreads: 4, UploadThreads: 2}, nil, NopReporter{}, nil)
	count := 0
	for p := range ready {
		count++
		p.cleanup()
		if count == 3 {
			cancel()
		}
	}
	// At most the queue plus one item per hash worker was in flight.
	if count < 3 || count > 3+2+4 {
		t.Fatalf("got %d results after cancel", count)
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
	for p := range prepareUploads(context.Background(), nil, items, UploadOptions{HashThreads: 3, UploadThreads: 2}, nil, reporter, nil) {
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
	for p := range prepareUploads(context.Background(), nil, items, UploadOptions{HashThreads: 3, UploadThreads: 2}, finder, NopReporter{}, finish) {
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
	for p := range prepareUploads(context.Background(), nil, items, UploadOptions{ForceUpload: true, HashThreads: 1, UploadThreads: 1}, finder, NopReporter{}, nil) {
		if p.err != nil || p.files[0].checked {
			t.Fatalf("%+v", p)
		}
	}
	if finder.calls != 0 {
		t.Fatalf("calls=%d", finder.calls)
	}
}
