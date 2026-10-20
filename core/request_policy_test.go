package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustPolicy(t *testing.T, maxRetries int, retry429 bool, requestsPerMinute int) *RequestPolicy {
	t.Helper()
	p, err := NewRequestPolicy(maxRetries, retry429, requestsPerMinute)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func statusResponse(code int, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader("status"))}
}

func TestNewRequestPolicyValidates(t *testing.T) {
	for _, tc := range []struct{ retries, rpm int }{{-1, 0}, {101, 0}, {0, -1}, {0, 1000001}} {
		if _, err := NewRequestPolicy(tc.retries, true, tc.rpm); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{{"12", 12 * time.Second}, {now.Add(20 * time.Second).Format(http.TimeFormat), 20 * time.Second}, {now.Add(-time.Second).Format(http.TimeFormat), 0}} {
		if got := retryAfter(tc.header, now); got != tc.want {
			t.Fatalf("%q: %v", tc.header, got)
		}
	}
	if got := retryAfter("", now); got < time.Minute || got >= 65*time.Second {
		t.Fatal(got)
	}
}

func TestSharedCooldownCancelsEveryWorker(t *testing.T) {
	p := mustPolicy(t, DefaultMaxRetries, true, DefaultRequestsPerMinute)
	p.observe(statusResponse(429, http.Header{"Retry-After": []string{"60"}}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.wait(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("got %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestRequestHonoursCallerContext(t *testing.T) {
	policy := mustPolicy(t, 0, true, 0)
	policy.observe(statusResponse(429, http.Header{"Retry-After": []string{"60"}}))
	api := &Api{policy: policy, authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("request sent during cooldown")
			return statusResponse(200, nil), nil
		})}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := api.FindRemoteMediaByHash(ctx, make([]byte, 20)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline ignored: %v", err)
	}
}

// A token refresh waits behind a 429 pause like any request, so it must
// honour the caller's context too; otherwise Ctrl+C hangs for the pause.
func TestTokenRefreshHonoursCallerContext(t *testing.T) {
	policy := mustPolicy(t, 0, true, 0)
	policy.observe(statusResponse(429, http.Header{"Retry-After": []string{"60"}}))
	api := &Api{policy: policy, authResponseCache: map[string]string{"Auth": "stale", "Expiry": "0"},
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("auth request sent during cooldown")
			return statusResponse(200, nil), nil
		})}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := api.FindRemoteMediaByHash(ctx, make([]byte, 20)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline ignored: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("token refresh waited out the pause")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLookupRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		max   int
		retry bool
		want  int
	}{{0, true, 1}, {1, false, 1}, {1, true, 2}} {
		calls := 0
		api := &Api{policy: mustPolicy(t, tc.max, tc.retry, 0), authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return statusResponse(429, http.Header{"Retry-After": []string{"0"}}), nil
			})}}
		if _, err := api.FindRemoteMediaByHash(context.Background(), make([]byte, 20)); err == nil {
			t.Fatal("429 was not reported")
		}
		if calls != tc.want {
			t.Fatalf("%+v calls=%d", tc, calls)
		}
	}
}

func TestFailedLookupDoesNotUpload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, []byte("photo"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	api := &Api{policy: mustPolicy(t, 0, true, 0), authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return statusResponse(503, nil), nil
		})}}
	if _, err := UploadFile(context.Background(), api, path, UploadOptions{}, 0, nil); err == nil {
		t.Fatal("lookup failure ignored")
	}
	if calls != 1 {
		t.Fatalf("made %d requests; must not upload after failed lookup", calls)
	}
}

func TestNilPolicyKeepsDefaultRetries(t *testing.T) {
	api := &Api{}
	if got := api.retryConfig().MaxRetries; got != DefaultRetryConfig().MaxRetries {
		t.Fatalf("retries=%d", got)
	}
	if !api.shouldRetry(&httpStatusError{statusCode: 429}) {
		t.Fatal("nil policy must keep retrying 429")
	}
	if api.shouldRetry(&httpStatusError{statusCode: 403}) {
		t.Fatal("403 is permanent")
	}
}

func TestBackoffLargeAndZero(t *testing.T) {
	if got := CalculateBackoff(100, DefaultRetryConfig()); got < 30*time.Second || got > 33*time.Second {
		t.Fatal(got)
	}
	if got := CalculateBackoff(1, RetryConfig{}); got != 0 {
		t.Fatal(got)
	}
}
