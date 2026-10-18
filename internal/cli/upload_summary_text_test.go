package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// On a terminal the run ends with a readable summary, while --report-file
// still gets the full JSON; piped output keeps the JSON
// (TestUploadRunReturnsFailureWithJSON).
func TestUploadOnTerminalPrintsSummaryNotJSON(t *testing.T) {
	previous := outputIsTerminal
	outputIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { outputIsTerminal = previous })
	config, photo := emptyAccount(t)
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	code, stdout, _ := runCLI(t, dir, "upload", photo, "--config", config, "--no-tui", "--report-file", report)
	if code != 1 {
		t.Errorf("exit %d, want 1 for a failed file", code)
	}
	for _, want := range []string{"Finished in ", ": 1 items", "uploaded", "failed", "Failed (1):", "photo.jpg: no account is selected", "Report: " + report} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("terminal got JSON:\n%s", stdout)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var summary uploadSummary
	if err := json.Unmarshal(data, &summary); err != nil || summary.Failed != 1 || len(summary.Results) != 1 {
		t.Fatalf("report %+v, %v", summary, err)
	}
}

func TestTextSummaryCountsAndLists(t *testing.T) {
	wd := filepath.FromSlash("/runs/today")
	in := func(name string) string { return filepath.Join(wd, "DCIM", name) }
	s := uploadSummary{WorkingDirectory: wd, Canceled: true, Warnings: []uploadWarning{{Code: "x"}}}
	s.Results = append(s.Results,
		uploadResult{Path: in("new.jpg"), Success: true},
		uploadResult{Path: in("dup.jpg"), Skipped: true, SkipCode: "remote-duplicate"},
		uploadResult{Path: in("dup2.jpg"), Skipped: true, SkipCode: "remote-duplicate"},
		uploadResult{Path: in("known.jpg"), Skipped: true, SkipCode: "state-cache"},
		uploadResult{Path: in("IMG_1.HEIC"), Skipped: true, SkipCode: "incomplete-live-photo-skipped"},
		uploadResult{Path: in("IMG_2.HEIC"), Paths: []string{in("IMG_2.HEIC"), in("IMG_2.MOV")}, Error: "upload Live Photo video: 503\nbody"},
	)
	for i := range 21 {
		s.Results = append(s.Results, uploadResult{Path: fmt.Sprintf("/elsewhere/f%02d.jpg", i), Error: "429"})
	}
	var out strings.Builder
	writeTextSummary(&out, s, 90*time.Second, "")
	text := out.String()
	for _, want := range []string{
		"Stopped (Ctrl+C) in 1m30s: 27 items",
		"already in library", "confirmed earlier (state file)", "skipped: Live Photo partner missing",
		"Failed (22):",
		filepath.FromSlash("DCIM/IMG_2.HEIC") + " + " + filepath.FromSlash("DCIM/IMG_2.MOV") + ": upload Live Photo video: 503\n",
		"... and 2 more (see the report)",
		"Not uploaded (1):", filepath.FromSlash("DCIM/IMG_1.HEIC") + ": Live Photo partner missing",
		"Warnings: 1", "Files that never started are not listed",
		"Full JSON report: add --report-file PATH",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
	// Routine skips are counted but not listed one by one.
	if strings.Contains(text, "dup.jpg") || strings.Contains(text, "known.jpg") {
		t.Errorf("listed routine skips:\n%s", text)
	}
	if !regexp.MustCompile(`(?m)^  already in library +2$`).MatchString(text) {
		t.Errorf("duplicates not counted as 2:\n%s", text)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1625: "1,625", 1234567: "1,234,567"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %s, want %s", n, got, want)
		}
	}
}
