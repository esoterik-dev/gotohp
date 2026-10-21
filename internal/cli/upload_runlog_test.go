package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"app/core"
)

func TestDefaultRunLogPathIsInWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path, err := defaultRunLogPath("upload", time.Date(2026, 10, 18, 5, 16, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(filepath.Dir(path), dir) {
		t.Fatalf("log in %s, want %s", filepath.Dir(path), dir)
	}
	if base := filepath.Base(path); !strings.HasPrefix(base, "gotohp-upload-20261018-051600-") || !strings.HasSuffix(base, ".log") {
		t.Fatalf("log name %s", base)
	}
}

// readLog returns the log's lines as decoded JSON objects.
func readLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("not a JSON line: %q", scanner.Text())
		}
		lines = append(lines, line)
	}
	return lines
}

func hasMsg(lines []map[string]any, msg string) bool {
	for _, l := range lines {
		if l["msg"] == msg {
			return true
		}
	}
	return false
}

// A run always writes a debug log, even when it fails, and the summary and
// stderr point to it.
func TestUploadRunWritesDebugLog(t *testing.T) {
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	previousStdout := os.Stdout
	t.Cleanup(func() {
		core.AppConfig, core.ConfigPath = previousConfig, previousPath
		os.Stdout = previousStdout
	})
	dir := t.TempDir()
	configPath := filepath.Join(dir, "empty.config")
	if err := os.WriteFile(configPath, []byte("account: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(photo, []byte("upload fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(dir, "stdout-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = output.Close() }()
	os.Stdout = output
	logPath := filepath.Join(dir, "logs", "run.log")
	Run([]string{"upload", photo, "--config", configPath, "--no-tui", "--log-file", logPath}, Info{ExecutableName: "gotohp-cli"})
	os.Stdout = previousStdout

	lines := readLog(t, logPath)
	for _, msg := range []string{"upload started", "file failed", "run finished"} {
		if !hasMsg(lines, msg) {
			t.Errorf("log has no %q line: %v", msg, lines)
		}
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var summary uploadSummary
	if err := json.NewDecoder(output).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	if summary.LogFile != logPath {
		t.Fatalf("summary logFile=%q", summary.LogFile)
	}
}

type countingReporter struct {
	core.NopReporter
	statuses int
}

func (r *countingReporter) ThreadStatus(core.ThreadStatus) { r.statuses++ }

// Byte-progress updates reach the UI but not the log; stage changes do.
func TestLoggingReporterLogsStageChangesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	file, err := createRunLog(path)
	if err != nil {
		t.Fatal(err)
	}
	handler := slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug})
	inner := &countingReporter{}
	r := newLoggingReporter(inner, slog.New(handler))
	r.ThreadStatus(core.ThreadStatus{WorkerID: 1, Status: "uploading", FilePath: "a.jpg", BytesUploaded: 1})
	r.ThreadStatus(core.ThreadStatus{WorkerID: 1, Status: "uploading", FilePath: "a.jpg", BytesUploaded: 2})
	r.ThreadStatus(core.ThreadStatus{WorkerID: 1, Status: "finalizing", FilePath: "a.jpg"})
	r.FileResult(core.FileUploadResult{Path: "a.jpg", MediaKey: "k"})
	_ = file.Close()
	lines := readLog(t, path)
	if inner.statuses != 3 || len(lines) != 3 || !hasMsg(lines, "file uploaded") {
		t.Fatalf("ui=%d log=%v", inner.statuses, lines)
	}
	// Each stage change says how long the stage before it took.
	if _, ok := lines[0]["previous_ms"]; ok {
		t.Fatalf("first stage has a previous stage: %v", lines[0])
	}
	if lines[1]["previous_status"] != "uploading" || lines[1]["previous_file"] != "a.jpg" {
		t.Fatalf("no previous stage on a stage change: %v", lines[1])
	}
	if _, ok := lines[1]["previous_ms"].(float64); !ok {
		t.Fatalf("no stage duration: %v", lines[1])
	}
}

// runUploadCLI runs the upload command through Run with an empty credential
// store, so it fails before any network request. It returns the exit code,
// stdout and stderr.
func runUploadCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	previousStdout, previousStderr := os.Stdout, os.Stderr
	t.Cleanup(func() {
		core.AppConfig, core.ConfigPath = previousConfig, previousPath
		os.Stdout, os.Stderr = previousStdout, previousStderr
	})
	dir := t.TempDir()
	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = stdout, stderr
	code := Run(append([]string{"upload", "--no-tui"}, args...), Info{ExecutableName: "gotohp-cli"})
	os.Stdout, os.Stderr = previousStdout, previousStderr
	_ = stdout.Close()
	_ = stderr.Close()
	out, _ := os.ReadFile(stdout.Name())
	errOut, _ := os.ReadFile(stderr.Name())
	return code, string(out), string(errOut)
}

func writeFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --log-file is refused before any work when it names a file the run reads or
// writes, or a directory, like --report-file; the file is left untouched.
func TestLogFileRefusedBeforeWork(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "gotohp.config")
	writeFixture(t, config, "account: {}\n")
	src := filepath.Join(dir, "src")
	photo := filepath.Join(src, "photo.jpg")
	writeFixture(t, photo, "photo")
	nested := filepath.Join(src, "sub", "other.jpg")
	writeFixture(t, nested, "other photo")
	single := filepath.Join(dir, "single.jpg")
	writeFixture(t, single, "single photo")
	notes := filepath.Join(src, "notes.txt")
	writeFixture(t, notes, "notes")
	report := filepath.Join(dir, "report.json")
	writeFixture(t, report, "old report")
	state := filepath.Join(dir, "state.json")
	writeFixture(t, state, "{}")
	writeFixture(t, state+".lock", "")
	logDir := filepath.Join(dir, "logs")
	if err := os.Mkdir(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		paths []string
		log   string
		extra []string
	}{
		"config":                         {[]string{src}, config, nil},
		"upload path":                    {[]string{single}, single, nil},
		"file in a folder":               {[]string{src, "--recursive"}, nested, nil},
		"any file with --disable-filter": {[]string{src, "--recursive", "--disable-filter"}, notes, nil},
		"report":                         {[]string{src}, report, []string{"--report-file", report}},
		"state file":                     {[]string{src}, state, []string{"--state-file", state}},
		"state lock":                     {[]string{src}, state + ".lock", []string{"--state-file", state}},
		"directory":                      {[]string{src}, logDir, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before, _ := os.ReadFile(c.log)
			args := append(append(append([]string{}, c.paths...), "--config", config, "--log-file", c.log), c.extra...)
			code, stdout, stderr := runUploadCLI(t, args...)
			if code != 1 || stdout != "" || !strings.Contains(stderr, "--log-file") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
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

// Any other existing file may be overwritten, as with --report-file.
func TestLogFileOverwritesUnrelatedFile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "gotohp.config")
	writeFixture(t, config, "account: {}\n")
	photo := filepath.Join(dir, "src", "photo.jpg")
	writeFixture(t, photo, "photo")
	logPath := filepath.Join(dir, "old.log")
	writeFixture(t, logPath, "old log\n")
	code, stdout, _ := runUploadCLI(t, filepath.Dir(photo), "--config", config, "--log-file", logPath)
	if code != 1 || stdout == "" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	if lines := readLog(t, logPath); !hasMsg(lines, "upload started") {
		t.Fatalf("log not written: %v", lines)
	}
}

// Without the TUI, stderr shows only core's messages at --log-level, as
// before the debug log existed; the run's own events go only to the log.
func TestRunLogEventsStayOutOfStderr(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "gotohp.config")
	writeFixture(t, config, "account: {}\n")
	photo := filepath.Join(dir, "photo.jpg")
	writeFixture(t, photo, "photo")
	logPath := filepath.Join(dir, "run.log")
	_, _, stderr := runUploadCLI(t, photo, "--config", config, "--log-file", logPath)
	for _, msg := range []string{"upload started", "file failed", "run finished", "batch"} {
		if strings.Contains(stderr, "msg="+msg) || strings.Contains(stderr, `msg="`+msg+`"`) {
			t.Errorf("stderr has the log-only %q line:\n%s", msg, stderr)
		}
	}
	lines := readLog(t, logPath)
	for _, msg := range []string{"upload started", "file failed", "run finished"} {
		if !hasMsg(lines, msg) {
			t.Errorf("log has no %q line", msg)
		}
	}
}
