package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestForceQuitReportsUnfinishedAlbum(t *testing.T) {
	for _, tc := range []struct {
		name      string
		finished  []fileCompleteMsg
		wantAlbum bool
	}{
		{"files uploaded", []fileCompleteMsg{{fileName: "photo.jpg", success: true, mediaKey: "key"}}, true},
		// Core adds a remote duplicate's existing item to the album too.
		{"only remote duplicates", []fileCompleteMsg{{fileName: "photo.jpg", skipped: true, mediaKey: "existing"}}, true},
		{"only skips without a media key", []fileCompleteMsg{{fileName: "live.heic", skipped: true}}, false},
		{"only failures", []fileCompleteMsg{{fileName: "photo.jpg", err: errors.New("boom")}}, false},
		{"nothing finished", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := initialModel()
			m.requestedAlbum = "Trip"
			m.stop, m.cancel = func() {}, func() {}
			updated, _ := m.Update(uploadStartMsg{total: 2})
			m = updated.(uploadModel)
			for _, msg := range tc.finished {
				updated, _ = m.Update(msg)
				m = updated.(uploadModel)
			}
			updated, _ = m.Update(cancelMsg{exitCode: exitCodeInterrupt})
			m = updated.(uploadModel)
			updated, _ = m.Update(cancelMsg{exitCode: exitCodeInterrupt})
			m = updated.(uploadModel)
			var out bytes.Buffer
			_ = finishUpload(m, &out, "")
			var summary uploadSummary
			if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			if !tc.wantAlbum {
				if summary.Album != nil {
					t.Fatalf("album reported although no finished item had a media key: %+v", summary.Album)
				}
				return
			}
			if summary.Album == nil || summary.Album.Name != "Trip" || !strings.Contains(summary.Album.Error, "aborted") {
				t.Fatalf("report does not say the album was not finished: %+v", summary.Album)
			}
		})
	}
}

func TestDrainedCancelKeepsCreatedAlbum(t *testing.T) {
	m := initialModel()
	m.requestedAlbum = "Trip"
	m.stop = func() {}
	updated, _ := m.Update(cancelMsg{exitCode: exitCodeInterrupt})
	m = updated.(uploadModel)
	for _, msg := range []tea.Msg{
		fileCompleteMsg{fileName: "photo.jpg", success: true, mediaKey: "key"},
		albumCompleteMsg{albumName: "Trip", itemsAdded: 1, albumKeys: []string{"album-key"}},
		uploadCompleteMsg{},
	} {
		updated, _ = m.Update(msg)
		m = updated.(uploadModel)
	}
	var out bytes.Buffer
	_ = finishUpload(m, &out, "")
	var summary uploadSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Album == nil || summary.Album.Error != "" || len(summary.Album.AlbumKeys) != 1 {
		t.Fatalf("album created during a drain was not reported as created: %+v", summary.Album)
	}
}

// A 429 pause is shown with a countdown, the queue length is shown, and the
// countdown stops redrawing once the pause is over.
func TestViewShowsPauseAndQueue(t *testing.T) {
	m := initialModel()
	updated, _ := m.Update(uploadStartMsg{total: 10})
	m = updated.(uploadModel)
	updated, cmd := m.Update(pauseMsg{until: time.Now().Add(42 * time.Second)})
	m = updated.(uploadModel)
	if cmd == nil {
		t.Fatal("no countdown tick started")
	}
	updated, _ = m.Update(queueMsg{n: 7})
	m = updated.(uploadModel)
	view := m.View()
	if !strings.Contains(view, "Paused: Google rate limit") || !strings.Contains(view, "7 hashed file(s) waiting") {
		t.Fatalf("view:\n%s", view)
	}
	m.pausedUntil = time.Now().Add(-time.Second)
	if updated, cmd = m.Update(pauseTickMsg{}); cmd != nil || strings.Contains(updated.(uploadModel).View(), "Paused") {
		t.Fatal("countdown kept running after the pause")
	}
}
