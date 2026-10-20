package core

import (
	"context"

	"app/generated"
)

// uploadAPI is the part of *Api an upload run uses, so tests can run the
// whole pipeline against a fake library.
type uploadAPI interface {
	livePhotoUploadAPI
	CommitUpload(ctx context.Context, token *generated.CommitToken, fileName string, sha1Hash []byte, uploadTimestamp int64) (string, error)
	livePhotoCommitPolicy() LivePhotoCommitPolicy
}

func (a *Api) livePhotoCommitPolicy() LivePhotoCommitPolicy { return buildLivePhotoCommitPolicy(a) }

// newUploadAPI is the production uploadAPI constructor.
func newUploadAPI(opts ApiOptions) (uploadAPI, error) {
	api, err := NewApi(opts)
	if err != nil {
		return nil, err // never a typed nil
	}
	return api, nil
}
