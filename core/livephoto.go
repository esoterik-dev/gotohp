package core

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type UploadWorkKind string

const (
	UploadWorkSingle    UploadWorkKind = "single"
	UploadWorkLivePhoto UploadWorkKind = "live-photo"
)

type SingleMedia struct {
	Path string
}

type LivePhotoPair struct {
	ContentIdentifier string
	PhotoPath         string
	VideoPath         string
}

type UploadWorkItem struct {
	Kind      UploadWorkKind
	Single    *SingleMedia
	LivePhoto *LivePhotoPair
}

type PreflightWarning struct {
	Paths   []string `json:"Paths"`
	Code    string   `json:"Code"`
	Message string   `json:"Message"`
	// Skipped means Paths are not uploaded; each such warning is also
	// reported as one skipped file result.
	Skipped bool `json:"Skipped"`
	// Failed means Paths are not uploaded because they could not be read;
	// each such warning is also reported as one failed file result, so the
	// run fails and a retry picks the files up again.
	Failed bool `json:"Failed"`
}

type LivePhotoClassificationOptions struct {
	Enabled             bool
	SkipIncomplete      bool
	IgnoreAppleMetadata bool
	Cancelled           func() bool
}

type LivePhotoMetadataReader interface {
	PhotoContentIdentifier(path string) (string, error)
	VideoLivePhotoMetadata(path string) (LivePhotoMetadata, error)
}

type fileLivePhotoMetadataReader struct{}

func (fileLivePhotoMetadataReader) PhotoContentIdentifier(path string) (string, error) {
	return ReadPhotoContentIdentifier(path)
}

func (fileLivePhotoMetadataReader) VideoLivePhotoMetadata(path string) (LivePhotoMetadata, error) {
	return ReadVideoLivePhotoMetadata(path)
}

type indexedLivePhotoMedia struct {
	path  string
	index int
}

type livePhotoCandidates struct {
	photos []indexedLivePhotoMedia
	videos []indexedLivePhotoMedia
}

// ClassifyUploadWork normally pairs files by their embedded Apple content
// identifier. The explicit CLI override uses case-insensitive filename stems;
// it remains one-to-one and still requires valid Live Photo video timing data.
//
// With SkipIncomplete, no file that may be half of a Live Photo is uploaded
// on its own: a still uploaded without its video cannot be linked to it
// later. Besides unmatched members, that covers videos without the
// still-image-time marker and identifiers shared by several photos or
// videos, which are skipped with a warning, and files whose metadata cannot
// be read, which fail so they can be retried. Without SkipIncomplete they
// are all uploaded as single files.
func ClassifyUploadWork(paths []string, options LivePhotoClassificationOptions, reader LivePhotoMetadataReader) ([]UploadWorkItem, []PreflightWarning) {
	if !options.Enabled {
		work := make([]UploadWorkItem, 0, len(paths))
		for _, path := range paths {
			if options.Cancelled != nil && options.Cancelled() {
				return nil, nil
			}
			work = append(work, UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}})
		}
		return work, nil
	}
	if reader == nil {
		reader = fileLivePhotoMetadataReader{}
	}

	candidatesByIdentifier := make(map[string]*livePhotoCandidates)
	warnings := make([]PreflightWarning, 0)
	consumed := make(map[int]bool)
	// uncertain records a file that may belong to a Live Photo but cannot be
	// paired; it is skipped unless incomplete members may be uploaded.
	uncertain := func(index int, warning PreflightWarning) {
		if options.SkipIncomplete {
			consumed[index] = true
			warning.Skipped = true
			warning.Message += "; skipped so it is not uploaded without its Live Photo partner"
		}
		warnings = append(warnings, warning)
	}
	// unreadable records a file whose metadata read failed, e.g. on a flaky
	// phone mount. It is not uploaded, like an uncertain file, but fails
	// instead of being skipped: the read may work next time.
	unreadable := func(index int, path string, err error) {
		warning := metadataReadWarning(path, err)
		if options.SkipIncomplete {
			consumed[index] = true
			warning.Failed = true
			warning.Message += "; not uploaded, since it may be half of a Live Photo; retry it once it can be read"
		}
		warnings = append(warnings, warning)
	}
	// unmarked holds videos with neither a content identifier nor the
	// still-image-time marker under the filename override. Each is an
	// ordinary video unless a photo shares its stem.
	var unmarked []indexedLivePhotoMedia
	for index, path := range paths {
		if options.Cancelled != nil && options.Cancelled() {
			return nil, nil
		}
		switch livePhotoCandidateType(path) {
		case "photo":
			if options.IgnoreAppleMetadata {
				candidates := candidatesForIdentifier(candidatesByIdentifier, livePhotoFilenameMatchKey(path))
				candidates.photos = append(candidates.photos, indexedLivePhotoMedia{path: path, index: index})
				continue
			}
			identifier, err := reader.PhotoContentIdentifier(path)
			if errors.Is(err, ErrContentIdentifierMissing) {
				continue
			}
			if err != nil {
				unreadable(index, path, err)
				continue
			}
			candidates := candidatesForIdentifier(candidatesByIdentifier, strings.ToLower(identifier))
			candidates.photos = append(candidates.photos, indexedLivePhotoMedia{path: path, index: index})
		case "video":
			metadata, err := reader.VideoLivePhotoMetadata(path)
			if errors.Is(err, ErrContentIdentifierMissing) && !options.IgnoreAppleMetadata {
				continue
			}
			if err != nil && !errors.Is(err, ErrContentIdentifierMissing) {
				unreadable(index, path, err)
				continue
			}
			if !metadata.HasStillImageTime && metadata.ContentIdentifier == "" {
				unmarked = append(unmarked, indexedLivePhotoMedia{path: path, index: index})
				continue
			}
			if !metadata.HasStillImageTime {
				uncertain(index, PreflightWarning{
					Paths:   []string{path},
					Code:    "missing-still-image-time",
					Message: "video has a Live Photo content identifier but no still-image-time marker",
				})
				continue
			}
			identifier := metadata.ContentIdentifier
			if options.IgnoreAppleMetadata {
				identifier = livePhotoFilenameMatchKey(path)
			} else {
				identifier = strings.ToLower(identifier)
			}
			candidates := candidatesForIdentifier(candidatesByIdentifier, identifier)
			candidates.videos = append(candidates.videos, indexedLivePhotoMedia{path: path, index: index})
		}
	}

	for _, video := range unmarked {
		if candidates := candidatesByIdentifier[livePhotoFilenameMatchKey(video.path)]; candidates != nil && len(candidates.photos) > 0 {
			uncertain(video.index, PreflightWarning{
				Paths:   []string{video.path},
				Code:    "missing-still-image-time",
				Message: "video shares a filename stem with a photo but has no still-image-time marker",
			})
			continue
		}
		warnings = append(warnings, PreflightWarning{
			Paths:   []string{video.path},
			Code:    "missing-still-image-time",
			Message: "video has no Live Photo content identifier or still-image-time marker; uploaded as an ordinary video",
		})
	}

	pairsByIndex := make(map[int]LivePhotoPair)
	identifiers := sortedLivePhotoIdentifiers(candidatesByIdentifier)
	for _, identifier := range identifiers {
		if options.Cancelled != nil && options.Cancelled() {
			return nil, nil
		}
		candidates := candidatesByIdentifier[identifier]
		if len(candidates.photos) == 1 && len(candidates.videos) == 1 {
			photo := candidates.photos[0]
			video := candidates.videos[0]
			pairIndex := min(photo.index, video.index)
			contentIdentifier := identifier
			if options.IgnoreAppleMetadata {
				contentIdentifier = ""
			}
			pairsByIndex[pairIndex] = LivePhotoPair{
				ContentIdentifier: contentIdentifier,
				PhotoPath:         photo.path,
				VideoPath:         video.path,
			}
			consumed[photo.index] = true
			consumed[video.index] = true
			continue
		}
		if len(candidates.photos) > 1 || len(candidates.videos) > 1 {
			ambiguousPaths := candidatePaths(candidates)
			code := "ambiguous-identifier"
			message := "multiple photo or video candidates share one Live Photo identifier"
			// Ambiguity skips every candidate instead of silently falling back
			// to single uploads: always with the filename override, and with
			// Apple metadata unless incomplete members may be uploaded.
			skip := options.IgnoreAppleMetadata || options.SkipIncomplete
			if options.IgnoreAppleMetadata {
				code = "ambiguous-filename-stem"
				message = "multiple photo or video candidates share one filename stem"
			}
			if skip {
				message += "; all of them are skipped"
				for _, photo := range candidates.photos {
					consumed[photo.index] = true
				}
				for _, video := range candidates.videos {
					consumed[video.index] = true
				}
			}
			warnings = append(warnings, PreflightWarning{
				Paths:   ambiguousPaths,
				Code:    code,
				Message: message,
				Skipped: skip,
			})
			continue
		}
		if options.SkipIncomplete {
			orphanPaths := candidatePaths(candidates)
			for _, photo := range candidates.photos {
				consumed[photo.index] = true
			}
			for _, video := range candidates.videos {
				consumed[video.index] = true
			}
			warnings = append(warnings, PreflightWarning{
				Paths:   orphanPaths,
				Code:    "incomplete-live-photo-skipped",
				Skipped: true,
				Message: "Skipped an incomplete Live Photo because its matching file is not in this queue. You can disable this check in Settings.",
			})
		}
	}

	work := make([]UploadWorkItem, 0, len(paths))
	for index, path := range paths {
		if options.Cancelled != nil && options.Cancelled() {
			return nil, nil
		}
		if pair, ok := pairsByIndex[index]; ok {
			pair := pair
			work = append(work, UploadWorkItem{Kind: UploadWorkLivePhoto, LivePhoto: &pair})
			continue
		}
		if consumed[index] {
			continue
		}
		work = append(work, UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: path}})
	}
	return work, warnings
}

func sortedLivePhotoIdentifiers(candidatesByIdentifier map[string]*livePhotoCandidates) []string {
	identifiers := make([]string, 0, len(candidatesByIdentifier))
	for identifier := range candidatesByIdentifier {
		identifiers = append(identifiers, identifier)
	}
	sort.Slice(identifiers, func(left, right int) bool {
		return candidateFirstIndex(candidatesByIdentifier[identifiers[left]]) < candidateFirstIndex(candidatesByIdentifier[identifiers[right]])
	})
	return identifiers
}

func candidateFirstIndex(candidates *livePhotoCandidates) int {
	first := int(^uint(0) >> 1)
	for _, photo := range candidates.photos {
		first = min(first, photo.index)
	}
	for _, video := range candidates.videos {
		first = min(first, video.index)
	}
	return first
}

func livePhotoCandidateType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".heic", ".heif", ".jpg", ".jpeg":
		return "photo"
	case ".mov":
		return "video"
	default:
		return ""
	}
}

func livePhotoFilenameMatchKey(path string) string {
	cleanPath := filepath.Clean(path)
	directory := filepath.Dir(cleanPath)
	baseName := filepath.Base(cleanPath)
	stem := strings.ToLower(strings.TrimSuffix(baseName, filepath.Ext(baseName)))
	return filepath.Join(directory, stem)
}

func candidatesForIdentifier(candidatesByIdentifier map[string]*livePhotoCandidates, identifier string) *livePhotoCandidates {
	candidates := candidatesByIdentifier[identifier]
	if candidates == nil {
		candidates = &livePhotoCandidates{}
		candidatesByIdentifier[identifier] = candidates
	}
	return candidates
}

func metadataReadWarning(path string, err error) PreflightWarning {
	return PreflightWarning{
		Paths:   []string{path},
		Code:    "metadata-read-failed",
		Message: fmt.Sprintf("could not read Live Photo metadata: %v", err),
	}
}

func candidatePaths(candidates *livePhotoCandidates) []string {
	paths := make([]string, 0, len(candidates.photos)+len(candidates.videos))
	for _, photo := range candidates.photos {
		paths = append(paths, photo.path)
	}
	for _, video := range candidates.videos {
		paths = append(paths, video.path)
	}
	return paths
}
