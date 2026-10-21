package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
)

// outputIsTerminal reports whether the run's output goes to a terminal. There
// the run ends with a readable summary; piped or redirected, it ends with the
// JSON summary, as scripts expect. Tests replace it.
var outputIsTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// maxListed is how many failed or skipped files the text summary lists.
const maxListed = 20

// skipLabels names skip codes for people; unknown codes are shown as is.
var skipLabels = map[string]string{
	"remote-duplicate":                   "already in library",
	"remote-live-photo-component-exists": "Live Photo already in library",
	"state-cache":                        "confirmed earlier (state file)",
	"incomplete-live-photo-skipped":      "Live Photo partner missing",
	"ambiguous-identifier":               "Live Photo ambiguous",
	"ambiguous-filename-stem":            "Live Photo ambiguous",
	"missing-still-image-time":           "Live Photo video without still marker",
}

// expectedSkip reports whether a skip is routine (the item is already in the
// library), as opposed to a file that was not uploaded and may need action.
func expectedSkip(code string) bool {
	return code == "remote-duplicate" || code == "state-cache" || code == "remote-live-photo-component-exists"
}

func skipLabel(code string) string {
	if label, ok := skipLabels[code]; ok {
		return label
	}
	if code == "" {
		return "skipped"
	}
	return code
}

// writeTextSummary writes the end-of-run summary for a terminal: counts by
// outcome, the files that failed or were not uploaded, and where the full
// report is. The JSON summary has the same facts in full.
func writeTextSummary(w io.Writer, s uploadSummary, elapsed time.Duration, reportFile string) {
	uploaded := 0
	skipCounts := map[string]int{}
	var failed, notUploaded []uploadResult
	for _, r := range s.Results {
		switch {
		case r.Skipped:
			skipCounts[r.SkipCode]++
			if !expectedSkip(r.SkipCode) {
				notUploaded = append(notUploaded, r)
			}
		case r.Success:
			uploaded++
		default:
			failed = append(failed, r)
		}
	}

	head := "Finished"
	if s.Canceled {
		head = "Stopped (Ctrl+C)"
	}
	if elapsed > 0 {
		head += " in " + elapsed.Round(time.Second).String()
	}
	if s.Canceled {
		// A stopped run reports only the items that finished; the total counts
		// every item found.
		fmt.Fprintf(w, "%s: %s of %s items finished\n", head, thousands(len(s.Results)), thousands(max(s.Total, len(s.Results))))
	} else {
		fmt.Fprintf(w, "%s: %s items\n", head, thousands(len(s.Results)))
	}
	rows := [][2]string{{"uploaded", thousands(uploaded)}}
	codes := make([]string, 0, len(skipCounts))
	for code := range skipCounts {
		codes = append(codes, code)
	}
	// Routine skips first, then the rest, each by count.
	sort.Slice(codes, func(i, j int) bool {
		if expectedSkip(codes[i]) != expectedSkip(codes[j]) {
			return expectedSkip(codes[i])
		}
		if skipCounts[codes[i]] != skipCounts[codes[j]] {
			return skipCounts[codes[i]] > skipCounts[codes[j]]
		}
		return codes[i] < codes[j]
	})
	for _, code := range codes {
		label := skipLabel(code)
		if !expectedSkip(code) {
			label = "skipped: " + label
		}
		rows = append(rows, [2]string{label, thousands(skipCounts[code])})
	}
	rows = append(rows, [2]string{"failed", thousands(len(failed))})
	width := 0
	for _, row := range rows {
		width = max(width, len(row[0]))
	}
	for _, row := range rows {
		fmt.Fprintf(w, "  %-*s  %6s\n", width, row[0], row[1])
	}

	listFiles(w, "Failed", failed, s.WorkingDirectory, func(r uploadResult) string { return firstLine(r.Error) })
	listFiles(w, "Not uploaded", notUploaded, s.WorkingDirectory, func(r uploadResult) string { return skipLabel(r.SkipCode) })

	if s.Album != nil {
		if s.Album.Error != "" {
			fmt.Fprintf(w, "\nAlbum %q failed: %s\n", s.Album.Name, firstLine(s.Album.Error))
		} else {
			fmt.Fprintf(w, "\nAlbum %q: %s items added\n", s.Album.Name, thousands(s.Album.ItemsAdded))
		}
	}
	if n := len(s.Warnings); n > 0 {
		fmt.Fprintf(w, "\nWarnings: %d (listed in the JSON report)\n", n)
	}
	if s.Canceled {
		fmt.Fprintln(w, "\nFiles that did not finish are not listed; rerun the same command (with --state-file) to include them.")
	}
	fmt.Fprintln(w)
	if reportFile != "" {
		fmt.Fprintf(w, "Report: %s\n", reportFile)
	} else {
		fmt.Fprintln(w, "Full JSON report: add --report-file PATH, or pipe the output (e.g. | jq).")
	}
}

func listFiles(w io.Writer, title string, results []uploadResult, workingDir string, reason func(uploadResult) string) {
	if len(results) == 0 {
		return
	}
	// Results arrive in the order files finish; sort them so that two runs
	// over the same files list them the same way.
	results = slices.Clone(results)
	sort.SliceStable(results, func(i, j int) bool {
		return displayPath(results[i], workingDir) < displayPath(results[j], workingDir)
	})
	fmt.Fprintf(w, "\n%s (%d):\n", title, len(results))
	for i, r := range results {
		if i == maxListed {
			fmt.Fprintf(w, "  ... and %d more (see the report)\n", len(results)-maxListed)
			break
		}
		fmt.Fprintf(w, "  %s: %s\n", displayPath(r, workingDir), reason(r))
	}
}

// displayPath shows a result's files relative to the working directory when
// they are below it, joining a Live Photo's two files with " + ".
func displayPath(r uploadResult, workingDir string) string {
	paths := r.Paths
	if len(paths) == 0 {
		paths = []string{r.Path}
	}
	shown := make([]string, len(paths))
	for i, p := range paths {
		shown[i] = p
		if workingDir != "" && filepath.IsAbs(p) {
			if rel, err := filepath.Rel(workingDir, p); err == nil && !strings.HasPrefix(rel, "..") {
				shown[i] = rel
			}
		}
	}
	return strings.Join(shown, " + ")
}

// firstLine shortens an error message to its first line, at most 200 bytes.
func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// thousands formats n with comma separators, e.g. 1,580.
func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
