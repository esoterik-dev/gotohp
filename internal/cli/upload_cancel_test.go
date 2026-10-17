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
	calls, aborts := 0, 0
	m.stop = func() { calls++ }
	m.cancel = func() { aborts++ }
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(uploadModel)
	if calls != 1 || aborts != 0 || !m.canceled || m.quitting || cmd != nil {
		t.Fatal("first Ctrl+C must stop new work without aborting in-flight files or quitting")
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
	aborted := false
	m.stop = func() {}
	m.cancel = func() { aborted = true }
	updated, _ := m.Update(cancelMsg{exitCode: exitCodeTerminate})
	m = updated.(uploadModel)
	updated, _ = m.Update(fileProgressMsg{workerID: 0, status: "uploading", fileName: "slow.mov"})
	m = updated.(uploadModel)
	updated, _ = m.Update(fileCompleteMsg{fileName: "photo.jpg", success: true})
	m = updated.(uploadModel)
	if view := m.View(); !strings.Contains(view, "waiting for 1 in-progress file(s) to finish") {
		t.Fatalf("no draining feedback in view:\n%s", view)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = updated.(uploadModel)
	if !m.quitting || !m.forceQuit || cmd == nil || !aborted {
		t.Fatal("second cancel did not abort in-flight files and quit")
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
