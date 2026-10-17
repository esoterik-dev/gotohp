package core

import (
	"context"
	"errors"
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

func TestRecheckFindsLaggingComponent(t *testing.T) {
	instantRechecks(t)
	calls := 0
	api := lookupAPI{lookup: func(hash []byte) (string, error) {
		calls++
		if string(hash) != "photo" {
			t.Fatalf("rechecked the component that was found: %s", hash)
		}
		if calls == 1 {
			return "", nil
		}
		return "existing-photo", nil
	}}
	photo, video, err := recheckMissingLivePhotoComponent(context.Background(), api, photoHash, videoHash, "", "existing-video")
	if err != nil || photo != "existing-photo" || video != "existing-video" || calls != 2 {
		t.Fatalf("photo=%q video=%q calls=%d err=%v", photo, video, calls, err)
	}
}

func TestNewPairIsNotRechecked(t *testing.T) {
	instantRechecks(t)
	api := lookupAPI{lookup: func([]byte) (string, error) {
		t.Fatal("a pair with both components missing must not be rechecked")
		return "", nil
	}}
	for _, keys := range [][2]string{{"", ""}, {"photo", "video"}} {
		photo, video, err := recheckMissingLivePhotoComponent(context.Background(), api, photoHash, videoHash, keys[0], keys[1])
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
	if _, _, err := recheckMissingLivePhotoComponent(context.Background(), api, photoHash, videoHash, "photo", ""); !errors.Is(err, expected) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := recheckMissingLivePhotoComponent(ctx, api, photoHash, videoHash, "photo", ""); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
