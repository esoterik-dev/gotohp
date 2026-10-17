package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"app/core"
)

func writeRetryReport(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRetryReportFiltersAndPreservesPairs(t *testing.T) {
	path := writeRetryReport(t, `{"results":[{"path":"ok.jpg","success":true},{"path":"skip.jpg","skipped":true},{"path":"failed.jpg","error":"429"},{"path":"photo.heic","paths":["photo.heic","photo.mov"],"error":"429"},{"path":"failed.jpg","error":"429"}]}`)
	if _, err := retryReportPaths(path, false, io.Discard); err == nil {
		t.Fatal("accepted pair without pairing flag")
	}
	got, err := retryReportPaths(path, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"failed.jpg", "photo.heic", "photo.mov"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRetryReportResolvesAgainstRecordedWorkingDirectory(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "home", "user")
	abs := filepath.Join(root, "elsewhere", "abs.jpg")
	path := writeRetryReport(t, `{"workingDirectory":`+quoteJSON(root)+`,"results":[{"path":"photos/IMG_1.jpg","error":"429"},{"path":`+quoteJSON(abs)+`,"error":"429"}]}`)
	got, err := retryReportPaths(path, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(root, "photos", "IMG_1.jpg"), abs}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRetryReportAcceptsCanceledRunWithWarning(t *testing.T) {
	path := writeRetryReport(t, `{"canceled":true,"total":5,"results":[{"path":"failed.jpg","error":"context canceled"}]}`)
	var warn bytes.Buffer
	got, err := retryReportPaths(path, false, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"failed.jpg"}) {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(warn.String(), "4 item(s) that never started") {
		t.Fatalf("missing warning: %q", warn.String())
	}
}

func TestRetryReportRejectsInvalidReports(t *testing.T) {
	for _, data := range []string{`{`, `{}`, `{"results":null}`, `{"total":10,"results":[]}`, `{"results":[{}]}`, `{"results":[{"paths":[""]}]}`, `{"results":[],"album":{"error":"failed"}}`} {
		path := writeRetryReport(t, data)
		if _, err := retryReportPaths(path, true, io.Discard); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

// pairMetadata reports every photo and video as belonging to one Live Photo.
type pairMetadata struct{}

func (pairMetadata) PhotoContentIdentifier(string) (string, error) { return "id", nil }
func (pairMetadata) VideoLivePhotoMetadata(string) (core.LivePhotoMetadata, error) {
	return core.LivePhotoMetadata{ContentIdentifier: "id", HasStillImageTime: true}, nil
}

func TestRetriedPairIsClassifiedAsOneLivePhoto(t *testing.T) {
	path := writeRetryReport(t, `{"results":[{"path":"photo.heic","paths":["photo.heic","photo.mov"],"error":"429"}]}`)
	paths, err := retryReportPaths(path, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, ignoreMetadata := range []bool{false, true} {
		items, warnings := core.ClassifyUploadWork(paths, core.LivePhotoClassificationOptions{
			Enabled:             true,
			SkipIncomplete:      true,
			IgnoreAppleMetadata: ignoreMetadata,
		}, pairMetadata{})
		if len(warnings) != 0 || len(items) != 1 || items[0].Kind != core.UploadWorkLivePhoto {
			t.Fatalf("ignoreMetadata=%v: items=%+v warnings=%+v", ignoreMetadata, items, warnings)
		}
	}
}

func TestRetryReportArgs(t *testing.T) {
	cmd := newUploadCommand()
	if err := cmd.Flags().Set("retry-report", "report.json"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Args(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Args(cmd, []string{"photo.jpg"}); err == nil {
		t.Fatal("accepted mixed retry and paths")
	}
}

func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
