package cli

import (
	"bytes"
	"strings"
	"testing"
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
