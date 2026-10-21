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
	root := t.TempDir()
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

func TestRetryReportRejectsRunCanceledBeforeStart(t *testing.T) {
	path := writeRetryReport(t, `{"canceled":true,"total":0,"results":[]}`)
	if _, err := retryReportPaths(path, false, io.Discard); err == nil {
		t.Fatal("a run canceled during scanning looked like nothing to retry")
	}
}

func TestRetrySkipsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "kept.jpg")
	if err := os.WriteFile(kept, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	got := existingRetryPaths([]string{filepath.Join(dir, "moved.jpg"), kept}, &warn)
	if !reflect.DeepEqual(got, []string{kept}) || !strings.Contains(warn.String(), "moved.jpg") {
		t.Fatalf("got %v, warning %q", got, warn.String())
	}
}

func TestEmptyRetryStillWritesReport(t *testing.T) {
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	t.Cleanup(func() { core.AppConfig, core.ConfigPath = previousConfig, previousPath })
	dir := t.TempDir()
	retry := writeRetryReport(t, `{"results":[{"path":"ok.jpg","success":true}]}`)
	out := filepath.Join(dir, "retry-report.json")
	cmd := newRootCommand(Info{})
	cmd.SetArgs([]string{"upload", "--retry-report", retry, "--report-file", out, "--config", filepath.Join(dir, "gotohp.config")})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(data), `"total": 0`) {
		t.Fatalf("report not written: %v %s", err, data)
	}
}

// With nothing left to retry, the report is still written, so it must be
// checked against the config in use, which may come from a default location.
func TestEmptyRetryDoesNotOverwriteDefaultConfig(t *testing.T) {
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	t.Cleanup(func() { core.AppConfig, core.ConfigPath = previousConfig, previousPath })
	core.ConfigPath = ""
	dir := t.TempDir()
	t.Chdir(dir) // the working directory is one of the default config locations
	config := []byte("account:\n    selected: tester@example.com\n")
	if err := os.WriteFile("gotohp.config", config, 0600); err != nil {
		t.Fatal(err)
	}
	retry := writeRetryReport(t, `{"results":[{"path":"ok.jpg","success":true}]}`)
	code := Run([]string{"upload", "--retry-report", retry, "--no-tui", "--report-file", "gotohp.config"}, Info{ExecutableName: "gotohp-cli"})
	if code == 0 {
		t.Error("an empty retry was allowed to write its report over the config in use")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "gotohp.config")); err != nil || !bytes.Equal(data, config) {
		t.Fatalf("config was replaced: %v\n%s", err, data)
	}
}
