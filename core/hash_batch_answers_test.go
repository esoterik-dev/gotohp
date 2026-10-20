package core

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// batchRequestHashes reads the hashes of a HashCheck request built by
// encodeHashBatch.
func batchRequestHashes(t *testing.T, body []byte) [][]byte {
	var hashes [][]byte
	err := eachField(body, func(num protowire.Number, v []byte) error {
		return eachField(v, func(num protowire.Number, v []byte) error {
			if num != 1 {
				return nil
			}
			return eachField(v, func(num protowire.Number, sha []byte) error {
				if num == 1 {
					hashes = append(hashes, sha)
				}
				return nil
			})
		})
	})
	if err != nil {
		t.Error(err)
	}
	return hashes
}

// A reply that leaves one hash out fails only that file's duplicate check;
// the other files in the same request keep the answers they got. Runs a
// whole upload against a real Api whose transport fakes the lookup endpoint.
func TestBatchReplyMissingOneHashFailsOnlyThatFile(t *testing.T) {
	paths := writePhotos(t, t.TempDir(), 8)
	known := map[string]string{}
	var lost []byte
	for i, p := range paths {
		hash, err := CalculateSHA1(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		known[string(hash)] = "key-" + p
		if i == 3 {
			lost = hash
		}
	}
	var mu sync.Mutex
	lostBatch, others := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/5084965799730810217") {
			mu.Lock()
			others++
			mu.Unlock()
			return statusResponse(500, nil), nil
		}
		body, _ := io.ReadAll(r.Body)
		hashes := batchRequestHashes(t, body)
		var answered [][]byte
		for _, h := range hashes {
			if bytes.Equal(h, lost) {
				mu.Lock()
				lostBatch = len(hashes)
				mu.Unlock()
				continue
			}
			answered = append(answered, h)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(bytes.NewReader(batchResponse(known, answered)))}, nil
	})
	rec := newRunRecorder()
	m := NewUploadManager(rec, nil)
	m.newAPI = func(ApiOptions) (uploadAPI, error) {
		return &Api{policy: mustPolicy(t, 0, false, 0), client: &http.Client{Transport: transport},
			authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"}}, nil
	}
	m.Upload(paths, UploadOptions{HashThreads: 4, UploadThreads: 1})
	rec.wait(t)

	if lostBatch < 2 {
		t.Fatalf("the unanswered hash was sent in a request of %d, want it batched with others", lostBatch)
	}
	ok, skipped, failed := countResults(rec.results)
	if ok != 0 || skipped != 7 || failed != 1 || others != 0 {
		t.Fatalf("succeeded=%d skipped=%d failed=%d other requests=%d", ok, skipped, failed, others)
	}
	for _, r := range rec.results {
		if r.IsError && (r.Path != paths[3] || !strings.Contains(r.ErrorMessage, "no answer")) {
			t.Fatalf("wrong failure: %+v", r)
		}
		if r.Skipped && r.MediaKey != "key-"+r.Path {
			t.Fatalf("wrong media key: %+v", r)
		}
	}
}
