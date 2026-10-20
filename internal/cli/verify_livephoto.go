package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"app/core"
)

// verifyUploadReport is deliberately smaller than the upload report model so
// this command can consume reports from older gotohp-cli builds as well.
type verifyUploadReport struct {
	WorkingDirectory string               `json:"workingDirectory"`
	Results          []verifyUploadResult `json:"results"`
}

type verifyUploadResult struct {
	Path       string   `json:"path"`
	Paths      []string `json:"paths"`
	Success    bool     `json:"success"`
	Skipped    bool     `json:"skipped"`
	SkipCode   string   `json:"skipCode"`
	SkipReason string   `json:"skipReason"`
	Error      string   `json:"error"`
	MediaKey   string   `json:"mediaKey"`
}

type verifyPairInput struct {
	Index      int
	Upload     verifyUploadResult
	PhotoPath  string
	VideoPath  string
	ResolveErr string
}

type livePhotoVerificationItem struct {
	Index           int    `json:"-"`
	Path            string `json:"path"`
	PhotoPath       string `json:"photoPath"`
	VideoPath       string `json:"videoPath"`
	UploadSuccess   bool   `json:"uploadSuccess"`
	UploadSkipped   bool   `json:"uploadSkipped"`
	UploadSkipCode  string `json:"uploadSkipCode,omitempty"`
	UploadMediaKey  string `json:"uploadMediaKey,omitempty"`
	LocalPhotoSHA1  string `json:"localPhotoSHA1,omitempty"`
	LocalVideoSHA1  string `json:"localVideoSHA1,omitempty"`
	PhotoMediaKey   string `json:"photoMediaKey,omitempty"`
	VideoMediaKey   string `json:"videoMediaKey,omitempty"`
	PhotoRelatedKey string `json:"photoRelatedKey,omitempty"`
	VideoRelatedKey string `json:"videoRelatedKey,omitempty"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
}

type livePhotoVerificationReport struct {
	Version              int                         `json:"version"`
	GeneratedAt          string                      `json:"generatedAt"`
	Method               string                      `json:"method"`
	MediaBytesDownloaded bool                        `json:"mediaBytesDownloaded"`
	UploadReport         string                      `json:"uploadReport"`
	SourceRoot           string                      `json:"sourceRoot,omitempty"`
	Canceled             bool                        `json:"canceled,omitempty"`
	TotalPairs           int                         `json:"totalPairs"`
	VerifiedPairs        int                         `json:"verifiedPairs"`
	PhotoOnlyPairs       int                         `json:"photoOnlyPairs"`
	VideoOnlyPairs       int                         `json:"videoOnlyPairs"`
	MismatchedPairs      int                         `json:"mismatchedPairs"`
	NotFoundPairs        int                         `json:"notFoundPairs"`
	LocalErrors          int                         `json:"localErrors"`
	APIErrors            int                         `json:"apiErrors"`
	CanceledPairs        int                         `json:"canceledPairs,omitempty"`
	Results              []livePhotoVerificationItem `json:"results"`
}

func newVerifyLivePhotosCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify-live-photos <upload-report.json>",
		Short: "Verify Live Photo pairs using remote hash responses",
		Long: "Verify Live Photo pairs without downloading media from Google Photos.\n\n" +
			"Each local HEIF and MOV is hashed, and the authenticated Google Photos\n" +
			"hash endpoint is queried for both hashes. A pair is verified when both\n" +
			"responses share a primary or related remote media key. This is a\n" +
			"metadata check, not playback or downloaded-content verification.\n\n" +
			"Files are read from the paths in the report. If the files have moved,\n" +
			"--source-root finds each one under a new root by file name, using its\n" +
			"parent folder name to tell apart files with the same name.",
		Args: cobra.ExactArgs(1),
		RunE: runVerifyLivePhotosCommand,
	}
	f := cmd.Flags()
	f.SortFlags = false
	f.String("source-root", "", "find the report's files under this directory instead of their recorded paths")
	f.String("account", "", "account to check (default: the selected account)")
	f.String("proxy", "", "proxy URL for all requests")
	f.IntP("threads", "t", 1, "number of concurrent verification workers")
	addRequestPolicyFlags(f)
	f.Int("limit", 0, "verify only the first N pairs (0 means all)")
	f.String("report-file", "", "write the verification JSON report to PATH")
	return cmd
}

func runVerifyLivePhotosCommand(cmd *cobra.Command, args []string) error {
	reportPath := args[0]
	f := cmd.Flags()
	sourceRoot, _ := f.GetString("source-root")
	threads, _ := f.GetInt("threads")
	if threads < 1 {
		return fmt.Errorf("threads must be a positive integer, got %d", threads)
	}
	limit, _ := f.GetInt("limit")
	if limit < 0 {
		return fmt.Errorf("limit must not be negative, got %d", limit)
	}
	if err := core.LoadConfig(getInheritedConfigPath(cmd)); err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	reportFile, _ := f.GetString("report-file")
	if err := checkReportFile(reportFile, map[string]string{"the upload report": reportPath}); err != nil {
		return err
	}

	uploadReport, err := readVerifyUploadReport(reportPath)
	if err != nil {
		return err
	}
	resolver := reportPathResolver{workingDirectory: uploadReport.WorkingDirectory}
	if sourceRoot != "" {
		if resolver.index, err = indexSourceRoot(sourceRoot); err != nil {
			return err
		}
	}
	pairs := verificationPairs(uploadReport, resolver)
	if len(pairs) == 0 {
		return fmt.Errorf("no two-file Live Photo pairs found in %s", reportPath)
	}
	if limit > 0 && limit < len(pairs) {
		pairs = pairs[:limit]
	}

	account, _ := f.GetString("account")
	proxy, _ := f.GetString("proxy")
	policy, err := requestPolicyFromFlags(f)
	if err != nil {
		return err
	}
	api, err := core.NewApi(core.ApiOptions{Account: account, Proxy: proxy, Policy: policy})
	if err != nil {
		return fmt.Errorf("create Google Photos API client: %w", err)
	}
	// Fail fast on bad credentials rather than once per pair.
	if _, err := api.BearerToken(cmd.Context()); err != nil {
		return fmt.Errorf("authenticate Google Photos API: %w", err)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out := cmd.ErrOrStderr()
	results := verifyPairs(ctx, pairs, api, threads, newVerifyProgress(out, len(pairs)))
	stop()

	report := buildLivePhotoVerificationReport(reportPath, sourceRoot, results)
	report.Canceled = ctx.Err() != nil
	if reportFile != "" {
		if err := writeLivePhotoVerificationReport(reportFile, report); err != nil {
			return err
		}
	}
	printLivePhotoVerificationSummary(out, report, reportFile)
	if report.Canceled {
		return exitError{code: exitCodeInterrupt}
	}
	if report.VerifiedPairs != report.TotalPairs {
		return exitError{code: 1}
	}
	return nil
}

// verifyPairs checks pairs on threads workers. Pairs not started before ctx
// is canceled are reported as canceled, so the report stays complete.
func verifyPairs(ctx context.Context, pairs []verifyPairInput, api remoteMatchAPI, threads int, progress func()) []livePhotoVerificationItem {
	results := make([]livePhotoVerificationItem, len(pairs))
	jobs := make(chan verifyPairInput)
	var workers sync.WaitGroup
	for range threads {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for pair := range jobs {
				results[pair.Index] = verifyLivePhotoPair(ctx, pair, api)
				progress()
			}
		}()
	}
	sent := 0
feed:
	for ; sent < len(pairs); sent++ {
		select {
		case jobs <- pairs[sent]:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	workers.Wait()
	for _, pair := range pairs[sent:] {
		results[pair.Index] = newVerificationItem(pair)
		results[pair.Index].Status = "canceled"
	}
	return results
}

// newVerifyProgress returns a per-pair progress callback. It redraws one line
// on a terminal and stays quiet otherwise, so logs are not filled with
// carriage returns.
func newVerifyProgress(out io.Writer, total int) func() {
	file, ok := out.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return func() {}
	}
	var mu sync.Mutex
	done := 0
	return func() {
		mu.Lock()
		defer mu.Unlock()
		done++
		fmt.Fprintf(out, "\rVerifying Live Photo pairs: %d/%d", done, total)
		if done == total {
			fmt.Fprintln(out)
		}
	}
}

// getInheritedConfigPath keeps the verification command aligned with upload
// and credentials, where --config is a root persistent flag.
func getInheritedConfigPath(cmd *cobra.Command) string {
	path, _ := cmd.Flags().GetString("config")
	if path == "" {
		path, _ = cmd.InheritedFlags().GetString("config")
	}
	return path
}

func readVerifyUploadReport(path string) (verifyUploadReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return verifyUploadReport{}, fmt.Errorf("read upload report %q: %w", path, err)
	}
	var report verifyUploadReport
	if err := json.Unmarshal(data, &report); err != nil {
		return verifyUploadReport{}, fmt.Errorf("parse upload report %q: %w", path, err)
	}
	return report, nil
}

func verificationPairs(report verifyUploadReport, resolver reportPathResolver) []verifyPairInput {
	pairs := make([]verifyPairInput, 0)
	for _, upload := range report.Results {
		if len(upload.Paths) != 2 {
			continue
		}
		photoReportPath, videoReportPath, ok := splitLivePhotoPaths(upload.Paths)
		if !ok {
			continue
		}
		photoPath, photoErr := resolver.resolve(photoReportPath)
		videoPath, videoErr := resolver.resolve(videoReportPath)
		input := verifyPairInput{Index: len(pairs), Upload: upload, PhotoPath: photoPath, VideoPath: videoPath}
		var problems []string
		if photoErr != nil {
			problems = append(problems, "photo: "+photoErr.Error())
		}
		if videoErr != nil {
			problems = append(problems, "video: "+videoErr.Error())
		}
		input.ResolveErr = strings.Join(problems, "; ")
		pairs = append(pairs, input)
	}
	return pairs
}

func splitLivePhotoPaths(paths []string) (photo, video string, ok bool) {
	for _, path := range paths {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".mov", ".mp4", ".m4v":
			if video != "" {
				return "", "", false
			}
			video = path
		default:
			if photo != "" {
				return "", "", false
			}
			photo = path
		}
	}
	return photo, video, photo != "" && video != ""
}

// reportPathResolver maps a path recorded in an upload report to a local
// file. Without an index it uses the recorded path, resolving relative paths
// against the upload's working directory. With an index (--source-root) it
// finds the file by name under the new root.
type reportPathResolver struct {
	workingDirectory string
	index            map[string][]string // lower-case base name -> paths under the root
}

func (r reportPathResolver) resolve(reportPath string) (string, error) {
	if r.index == nil {
		path := reportPath
		if !filepath.IsAbs(path) && r.workingDirectory != "" {
			path = filepath.Join(r.workingDirectory, path)
		}
		return path, nil
	}
	candidates := r.index[strings.ToLower(filepath.Base(reportPath))]
	if len(candidates) > 1 {
		// IMG_####.HEIC repeats across 100APPLE, 101APPLE, ...; the parent
		// folder name tells them apart when the tree kept its folders.
		parent := filepath.Base(filepath.Dir(reportPath))
		var sameParent []string
		for _, candidate := range candidates {
			if strings.EqualFold(filepath.Base(filepath.Dir(candidate)), parent) {
				sameParent = append(sameParent, candidate)
			}
		}
		candidates = sameParent
		if len(candidates) != 1 {
			return "", fmt.Errorf("ambiguous source for %s: several files share its name and none or several are in a %q folder", reportPath, parent)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("cannot find %s under the source root", filepath.Base(reportPath))
	}
	return candidates[0], nil
}

// indexSourceRoot lists every regular file under root by lower-case name.
func indexSourceRoot(root string) (map[string][]string, error) {
	index := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			name := strings.ToLower(entry.Name())
			index[name] = append(index[name], path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan --source-root %q: %w", root, err)
	}
	return index, nil
}

type remoteMatchAPI interface {
	FindRemoteMediaMatchesByHash(context.Context, []byte) (core.RemoteMediaMatch, error)
}

func newVerificationItem(pair verifyPairInput) livePhotoVerificationItem {
	return livePhotoVerificationItem{
		Index:          pair.Index,
		Path:           pair.Upload.Path,
		PhotoPath:      pair.PhotoPath,
		VideoPath:      pair.VideoPath,
		UploadSuccess:  pair.Upload.Success,
		UploadSkipped:  pair.Upload.Skipped,
		UploadSkipCode: pair.Upload.SkipCode,
		UploadMediaKey: pair.Upload.MediaKey,
	}
}

func verifyLivePhotoPair(ctx context.Context, pair verifyPairInput, api remoteMatchAPI) livePhotoVerificationItem {
	item := newVerificationItem(pair)
	if pair.ResolveErr != "" {
		item.Status = "local-error"
		item.Error = pair.ResolveErr
		return item
	}
	canceled := func(err error) bool {
		if errors.Is(err, context.Canceled) {
			item.Status = "canceled"
			return true
		}
		return false
	}

	photoHash, err := core.CalculateSHA1(ctx, pair.PhotoPath)
	if err != nil {
		if !canceled(err) {
			item.Status = "local-error"
			item.Error = fmt.Sprintf("hash photo: %v", err)
		}
		return item
	}
	videoHash, err := core.CalculateSHA1(ctx, pair.VideoPath)
	if err != nil {
		if !canceled(err) {
			item.Status = "local-error"
			item.Error = fmt.Sprintf("hash video: %v", err)
		}
		return item
	}
	item.LocalPhotoSHA1 = hex.EncodeToString(photoHash)
	item.LocalVideoSHA1 = hex.EncodeToString(videoHash)

	photoMatch, photoErr := api.FindRemoteMediaMatchesByHash(ctx, photoHash)
	videoMatch, videoErr := api.FindRemoteMediaMatchesByHash(ctx, videoHash)
	item.PhotoMediaKey = photoMatch.PrimaryMediaKey
	item.VideoMediaKey = videoMatch.PrimaryMediaKey
	item.PhotoRelatedKey = photoMatch.RelatedMediaKey
	item.VideoRelatedKey = videoMatch.RelatedMediaKey
	if photoErr != nil || videoErr != nil {
		if canceled(errors.Join(photoErr, videoErr)) {
			return item
		}
		var problems []string
		if photoErr != nil {
			problems = append(problems, "photo: "+photoErr.Error())
		}
		if videoErr != nil {
			problems = append(problems, "video: "+videoErr.Error())
		}
		item.Status = "api-error"
		item.Error = strings.Join(problems, "; ")
		return item
	}

	photoKeys := mediaMatchKeys(photoMatch)
	videoKeys := mediaMatchKeys(videoMatch)
	switch {
	case hasMediaKeyIntersection(photoKeys, videoKeys):
		item.Status = "paired"
	case len(photoKeys) > 0 && len(videoKeys) == 0:
		item.Status = "photo-only"
	case len(photoKeys) == 0 && len(videoKeys) > 0:
		item.Status = "video-only"
	case len(photoKeys) > 0 && len(videoKeys) > 0:
		item.Status = "mismatched-media-keys"
	default:
		item.Status = "not-found"
	}
	return item
}

func mediaMatchKeys(match core.RemoteMediaMatch) []string {
	keys := make([]string, 0, 2)
	if match.PrimaryMediaKey != "" {
		keys = append(keys, match.PrimaryMediaKey)
	}
	if match.RelatedMediaKey != "" && match.RelatedMediaKey != match.PrimaryMediaKey {
		keys = append(keys, match.RelatedMediaKey)
	}
	return keys
}

func hasMediaKeyIntersection(left, right []string) bool {
	for _, a := range left {
		for _, b := range right {
			if a == b {
				return true
			}
		}
	}
	return false
}

func buildLivePhotoVerificationReport(uploadReport, sourceRoot string, results []livePhotoVerificationItem) livePhotoVerificationReport {
	report := livePhotoVerificationReport{
		Version:              1,
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
		Method:               "local SHA-1 hashes compared through the Google Photos remote-hash API; no Google Photos media bytes downloaded",
		MediaBytesDownloaded: false,
		UploadReport:         uploadReport,
		SourceRoot:           sourceRoot,
		TotalPairs:           len(results),
		Results:              results,
	}
	for _, result := range results {
		switch result.Status {
		case "paired":
			report.VerifiedPairs++
		case "photo-only":
			report.PhotoOnlyPairs++
		case "video-only":
			report.VideoOnlyPairs++
		case "mismatched-media-keys":
			report.MismatchedPairs++
		case "not-found":
			report.NotFoundPairs++
		case "local-error":
			report.LocalErrors++
		case "api-error":
			report.APIErrors++
		case "canceled":
			report.CanceledPairs++
		}
	}
	return report
}

func writeLivePhotoVerificationReport(path string, report livePhotoVerificationReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode verification report: %w", err)
	}
	if err := writeUploadReport(path, append(data, '\n')); err != nil {
		return fmt.Errorf("write verification report %q: %w", path, err)
	}
	return nil
}

func printLivePhotoVerificationSummary(out io.Writer, report livePhotoVerificationReport, reportFile string) {
	counts := []string{
		fmt.Sprintf("paired=%d", report.VerifiedPairs),
		fmt.Sprintf("photo-only=%d", report.PhotoOnlyPairs),
		fmt.Sprintf("video-only=%d", report.VideoOnlyPairs),
		fmt.Sprintf("mismatched=%d", report.MismatchedPairs),
		fmt.Sprintf("not-found=%d", report.NotFoundPairs),
	}
	if report.CanceledPairs > 0 {
		counts = append(counts, fmt.Sprintf("canceled=%d", report.CanceledPairs))
	}
	fmt.Fprintf(out, "Live Photo verification: %d pairs (%s)\n", report.TotalPairs, strings.Join(counts, ", "))
	if report.LocalErrors > 0 || report.APIErrors > 0 {
		fmt.Fprintf(out, "Errors: local=%d, api=%d\n", report.LocalErrors, report.APIErrors)
	}
	if reportFile != "" {
		fmt.Fprintf(out, "Report written to %s\n", reportFile)
	}
}
