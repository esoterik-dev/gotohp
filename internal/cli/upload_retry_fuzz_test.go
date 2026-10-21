package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// FuzzRetryReportPaths feeds arbitrary report JSON to retryReportPaths.
// Properties: it never panics; on success every returned path is non-empty,
// unique, and is a source of some failed, non-skipped result (resolved the
// way the report says), so a path that only success or skipped results name
// is never retried; every failed source is returned; and without pairing no
// failed result has several paths.
func FuzzRetryReportPaths(f *testing.F) {
	for _, seed := range []string{
		`{"results":[{"path":"ok.jpg","success":true},{"path":"skip.jpg","skipped":true},{"path":"failed.jpg","error":"429"},{"path":"photo.heic","paths":["photo.heic","photo.mov"],"error":"429"},{"path":"failed.jpg","error":"429"}]}`,
		`{"workingDirectory":"/home/user","total":2,"results":[{"path":"photos/IMG_1.jpg","error":"429"},{"path":"/home/user/elsewhere/abs.jpg","error":"429"}]}`,
		`{"canceled":true,"total":5,"results":[{"path":"failed.jpg","error":"context canceled"}]}`,
		`{"canceled":true,"total":0,"results":[]}`,
		`{"results":[{"path":"a.jpg","success":true,"skipped":true},{"path":"a.jpg","success":false,"error":"x"}]}`,
		`{"results":[{"path":"ok.jpg","success":true,"paths":["ok.jpg","ok.mov"]},{"paths":["../up.jpg"],"error":"x"}],"workingDirectory":"/wd"}`,
		`{"results":[{"Path":"case.jpg","Success":false},{"PATH":"case.jpg","SUCCESS":true}]}`,
		`{"results":[{"path":"dup.jpg","success":true,"success":false}]}`,
		`{"results":[null,{"path":"x.jpg"}]}`,
		`{"results":[],"album":{"error":"failed"}}`,
		`{"results":[{"path":"x.jpg","error":"e"}],"album":{"error":"failed"}}`,
		`{"total":10,"results":[]}`,
		`{"results":[{"paths":[""]}]}`,
		`{"results":[{}]}`,
		`{"results":null}`,
		`{"results":{}}`,
		`[]`,
		`null`,
		`{`,
		``,
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}

	f.Fuzz(func(t *testing.T, data []byte, pair bool) {
		path := filepath.Join(t.TempDir(), "report.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := retryReportPaths(path, pair, io.Discard)
		if err != nil {
			if got != nil {
				t.Fatalf("error %v returned paths %q", err, got)
			}
			return
		}

		// Independent oracle over the same JSON. Results is decoded in a
		// second step so a repeated "results" key resolves as it does in the
		// loader (last one wins, nothing merged).
		var report struct {
			WorkingDirectory string          `json:"workingDirectory"`
			Results          json.RawMessage `json:"results"`
		}
		var results []struct {
			Path    string   `json:"path"`
			Paths   []string `json:"paths"`
			Success bool     `json:"success"`
			Skipped bool     `json:"skipped"`
		}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatalf("accepted a report the oracle cannot parse: %v\n%s", err, data)
		}
		if err := json.Unmarshal(report.Results, &results); err != nil {
			t.Fatalf("accepted results the oracle cannot parse: %v\n%s", err, data)
		}
		resolve := func(file string) string {
			if !filepath.IsAbs(file) && report.WorkingDirectory != "" {
				return filepath.Join(report.WorkingDirectory, file)
			}
			return file
		}
		failed := make(map[string]bool)
		for _, result := range results {
			if result.Success || result.Skipped {
				continue
			}
			files := result.Paths
			if len(files) == 0 {
				files = []string{result.Path}
			}
			if len(files) > 1 && !pair {
				t.Fatalf("accepted a failed media pair without pairing:\n%s", data)
			}
			for _, file := range files {
				failed[resolve(file)] = true
			}
		}
		seen := make(map[string]bool)
		for _, p := range got {
			if p == "" {
				t.Fatalf("returned an empty path:\n%s", data)
			}
			if seen[p] {
				t.Fatalf("returned %q twice:\n%s", p, data)
			}
			seen[p] = true
			if !failed[p] {
				t.Fatalf("returned %q, which no failed, non-skipped result names:\n%s", p, data)
			}
		}
		for p := range failed {
			if !seen[p] {
				t.Fatalf("dropped failed path %q:\n%s", p, data)
			}
		}
	})
}
