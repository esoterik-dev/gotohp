package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func uploadArgs(cmd *cobra.Command, args []string) error {
	report, _ := cmd.Flags().GetString("retry-report")
	if report != "" {
		if len(args) > 0 {
			return fmt.Errorf("--retry-report cannot be combined with positional paths")
		}
		return nil
	}
	return cobra.MinimumNArgs(1)(cmd, args)
}

type retryReportResult struct {
	Path    string   `json:"path"`
	Paths   []string `json:"paths"`
	Success bool     `json:"success"`
	Skipped bool     `json:"skipped"`
}

// retryReportPaths returns the source paths of failed, non-skipped results.
// Relative paths are resolved against the directory the original upload ran
// in, which the report records; reports from older builds fall back to the
// current directory. A canceled run is retried with a warning on warn, since
// items that never started are not in its results.
func retryReportPaths(path string, pairLivePhotos bool, warn io.Writer) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read retry report: %w", err)
	}
	var report struct {
		WorkingDirectory string          `json:"workingDirectory"`
		Total            int             `json:"total"`
		Canceled         bool            `json:"canceled"`
		Results          json.RawMessage `json:"results"`
		Album            *struct {
			Error string `json:"error"`
		} `json:"album"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse retry report: %w", err)
	}
	var results []retryReportResult
	if len(report.Results) > 0 {
		if err := json.Unmarshal(report.Results, &results); err != nil {
			return nil, fmt.Errorf("parse retry report results: %w", err)
		}
	}
	if results == nil {
		return nil, fmt.Errorf("retry report must contain a results array")
	}
	if report.Canceled && report.Total == 0 && len(results) == 0 {
		return nil, fmt.Errorf("the report is from a run canceled before any file started; rerun the original sources instead")
	}
	if missing := report.Total - len(results); missing > 0 {
		if !report.Canceled {
			return nil, fmt.Errorf("incomplete report lacks source paths; rerun the original sources instead, with --state-file to skip files already confirmed")
		}
		fmt.Fprintf(warn, "Warning: the report is from a canceled run; %d item(s) that never started are not retried. Rerun the original sources to include them.\n", missing)
	}
	paths := []string{}
	seen := make(map[string]bool)
	for i, result := range results {
		if result.Success || result.Skipped {
			continue
		}
		files := result.Paths
		if len(files) > 1 && !pairLivePhotos {
			return nil, fmt.Errorf("failed result %d contains a media pair; repeat --pair-live-photos and any repair flags", i)
		}
		if len(files) == 0 && result.Path != "" {
			files = []string{result.Path}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("failed result %d has no source paths", i)
		}
		for _, file := range files {
			if file == "" {
				return nil, fmt.Errorf("failed result %d contains an empty source path", i)
			}
			if !filepath.IsAbs(file) && report.WorkingDirectory != "" {
				file = filepath.Join(report.WorkingDirectory, file)
			}
			if !seen[file] {
				paths = append(paths, file)
				seen[file] = true
			}
		}
	}
	if len(paths) == 0 && report.Album != nil && report.Album.Error != "" {
		return nil, fmt.Errorf("report contains only album failures; file retries cannot repair album work")
	}
	return paths, nil
}

// relocateRetryPaths finds each retry path under sourceRoot (--source-root)
// by name, the way verify-live-photos --source-root does, for reports whose
// recorded paths are gone, such as a temporary folder or an old mount point.
// With folders (--folder) it keeps only files in one of those folders
// directly below sourceRoot. Paths it cannot find are skipped with a warning.
// It fails when no found file is in the requested folders, so a misspelt
// folder is not mistaken for nothing left to retry.
func relocateRetryPaths(paths []string, sourceRoot string, folders []string, warn io.Writer) ([]string, error) {
	if sourceRoot == "" {
		return paths, nil
	}
	if info, err := os.Stat(sourceRoot); err != nil {
		return nil, fmt.Errorf("--source-root: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("--source-root %q is not a directory", sourceRoot)
	}
	index, err := indexSourceRoot(sourceRoot)
	if err != nil {
		return nil, err
	}
	resolver := reportPathResolver{index: index}
	kept := []string{}
	seen := make(map[string]bool)
	for _, path := range paths {
		found, err := resolver.resolve(path)
		if err != nil {
			fmt.Fprintf(warn, "Warning: not retrying %q: %v\n", path, err)
			continue
		}
		if len(folders) > 0 && !inRetryFolder(sourceRoot, found, folders) {
			continue
		}
		if !seen[found] {
			kept = append(kept, found)
			seen[found] = true
		}
	}
	if len(folders) > 0 && len(kept) == 0 {
		return nil, fmt.Errorf("no failed files were found in --folder %s under %s", strings.Join(folders, ", "), sourceRoot)
	}
	return kept, nil
}

// inRetryFolder reports whether path is in one of folders directly below root.
func inRetryFolder(root, path string, folders []string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	top, _, nested := strings.Cut(filepath.ToSlash(rel), "/")
	if !nested {
		return false // directly in the root, not in a folder
	}
	for _, folder := range folders {
		if strings.EqualFold(top, folder) {
			return true
		}
	}
	return false
}

// existingRetryPaths drops retry paths that no longer exist, warning about
// each, so one moved file does not block retrying the others.
func existingRetryPaths(paths []string, warn io.Writer) []string {
	kept := paths[:0:0]
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(warn, "Warning: not retrying %q: %v\n", path, err)
			continue
		}
		kept = append(kept, path)
	}
	return kept
}
