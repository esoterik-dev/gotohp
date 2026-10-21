package cli

import (
	"os"
	"testing"
)

// TestMain runs the tests in a temporary directory, where commands write
// their default debug logs, so no test leaves a log in the source tree.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gotohp-cli-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
