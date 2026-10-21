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
		log := newRunLog(cmd.Flags(), "upload", slog.New(slog.DiscardHandler))
		log.Info("before the crash")
		log.open()
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

// The log waits in memory until --log-file is checked, so a refused path is
// never written. A run that stops before the check still writes its log to
// a new file, but never replaces an existing one it could not check.
func TestLogFileWrittenOnlyAfterCheck(t *testing.T) {
	config, photo := emptyAccount(t)
	dir := t.TempDir()

	newLog := filepath.Join(dir, "new.log")
	code, _, stderr := runCLI(t, dir, "upload", photo, "--config", config, "--no-tui", "--threads", "0", "--log-file", newLog)
	if code == 0 || !strings.Contains(stderr, "Debug log: "+newLog) {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if lines := readLog(t, newLog); !hasMsg(lines, "upload command") || !hasMsg(lines, "upload failed") {
		t.Fatalf("log %v", lines)
	}

	existing := filepath.Join(dir, "existing.jpg")
	writeFixture(t, existing, "a photo the run never checked")
	code, _, stderr = runCLI(t, dir, "upload", photo, "--config", config, "--no-tui", "--threads", "0", "--log-file", existing)
	if after, _ := os.ReadFile(existing); code == 0 || string(after) != "a photo the run never checked" {
		t.Fatalf("exit %d, %s now %q", code, existing, after)
	}
	if strings.Contains(stderr, "Debug log:") {
		t.Fatalf("stderr %q", stderr)
	}

	// Refused although it does not exist yet: it is the report's path.
	report := filepath.Join(dir, "report.json")
	code, _, _ = runCLI(t, dir, "upload", photo, "--config", config, "--no-tui", "--log-file", report, "--report-file", report)
	if _, err := os.Stat(report); code == 0 || err == nil {
		t.Fatalf("exit %d, %s written (%v)", code, report, err)
	}
}

func TestVerifyLogFileRefusedBeforeWork(t *testing.T) {
	config, _ := emptyAccount(t)
	dir := t.TempDir()
	report := filepath.Join(dir, "upload.json")
	writeFixture(t, report, `{"results":[]}`)
	out := filepath.Join(dir, "verify.json")
	writeFixture(t, out, "old verification")
	cases := map[string]struct {
		log   string
		extra []string
	}{
		"upload report": {report, nil},
		"config":        {config, nil},
		"report file":   {out, []string{"--report-file", out}},
		"directory":     {dir, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before, _ := os.ReadFile(c.log)
			args := append([]string{"verify-live-photos", report, "--config", config, "--log-file", c.log}, c.extra...)
			code, _, stderr := runCLI(t, t.TempDir(), args...)
			if code == 0 || !strings.Contains(stderr, "--log-file") {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if info, err := os.Stat(c.log); err != nil {
				t.Fatal(err)
			} else if !info.IsDir() {
				if after, _ := os.ReadFile(c.log); string(after) != string(before) {
					t.Fatalf("%s was overwritten: %q", c.log, after)
				}
			}
		})
	}
}

// The default log is created in the working directory, which may be the
// folder being uploaded; the run must not upload its own log.
func TestUploadSkipsItsOwnLog(t *testing.T) {
	config, _ := emptyAccount(t)
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "photo.jpg"), "photo")
	_, stdout, _ := runCLI(t, dir, "upload", ".", "--recursive", "--disable-filter", "--config", config, "--no-tui")
	var summary uploadSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.LogFile == "" || summary.Total != 1 {
		t.Fatalf("logFile %q, results %+v", summary.LogFile, summary.Results)
	}
	for _, r := range summary.Results {
		if strings.HasSuffix(r.Path, ".log") {
			t.Fatalf("the run's own log was queued: %+v", summary.Results)
		}
	}
}
