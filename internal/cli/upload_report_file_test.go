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

func TestReportFileDirectoryRejectedBeforeUpload(t *testing.T) {
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	previousStdout := os.Stdout
	t.Cleanup(func() {
		core.AppConfig, core.ConfigPath = previousConfig, previousPath
		os.Stdout = previousStdout
	})
	dir := t.TempDir()
	configPath := filepath.Join(dir, "gotohp.config")
	if err := os.WriteFile(configPath, []byte("account: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(photo, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	reportDir := filepath.Join(dir, "reports")
	if err := os.Mkdir(reportDir, 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(dir, "stdout-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	os.Stdout = output
	// The empty credential store makes an upload fail before any network
	// request, so a run that gets that far still prints a summary.
	code := Run([]string{"upload", photo, "--config", configPath, "--no-tui", "--report-file", reportDir}, Info{ExecutableName: "gotohp-cli"})
	os.Stdout = previousStdout
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if info, err := output.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("upload ran before rejecting the report path (stdout size %v, err %v)", info.Size(), err)
	}
	if info, err := os.Stat(reportDir); err != nil || !info.IsDir() {
		t.Fatalf("report directory was replaced: %v", err)
	}
}
