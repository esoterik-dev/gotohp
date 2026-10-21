package core

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeMetadata answers metadata reads from tables keyed by path. A path in
// neither table has no Live Photo identifier, like an ordinary photo or video.
type fakeMetadata struct {
	photos map[string]string            // path -> content identifier
	videos map[string]LivePhotoMetadata // path -> metadata
	errs   map[string]error             // path -> read error
}

func (f fakeMetadata) PhotoContentIdentifier(path string) (string, error) {
	if err := f.errs[path]; err != nil {
		return "", err
	}
	if id, ok := f.photos[path]; ok {
		return id, nil
	}
	return "", ErrContentIdentifierMissing
}

func (f fakeMetadata) VideoLivePhotoMetadata(path string) (LivePhotoMetadata, error) {
	if err := f.errs[path]; err != nil {
		return LivePhotoMetadata{}, err
	}
	if m, ok := f.videos[path]; ok {
		return m, nil
	}
	return LivePhotoMetadata{}, ErrContentIdentifierMissing
}

// classified returns the work's paths: pairs as "photo+video", singles as is.
func classified(work []UploadWorkItem) []string {
	var out []string
	for _, item := range work {
		if item.Kind == UploadWorkLivePhoto {
			out = append(out, item.LivePhoto.PhotoPath+"+"+item.LivePhoto.VideoPath)
		} else {
			out = append(out, item.Single.Path)
		}
	}
	sort.Strings(out)
	return out
}

func warningCodes(warnings []PreflightWarning) map[string][]string {
	codes := map[string][]string{}
	for _, w := range warnings {
		codes[w.Code] = append(codes[w.Code], w.Paths...)
	}
	return codes
}

// A Live Photo file whose pairing is uncertain must never be uploaded on its
// own: a still uploaded without its video cannot be linked to it later.
// --pair-live-photos documents that incomplete pairs are skipped; these
// files are skipped the same way, with a warning, unless the user asks for
// --upload-incomplete-live-photos.
func TestUncertainLivePhotoFilesAreSkipped(t *testing.T) {
	live := LivePhotoMetadata{ContentIdentifier: "ID-OK", HasStillImageTime: true}
	meta := fakeMetadata{
		photos: map[string]string{
			"ok.HEIC":   "ID-OK",
			"amb1.HEIC": "ID-AMB", "amb2.HEIC": "ID-AMB", // e.g. an original and its edit
			"lone.HEIC": "ID-LONE",
		},
		videos: map[string]LivePhotoMetadata{
			"ok.MOV":      live,
			"amb.MOV":     {ContentIdentifier: "ID-AMB", HasStillImageTime: true},
			"nostill.MOV": {ContentIdentifier: "ID-NOSTILL"},
		},
		errs: map[string]error{
			"unreadable.HEIC": errors.New("read photo metadata: input/output error"),
			"unreadable.MOV":  errors.New("invalid QuickTime box size at 0"),
		},
	}
	paths := []string{"plain.jpg", "plain.mp4", "ok.HEIC", "ok.MOV", "amb1.HEIC", "amb2.HEIC", "amb.MOV",
		"lone.HEIC", "nostill.MOV", "unreadable.HEIC", "unreadable.MOV"}

	work, warnings := ClassifyUploadWork(paths, LivePhotoClassificationOptions{Enabled: true, SkipIncomplete: true}, meta)
	if got, want := classified(work), []string{"ok.HEIC+ok.MOV", "plain.jpg", "plain.mp4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("would upload %v, want %v", got, want)
	}
	codes := warningCodes(warnings)
	for code, want := range map[string][]string{
		"ambiguous-identifier":          {"amb1.HEIC", "amb2.HEIC", "amb.MOV"},
		"incomplete-live-photo-skipped": {"lone.HEIC"},
		"missing-still-image-time":      {"nostill.MOV"},
		"metadata-read-failed":          {"unreadable.HEIC", "unreadable.MOV"},
	} {
		got := append([]string(nil), codes[code]...)
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s warns about %v, want %v", code, got, want)
		}
	}
	// An unreadable file fails instead of being skipped, so the run reports
	// it and --retry-report tries it again once the mount behaves.
	for _, w := range warnings {
		if failed := w.Code == "metadata-read-failed"; w.Failed != failed || w.Skipped == failed {
			t.Errorf("warning %s: Failed=%v Skipped=%v", w.Code, w.Failed, w.Skipped)
		}
	}

	// --upload-incomplete-live-photos: the user asked for unmatched members
	// as single files, so the uncertain ones are uploaded singly too.
	work, warnings = ClassifyUploadWork(paths, LivePhotoClassificationOptions{Enabled: true, SkipIncomplete: false}, meta)
	want := []string{"amb.MOV", "amb1.HEIC", "amb2.HEIC", "lone.HEIC", "nostill.MOV", "ok.HEIC+ok.MOV",
		"plain.jpg", "plain.mp4", "unreadable.HEIC", "unreadable.MOV"}
	if got := classified(work); !reflect.DeepEqual(got, want) {
		t.Fatalf("with incomplete uploads allowed: %v, want %v", got, want)
	}
	for _, w := range warnings {
		if w.Skipped || w.Failed {
			t.Errorf("with incomplete uploads allowed, warning %s: Failed=%v Skipped=%v", w.Code, w.Failed, w.Skipped)
		}
	}
}

// --ignore-apple-metadata pairs by filename stem. A video with neither a
// Live Photo identifier nor the still-image-time marker, and no photo of the
// same stem, is an ordinary video: it is uploaded, as without the override.
// One that shares a stem with a photo may be half of a Live Photo, so it is
// skipped like other files that cannot be paired.
func TestFilenameOverrideUploadsOrdinaryVideos(t *testing.T) {
	meta := fakeMetadata{videos: map[string]LivePhotoMetadata{
		"IMG_2.MOV": {HasStillImageTime: true},
	}}
	paths := []string{"clip.MOV", "IMG_1.HEIC", "IMG_1.MOV", "IMG_2.HEIC", "IMG_2.MOV"}
	work, warnings := ClassifyUploadWork(paths,
		LivePhotoClassificationOptions{Enabled: true, SkipIncomplete: true, IgnoreAppleMetadata: true}, meta)
	if got, want := classified(work), []string{"IMG_2.HEIC+IMG_2.MOV", "clip.MOV"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("would upload %v, want %v", got, want)
	}
	byPath := map[string]PreflightWarning{}
	for _, w := range warnings {
		for _, path := range w.Paths {
			byPath[path] = w
		}
	}
	if w := byPath["clip.MOV"]; w.Skipped || strings.Contains(w.Message, "content identifier but") {
		t.Errorf("ordinary video warning %+v", w)
	}
	if w := byPath["IMG_1.MOV"]; !w.Skipped || w.Code != "missing-still-image-time" || strings.Contains(w.Message, "content identifier but") {
		t.Errorf("stem-matched video without marker: %+v", w)
	}
	if w := byPath["IMG_1.HEIC"]; !w.Skipped || w.Code != "incomplete-live-photo-skipped" {
		t.Errorf("photo whose video is skipped: %+v", w)
	}
}

type warningRecorder struct {
	*runRecorder
	mu       sync.Mutex
	warnings []PreflightWarning
}

func (r *warningRecorder) Warning(w PreflightWarning) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnings = append(r.warnings, w)
}

// Through the real upload pipeline: a MOV whose metadata cannot be read (a
// truncated or garbled file, as a flaky phone mount can return) is not
// uploaded as a standalone video, while an ordinary photo still is. The
// video fails rather than being skipped, so the run reports a failure and
// --retry-report retries it.
func TestUploadFailsLivePhotoVideoWithUnreadableMetadata(t *testing.T) {
	dir := t.TempDir()
	photo := filepath.Join(dir, "IMG_0001.JPG")
	video := filepath.Join(dir, "IMG_0002.MOV")
	if err := os.WriteFile(photo, []byte("ordinary photo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(video, []byte("not a QuickTime file"), 0600); err != nil {
		t.Fatal(err)
	}
	lib := newFakeLibrary()
	rec := &warningRecorder{runRecorder: newRunRecorder()}
	m := NewUploadManager(rec, nil)
	m.newAPI = func(ApiOptions) (uploadAPI, error) { return lib, nil }
	m.Upload([]string{photo, video}, UploadOptions{HashThreads: 1, UploadThreads: 1, PairLivePhotos: true, SkipIncompleteLivePhotos: true})
	rec.wait(t)
	if lib.uploads != 1 {
		t.Fatalf("uploads=%d; want only the photo", lib.uploads)
	}
	// The video still appears in the results, as failed, so the report
	// accounts for every file and a retry picks it up.
	byPath := map[string]FileUploadResult{}
	for _, r := range rec.results {
		byPath[r.Path] = r
	}
	if r := byPath[photo]; len(rec.results) != 2 || r.Skipped || r.IsError || r.MediaKey == "" {
		t.Fatalf("photo result %+v (results %+v)", r, rec.results)
	}
	if r := byPath[video]; !r.IsError || r.Skipped || r.ErrorMessage == "" || !reflect.DeepEqual(r.Paths, []string{video}) {
		t.Fatalf("video result %+v", r)
	}
	if codes := warningCodes(rec.warnings); !reflect.DeepEqual(codes["metadata-read-failed"], []string{video}) {
		t.Fatalf("warnings %+v", rec.warnings)
	}
}
