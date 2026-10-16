package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRetryReportFiltersAndPreservesPairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	data := `{"results":[{"path":"ok.jpg","success":true},{"path":"skip.jpg","skipped":true},{"path":"failed.jpg","error":"429"},{"path":"photo.heic","paths":["photo.heic","photo.mov"],"error":"429"},{"path":"failed.jpg","error":"429"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := retryReportPaths(path, false); err == nil {
		t.Fatal("accepted pair without pairing flag")
	}
	got, err := retryReportPaths(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"failed.jpg", "photo.heic", "photo.mov"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRetryReportRejectsInvalidReports(t *testing.T) {
	for _, data := range []string{`{`, `{}`, `{"results":null}`, `{"total":10,"results":[]}`, `{"canceled":true,"results":[]}`, `{"results":[{}]}`, `{"results":[{"paths":[""]}]}`, `{"results":[],"album":{"error":"failed"}}`} {
		path := filepath.Join(t.TempDir(), "report.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := retryReportPaths(path, true); err == nil {
			t.Fatalf("accepted %s", data)
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
