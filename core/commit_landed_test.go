package core

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"app/generated"

	"google.golang.org/protobuf/proto"
)

// commitLibrary answers commits with commitStatus and hash lookups with
// lookupKey, counting each.
func commitLibrary(t *testing.T, commitStatus int, lookupKey string) (*Api, *int, *int) {
	t.Helper()
	commits, lookups := 0, 0
	match, err := proto.Marshal(&generated.RemoteMatches{Field1: &generated.RemoteMatchesField1Type{
		Field2: &generated.RemoteMatchesField1TypeField2Type{
			Field2: &generated.RemoteMatchesField1TypeField2TypeField2Type{MediaKey: lookupKey},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	api := &Api{
		policy:            mustPolicy(t, 3, true, 0),
		commitRetryConfig: &RetryConfig{MaxRetries: 3},
		authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/16538846908252377752") {
				commits++
				return statusResponse(commitStatus, http.Header{"Retry-After": []string{"0"}}), nil
			}
			lookups++
			body := match
			if lookupKey == "" {
				body = nil
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
		})},
	}
	return api, &commits, &lookups
}

// A commit that returns 429 may still have stored the item (real run,
// 2026-10-16: 94 such "failures" were all in the library). The lookup after
// the failure must report it as uploaded and must not commit it again.
func TestFailedCommitThatLandedIsASuccess(t *testing.T) {
	api, commits, _ := commitLibrary(t, http.StatusTooManyRequests, "landed")
	key, err := api.CommitUpload(context.Background(), &generated.CommitToken{}, "a.jpg", make([]byte, 20), 1)
	if err != nil || key != "landed" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	if *commits != 1 {
		t.Fatalf("committed %d times; a landed item must not be committed again", *commits)
	}
}

func TestFailedCommitThatDidNotLandStillFails(t *testing.T) {
	api, commits, _ := commitLibrary(t, http.StatusTooManyRequests, "")
	if _, err := api.CommitUpload(context.Background(), &generated.CommitToken{}, "a.jpg", make([]byte, 20), 1); err == nil {
		t.Fatal("reported success without the item in the library")
	}
	if *commits != 4 {
		t.Fatalf("commits=%d, want 1 + 3 retries", *commits)
	}
}
