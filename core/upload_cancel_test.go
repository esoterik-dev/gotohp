package core

import "testing"

type albumErrorRecorder struct {
	NopReporter
	errors []AlbumError
}

func (r *albumErrorRecorder) AlbumError(e AlbumError) { r.errors = append(r.errors, e) }

// The GUI shows every AlbumError as a failure toast, so an aborted run skips
// its albums silently; the CLI reports the skipped album itself.
func TestCancelSkipsAlbumCreationSilently(t *testing.T) {
	for _, tc := range []struct {
		name      string
		albumName string
		auto      bool
	}{{"named", "Trip", false}, {"auto", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			reporter := &albumErrorRecorder{}
			m := NewUploadManager(reporter, nil)
			m.stop, m.cancel = make(chan struct{}), make(chan struct{})
			m.Cancel()
			m.handleAlbumCreation(map[string]string{"photo.jpg": "media-key"}, tc.albumName, tc.auto)
			if len(reporter.errors) != 0 {
				t.Fatalf("aborted run reported album errors: %+v", reporter.errors)
			}
		})
	}
}
