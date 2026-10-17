package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"app/core"
)

func TestReportDoesNotOverwriteInputs(t *testing.T) {
	for _, name := range reportInputFlags {
		t.Run(name, func(t *testing.T) {
			cmd := newRootCommand(Info{})
			upload, _, err := cmd.Find([]string{"upload"})
			if err != nil {
				t.Fatal(err)
			}
			f := upload.Flags()
			f.AddFlagSet(cmd.PersistentFlags())
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			_ = f.Set(name, path)
			_ = f.Set("report-file", path)
			if err := validateReportFile(f); err == nil {
				t.Fatal("allowed overwriting input")
			}
			_ = f.Set("report-file", path+".new")
			if err := validateReportFile(f); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range reportInputSuffixes[name] {
				_ = f.Set("report-file", path+suffix)
				if err := validateReportFile(f); err == nil {
					t.Fatalf("allowed overwriting %s", path+suffix)
				}
			}
		})
	}
}

func TestReportDoesNotOverwriteDefaultConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gotohp.config")
	if err := os.WriteFile(path, []byte("credentials"), 0600); err != nil {
		t.Fatal(err)
	}
	saved := core.ConfigPath
	core.ConfigPath = path // resolved by LoadConfig without --config
	t.Cleanup(func() { core.ConfigPath = saved })
	f := newUploadCommand().Flags()
	_ = f.Set("report-file", path)
	if err := validateReportFile(f); err == nil {
		t.Fatal("allowed overwriting the default config")
	}
}

func TestReportDirectoryCheckedBeforeUpload(t *testing.T) {
	f := newUploadCommand().Flags()
	dir := t.TempDir()
	_ = f.Set("report-file", filepath.Join(dir, "missing", "report.json"))
	if err := validateReportFile(f); err == nil {
		t.Fatal("accepted a report path in a missing directory")
	}
	_ = f.Set("report-file", filepath.Join(dir, "report.json"))
	if err := validateReportFile(f); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("writability probe left files behind: %v", entries)
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
