package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"app/core"
)

func TestDefaultRunLogPathUsesStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	path, err := defaultRunLogPath(time.Date(2026, 10, 18, 5, 16, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if dir := filepath.Dir(path); dir != filepath.Join("/state", "gotohp", "logs") {
		t.Fatalf("log in %s", dir)
	}
	if base := filepath.Base(path); !strings.HasPrefix(base, "upload-20261018-051600-") || !strings.HasSuffix(base, ".log") {
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
	file, handler, err := openRunLog(path)
	if err != nil {
		t.Fatal(err)
	}
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
}

// The default log directory keeps only the newest logs; other files in it
// are left alone.
func TestPruneRunLogsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		name := filepath.Join(dir, fmt.Sprintf("upload-20261018-0%d0000-1.log", i))
		if err := os.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, nil, 0600); err != nil {
		t.Fatal(err)
	}
	pruneRunLogs(dir, 2)
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	want := []string{other, filepath.Join(dir, "upload-20261018-030000-1.log"), filepath.Join(dir, "upload-20261018-040000-1.log")}
	sort.Strings(left)
	sort.Strings(want)
	if fmt.Sprint(left) != fmt.Sprint(want) {
		t.Fatalf("left %v", left)
	}
}
