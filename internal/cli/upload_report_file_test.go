package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReportDoesNotOverwriteInputs(t *testing.T) {
	for _, name := range []string{"state-file", "retry-report", "config"} {
		t.Run(name, func(t *testing.T) {
			cmd := newUploadCommand()
			f := cmd.Flags()
			if f.Lookup(name) == nil {
				f.String(name, "", "")
			}
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			_ = f.Set(name, path)
			_ = f.Set("report-file", path)
			if err := validateReportPaths(f); err == nil {
				t.Fatal("allowed overwriting input")
			}
			_ = f.Set("report-file", path+".new")
			if err := validateReportPaths(f); err != nil {
				t.Fatal(err)
			}
			if name == "state-file" {
				_ = f.Set("report-file", path+".lock")
				if err := validateReportPaths(f); err == nil {
					t.Fatal("allowed overwriting lock")
				}
			}
		})
	}
}

func TestReportAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeUploadReport(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("%q %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%v", info.Mode())
	}
}
