package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// lookupAPI answers hash lookups from a function; the other methods of
// livePhotoUploadAPI are never reached by these tests.
type lookupAPI struct {
	livePhotoUploadAPI
	lookup func(hash []byte) (string, error)
}

func (a lookupAPI) FindRemoteMediaByHash(_ context.Context, hash []byte) (string, error) {
	return a.lookup(hash)
}

func instantRechecks(t *testing.T) {
	t.Helper()
	saved := livePhotoRecheckDelays
	livePhotoRecheckDelays = []time.Duration{0, 0}
	t.Cleanup(func() { livePhotoRecheckDelays = saved })
}

var (
	photoHash = []byte("photo")
	videoHash = []byte("video")
)

func TestRecheckFindsAComponentOfANewPair(t *testing.T) {
	instantRechecks(t)
	calls := 0
	api := lookupAPI{lookup: func(hash []byte) (string, error) {
		calls++
		if calls == 3 && string(hash) == "photo" {
			return "existing-photo", nil // the second round sees it
		}
		return "", nil
	}}
	photo, video, err := recheckNewLivePhotoPair(context.Background(), api, photoHash, videoHash, "", "")
	if err != nil || photo != "existing-photo" || video != "" || calls != 4 {
		t.Fatalf("photo=%q video=%q calls=%d err=%v", photo, video, calls, err)
	}
}

func TestPairWithAFoundComponentIsNotRechecked(t *testing.T) {
	instantRechecks(t)
	api := lookupAPI{lookup: func([]byte) (string, error) {
		t.Fatal("nothing new would be created, so nothing should be rechecked")
		return "", nil
	}}
	for _, keys := range [][2]string{{"photo", ""}, {"", "video"}, {"photo", "video"}} {
		photo, video, err := recheckNewLivePhotoPair(context.Background(), api, photoHash, videoHash, keys[0], keys[1])
		if err != nil || photo != keys[0] || video != keys[1] {
			t.Fatalf("keys changed: %q %q %v", photo, video, err)
		}
	}
}

func TestRecheckStopsOnErrorOrCancellation(t *testing.T) {
	instantRechecks(t)
	expected := errors.New("lookup failed")
	calls := 0
	api := lookupAPI{lookup: func([]byte) (string, error) { calls++; return "", expected }}
	if _, _, err := recheckNewLivePhotoPair(context.Background(), api, photoHash, videoHash, "", ""); !errors.Is(err, expected) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := recheckNewLivePhotoPair(ctx, api, photoHash, videoHash, "", ""); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

// The outcome that matters: a pair whose still photo only shows up on the recheck
// is skipped, not uploaded as a duplicate Live Photo. lookupAPI has no upload
// or commit methods, so reaching them panics.
func TestLaggingStillPreventsADuplicateLivePhoto(t *testing.T) {
	instantRechecks(t)
	dir := t.TempDir()
	pair := LivePhotoPair{PhotoPath: filepath.Join(dir, "IMG_1.HEIC"), VideoPath: filepath.Join(dir, "IMG_1.MOV")}
	for _, p := range []string{pair.PhotoPath, pair.VideoPath} {
		if err := os.WriteFile(p, []byte(p), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	api := lookupAPI{lookup: func(hash []byte) (string, error) {
		calls++
		if calls > 2 && len(hash) == 20 && calls%2 == 1 {
			return "existing-photo", nil
		}
		return "", nil
	}}
	prepared := prepareUpload(context.Background(), UploadWorkItem{Kind: UploadWorkLivePhoto, LivePhoto: &pair}, UploadOptions{}, 0, NopReporter{})
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
	_, skipped, err := uploadLivePhotoWithCallback(context.Background(), api, pair, prepared.files[0], prepared.files[1], LivePhotoUploadOptions{}, 0, NopReporter{})
	if err != nil || !skipped {
		t.Fatalf("skipped=%v err=%v", skipped, err)
	}
}
