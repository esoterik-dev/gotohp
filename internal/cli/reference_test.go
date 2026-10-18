package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var updateReference = flag.Bool("update", false, "rewrite docs/cli-reference.md from the CLI's help text")

// docs/cli-reference.md must match the CLI. After changing a command or
// flag, regenerate it: go test ./internal/cli -run TestCLIReference -update
func TestCLIReferenceIsCurrent(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(here), "..", "..", "docs", "cli-reference.md")
	var buf bytes.Buffer
	writeReference(&buf, newRootCommand(Info{ExecutableName: "gotohp-cli"}))
	if *updateReference {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; generate it with: go test ./internal/cli -run TestCLIReference -update", err)
	}
	if !bytes.Equal(current, buf.Bytes()) {
		t.Fatal("docs/cli-reference.md is out of date; regenerate it with: go test ./internal/cli -run TestCLIReference -update")
	}
}
