package cli

import (
	"os"
	"testing"
)

// TestMain keeps every run's debug log out of the real state directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gotohp-cli-test-state-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
