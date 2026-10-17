package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"app/core"
)

type remoteMatchFunc func([]byte) (core.RemoteMediaMatch, error)

func (f remoteMatchFunc) FindRemoteMediaMatchesByHash(_ context.Context, h []byte) (core.RemoteMediaMatch, error) {
	return f(h)
}

func writeFiles(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(p), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerifyLivePhotoStatuses(t *testing.T) {
	dir := t.TempDir()
	photo := filepath.Join(dir, "image.heic")
	video := filepath.Join(dir, "image.mov")
	writeFiles(t, photo, video)
	for _, tc := range []struct {
		name         string
		photo, video core.RemoteMediaMatch
		err          error
		want         string
	}{
		{name: "primary", photo: core.RemoteMediaMatch{PrimaryMediaKey: "same"}, video: core.RemoteMediaMatch{PrimaryMediaKey: "same"}, want: "paired"},
		{name: "related", photo: core.RemoteMediaMatch{PrimaryMediaKey: "still"}, video: core.RemoteMediaMatch{RelatedMediaKey: "still"}, want: "paired"},
		{name: "photo", photo: core.RemoteMediaMatch{PrimaryMediaKey: "still"}, want: "photo-only"},
		{name: "video", video: core.RemoteMediaMatch{PrimaryMediaKey: "video"}, want: "video-only"},
		{name: "mismatch", photo: core.RemoteMediaMatch{PrimaryMediaKey: "still"}, video: core.RemoteMediaMatch{PrimaryMediaKey: "other"}, want: "mismatched-media-keys"},
		{name: "missing", want: "not-found"},
		{name: "error", err: errors.New("429"), want: "api-error"},
		{name: "canceled", err: context.Canceled, want: "canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api := remoteMatchFunc(func([]byte) (core.RemoteMediaMatch, error) {
				calls++
				if calls == 1 {
					return tc.photo, tc.err
				}
				return tc.video, tc.err
			})
			item := verifyLivePhotoPair(context.Background(), verifyPairInput{PhotoPath: photo, VideoPath: video}, api)
			if item.Status != tc.want {
				t.Fatalf("%+v", item)
			}
		})
	}
}

func TestResolverUsesRecordedPathsByDefault(t *testing.T) {
	r := reportPathResolver{workingDirectory: filepath.FromSlash("/runs/today")}
	if got, _ := r.resolve(filepath.FromSlash("DCIM/IMG_1.HEIC")); got != filepath.FromSlash("/runs/today/DCIM/IMG_1.HEIC") {
		t.Fatalf("relative path resolved to %s", got)
	}
	abs := filepath.Join(t.TempDir(), "IMG_1.HEIC")
	if got, _ := r.resolve(abs); got != abs {
		t.Fatalf("absolute path changed to %s", got)
	}
}

func TestSourceRootResolvesByNameAndFolder(t *testing.T) {
	root := t.TempDir()
	unique := filepath.Join(root, "flat", "IMG_0002.HEIC")
	in100 := filepath.Join(root, "100APPLE", "IMG_0001.HEIC")
	in101 := filepath.Join(root, "101APPLE", "IMG_0001.HEIC")
	writeFiles(t, unique, in100, in101)
	index, err := indexSourceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	r := reportPathResolver{index: index}
	for report, want := range map[string]string{
		"/old/DCIM/100APPLE/IMG_0002.HEIC": unique, // unique name: folder may differ
		"/old/DCIM/101APPLE/IMG_0001.HEIC": in101,  // duplicate name: folder decides
	} {
		if got, err := r.resolve(filepath.FromSlash(report)); err != nil || got != want {
			t.Errorf("%s -> %s, %v; want %s", report, got, err, want)
		}
	}
	// A flattened copy loses the folders, so a repeated name is ambiguous.
	if _, err := r.resolve(filepath.FromSlash("/old/flat/IMG_0001.HEIC")); err == nil {
		t.Error("picked one of several files with the same name")
	}
	if _, err := r.resolve(filepath.FromSlash("/old/IMG_9999.HEIC")); err == nil {
		t.Error("resolved a missing file")
	}
}

func TestVerificationReportFileCannotReplaceInput(t *testing.T) {
	input := filepath.Join(t.TempDir(), "upload.json")
	writeFiles(t, input)
	if err := validateVerificationReportFile(input, input); err == nil {
		t.Fatal("allowed overwriting the upload report")
	}
	if err := validateVerificationReportFile(input+".verify", input); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledVerificationReportsUnstartedPairs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pairs := []verifyPairInput{{Index: 0, ResolveErr: "x"}, {Index: 1, ResolveErr: "x"}}
	results := verifyPairs(ctx, pairs, remoteMatchFunc(nil), 1, func() {})
	for _, r := range results {
		if r.Status == "" {
			t.Fatalf("pair left without a status: %+v", results)
		}
	}
	if report := buildLivePhotoVerificationReport("in", "", results); report.CanceledPairs+report.LocalErrors != 2 {
		t.Fatalf("%+v", report)
	}
}
