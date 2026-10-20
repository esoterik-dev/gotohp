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
	UpdateExistingPhotosToLive bool
}

// uploadLivePhotoWithCallback uploads a pair whose files a hash worker has
// already prepared: photo and video carry the hashes and the files to read.
func uploadLivePhotoWithCallback(
	ctx context.Context,
	api livePhotoUploadAPI,
	pair LivePhotoPair,
	photo, video preparedFile,
	options LivePhotoUploadOptions,
	workerID int,
	reporter UploadReporter,
) (mediaKey string, skipped bool, err error) {
	for _, file := range []preparedFile{photo, video} {
		if err := file.uploadable(); err != nil {
			return "", false, err
		}
	}
	photoInfo, videoInfo := photo.info, video.info
	photoSHA1, videoSHA1 := photo.hash, video.hash
	displayName := filepath.Base(pair.PhotoPath) + " + " + filepath.Base(pair.VideoPath)
	// uploadPair is what gets uploaded; pair stays the source for names,
	// dates and deletion.
	uploadPair := LivePhotoPair{PhotoPath: photo.upload, VideoPath: video.upload}
	totalBytes := photoInfo.Size() + videoInfo.Size()

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
	photoRemoteKey, videoRemoteKey, err = recheckNewLivePhotoPair(ctx, api, photoSHA1, videoSHA1, photoRemoteKey, videoRemoteKey)
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
				[]preparedFile{photo, video},
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
		if err := removeLivePhotoSources(pair, photo, video); err != nil {
			return mediaKey, false, err
		}
	}
	return mediaKey, false, nil
}

// livePhotoRecheckDelays are the waits before each recheck of a pair whose
// components were both missing. Tests shorten them.
var livePhotoRecheckDelays = []time.Duration{500 * time.Millisecond, time.Second}

// recheckNewLivePhotoPair looks both components up again when neither was
// found, just before a new pair would be created. The private hash endpoint
// has been seen to return an empty result for an item that exists, and in
// this case that creates a duplicate Live Photo. If either component turns
// up, its key is returned and the caller skips or reconciles instead. When
// one component was already found nothing new would be created, so nothing
// is rechecked. Transport errors are already retried inside
// FindRemoteMediaByHash; this only rechecks empty answers.
func recheckNewLivePhotoPair(
	ctx context.Context,
	api livePhotoUploadAPI,
	photoSHA1, videoSHA1 []byte,
	photoKey, videoKey string,
) (string, string, error) {
	if photoKey != "" || videoKey != "" {
		return photoKey, videoKey, nil
	}
	for _, delay := range livePhotoRecheckDelays {
		if err := waitRequest(ctx, delay); err != nil {
			return "", "", err
		}
		var err error
		if photoKey, err = api.FindRemoteMediaByHash(ctx, photoSHA1); err != nil {
			return "", "", err
		}
		if videoKey, err = api.FindRemoteMediaByHash(ctx, videoSHA1); err != nil {
			return "", "", err
		}
		if photoKey != "" || videoKey != "" {
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
	prepared []preparedFile,
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
		if err := removeLivePhotoSources(pair, prepared...); err != nil {
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
// changed after it was hashed or staged.
func removeLivePhotoSources(pair LivePhotoPair, prepared ...preparedFile) error {
	for _, file := range prepared {
		if err := file.unchanged(); err != nil {
			return fmt.Errorf("refusing to delete: %w", err)
		}
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
