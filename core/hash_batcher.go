package core

import (
	"context"
	"time"
)

// batchHashFinder is implemented by APIs that look up many hashes per
// request (see Api.FindRemoteMediaByHashes).
type batchHashFinder interface {
	FindRemoteMediaByHashes(ctx context.Context, hashes [][]byte) (map[string]RemoteMediaMatch, error)
}

// Batching limits: a batch is sent when it holds hashBatchMax lookups or
// hashBatchWait after its first one, whichever comes first.
const (
	hashBatchMax  = 50
	hashBatchWait = 20 * time.Millisecond
)

type hashLookup struct {
	hash  []byte
	reply chan<- hashLookupReply
}

type hashLookupReply struct {
	key string
	err error
}

// hashBatcher answers single-hash lookups (it is a remoteHashFinder) by
// sending lookups that arrive within a short window in one request, such as
// the hash workers' first files or their checks after a 429 pause. Each
// batch is sent at once, without waiting for earlier requests: a lookup
// never queues behind another batch, so checks run at full speed. At most
// one request per hash worker is in flight, since each has one lookup.
type hashBatcher struct {
	ctx  context.Context
	api  batchHashFinder
	max  int
	wait time.Duration
	reqs chan hashLookup
}

// newHashBatcher starts a batcher that runs until ctx ends.
func newHashBatcher(ctx context.Context, api batchHashFinder, max int, wait time.Duration) *hashBatcher {
	b := &hashBatcher{ctx: ctx, api: api, max: max, wait: wait, reqs: make(chan hashLookup)}
	go b.run()
	return b
}

// FindRemoteMediaByHash returns hash's media key, or "" when it is not in
// the library.
func (b *hashBatcher) FindRemoteMediaByHash(ctx context.Context, hash []byte) (string, error) {
	reply := make(chan hashLookupReply, 1)
	select {
	case b.reqs <- hashLookup{hash: hash, reply: reply}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-b.ctx.Done():
		return "", b.ctx.Err()
	}
	select {
	case r := <-reply:
		return r.key, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (b *hashBatcher) run() {
	for {
		var batch []hashLookup
		select {
		case first := <-b.reqs:
			batch = append(batch, first)
		case <-b.ctx.Done():
			return
		}
		timer := time.NewTimer(b.wait)
	collect:
		for len(batch) < b.max {
			select {
			case next := <-b.reqs:
				batch = append(batch, next)
			case <-timer.C:
				break collect
			case <-b.ctx.Done():
				break collect
			}
		}
		timer.Stop()
		go b.send(batch)
	}
}

// send looks a batch up in one request and answers every lookup in it.
func (b *hashBatcher) send(batch []hashLookup) {
	seen := make(map[string]bool, len(batch))
	hashes := make([][]byte, 0, len(batch))
	for _, l := range batch {
		if !seen[string(l.hash)] {
			seen[string(l.hash)] = true
			hashes = append(hashes, l.hash)
		}
	}
	matches, err := b.api.FindRemoteMediaByHashes(b.ctx, hashes)
	for _, l := range batch {
		if err != nil {
			l.reply <- hashLookupReply{err: err}
			continue
		}
		l.reply <- hashLookupReply{key: matches[string(l.hash)].PrimaryMediaKey}
	}
}
