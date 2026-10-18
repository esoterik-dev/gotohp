package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"app/core"
)

// retryFromGoneFolder writes a source tree and a report whose failed paths
// point into a temporary folder that no longer exists, like the reports of
// an upload run through a folder of symlinks.
func retryFromGoneFolder(t *testing.T) (root, report, configPath string, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	root = filepath.Join(dir, "DCIM")
	files = map[string]string{
		"100/1":   filepath.Join(root, "100APPLE", "IMG_0001.JPG"),
		"100/ok":  filepath.Join(root, "100APPLE", "IMG_0002.JPG"),
		"101/1":   filepath.Join(root, "101APPLE", "IMG_0001.JPG"),
		"101/3":   filepath.Join(root, "101APPLE", "IMG_0003.JPG"),
		"rootJPG": filepath.Join(root, "IMG_0004.JPG"),
	}
	for _, path := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("upload fixture "+path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gone := filepath.Join(dir, "gphotos-upload-iphone.gone")
	result := func(rel string, ok bool) map[string]any {
		r := map[string]any{"path": filepath.Join(gone, filepath.FromSlash(rel)), "success": ok}
		if !ok {
			r["error"] = "429"
		}
		return r
	}
	data, err := json.Marshal(map[string]any{"total": 6, "results": []any{
		result("100APPLE/IMG_0001.JPG", false),
		result("100APPLE/IMG_0002.JPG", true),
		result("101APPLE/IMG_0001.JPG", false),
		result("101APPLE/IMG_0003.JPG", false),
		result("IMG_0004.JPG", false),
		result("101APPLE/IMG_0009.JPG", false), // deleted since
	}})
	if err != nil {
		t.Fatal(err)
	}
	report = filepath.Join(dir, "report.json")
	if err := os.WriteFile(report, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty account fails every file before any network request, so the
	// new report lists exactly the files the retry attempted.
	configPath = filepath.Join(dir, "empty.config")
	if err := os.WriteFile(configPath, []byte("account: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, report, configPath, files
}

// runRetry runs the upload command the way a user would and returns the
// paths its report says it attempted, plus stderr and the exit code.
func runRetry(t *testing.T, args ...string) (attempted []string, stderr string, code int) {
	t.Helper()
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	previousStdout, previousStderr := os.Stdout, os.Stderr
	t.Cleanup(func() {
		core.AppConfig, core.ConfigPath = previousConfig, previousPath
		os.Stdout, os.Stderr = previousStdout, previousStderr
	})
	dir := t.TempDir()
	out := filepath.Join(dir, "retry.json")
	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	errFile, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()
	os.Stdout, os.Stderr = stdout, errFile
	code = Run(append([]string{"upload", "--no-tui", "--report-file", out}, args...), Info{ExecutableName: "gotohp-cli"})
	os.Stdout, os.Stderr = previousStdout, previousStderr
	errText, _ := os.ReadFile(errFile.Name())
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, string(errText), code
	}
	var summary uploadSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	for _, result := range summary.Results {
		if result.Error != "no account is selected" {
			t.Errorf("unexpected result %+v", result)
		}
		attempted = append(attempted, result.Path)
	}
	return attempted, string(errText), code
}

func TestRetryReportFromGoneFolderNeedsSourceRoot(t *testing.T) {
	_, report, configPath, _ := retryFromGoneFolder(t)
	attempted, stderr, code := runRetry(t, "--config", configPath, "--retry-report", report)
	if len(attempted) != 0 || code != 0 || !strings.Contains(stderr, "No failed non-skipped files to retry") {
		t.Fatalf("attempted %v, exit %d, stderr %q", attempted, code, stderr)
	}
}

func TestRetryReportFindsFilesUnderSourceRoot(t *testing.T) {
	root, report, configPath, files := retryFromGoneFolder(t)
	attempted, stderr, _ := runRetry(t, "--config", configPath, "--retry-report", report, "--source-root", root)
	// The same name in two folders is told apart by the recorded folder.
	want := []string{files["100/1"], files["101/1"], files["101/3"], files["rootJPG"]}
	if !reflect.DeepEqual(sorted(attempted), sorted(want)) {
		t.Fatalf("attempted %v, want %v", attempted, want)
	}
	if !strings.Contains(stderr, "IMG_0009.JPG") {
		t.Fatalf("no warning about the deleted file: %q", stderr)
	}
}

func TestRetryReportFolderFilter(t *testing.T) {
	root, report, configPath, files := retryFromGoneFolder(t)
	attempted, _, _ := runRetry(t, "--config", configPath, "--retry-report", report,
		"--source-root", root, "--folder", "101APPLE", "--folder", "102APPLE")
	if want := []string{files["101/1"], files["101/3"]}; !reflect.DeepEqual(sorted(attempted), sorted(want)) {
		t.Fatalf("attempted %v, want %v", attempted, want)
	}
}

func TestRetryReportFolderMatchingNothingFails(t *testing.T) {
	root, report, configPath, _ := retryFromGoneFolder(t)
	attempted, stderr, code := runRetry(t, "--config", configPath, "--retry-report", report,
		"--source-root", root, "--folder", "101APLE")
	if code == 0 || len(attempted) != 0 || !strings.Contains(stderr, "101APLE") {
		t.Fatalf("attempted %v, exit %d, stderr %q", attempted, code, stderr)
	}
}

func TestSourceRootAndFolderFlagsNeedRetryReport(t *testing.T) {
	root, report, configPath, files := retryFromGoneFolder(t)
	for _, args := range [][]string{
		{"--source-root", root, files["100/1"]},
		{"--retry-report", report, "--folder", "100APPLE"},
		{"--retry-report", report, "--source-root", files["100/1"]},
		{"--retry-report", report, "--source-root", filepath.Join(root, "missing")},
	} {
		attempted, _, code := runRetry(t, append([]string{"--config", configPath}, args...)...)
		if code == 0 || len(attempted) != 0 {
			t.Errorf("%v: exit %d, attempted %v", args, code, attempted)
		}
	}
}

func TestRelocatedPairStaysOneLivePhoto(t *testing.T) {
	root := t.TempDir()
	photo := filepath.Join(root, "100APPLE", "IMG_0001.HEIC")
	video := filepath.Join(root, "100APPLE", "IMG_0001.MOV")
	writeFiles(t, photo, video)
	gone := filepath.FromSlash("/tmp/gone/100APPLE/")
	path := writeRetryReport(t, `{"results":[{"path":`+quoteJSON(gone+"IMG_0001.HEIC")+`,"paths":[`+
		quoteJSON(gone+"IMG_0001.HEIC")+`,`+quoteJSON(gone+"IMG_0001.MOV")+`],"error":"429"}]}`)
	paths, err := retryReportPaths(path, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	if paths, err = relocateRetryPaths(paths, root, []string{"100apple"}, &warn); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{photo, video}) || warn.Len() != 0 {
		t.Fatalf("got %v, warnings %q", paths, warn.String())
	}
	items, warnings := core.ClassifyUploadWork(paths, core.LivePhotoClassificationOptions{
		Enabled: true, SkipIncomplete: true,
	}, pairMetadata{})
	if len(warnings) != 0 || len(items) != 1 || items[0].Kind != core.UploadWorkLivePhoto {
		t.Fatalf("items=%+v warnings=%+v", items, warnings)
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
