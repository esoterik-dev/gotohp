package cli

import (
	"encoding/json"
	"fmt"
	"os"

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

func retryReportPaths(path string, pairLivePhotos bool) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read retry report: %w", err)
	}
	var report struct {
		Total    int  `json:"total"`
		Canceled bool `json:"canceled"`
		Results  *[]struct {
			Path    string   `json:"path"`
			Paths   []string `json:"paths"`
			Success bool     `json:"success"`
			Skipped bool     `json:"skipped"`
		} `json:"results"`
		Album *struct {
			Error string `json:"error"`
		} `json:"album"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse retry report: %w", err)
	}
	if report.Results == nil {
		return nil, fmt.Errorf("retry report must contain a results array")
	}
	if report.Total > len(*report.Results) || (report.Canceled && len(*report.Results) == 0) {
		return nil, fmt.Errorf("incomplete report lacks source paths; rerun original sources with upload state instead")
	}
	paths := []string{}
	seen := make(map[string]bool)
	for i, result := range *report.Results {
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
