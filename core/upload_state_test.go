package core

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"app/generated"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func stateFixture(t *testing.T) (UploadOptions, preparedUpload) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(path, []byte("photo"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := UploadOptions{Api: ApiOptions{Account: "one@example.test"}, StateFile: filepath.Join(dir, "state.json"), UpdateState: true}
	item := UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}}
	prepared := prepareUpload(context.Background(), item, opts, 0, NopReporter{})
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
	return opts, prepared
}

func mustOpenState(t *testing.T, opts UploadOptions) *uploadState {
	t.Helper()
	s, err := openUploadState(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}

func TestUploadStateCheckpointScopeAndInvalidation(t *testing.T) {
	opts, prepared := stateFixture(t)
	item := prepared.item
	state := mustOpenState(t, opts)
	if err := state.save(prepared, "remote", false); err != nil {
		t.Fatal(err)
	}
	if key := state.lookup(item, opts); key != "remote" {
		t.Fatal(key)
	}
	if _, err := openUploadState(opts); err == nil {
		t.Fatal("allowed two writers")
	}
	state.close()

	readOnly := opts
	readOnly.UpdateState = false
	state = mustOpenState(t, readOnly)
	if key := state.lookup(item, readOnly); key != "remote" {
		t.Fatal("checkpoint not persisted")
	}
	for _, changed := range []UploadOptions{
		{Api: ApiOptions{Account: "two@example.test"}, StateFile: opts.StateFile},
		{Api: ApiOptions{Account: opts.Api.Account, Saver: true}, StateFile: opts.StateFile},
		{Api: ApiOptions{Account: opts.Api.Account, UseQuota: true}, StateFile: opts.StateFile},
	} {
		if key := mustOpenState(t, changed).lookup(item, changed); key != "" {
			t.Fatal("cross-policy cache hit")
		}
	}
	force := readOnly
	force.ForceUpload = true
	if state.lookup(item, force) != "" {
		t.Fatal("force bypassed")
	}
	revalidate := readOnly
	revalidate.RevalidateState = true
	if state.lookup(item, revalidate) != "" {
		t.Fatal("revalidation bypassed")
	}
	if err := os.WriteFile(uploadWorkPrimaryPath(item), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if state.lookup(item, readOnly) != "" {
		t.Fatal("stale source cache hit")
	}
}

func TestLeftoverLockFileDoesNotBlock(t *testing.T) {
	opts, _ := stateFixture(t)
	if err := os.WriteFile(opts.StateFile+".lock", []byte("12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustOpenState(t, opts) // A lock file without a live holder is not a lock.
}

func TestStateIsAppendedAndCompacted(t *testing.T) {
	opts, prepared := stateFixture(t)
	state := mustOpenState(t, opts)
	for _, key := range []string{"first", "second", "third"} {
		if err := state.save(prepared, key, false); err != nil {
			t.Fatal(err)
		}
	}
	state.close()
	data, _ := os.ReadFile(opts.StateFile)
	if lines := strings.Count(string(data), "\n"); lines != 4 {
		t.Fatalf("want header + 3 appended lines, got %d:\n%s", lines, data)
	}
	state = mustOpenState(t, opts)
	if key := state.lookup(prepared.item, opts); key != "third" {
		t.Fatalf("last entry should win, got %q", key)
	}
	data, _ = os.ReadFile(opts.StateFile)
	if lines := strings.Count(string(data), "\n"); lines != 2 {
		t.Fatalf("open did not compact:\n%s", data)
	}
}

func TestTruncatedLastLineIsIgnored(t *testing.T) {
	opts, prepared := stateFixture(t)
	state := mustOpenState(t, opts)
	if err := state.save(prepared, "remote", false); err != nil {
		t.Fatal(err)
	}
	state.close()
	f, err := os.OpenFile(opts.StateFile, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"key":"cut off by a cra`)
	_ = f.Close()
	if key := mustOpenState(t, opts).lookup(prepared.item, opts); key != "remote" {
		t.Fatalf("confirmed entry lost: %q", key)
	}
}

func TestStateCheckpointFailureRollsBack(t *testing.T) {
	opts, prepared := stateFixture(t)
	s := mustOpenState(t, opts)
	_ = s.log.Close() // Every write now fails.
	if err := s.save(prepared, "remote", false); err == nil {
		t.Fatal("checkpoint should fail")
	}
	if s.lookup(prepared.item, opts) != "" {
		t.Fatal("failed checkpoint became a cache hit")
	}
}

func TestReplacementWithSameMetadataCannotBeCheckpointed(t *testing.T) {
	opts, prepared := stateFixture(t)
	s := mustOpenState(t, opts)
	path := uploadWorkPrimaryPath(prepared.item)
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	mtime := prepared.files[0].info.ModTime()
	if err := os.Chtimes(replacement, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := s.save(prepared, "remote", false); err == nil {
		t.Fatal("checkpointed a different file with identical metadata")
	}
	if err := removePreparedSources(prepared); err == nil {
		t.Fatal("deleted the replacement")
	}
}

func TestStateReadOnlyAndIncompletePair(t *testing.T) {
	opts, prepared := stateFixture(t)
	s := mustOpenState(t, opts)
	pair := prepared
	pair.item.Kind = UploadWorkLivePhoto
	if err := s.save(pair, "still-only", true); err != nil {
		t.Fatal(err)
	}
	if len(s.entries) != 0 {
		t.Fatal("cached incomplete pair")
	}
	s.close()
	_ = os.Remove(opts.StateFile)
	readOnly := opts
	readOnly.UpdateState = false
	if _, err := openUploadState(readOnly); err == nil {
		t.Fatal("read-only state accepted a missing file")
	}
}

// commitAPI answers every request: the duplicate check (finding only the
// hashes in existing), an upload session, the upload, and a commit
// returning mediaKey.
func commitAPI(t *testing.T, mediaKey string, existing ...[]byte) *Api {
	t.Helper()
	commit := createMediaItemsResponse(t, mediaKey)
	return &Api{
		model:             "Pixel XL",
		make:              "Google",
		androidAPIVersion: 28,
		authResponseCache: map[string]string{"Auth": "test", "Expiry": "9999999999"},
		client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, header := []byte{}, http.Header{}
			switch {
			case r.Method == http.MethodPut:
				body = finalizeTokenResponse(t)
			case r.URL.Host == "photos.googleapis.com":
				header.Set("X-GUploader-UploadID", "session")
			case strings.HasSuffix(r.URL.Path, "/16538846908252377752"):
				body = commit
			case strings.HasSuffix(r.URL.Path, "/5084965799730810217"):
				request, _ := io.ReadAll(r.Body)
				for _, hash := range existing {
					if bytes.Contains(request, hash) {
						body = remoteMatchResponse(t, "existing")
					}
				}
			}
			return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(bytes.NewReader(body))}, nil
		})},
	}
}

func remoteMatchResponse(t *testing.T, mediaKey string) []byte {
	t.Helper()
	data, err := proto.Marshal(&generated.RemoteMatches{Field1: &generated.RemoteMatchesField1Type{
		Field2: &generated.RemoteMatchesField1TypeField2Type{
			Field2: &generated.RemoteMatchesField1TypeField2TypeField2Type{MediaKey: mediaKey},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func pairFixture(t *testing.T) preparedUpload {
	t.Helper()
	dir := t.TempDir()
	pair := LivePhotoPair{PhotoPath: filepath.Join(dir, "IMG.HEIC"), VideoPath: filepath.Join(dir, "IMG.MOV")}
	for _, path := range []string{pair.PhotoPath, pair.VideoPath} {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prepared := prepareUpload(context.Background(), UploadWorkItem{Kind: UploadWorkLivePhoto, LivePhoto: &pair}, UploadOptions{}, 0, NopReporter{})
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
	return prepared
}

func assertDeleted(t *testing.T, prepared preparedUpload, want bool) {
	t.Helper()
	for _, file := range prepared.files {
		_, err := os.Stat(file.source)
		if deleted := os.IsNotExist(err); deleted != want {
			t.Fatalf("%s deleted=%v, want %v", file.source, deleted, want)
		}
	}
}

func TestDeleteWithoutStateIsUnchanged(t *testing.T) {
	opts, prepared := stateFixture(t)
	opts.StateFile, opts.UpdateState, opts.DeleteFromHost = "", false, true
	key, _, err := uploadPreparedItem(context.Background(), commitAPI(t, "media"), prepared, opts, nil, 0, NopReporter{})
	if err != nil || key != "media" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	if _, err := os.Stat(prepared.files[0].source); !os.IsNotExist(err) {
		t.Fatal("--delete without a state file no longer deletes")
	}
}

func TestDeleteWaitsForCheckpoint(t *testing.T) {
	opts, prepared := stateFixture(t)
	opts.DeleteFromHost = true
	state := mustOpenState(t, opts)
	_ = state.log.Close() // The checkpoint will fail.
	if _, _, err := uploadPreparedItem(context.Background(), commitAPI(t, "media"), prepared, opts, state, 0, NopReporter{}); err == nil {
		t.Fatal("checkpoint failure not reported")
	}
	if _, err := os.Stat(prepared.files[0].source); err != nil {
		t.Fatal("deleted a source without a checkpoint")
	}

	state = mustOpenState(t, UploadOptions{Api: opts.Api, StateFile: opts.StateFile + ".2", UpdateState: true})
	if _, _, err := uploadPreparedItem(context.Background(), commitAPI(t, "media"), prepared, opts, state, 0, NopReporter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.files[0].source); !os.IsNotExist(err) {
		t.Fatal("source kept after a durable checkpoint")
	}
}

func finalizeTokenResponse(t *testing.T) []byte {
	t.Helper()
	raw := protowire.AppendTag(nil, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, scottyFinalizeTokenVersion)
	raw = protowire.AppendTag(raw, 2, protowire.BytesType)
	return protowire.AppendBytes(raw, []byte("opaque"))
}

func createMediaItemsResponse(t *testing.T, mediaKey string) []byte {
	t.Helper()
	data, err := proto.Marshal(&generated.CreateMediaItemsResponse{Item: []*generated.CreateMediaItemResponseItem{
		{ResultItem: &generated.CreateMediaItemResult{MediaKey: mediaKey}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDeleteBehaviourForPairsAndReconcile(t *testing.T) {
	instantRechecks(t)
	for _, withState := range []bool{false, true} {
		for _, reconcile := range []bool{false, true} {
			prepared := pairFixture(t)
			opts := UploadOptions{DeleteFromHost: true, UpdateExistingPhotosToLive: reconcile}
			var state *uploadState
			if withState {
				opts.StateFile, opts.UpdateState = filepath.Join(t.TempDir(), "state.json"), true
				state = mustOpenState(t, opts)
			}
			var existing [][]byte
			if reconcile {
				existing = append(existing, prepared.files[0].hash) // The still is already in the library.
			}
			key, skipped, err := uploadPreparedItem(context.Background(), commitAPI(t, "live", existing...), prepared, opts, state, 0, NopReporter{})
			if err != nil || key != "live" || skipped {
				t.Fatalf("state=%v reconcile=%v: key=%q skipped=%v err=%v", withState, reconcile, key, skipped, err)
			}
			assertDeleted(t, prepared, true)
			if withState && state.entries[state.key(prepared.item)].MediaKey != "live" {
				t.Fatalf("reconcile=%v: pair not checkpointed", reconcile)
			}
		}
	}
}
