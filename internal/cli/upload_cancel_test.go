package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCtrlCWaitsForWorkersAndPreservesReport(t *testing.T) {
	m := initialModel()
	calls := 0
	m.cancel = func() { calls++ }
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(uploadModel)
	if calls != 1 || !m.canceled || m.quitting || cmd != nil {
		t.Fatal("Ctrl+C quit before workers drained")
	}
	updated, _ = m.Update(uploadStartMsg{total: 2})
	m = updated.(uploadModel)
	if calls != 2 {
		t.Fatal("early cancellation not replayed after manager start")
	}
	updated, _ = m.Update(fileCompleteMsg{fileName: "photo.jpg", success: true})
	m = updated.(uploadModel)
	updated, cmd = m.Update(uploadCompleteMsg{})
	m = updated.(uploadModel)
	if !m.quitting || cmd == nil {
		t.Fatal("did not quit after worker completion")
	}
	var out bytes.Buffer
	err := finishUpload(m, &out, "")
	var exit exitError
	if !errors.As(err, &exit) || exit.code != exitCodeInterrupt {
		t.Fatalf("got %v", err)
	}
	var summary uploadSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.Canceled || summary.Succeeded != 1 || len(summary.Results) != 1 {
		t.Fatalf("lost results: %+v", summary)
	}
}

func TestSecondCancelForceQuitsWithFinishedResults(t *testing.T) {
	m := initialModel()
	m.cancel = func() {}
	updated, _ := m.Update(cancelMsg{exitCode: exitCodeTerminate})
	m = updated.(uploadModel)
	updated, _ = m.Update(fileProgressMsg{workerID: 0, status: "uploading", fileName: "slow.mov"})
	m = updated.(uploadModel)
	updated, _ = m.Update(fileCompleteMsg{fileName: "photo.jpg", success: true})
	m = updated.(uploadModel)
	if view := m.View(); !strings.Contains(view, "Canceling, waiting for 1 worker(s)") {
		t.Fatalf("no draining feedback in view:\n%s", view)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(uploadModel)
	if !m.quitting || !m.forceQuit || cmd == nil {
		t.Fatal("second cancel did not force quit")
	}
	var out bytes.Buffer
	err := finishUpload(m, &out, "")
	var exit exitError
	if !errors.As(err, &exit) || exit.code != exitCodeTerminate {
		t.Fatalf("exit=%v, want the first signal's code %d", err, exitCodeTerminate)
	}
	var summary uploadSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.Canceled || len(summary.Results) != 1 {
		t.Fatalf("finished results lost on force quit: %+v", summary)
	}
}
