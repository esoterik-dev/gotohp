package core

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"app/generated"

	"google.golang.org/protobuf/proto"
)

func TestRemoteDuplicateIsSkippedWithItsMediaKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, []byte("photo"), 0600); err != nil {
		t.Fatal(err)
	}
	match, err := proto.Marshal(&generated.RemoteMatches{Field1: &generated.RemoteMatchesField1Type{
		Field2: &generated.RemoteMatchesField1TypeField2Type{
			Field2: &generated.RemoteMatchesField1TypeField2TypeField2Type{MediaKey: "existing"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	api := &Api{
		authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(match))}, nil
		})},
	}
	prepared := prepareUpload(context.Background(), UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}, UploadOptions{}, 0, NopReporter{})
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
	key, skipped, err := uploadSingleFile(context.Background(), api, prepared.files[0], UploadOptions{}, 0, NopReporter{})
	if err != nil || !skipped || key != "existing" {
		t.Fatalf("key=%q skipped=%v err=%v", key, skipped, err)
	}
	if requests != 1 {
		t.Fatalf("made %d requests; a duplicate must not be uploaded", requests)
	}
}
