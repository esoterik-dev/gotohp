package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type livePhotoUploadAPI interface {
	FindRemoteMediaByHash(ctx context.Context, hash []byte) (string, error)
	UploadFileWithHashAndProgress(ctx context.Context, filePath string, sha1Hash []byte, fileSize int64, onProgress UploadProgressCallback) (ScottyFinalizeToken, error)
	CommitLivePhoto(ctx context.Context, input LivePhotoCreateRequest) (string, error)
	ReconcileLivePhoto(ctx context.Context, input LivePhotoReconcileRequest) (string, error)
}

type LivePhotoUploadOptions struct {
	Policy                     LivePhotoCommitPolicy
	DeleteFromHost             bool
	SetDateFromFilename        bool
	StageLocal                 bool
	UpdateExistingPhotosToLive bool
}

func uploadLivePhotoWithCallback(
	ctx context.Context,
	api livePhotoUploadAPI,
	pair LivePhotoPair,
	options LivePhotoUploadOptions,
	workerID int,
	reporter UploadReporter,
) (mediaKey string, skipped bool, err error) {
	photoInfo, err := os.Stat(pair.PhotoPath)
	if err != nil {
		return "", false, fmt.Errorf("stat Live Photo still: %w", err)
	}
	videoInfo, err := os.Stat(pair.VideoPath)
	if err != nil {
		return "", false, fmt.Errorf("stat Live Photo video: %w", err)
	}
	displayName := filepath.Base(pair.PhotoPath) + " + " + filepath.Base(pair.VideoPath)
	// uploadPair is what gets hashed and uploaded; pair stays the source for
	// names, dates and deletion.
	uploadPair := pair
	var staged *stagedFiles
	if options.StageLocal {
		reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   "staging",
			FilePath: pair.PhotoPath,
			FileName: displayName,
			Message:  "Copying Live Photo pair to local temporary storage...",
		})
		staged, err = stageFilesLocally(ctx, []string{pair.PhotoPath, pair.VideoPath})
		if err != nil {
			return "", false, fmt.Errorf("stage Live Photo pair locally: %w", err)
		}
		defer staged.cleanup()
		uploadPair = LivePhotoPair{PhotoPath: staged.paths[0], VideoPath: staged.paths[1]}
		photoInfo, videoInfo = staged.infos[0], staged.infos[1]
	}
	totalBytes := photoInfo.Size() + videoInfo.Size()
	reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "hashing",
		FilePath: pair.PhotoPath,
		FileName: displayName,
		Message:  "Hashing Live Photo pair...",
	})

	photoSHA1, err := CalculateSHA1(ctx, uploadPair.PhotoPath)
	if err != nil {
		return "", false, fmt.Errorf("hash Live Photo still: %w", err)
	}
	videoSHA1, err := CalculateSHA1(ctx, uploadPair.VideoPath)
	if err != nil {
		return "", false, fmt.Errorf("hash Live Photo video: %w", err)
	}

	reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "checking",
		FilePath: pair.PhotoPath,
		FileName: displayName,
		Message:  "Checking both Live Photo components...",
	})
	photoRemoteKey, photoCheckErr := api.FindRemoteMediaByHash(ctx, photoSHA1)
	videoRemoteKey, videoCheckErr := api.FindRemoteMediaByHash(ctx, videoSHA1)
	if photoCheckErr != nil {
		return "", false, fmt.Errorf("check Live Photo still deduplication: %w", photoCheckErr)
	}
	if videoCheckErr != nil {
		return "", false, fmt.Errorf("check Live Photo video deduplication: %w", videoCheckErr)
	}
	photoRemoteKey, videoRemoteKey, err = recheckMissingLivePhotoComponent(ctx, api, photoSHA1, videoSHA1, photoRemoteKey, videoRemoteKey)
	if err != nil {
		return "", false, fmt.Errorf("recheck Live Photo deduplication: %w", err)
	}
	if photoRemoteKey != "" {
		if options.UpdateExistingPhotosToLive {
			reporter.TotalBytesDelta(-photoInfo.Size())
			mediaKey, err := reconcileExistingLivePhoto(
				ctx,
				api,
				uploadPair,
				pair,
				staged,
				photoInfo,
				videoInfo,
				photoSHA1,
				videoSHA1,
				options,
				workerID,
				displayName,
				reporter,
			)
			return mediaKey, false, err
		}
		reporter.TotalBytesDelta(-totalBytes)
		reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   "skipped",
			FilePath: pair.PhotoPath,
			FileName: displayName,
			Message:  "Skipped: a Live Photo component already exists remotely",
		})
		return "", true, nil
	}
	// The VideoOriginal direction has not been recovered. A standalone remote MOV
	// must not be combined with a newly uploaded still using the Phodeo request.
	if videoRemoteKey != "" {
		reporter.TotalBytesDelta(-totalBytes)
		reporter.ThreadStatus(ThreadStatus{
			WorkerID: workerID,
			Status:   "skipped",
			FilePath: pair.PhotoPath,
			FileName: displayName,
			Message:  "Skipped: the Live Photo video already exists remotely",
		})
		return "", true, nil
	}

	photoToken, err := uploadLivePhotoComponent(ctx, api, uploadPair.PhotoPath, pair.PhotoPath, photoInfo.Size(), photoSHA1, 0, totalBytes, workerID, displayName, reporter)
	if err != nil {
		return "", false, fmt.Errorf("upload Live Photo still: %w", err)
	}
	videoToken, err := uploadLivePhotoComponent(ctx, api, uploadPair.VideoPath, pair.PhotoPath, videoInfo.Size(), videoSHA1, photoInfo.Size(), totalBytes, workerID, displayName, reporter)
	if err != nil {
		return "", false, fmt.Errorf("upload Live Photo video: %w", err)
	}

	uploadTime := photoInfo.ModTime()
	if options.SetDateFromFilename {
		if parsed, ok := parseTimestampFromFilename(pair.PhotoPath); ok {
			uploadTime = parsed
		}
	}
	reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "finalizing",
		FilePath: pair.PhotoPath,
		FileName: displayName,
		Message:  "Committing linked Live Photo...",
	})
	mediaKey, err = api.CommitLivePhoto(ctx, LivePhotoCreateRequest{
		PhotoToken:       photoToken,
		VideoToken:       videoToken,
		FileName:         photoInfo.Name(),
		PhotoSHA1:        photoSHA1,
		VideoSHA1:        videoSHA1,
		CreatedAt:        uploadTime,
		ModifiedAt:       uploadTime,
		StoragePolicy:    options.Policy.StoragePolicy,
		UploadQuality:    options.Policy.UploadQuality,
		UploadDeviceInfo: options.Policy.UploadDeviceInfo,
	})
	if err != nil {
		return "", false, fmt.Errorf("commit Live Photo: %w", err)
	}
	if mediaKey == "" {
		return "", false, fmt.Errorf("Live Photo media key not received")
	}

	if options.DeleteFromHost {
		if err := removeLivePhotoSources(pair, staged); err != nil {
			return mediaKey, false, err
		}
	}
	return mediaKey, false, nil
}

// livePhotoRecheckDelays are the waits before each recheck of a component
// that was missing while its partner was found. Tests shorten them.
var livePhotoRecheckDelays = []time.Duration{500 * time.Millisecond, time.Second}

// recheckMissingLivePhotoComponent looks up a component again when exactly
// one of the pair was found. The private hash endpoint has been seen to
// return an empty result for an item that exists, which led to duplicate
// Live Photos. One found and one missing is the inconsistent answer worth
// rechecking. Both missing is the normal result for a new pair, so it is
// not rechecked and costs no extra lookups. Transport errors are already
// retried inside FindRemoteMediaByHash; this only rechecks empty answers.
func recheckMissingLivePhotoComponent(
	ctx context.Context,
	api livePhotoUploadAPI,
	photoSHA1, videoSHA1 []byte,
	photoKey, videoKey string,
) (string, string, error) {
	if (photoKey == "") == (videoKey == "") {
		return photoKey, videoKey, nil
	}
	missing, key := photoSHA1, &photoKey
	if videoKey == "" {
		missing, key = videoSHA1, &videoKey
	}
	for _, delay := range livePhotoRecheckDelays {
		if err := waitRequest(ctx, delay); err != nil {
			return "", "", err
		}
		found, err := api.FindRemoteMediaByHash(ctx, missing)
		if err != nil {
			return "", "", err
		}
		if found != "" {
			*key = found
			break
		}
	}
	return photoKey, videoKey, nil
}

func reconcileExistingLivePhoto(
	ctx context.Context,
	api livePhotoUploadAPI,
	uploadPair LivePhotoPair,
	pair LivePhotoPair,
	staged *stagedFiles,
	photoInfo os.FileInfo,
	videoInfo os.FileInfo,
	photoSHA1 []byte,
	videoSHA1 []byte,
	options LivePhotoUploadOptions,
	workerID int,
	displayName string,
	reporter UploadReporter,
) (string, error) {
	reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "uploading",
		FilePath: pair.PhotoPath,
		FileName: displayName,
		Message:  "Uploading Live Photo video for existing photo...",
	})
	videoToken, err := uploadLivePhotoComponent(
		ctx,
		api,
		uploadPair.VideoPath,
		pair.PhotoPath,
		videoInfo.Size(),
		videoSHA1,
		0,
		videoInfo.Size(),
		workerID,
		displayName,
		reporter,
	)
	if err != nil {
		return "", fmt.Errorf("upload Live Photo video for existing photo: %w", err)
	}

	uploadTime := photoInfo.ModTime()
	if options.SetDateFromFilename {
		if parsed, ok := parseTimestampFromFilename(pair.PhotoPath); ok {
			uploadTime = parsed
		}
	}
	reporter.ThreadStatus(ThreadStatus{
		WorkerID: workerID,
		Status:   "finalizing",
		FilePath: pair.PhotoPath,
		FileName: displayName,
		Message:  "Updating existing photo to Live...",
	})
	mediaKey, err := api.ReconcileLivePhoto(ctx, LivePhotoReconcileRequest{
		VideoToken:       videoToken,
		FileName:         videoInfo.Name(),
		PhotoSHA1:        photoSHA1,
		VideoSHA1:        videoSHA1,
		CreatedAt:        uploadTime,
		ModifiedAt:       uploadTime,
		StoragePolicy:    options.Policy.StoragePolicy,
		UploadQuality:    options.Policy.UploadQuality,
		UploadDeviceInfo: options.Policy.UploadDeviceInfo,
	})
	if err != nil {
		return "", fmt.Errorf("update existing photo to Live: %w", err)
	}
	if mediaKey == "" {
		return "", fmt.Errorf("updated Live Photo media key not received")
	}
	if options.DeleteFromHost {
		if err := removeLivePhotoSources(pair, staged); err != nil {
			return mediaKey, err
		}
	}
	return mediaKey, nil
}

func uploadLivePhotoComponent(
	ctx context.Context,
	api livePhotoUploadAPI,
	componentPath string,
	progressPath string,
	size int64,
	hash []byte,
	completedBytes int64,
	totalBytes int64,
	workerID int,
	displayName string,
	reporter UploadReporter,
) (ScottyFinalizeToken, error) {
	progress := func(bytesUploaded, _ int64, attempt int) {
		message := "Uploading Live Photo pair..."
		if attempt > 1 {
			message = fmt.Sprintf("Retrying Live Photo component (attempt %d)...", attempt)
		}
		reporter.ThreadStatus(ThreadStatus{
			WorkerID:      workerID,
			Status:        "uploading",
			FilePath:      progressPath,
			FileName:      displayName,
			Message:       message,
			BytesUploaded: completedBytes + bytesUploaded,
			BytesTotal:    totalBytes,
			Attempt:       attempt,
		})
	}
	return api.UploadFileWithHashAndProgress(ctx, componentPath, hash, size, progress)
}

// removeLivePhotoSources deletes an uploaded pair, refusing if either source
// changed after it was staged.
func removeLivePhotoSources(pair LivePhotoPair, staged *stagedFiles) error {
	if err := staged.verifySourcesUnchanged(); err != nil {
		return err
	}
	return removeLivePhotoFiles(pair)
}

func removeLivePhotoFiles(pair LivePhotoPair) error {
	for _, path := range []string{pair.VideoPath, pair.PhotoPath} {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("Live Photo uploaded but failed to delete %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}
