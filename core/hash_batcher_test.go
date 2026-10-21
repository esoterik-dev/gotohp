package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func msg(fields ...[]byte) []byte { return bytes.Join(fields, nil) }

func field(num protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), v)
}

// batchResponse builds a reply shaped like the live endpoint's (2026-10-18):
// one entry per hash; a hash not in the library has no match part.
func batchResponse(entries map[string]string, order [][]byte) []byte {
	var inner [][]byte
	for _, h := range order {
		entry := field(1, field(1, h))
		if key := entries[string(h)]; key != "" {
			entry = append(entry, field(2, msg(field(1, []byte(key)), field(6, field(1, []byte(key)))))...)
		}
		inner = append(inner, field(2, entry))
	}
	return field(1, msg(inner...))
}

func TestDecodeHashBatch(t *testing.T) {
	a, b, u := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20), bytes.Repeat([]byte{3}, 20)
	body := batchResponse(map[string]string{string(a): "AF1QipA", string(b): "AF1QipB"}, [][]byte{a, b, u})
	got, err := decodeHashBatch(body)
	if err != nil {
		t.Fatal(err)
	}
	if got[string(a)].PrimaryMediaKey != "AF1QipA" || got[string(a)].RelatedMediaKey != "AF1QipA" ||
		got[string(b)].PrimaryMediaKey != "AF1QipB" || got[string(u)].PrimaryMediaKey != "" {
		t.Fatalf("got %+v", got)
	}
	// A hash the server did not answer must be absent, never an empty
	// "not found" match: treating it as new would upload a duplicate.
	got, err = decodeHashBatch(batchResponse(nil, [][]byte{a}))
	if _, answered := got[string(b)]; err != nil || answered {
		t.Fatalf("unanswered hash in result: %+v, %v", got, err)
	}
	if !bytes.Contains(encodeHashBatch([][]byte{a, b}), a) {
		t.Fatal("request lacks a hash")
	}
}

// countingBatcher answers batches from known and counts requests.
type countingBatcher struct {
	mu      sync.Mutex
	calls   int
	sizes   []int
	known   map[string]string
	err     error
	release chan struct{}
}

func (c *countingBatcher) FindRemoteMediaByHashes(_ context.Context, hashes [][]byte) (map[string]RemoteMediaMatch, error) {
	if c.release != nil {
		<-c.release
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.sizes = append(c.sizes, len(hashes))
	if c.err != nil {
		return nil, c.err
	}
	out := make(map[string]RemoteMediaMatch)
	for _, h := range hashes {
		out[string(h)] = RemoteMediaMatch{PrimaryMediaKey: c.known[string(h)]}
	}
	return out, nil
}

func TestBatcherGroupsConcurrentLookups(t *testing.T) {
	api := &countingBatcher{known: map[string]string{"h0": "k0", "h3": "k3"}}
	b := newHashBatcher(t.Context(), api, 8, 200*time.Millisecond)
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := b.FindRemoteMediaByHash(context.Background(), []byte(fmt.Sprintf("h%d", i)))
			if err != nil {
				t.Error(err)
			}
			got[i] = key
		}()
	}
	wg.Wait()
	if api.calls != 1 || api.sizes[0] != 8 {
		t.Fatalf("calls=%d sizes=%v, want one batch of 8", api.calls, api.sizes)
	}
	if got[0] != "k0" || got[3] != "k3" || got[1] != "" {
		t.Fatalf("answers %v", got)
	}
}

func TestBatcherSendsAfterWaitAndSharesErrors(t *testing.T) {
	api := &countingBatcher{err: errors.New("quota")}
	b := newHashBatcher(t.Context(), api, 50, 10*time.Millisecond)
	start := time.Now()
	if _, err := b.FindRemoteMediaByHash(context.Background(), []byte("x")); err == nil {
		t.Fatal("error lost")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a lone lookup waited for a full batch")
	}
}

func TestBatcherHonoursCancellation(t *testing.T) {
	api := &countingBatcher{release: make(chan struct{})}
	defer close(api.release)
	b := newHashBatcher(t.Context(), api, 50, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := b.FindRemoteMediaByHash(ctx, []byte("x")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

// The debug log shows how many hashes each batched lookup carried, through
// the real Api and its request policy logger.
func TestBatchLookupIsLogged(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20)
	reply := batchResponse(map[string]string{string(a): "AF1QipA"}, [][]byte{a, b})
	api := &Api{
		policy:            mustPolicy(t, 0, true, 0),
		authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(reply))}, nil
		})},
	}
	var out bytes.Buffer
	api.policy.SetLogger(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if _, err := api.FindRemoteMediaByHashes(context.Background(), [][]byte{a, b}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"msg":"hash lookup batch","hashes":2,"found":1`) {
		t.Fatalf("log: %s", out.String())
	}
}
