package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"app/core"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

// Messages for bubbletea
type uploadStartMsg struct {
	total int
}

type fileProgressMsg struct {
	workerID int
	status   string
	fileName string
	message  string
}

type fileCompleteMsg struct {
	success    bool
	skipped    bool
	fileName   string
	paths      []string
	mediaKey   string
	skipCode   string
	skipReason string
	err        error
}

type preflightWarningMsg struct {
	paths   []string
	code    string
	message string
}

type uploadCompleteMsg struct{}

// cancelMsg asks the run to stop. The first one drains in-flight work so the
// report stays complete; a second one quits immediately with what has finished.
type cancelMsg struct {
	exitCode int
}

// Exit codes follow the shell convention of 128 plus the signal number.
const (
	exitCodeInterrupt = 130 // SIGINT
	exitCodeTerminate = 143 // SIGTERM
)

// Album messages
type albumProgressMsg struct {
	albumName  string
	itemsAdded int
	totalItems int
}

type albumCompleteMsg struct {
	albumName  string
	itemsAdded int
	albumKeys  []string
}

type albumErrorMsg struct {
	albumName string
	error     string
}

// Bubbletea model
type uploadModel struct {
	progress     progress.Model
	totalFiles   int
	completed    int
	failed       int
	skipped      int
	currentFiles map[int]string // workerID -> current file
	workers      map[int]string // workerID -> status message
	results      []uploadResult // Track all upload results
	warnings     []uploadWarning
	width        int
	quitting     bool
	canceled     bool
	forceQuit    bool
	cancelExit   int
	busy         map[int]bool // workerID -> currently processing an item
	cancel       func()
	// Album state
	albumName       string
	albumItemsAdded int
	albumTotalItems int
	albumComplete   bool
	albumError      string
	albumKeys       []string
}

type uploadResult struct {
	Path       string   `json:"path"`
	Paths      []string `json:"paths,omitempty"`
	Success    bool     `json:"success"`
	Skipped    bool     `json:"skipped,omitempty"`
	MediaKey   string   `json:"mediaKey,omitempty"`
	SkipCode   string   `json:"skipCode,omitempty"`
	SkipReason string   `json:"skipReason,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type uploadWarning struct {
	Paths   []string `json:"paths,omitempty"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
}

type albumSummary struct {
	Name       string   `json:"name,omitempty"`
	ItemsAdded int      `json:"itemsAdded,omitempty"`
	AlbumKeys  []string `json:"albumKeys,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type uploadSummary struct {
	Canceled  bool            `json:"canceled,omitempty"`
	Total     int             `json:"total"`
	Succeeded int             `json:"succeeded"`
	Failed    int             `json:"failed"`
	Skipped   int             `json:"skipped"`
	Results   []uploadResult  `json:"results"`
	Warnings  []uploadWarning `json:"warnings,omitempty"`
	Album     *albumSummary   `json:"album,omitempty"`
}

func initialModel() uploadModel {
	return uploadModel{
		progress:     progress.New(progress.WithDefaultGradient()),
		currentFiles: make(map[int]string),
		workers:      make(map[int]string),
		busy:         make(map[int]bool),
		results:      []uploadResult{},
		warnings:     []uploadWarning{},
		width:        80,
	}
}

func (m uploadModel) Init() tea.Cmd {
	return nil
}

func (m uploadModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.progress.Width = msg.Width - 4
		return m, nil

	case uploadStartMsg:
		m.totalFiles = msg.total
		if m.canceled && m.cancel != nil {
			m.cancel()
		}
		return m, nil

	case fileProgressMsg:
		m.workers[msg.workerID] = fmt.Sprintf("[%d] %s: %s", msg.workerID, msg.status, msg.fileName)
		m.busy[msg.workerID] = msg.status != "idle" && msg.status != "error"
		if msg.fileName != "" {
			m.currentFiles[msg.workerID] = msg.fileName
		}
		return m, nil

	case fileCompleteMsg:
		result := uploadResult{
			Path:       msg.fileName,
			Paths:      msg.paths,
			Success:    msg.success,
			Skipped:    msg.skipped,
			MediaKey:   msg.mediaKey,
			SkipCode:   msg.skipCode,
			SkipReason: msg.skipReason,
		}
		if msg.skipped {
			m.skipped++
		} else if msg.success {
			m.completed++
		} else {
			m.failed++
			if msg.err != nil {
				result.Error = msg.err.Error()
			}
		}
		m.results = append(m.results, result)
		return m, nil

	case preflightWarningMsg:
		m.warnings = append(m.warnings, uploadWarning{
			Paths:   msg.paths,
			Code:    msg.code,
			Message: msg.message,
		})
		return m, nil

	case uploadCompleteMsg:
		m.quitting = true
		return m, tea.Quit

	case albumProgressMsg:
		m.albumName = msg.albumName
		m.albumItemsAdded = msg.itemsAdded
		m.albumTotalItems = msg.totalItems
		m.albumComplete = false
		return m, nil

	case albumCompleteMsg:
		m.albumName = msg.albumName
		m.albumItemsAdded = msg.itemsAdded
		m.albumComplete = true
		m.albumKeys = msg.albumKeys
		return m, nil

	case albumErrorMsg:
		m.albumName = msg.albumName
		m.albumError = msg.error
		return m, nil

	case cancelMsg:
		return m.requestCancel(msg.exitCode)

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m.requestCancel(exitCodeInterrupt)
		}
	}

	return m, nil
}

// requestCancel drains in-flight work on the first request and quits
// immediately on the second, keeping whatever results have arrived.
func (m uploadModel) requestCancel(exitCode int) (tea.Model, tea.Cmd) {
	if m.canceled {
		m.forceQuit = true
		m.quitting = true
		return m, tea.Quit
	}
	m.canceled = true
	m.cancelExit = exitCode
	if m.cancel != nil {
		m.cancel()
	}
	return m, nil // Drain worker results before writing the final report.
}

func (m uploadModel) busyWorkers() int {
	n := 0
	for _, busy := range m.busy {
		if busy {
			n++
		}
	}
	return n
}

func (m uploadModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	b.WriteString(titleStyle.Render("Uploading to Google Photos"))
	b.WriteString("\n\n")

	if m.canceled {
		cancelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
		b.WriteString(cancelStyle.Render(fmt.Sprintf(
			"Canceling, waiting for %d worker(s). Press Ctrl+C again to force quit.", m.busyWorkers())))
		b.WriteString("\n\n")
	}

	// Progress bar
	if m.totalFiles > 0 {
		percent := float64(m.completed+m.failed+m.skipped) / float64(m.totalFiles)
		b.WriteString(m.progress.ViewAs(percent))
		fmt.Fprintf(&b, "\n%d/%d items", m.completed+m.failed+m.skipped, m.totalFiles)
		fmt.Fprintf(&b, " (? %d success, ? %d skipped, ? %d failed)\n\n", m.completed, m.skipped, m.failed)
	}

	// Worker status
	for i := 0; i < len(m.workers); i++ {
		if status, ok := m.workers[i]; ok {
			b.WriteString(status)
			b.WriteString("\n")
		}
	}

	if len(m.warnings) > 0 {
		warningStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
		b.WriteString("\n")
		b.WriteString(warningStyle.Render("Upload warnings:"))
		b.WriteString("\n")
		for _, warning := range m.warnings {
			fmt.Fprintf(&b, "- [%s] %s", warning.Code, warning.Message)
			if len(warning.Paths) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(warning.Paths, ", "))
			}
			b.WriteString("\n")
		}
	}

	// Album progress
	if m.albumName != "" {
		b.WriteString("\n")
		albumStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
		if m.albumError != "" {
			errorStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
			b.WriteString(errorStyle.Render("? Album error: "))
			b.WriteString(m.albumError)
			b.WriteString("\n")
		} else if m.albumComplete {
			b.WriteString(albumStyle.Render("? Added to album: "))
			b.WriteString(m.albumName)
			fmt.Fprintf(&b, " (%d items)\n", m.albumItemsAdded)
		} else if m.albumTotalItems > 0 {
			b.WriteString(albumStyle.Render("Adding to album: "))
			b.WriteString(m.albumName)
			fmt.Fprintf(&b, " (%d/%d items)\n", m.albumItemsAdded, m.albumTotalItems)
		}
	}

	b.WriteString("\n\nPress Ctrl+C to cancel\n")

	return b.String()
}

// parseLogLevel converts a string log level to slog.Level
func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		// Default to info for CLI
		return slog.LevelInfo
	}
}

func shouldUseTUI(settings uploadRunSettings) bool {
	return !settings.noTUI &&
		term.IsTerminal(os.Stdin.Fd()) &&
		term.IsTerminal(os.Stdout.Fd())
}

// teaReporter renders backend progress by forwarding it to the bubbletea program.
type teaReporter struct {
	p *tea.Program
}

func (r teaReporter) UploadStart(start core.UploadBatchStart) {
	r.p.Send(uploadStartMsg{total: start.Total})
}

func (r teaReporter) UploadStop() {
	r.p.Send(uploadCompleteMsg{})
}

func (r teaReporter) TotalBytes(int64)      {}
func (r teaReporter) TotalBytesDelta(int64) {}

func (r teaReporter) Warning(warning core.PreflightWarning) {
	r.p.Send(preflightWarningMsg{
		paths:   warning.Paths,
		code:    warning.Code,
		message: warning.Message,
	})
}

func (r teaReporter) ThreadStatus(status core.ThreadStatus) {
	r.p.Send(fileProgressMsg{
		workerID: status.WorkerID,
		status:   status.Status,
		fileName: status.FileName,
		message:  status.Message,
	})
}

func (r teaReporter) FileResult(result core.FileUploadResult) {
	r.p.Send(fileCompleteMsg{
		success:    !result.IsError && !result.Skipped,
		skipped:    result.Skipped,
		fileName:   result.Path,
		paths:      result.Paths,
		mediaKey:   result.MediaKey,
		skipCode:   result.SkipCode,
		skipReason: result.SkipReason,
		err:        result.Error,
	})
}

func (r teaReporter) AlbumProgress(status core.AlbumStatus) {
	r.p.Send(albumProgressMsg{
		albumName:  status.AlbumName,
		itemsAdded: status.ItemsAdded,
		totalItems: status.TotalItems,
	})
}

func (r teaReporter) AlbumComplete(status core.AlbumStatus) {
	r.p.Send(albumCompleteMsg{
		albumName:  status.AlbumName,
		itemsAdded: status.ItemsAdded,
		albumKeys:  status.AlbumKeys,
	})
}

func (r teaReporter) AlbumError(albumErr core.AlbumError) {
	r.p.Send(albumErrorMsg{
		albumName: albumErr.AlbumName,
		error:     albumErr.Error,
	})
}

func newLogger(level slog.Level) *slog.Logger {
	if level > slog.LevelDebug {
		// Anything above debug would only clutter the TUI; the JSON summary
		// carries the outcome.
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// runUpload runs the upload with already-resolved options and prints a JSON
// summary when it completes.
func runUpload(paths []string, opts core.UploadOptions, settings uploadRunSettings) error {
	model := initialModel()
	programOptions := []tea.ProgramOption{tea.WithoutSignalHandler()}
	if !shouldUseTUI(settings) {
		programOptions = append(
			programOptions,
			tea.WithInput(nil),
			tea.WithoutRenderer(),
		)
	}
	var uploadManager *core.UploadManager
	model.cancel = func() { uploadManager.Cancel() }
	p := tea.NewProgram(model, programOptions...)
	uploadManager = core.NewUploadManager(teaReporter{p: p}, newLogger(parseLogLevel(settings.logLevel)))
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	useTUI := shouldUseTUI(settings)
	go forwardSignals(p, signals, done, useTUI)
	go uploadManager.Upload(paths, opts)

	// Run until the upload manager reports UploadStop.
	finalModel, err := p.Run()
	if err != nil {
		uploadManager.Cancel()
		return fmt.Errorf("error running upload program: %w", err)
	}

	if m, ok := finalModel.(uploadModel); ok {
		return finishUpload(m, os.Stdout, settings.reportFile)
	}

	return nil
}

// forwardSignals turns SIGINT and SIGTERM into cancel requests until done is
// closed. Without the TUI there is no cancel banner, so it explains on stderr.
func forwardSignals(p *tea.Program, signals <-chan os.Signal, done <-chan struct{}, useTUI bool) {
	for first := true; ; first = false {
		select {
		case sig := <-signals:
			exitCode := exitCodeInterrupt
			if sig == syscall.SIGTERM {
				exitCode = exitCodeTerminate
			}
			if first && !useTUI {
				fmt.Fprintln(os.Stderr, "Canceling: waiting for in-progress uploads. Send the signal again to force quit.")
			}
			p.Send(cancelMsg{exitCode: exitCode})
		case <-done:
			return
		}
	}
}

// finishUpload writes the complete result before reporting a failed run. The
// report file is the durable record, so it is written even if stdout fails.
func finishUpload(model uploadModel, output io.Writer, reportFile string) error {
	jsonOutput, err := json.MarshalIndent(buildUploadSummary(model), "", "  ")
	if err != nil {
		return fmt.Errorf("error generating JSON: %w", err)
	}
	jsonOutput = append(jsonOutput, '\n')
	var reportErr, outputErr error
	if reportFile != "" {
		if err := writeUploadReport(reportFile, jsonOutput); err != nil {
			reportErr = fmt.Errorf("error writing JSON report %q: %w", reportFile, err)
		}
	}
	if _, err := output.Write(jsonOutput); err != nil {
		outputErr = fmt.Errorf("error writing JSON: %w", err)
	}
	if err := errors.Join(reportErr, outputErr); err != nil {
		return err
	}
	if model.canceled {
		return exitError{code: model.cancelExit}
	}
	if model.failed > 0 || model.albumError != "" {
		return exitError{code: 1}
	}
	return nil
}

func buildUploadSummary(model uploadModel) uploadSummary {
	warnings := make([]uploadWarning, 0, len(model.warnings))
	for _, warning := range model.warnings {
		if warning.Code == "incomplete-live-photo-skipped" || warning.Code == "ambiguous-filename-stem" {
			continue
		}
		warnings = append(warnings, warning)
	}
	summary := uploadSummary{
		Canceled:  model.canceled,
		Total:     model.totalFiles,
		Succeeded: model.completed,
		Failed:    model.failed,
		Skipped:   model.skipped,
		Results:   model.results,
		Warnings:  warnings,
	}
	if model.albumName != "" {
		summary.Album = &albumSummary{
			Name:       model.albumName,
			ItemsAdded: model.albumItemsAdded,
			AlbumKeys:  model.albumKeys,
			Error:      model.albumError,
		}
	}
	return summary
}
