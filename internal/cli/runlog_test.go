package cli

import (
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"app/core"
)

// runCLI runs the command line in dir as a user would, with stdout and
// stderr captured, and returns the exit code and both outputs.
func runCLI(t *testing.T, dir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	previousStdout, previousStderr := os.Stdout, os.Stderr
	t.Cleanup(func() {
		core.AppConfig, core.ConfigPath = previousConfig, previousPath
		os.Stdout, os.Stderr = previousStdout, previousStderr
	})
	t.Chdir(dir)
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	errOut, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = out, errOut
	code = Run(args, Info{ExecutableName: "gotohp-cli"})
	os.Stdout, os.Stderr = previousStdout, previousStderr
	_ = out.Close()
	_ = errOut.Close()
	o, _ := os.ReadFile(out.Name())
	e, _ := os.ReadFile(errOut.Name())
	return code, string(o), string(e)
}

// emptyAccount writes a config whose missing account fails every file
// before any network request.
func emptyAccount(t *testing.T) (config, photo string) {
	t.Helper()
	dir := t.TempDir()
	config = filepath.Join(dir, "empty.config")
	photo = filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(config, []byte("account: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(photo, []byte("upload fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config, photo
}

// logsIn returns the default debug logs a command left in dir.
func logsIn(t *testing.T, dir, command string) []string {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(dir, "gotohp-"+command+"-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestUploadWritesDebugLogInWorkingDirectory(t *testing.T) {
	config, photo := emptyAccount(t)
	dir := t.TempDir()
	_, stdout, stderr := runCLI(t, dir, "upload", photo, "--config", config, "--no-tui")
	logs := logsIn(t, dir, "upload")
	if len(logs) != 1 {
		t.Fatalf("logs in working directory: %v", logs)
	}
	lines := readLog(t, logs[0])
	for _, msg := range []string{"upload command", "upload started", "file failed", "run finished"} {
		if !hasMsg(lines, msg) {
			t.Errorf("log has no %q line", msg)
		}
	}
	var summary uploadSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if !samePath(summary.LogFile, logs[0]) || !strings.Contains(stderr, "Debug log: "+summary.LogFile) {
		t.Fatalf("logFile %q, stderr %q", summary.LogFile, stderr)
	}
}

func TestNoLogFileWritesNoLog(t *testing.T) {
	config, photo := emptyAccount(t)
	dir := t.TempDir()
	_, stdout, stderr := runCLI(t, dir, "upload", photo, "--config", config, "--no-tui", "--no-log-file")
	if logs, _ := filepath.Glob(filepath.Join(dir, "*")); len(logs) != 0 {
		t.Fatalf("files written: %v", logs)
	}
	var summary uploadSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.LogFile != "" || strings.Contains(stderr, "Debug log:") {
		t.Fatalf("logFile %q, stderr %q", summary.LogFile, stderr)
	}
}

func TestLogFileAndNoLogFileConflict(t *testing.T) {
	config, photo := emptyAccount(t)
	dir := t.TempDir()
	code, _, _ := runCLI(t, dir, "upload", photo, "--config", config, "--no-tui", "--no-log-file", "--log-file", "x.log")
	if code == 0 || len(logsIn(t, dir, "upload")) != 0 {
		t.Fatalf("exit %d", code)
	}
}

// A run that ends before uploading still leaves a log saying why: its
// warnings and the error that stopped it.
func TestLogRecordsWarningsAndErrorsBeforeUpload(t *testing.T) {
	config, _ := emptyAccount(t)
	dir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(report, []byte(`{"results":[{"path":"/gone/IMG_1.JPG","error":"429"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLI(t, dir, "upload", "--retry-report", report, "--config", config, "--no-tui")
	logs := logsIn(t, dir, "upload")
	if len(logs) != 1 {
		t.Fatalf("logs %v", logs)
	}
	text, _ := os.ReadFile(logs[0])
	for _, want := range []string{`"msg":"stderr"`, `not retrying \"/gone/IMG_1.JPG\"`, "No failed non-skipped files to retry"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("log lacks %s:\n%s", want, text)
		}
	}

	dir = t.TempDir()
	code, _, _ := runCLI(t, dir, "upload", "photo.jpg", "--config", config, "--no-tui", "--threads", "0")
	logs = logsIn(t, dir, "upload")
	if code == 0 || len(logs) != 1 {
		t.Fatalf("exit %d, logs %v", code, logs)
	}
	text, _ = os.ReadFile(logs[0])
	if !strings.Contains(string(text), `"msg":"upload failed"`) {
		t.Fatalf("error not logged:\n%s", text)
	}
}

func TestVerifyLivePhotosWritesDebugLog(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(report, []byte(`{"results":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, dir, "verify-live-photos", report)
	logs := logsIn(t, dir, "verify-live-photos")
	if code == 0 || len(logs) != 1 || !strings.Contains(stderr, "Debug log: ") {
		t.Fatalf("exit %d, logs %v, stderr %q", code, logs, stderr)
	}
	lines := readLog(t, logs[0])
	if !hasMsg(lines, "verify-live-photos command") || !hasMsg(lines, "verify-live-photos failed") {
		t.Fatalf("log %v", lines)
	}

	dir = t.TempDir()
	runCLI(t, dir, "verify-live-photos", report, "--no-log-file")
	if logs := logsIn(t, dir, "verify-live-photos"); len(logs) != 0 {
		t.Fatalf("logs %v", logs)
	}
}

// A crash in any goroutine ends up in the log file. The crash runs in a
// child process, the test binary itself, so it cannot take the tests down.
func TestCrashIsRecordedInLog(t *testing.T) {
	if path := os.Getenv("GOTOHP_TEST_CRASH_LOG"); path != "" {
		cmd := &cobra.Command{Use: "upload"}
		addRunLogFlags(cmd)
		_ = cmd.Flags().Set("log-file", path)
		log := openRunLog(cmd.Flags(), "upload", slog.New(slog.DiscardHandler))
		log.Info("before the crash")
		done := make(chan struct{})
		go func() {
			defer close(done)
			panic("worker exploded")
		}()
		<-done
		return
	}
	path := filepath.Join(t.TempDir(), "crash.log")
	child := exec.Command(os.Args[0], "-test.run=^TestCrashIsRecordedInLog$")
	child.Env = append(os.Environ(), "GOTOHP_TEST_CRASH_LOG="+path)
	if err := child.Run(); err == nil {
		t.Fatal("child did not crash")
	}
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"msg":"before the crash"`, "panic: worker exploded", "goroutine "} {
		if !strings.Contains(string(text), want) {
			t.Errorf("log lacks %q:\n%s", want, text)
		}
	}
}
