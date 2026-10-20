package core

import (
	"context"
	"testing"
	"time"
)

// heldBatcher holds every request open until release is closed, and
// announces each request as it starts.
type heldBatcher struct {
	started chan int // number of hashes in each request that starts
	release chan struct{}
}

func (h *heldBatcher) FindRemoteMediaByHashes(_ context.Context, hashes [][]byte) (map[string]RemoteMediaMatch, error) {
	h.started <- len(hashes)
	<-h.release
	return map[string]RemoteMediaMatch{}, nil
}

// A lookup that arrives while another batch's request is in flight must not
// wait for that request (real run, 2026-10-18: with 2 hash workers, 334 of
// 362 batches held one hash and each check took two requests' time).
func TestBatcherDoesNotQueueBehindRequestInFlight(t *testing.T) {
	api := &heldBatcher{started: make(chan int, 2), release: make(chan struct{})}
	defer close(api.release)
	b := newHashBatcher(t.Context(), api, 8, time.Millisecond)
	lookup := func(hash string) {
		go func() { _, _ = b.FindRemoteMediaByHash(t.Context(), []byte(hash)) }()
	}
	lookup("a")
	select {
	case <-api.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never started")
	}
	lookup("b")
	select {
	case <-api.started:
	case <-time.After(2 * time.Second):
		t.Fatal("second lookup waited for the first request to finish")
	}
}
