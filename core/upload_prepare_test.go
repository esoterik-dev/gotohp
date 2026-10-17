package core

import (
	"context"
	"errors"
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
		{UploadOptions{Threads: 8}, 8, 8},
		{UploadOptions{Threads: 8, HashThreads: 6, UploadThreads: 2}, 6, 2},
		{UploadOptions{}, 1, 1},
	} {
		got := tc.opts.normalized()
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

func TestCancelledPreparationDrainsBoundedQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := make([]UploadWorkItem, 40)
	for i := range items {
		items[i] = UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: "missing.jpg"}}
	}
	ready := prepareUploads(ctx, items, UploadOptions{HashThreads: 8, UploadThreads: 2}, NopReporter{})
	if cap(ready) != 2 {
		t.Fatalf("queue capacity=%d", cap(ready))
	}
	count := 0
	for p := range ready {
		count++
		p.cleanup()
		if p.err == nil {
			t.Fatal("canceled preparation succeeded")
		}
	}
	if count != len(items) {
		t.Fatalf("got %d items", count)
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
	for p := range prepareUploads(context.Background(), items, UploadOptions{HashThreads: 3, UploadThreads: 2}, reporter) {
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
