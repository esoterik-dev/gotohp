package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"app/core"
)

func TestLogLevelFiltersStderrWithoutTUI(t *testing.T) {
	for _, tc := range []struct {
		level      string
		tui        bool
		info, warn bool
	}{
		{"info", false, true, true},
		{"warn", false, false, true},
		{"error", false, false, false},
		{"debug", true, false, false}, // the TUI owns the terminal
	} {
		level, err := parseLogLevel(tc.level)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		logger := newLogger(level, &out, tc.tui)
		logger.Info("info-line")
		logger.Warn("warn-line")
		if got := strings.Contains(out.String(), "info-line"); got != tc.info {
			t.Errorf("%+v: info logged=%v", tc, got)
		}
		if got := strings.Contains(out.String(), "warn-line"); got != tc.warn {
			t.Errorf("%+v: warn logged=%v", tc, got)
		}
	}
	if _, err := parseLogLevel("verbose"); err == nil {
		t.Fatal("accepted an unknown log level")
	}
}

// An unknown --log-level is rejected even when --retry-report leaves nothing
// to upload, before the empty report is written.
func TestUnknownLogLevelRejectedOnEmptyRetry(t *testing.T) {
	previousConfig, previousPath := core.AppConfig, core.ConfigPath
	t.Cleanup(func() { core.AppConfig, core.ConfigPath = previousConfig, previousPath })
	dir := t.TempDir()
	retry := writeRetryReport(t, `{"results":[{"path":"ok.jpg","success":true}]}`)
	out := filepath.Join(dir, "retry-report.json")
	cmd := newRootCommand(Info{})
	cmd.SetArgs([]string{"upload", "--retry-report", retry, "--report-file", out, "--config", filepath.Join(dir, "gotohp.config"), "--log-level", "verbose"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--log-level") {
		t.Fatalf("unknown log level accepted: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("report written despite an invalid flag")
	}
}
