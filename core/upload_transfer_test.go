package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&uploadHTTPError{statusCode: 429}, true},
		{&uploadHTTPError{statusCode: 503}, true},
		// Body captured from a real failed upload (gp-run reports, 2026-09).
		{&uploadHTTPError{statusCode: 400, body: "Checksum from header does not match received payload content."}, true},
		{&uploadHTTPError{statusCode: 400, body: "invalid media"}, false},
		{context.Canceled, false}, {errors.New("invalid response"), false},
	} {
		if got := shouldRetryUploadError(tc.err); got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestUploadSendsExactBodyLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.ContentLength != 5 || string(body) != "photo" || len(r.TransferEncoding) != 0 {
			t.Errorf("length=%d encoding=%v body=%q", r.ContentLength, r.TransferEncoding, body)
		}
		if r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Error("missing content type")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	api := &Api{client: server.Client(), authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"}}
	_, err := api.doUploadRequest(context.Background(), server.URL, strings.NewReader("photo"), 5)
	var httpErr *uploadHTTPError
	if !errors.As(err, &httpErr) || httpErr.statusCode != 429 {
		t.Fatalf("got %v", err)
	}
}

type uploadRoundTrip func(*http.Request) (*http.Response, error)

func (f uploadRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUploadRetryOpensNewSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, []byte("photo"), 0600); err != nil {
		t.Fatal(err)
	}
	var sessions, uploads []string
	api := &Api{
		authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
		client: &http.Client{Transport: uploadRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPut {
				uploads = append(uploads, r.URL.Query().Get("upload_id"))
				body := "Checksum from header does not match received payload content."
				if len(uploads) > 1 {
					body = "invalid media" // Permanent, so the test stops after one retry.
				}
				return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			sessions = append(sessions, fmt.Sprintf("session-%d", len(sessions)+1))
			header := http.Header{"X-Guploader-Uploadid": []string{sessions[len(sessions)-1]}}
			return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	_, err := api.UploadFileWithHashAndProgress(context.Background(), path, make([]byte, 20), 5, nil)
	if err == nil {
		t.Fatal("expected the permanent error")
	}
	if len(uploads) != 2 || uploads[0] != "session-1" || uploads[1] != "session-2" {
		t.Fatalf("retries must use a new session each time: sessions=%v uploads=%v", sessions, uploads)
	}
}
