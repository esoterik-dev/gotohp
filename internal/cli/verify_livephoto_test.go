package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

const verifyTestConfig = "account:\n    selected: tester@example.com\n"

// runVerifyInDir runs verify-live-photos on a one-pair report in a fresh
// working directory, which is also a default config location. The configs
// hold no credentials, so a run that gets past validation stops before any
// request.
func runVerifyInDir(t *testing.T, args ...string) error {
	t.Helper()
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	t.Cleanup(func() { core.AppConfig, core.ConfigPath = previousConfig, previousPath })
	core.ConfigPath = ""
	t.Chdir(t.TempDir())
	writeFiles(t, "IMG_1.HEIC", "IMG_1.MOV", "dcim/IMG_9.JPG")
	for name, data := range map[string]string{
		"gotohp.config": verifyTestConfig,
		"other.config":  verifyTestConfig,
		"upload.json":   `{"results":[{"path":"IMG_1.HEIC","paths":["IMG_1.HEIC","IMG_1.MOV"],"success":true}]}`,
	} {
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir("out", 0700); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand(Info{})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"verify-live-photos", "upload.json", "--proxy", "http://127.0.0.1:1", "--max-retries", "0"}, args...))
	return root.Execute()
}

func TestVerificationReportFileRefusedBeforeWork(t *testing.T) {
	for name, args := range map[string][]string{
		"upload report":  {"--report-file", "upload.json"},
		"default config": {"--report-file", "gotohp.config"},
		"--config":       {"--config", "other.config", "--report-file", "other.config"},
		"directory":      {"--report-file", "out"},
		"pair photo":     {"--report-file", "IMG_1.HEIC"},
		"pair video":     {"--report-file", "IMG_1.MOV"},
		"source root":    {"--source-root", "dcim", "--report-file", "dcim/IMG_9.JPG"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runVerifyInDir(t, args...)
			if err == nil || !strings.Contains(err.Error(), "--report-file") {
				t.Fatalf("%v was not refused before verification: %v", args, err)
			}
			for _, config := range []string{"gotohp.config", "other.config"} {
				if data, _ := os.ReadFile(config); string(data) != verifyTestConfig {
					t.Fatalf("%s was replaced: %s", config, data)
				}
			}
			for _, media := range []string{"IMG_1.HEIC", "IMG_1.MOV", "dcim/IMG_9.JPG"} {
				if data, _ := os.ReadFile(media); string(data) != media {
					t.Fatalf("%s was replaced: %s", media, data)
				}
			}
		})
	}
}

func TestVerificationReportFileAllowsNewFile(t *testing.T) {
	err := runVerifyInDir(t, "--report-file", "verification.json")
	if err == nil || strings.Contains(err.Error(), "--report-file") {
		t.Fatalf("a new report file should pass validation and stop at the API client: %v", err)
	}
}

func TestCanceledVerificationReportsUnstartedPairs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pairs := []verifyPairInput{{Index: 0, ResolveErr: "x"}, {Index: 1, ResolveErr: "x"}}
	results := verifyPairs(ctx, pairs, remoteMatchFunc(nil), 1, func(livePhotoVerificationItem) {})
	for _, r := range results {
		if r.Status == "" {
			t.Fatalf("pair left without a status: %+v", results)
		}
	}
	if report := buildLivePhotoVerificationReport("in", "", results); report.CanceledPairs+report.LocalErrors != 2 {
		t.Fatalf("%+v", report)
	}
}
