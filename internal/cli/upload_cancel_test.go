package cli

import (
	"bytes"
	"encoding/json"
	"errors"
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
	err := finishUpload(m, &out)
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 130 {
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
