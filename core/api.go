package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"app/generated"

	"google.golang.org/protobuf/proto"
)

// PhotosCreateMediaItems is the shared commit RPC observed for both ordinary
// images and linked Live Photos; pairing changes the request body, not the RPC.
const photosCreateMediaItemsEndpoint = "https://photosdata-pa.googleapis.com/6439526531001121323/16538846908252377752"

type Api struct {
	androidAPIVersion int64
	model             string
	make              string
	clientVersionCode int64
	userAgent         string
	language          string
	authData          string
	client            *http.Client
	authMu            sync.Mutex // guards authResponseCache for concurrent callers
	policy            *RequestPolicy
	authResponseCache map[string]string
	commitEndpoint    string
	commitRetryConfig *RetryConfig
	saver             bool
	useQuota          bool
}

type AuthResponse struct {
	Expiry string
	Auth   string
}

// ApiOptions selects the account and per-run upload policy for an API client.
type ApiOptions struct {
	// Account is the credential email to use; empty means the selected account.
	Account  string
	Proxy    string
	Saver    bool
	UseQuota bool
	// Policy paces requests and shares 429 cooldowns. nil means no pacing,
	// no shared cooldown and default retries; typed retry rules and the
	// "fail the file when the duplicate check fails" rule apply either way.
	Policy *RequestPolicy
}

// NewApi creates a client for the account in opts using the loaded credential store.
func NewApi(opts ApiOptions) (*Api, error) {
	account := currentConfig().Account
	email := opts.Account
	if email == "" {
		email = account.Selected
	}
	if email == "" {
		return nil, fmt.Errorf("no account is selected")
	}
	credentials := ""
	for _, c := range account.Credentials {
		params, err := url.ParseQuery(c)
		if err != nil {
			continue
		}
		if strings.EqualFold(params.Get("Email"), email) {
			credentials = c
		}
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no credentials found for account %s", email)
	}

	api, err := newAPIFromCredential(credentials, opts.Proxy)
	if err != nil {
		return nil, err
	}
	api.saver = opts.Saver
	api.useQuota = opts.UseQuota
	api.policy = opts.Policy
	return api, nil
}

func newAPIFromCredential(credentials string, proxy string) (*Api, error) {
	params, err := url.ParseQuery(credentials)
	if err != nil {
		return nil, fmt.Errorf("failed to parse credentials: %w", err)
	}

	client, err := NewHTTPClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP client: %w", err)
	}

	api := &Api{
		androidAPIVersion: 28,
		model:             "Pixel XL",
		make:              "Google",
		clientVersionCode: 49029607,
		language:          params.Get("lang"),
		authData:          strings.TrimSpace(credentials),
		client:            client,
		authResponseCache: map[string]string{
			"Expiry": "0",
			"Auth":   "",
		},
	}

	api.userAgent = fmt.Sprintf(
		"com.google.android.apps.photos/%d (Linux; U; Android 9; %s; %s; Build/PQ2A.190205.001; Cronet/127.0.6510.5) (gzip)",
		api.clientVersionCode,
		api.language,
		api.model,
	)

	return api, nil
}

// BearerToken returns a valid token, refreshing it when expired. It is safe
// for concurrent use by goroutines sharing one Api. The lock covers only the
// cache, not the refresh request, so ctx can always cancel a waiting caller
// (a refresh may sit behind a 429 pause); at worst a few workers refresh at
// the same time when the token expires.
func (a *Api) BearerToken(ctx context.Context) (string, error) {
	a.authMu.Lock()
	expiryStr, token := a.authResponseCache["Expiry"], a.authResponseCache["Auth"]
	a.authMu.Unlock()
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid expiry time: %w", err)
	}

	if expiry <= time.Now().Unix() {
		resp, err := a.getAuthToken(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to get auth token: %w", err)
		}
		a.authMu.Lock()
		a.authResponseCache = resp
		a.authMu.Unlock()
		token = resp["Auth"]
	}

	if token != "" {
		return token, nil
	}

	return "", errors.New("auth response does not contain bearer token")
}

func (a *Api) getAuthToken(ctx context.Context) (map[string]string, error) {
	authDataValues, err := url.ParseQuery(a.authData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse auth data: %w", err)
	}

	authRequestData := url.Values{}
	for key, values := range authDataValues {
		authRequestData[key] = append([]string(nil), values...)
	}
	authRequestData.Set("app", "com.google.android.apps.photos")
	authRequestData.Set("callerPkg", "com.google.android.apps.photos")
	authRequestData.Del("it_caveat_types")
	authRequestData.Del("assertion_jwt")

	var tokenBinding *tokenBindingSession
	if alias := authRequestData.Get("token_binding_alias"); alias != "" {
		var assertionJWT string
		tokenBinding, assertionJWT, err = newTokenBindingSession(alias)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare token binding assertion: %w", err)
		}
		authRequestData.Set("assertion_jwt", assertionJWT)
	}
	authRequestData.Del("token_binding_alias")

	headers := map[string]string{
		"Accept-Encoding": "gzip",
		"app":             "com.google.android.apps.photos",
		"Connection":      "Keep-Alive",
		"Content-Type":    "application/x-www-form-urlencoded",
		"device":          authRequestData.Get("androidId"),
		"User-Agent":      "GoogleAuth/1.4 (Pixel XL PQ2A.190205.001); gzip",
	}

	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://android.googleapis.com/auth",
		strings.NewReader(authRequestData.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := a.do(req)
	if err != nil {
		return nil, fmt.Errorf("auth request failed after retries: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check for errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ReadResponseBody(resp)
		return make(map[string]string), fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse the response body
	bodyBytes, err := ReadResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse the key=value response format
	parsedAuthResponse := make(map[string]string)
	for _, line := range strings.Split(string(bodyBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			parsedAuthResponse[parts[0]] = parts[1]
		}
	}
	if err := decryptTokenEncryptedResponse(parsedAuthResponse, tokenBinding); err != nil {
		return nil, err
	}

	// Validate we got the required fields
	if parsedAuthResponse["Auth"] == "" {
		return nil, errors.New("auth response missing Auth token")
	}
	if parsedAuthResponse["Expiry"] == "" {
		return nil, errors.New("auth response missing Expiry")
	}

	return parsedAuthResponse, nil
}

// Obtain a file upload token from the Google Photos API.
func (a *Api) GetUploadToken(ctx context.Context, shaHashB64 string, fileSize int64) (string, error) {
	// Create the protobuf message
	protoBody := generated.GetUploadToken{
		F1:            2,
		F2:            2,
		F3:            1,
		F4:            3,
		FileSizeBytes: fileSize,
	}

	// Serialize the protobuf message
	serializedData, err := proto.Marshal(&protoBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal protobuf: %w", err)
	}

	// Get the bearer token
	bearerToken, err := a.BearerToken(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get bearer token: %w", err)
	}

	// Prepare headers
	headers := map[string]string{
		"Accept-Encoding":         "gzip",
		"Accept-Language":         a.language,
		"Content-Type":            "application/x-protobuf",
		"User-Agent":              a.userAgent,
		"Authorization":           "Bearer " + bearerToken,
		"X-Goog-Hash":             "sha1=" + shaHashB64,
		"X-Upload-Content-Length": strconv.FormatInt(fileSize, 10),
	}

	// Create the request
	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://photos.googleapis.com/data/upload/uploadmedia/interactive",
		bytes.NewReader(serializedData),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Make the request
	resp, err := a.do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check for errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ReadResponseBody(resp)
		return "", &httpStatusError{statusCode: resp.StatusCode, body: string(body)}
	}

	// Get the upload token from headers
	uploadToken := resp.Header.Get("X-GUploader-UploadID")
	if uploadToken == "" {
		return "", errors.New("response missing X-GUploader-UploadID header")
	}

	return uploadToken, nil
}

// FindRemoteMediaByHash checks the library for an item with this SHA-1 and
// returns its media key, or "" when none exists. Transient failures are
// retried here; an empty answer is a real "not found" and is not retried.
func (a *Api) FindRemoteMediaByHash(ctx context.Context, shaHash []byte) (string, error) {
	match, err := a.FindRemoteMediaMatchesByHash(ctx, shaHash)
	return match.PrimaryMediaKey, err
}

// RemoteMediaMatch holds the media keys the private hash lookup returns.
// PrimaryMediaKey is the key the uploader has always used; RelatedMediaKey is
// a nested key Google returns for some composite items such as Live Photos.
// The lookup is metadata-only and never fetches media bytes.
type RemoteMediaMatch struct {
	PrimaryMediaKey string
	RelatedMediaKey string
}

// FindRemoteMediaMatchesByHash is FindRemoteMediaByHash with both modeled
// media-key fields, for callers that verify composite items.
func (a *Api) FindRemoteMediaMatchesByHash(ctx context.Context, shaHash []byte) (RemoteMediaMatch, error) {
	var match RemoteMediaMatch
	err := a.withRetry(ctx, a.retryConfig(), func(int) error {
		var err error
		match, err = a.findRemoteMediaOnce(ctx, shaHash)
		return err
	})
	return match, err
}

func (a *Api) findRemoteMediaOnce(ctx context.Context, shaHash []byte) (RemoteMediaMatch, error) {
	// Create the protobuf message

	// Create and initialize the protobuf message with all required nested structures
	protoBody := generated.HashCheck{
		Field1: &generated.HashCheckField1Type{
			Field1: &generated.HashCheckField1TypeField1Type{
				Sha1Hash: shaHash,
			},
			Field2: &generated.HashCheckField1TypeField2Type{},
		},
	}

	// Serialize the protobuf message
	serializedData, err := proto.Marshal(&protoBody)
	if err != nil {
		return RemoteMediaMatch{}, fmt.Errorf("failed to marshal protobuf: %w", err)
	}

	// Get the bearer token
	bearerToken, err := a.BearerToken(ctx)
	if err != nil {
		return RemoteMediaMatch{}, fmt.Errorf("failed to get bearer token: %w", err)
	}

	// Prepare headers
	headers := map[string]string{
		"Accept-Encoding": "gzip",
		"Accept-Language": a.language,
		"Content-Type":    "application/x-protobuf",
		"User-Agent":      a.userAgent,
		"Authorization":   "Bearer " + bearerToken,
	}

	// Create the request
	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://photosdata-pa.googleapis.com/6439526531001121323/5084965799730810217",
		bytes.NewReader(serializedData),
	)
	if err != nil {
		return RemoteMediaMatch{}, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Make the request
	resp, err := a.do(req)
	if err != nil {
		return RemoteMediaMatch{}, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check for errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ReadResponseBody(resp)
		return RemoteMediaMatch{}, &httpStatusError{statusCode: resp.StatusCode, body: string(body)}
	}

	// Parse the response body
	bodyBytes, err := ReadResponseBody(resp)
	if err != nil {
		return RemoteMediaMatch{}, fmt.Errorf("failed to read response body: %w", err)
	}

	var pbResp generated.RemoteMatches
	if err := proto.Unmarshal(bodyBytes, &pbResp); err != nil {
		return RemoteMediaMatch{}, fmt.Errorf("failed to unmarshal protobuf: %w", err)
	}

	return RemoteMediaMatch{
		PrimaryMediaKey: pbResp.GetMediaKey(),
		RelatedMediaKey: pbResp.GetField1().GetField2().GetField2().GetField6().GetMediaKey(),
	}, nil
}

// UploadProgressCallback is called with progress updates during file upload
// attempt is 1-based (1 = first attempt, 2 = first retry, etc.)
type UploadProgressCallback func(bytesUploaded, bytesTotal int64, attempt int)

// errBadUploadResponse marks a 2xx upload response whose body could not be
// read or parsed.
var errBadUploadResponse = errors.New("unreadable upload response")

// UploadFileWithHashAndProgress creates a new resumable upload session for
// each retry. The hash is sent when creating the session, so the request body
// and the declared checksum are always read from the same stable file.
func (a *Api) UploadFileWithHashAndProgress(
	ctx context.Context,
	filePath string,
	sha1Hash []byte,
	fileSize int64,
	onProgress UploadProgressCallback,
) (ScottyFinalizeToken, error) {
	shaHashB64 := base64.StdEncoding.EncodeToString(sha1Hash)
	var result ScottyFinalizeToken
	err := a.withRetry(ctx, a.retryConfig(), func(attempt int) error {
		uploadToken, err := a.GetUploadToken(ctx, shaHashB64, fileSize)
		if err == nil {
			result, err = a.uploadFileAttempt(ctx, filePath, uploadToken, fileSize, onProgress, attempt)
		}
		return err
	})
	if err != nil && ctx.Err() == nil {
		return ScottyFinalizeToken{}, fmt.Errorf("upload %w", err)
	}
	return result, err
}

func (a *Api) uploadFileAttempt(
	ctx context.Context,
	filePath string,
	uploadToken string,
	fileSize int64,
	onProgress UploadProgressCallback,
	attemptNum int,
) (ScottyFinalizeToken, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("error getting file info: %w", err)
	}
	if fileInfo.Size() != fileSize {
		return ScottyFinalizeToken{}, fmt.Errorf("file size changed during upload: got %d bytes, expected %d", fileInfo.Size(), fileSize)
	}

	if onProgress != nil {
		onProgress(0, fileSize, attemptNum)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("error opening file: %w", err)
	}
	var reader io.Reader = file
	if onProgress != nil {
		reader = NewProgressReader(file, fileSize, func(bytesRead, total int64) {
			onProgress(bytesRead, total, attemptNum)
		})
	}

	uploadURL := "https://photos.googleapis.com/data/upload/uploadmedia/interactive?upload_id=" + uploadToken
	result, err := a.doUploadRequest(ctx, uploadURL, reader, fileSize)
	closeErr := file.Close()
	if err == nil && closeErr != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("error closing file: %w", closeErr)
	}
	return result, err
}

// doUploadRequest performs a single upload attempt
func (a *Api) doUploadRequest(ctx context.Context, uploadURL string, reader io.Reader, fileSize int64) (ScottyFinalizeToken, error) {
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, reader)
	if err != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("error creating request: %w", err)
	}

	// The upload endpoint validates the bytes against the hash supplied when
	// creating the upload session. Send the exact length rather than chunked
	// transfer encoding so it receives the request body it expects.
	req.ContentLength = fileSize

	bearerToken, err := a.BearerToken(ctx)
	if err != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("failed to get bearer token: %w", err)
	}

	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept-Language", a.language)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	resp, err := a.do(req)
	if err != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check for non-success status codes (includes retryable 5xx/429 and non-retryable 4xx)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ReadResponseBody(resp)
		return ScottyFinalizeToken{}, &httpStatusError{statusCode: resp.StatusCode, body: string(body)}
	}

	bodyBytes, err := ReadResponseBody(resp)
	if err != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("failed to read response body: %w: %w", errBadUploadResponse, err)
	}
	token, err := ParseScottyFinalizeToken(bodyBytes)
	if err != nil {
		return ScottyFinalizeToken{}, fmt.Errorf("invalid upload finalize response: %w: %w", errBadUploadResponse, err)
	}
	return token, nil
}

// CommitUpload commits the upload to Google Photos
func (a *Api) CommitUpload(
	ctx context.Context,
	uploadResponseDecoded *generated.CommitToken,
	fileName string,
	sha1Hash []byte,
	uploadTimestamp int64,
) (string, error) {
	if uploadTimestamp == 0 {
		uploadTimestamp = time.Now().Unix()
	}

	var qualityVal int64 = 3
	if a.saver {
		qualityVal = 1
		a.model = "Pixel 2"
	}

	if a.useQuota {
		a.model = "Pixel 8"
	}

	unknownInt := int64(46000000)

	// Create the protobuf message
	protoBody := generated.CommitUpload{
		Field1: &generated.CommitUploadField1Type{
			Field1: &generated.CommitUploadField1TypeField1Type{
				Field1: uploadResponseDecoded.Field1,
				Field2: uploadResponseDecoded.Field2,
			},
			FileName: fileName,
			Sha1Hash: sha1Hash,
			Field4: &generated.CommitUploadField1TypeField4Type{
				FileLastModifiedTimestamp: uploadTimestamp,
				Field2:                    unknownInt,
			},
			Quality: qualityVal,
			Field10: 1,
		},
		Field2: &generated.CommitUploadField2Type{
			Model:             a.model,
			Make:              a.make,
			AndroidApiVersion: a.androidAPIVersion,
		},
		Field3: []byte{1, 3},
	}

	// Serialize the protobuf message
	serializedData, err := proto.Marshal(&protoBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal protobuf: %w", err)
	}

	return a.commitSerialized(ctx, serializedData, func(ctx context.Context) string {
		return a.findLanded(ctx, sha1Hash)
	})
}

func (a *Api) CommitLivePhoto(ctx context.Context, input LivePhotoCreateRequest) (string, error) {
	serializedData, err := BuildLivePhotoCreateMediaItemsRequest(input)
	if err != nil {
		return "", fmt.Errorf("build Live Photo create request: %w", err)
	}
	// A new pair is only created when neither component was in the library,
	// so the still appearing means this commit landed.
	return a.commitSerialized(ctx, serializedData, func(ctx context.Context) string {
		return a.findLanded(ctx, input.PhotoSHA1)
	})
}

func (a *Api) ReconcileLivePhoto(ctx context.Context, input LivePhotoReconcileRequest) (string, error) {
	serializedData, err := BuildLivePhotoReconcileMediaItemsRequest(input)
	if err != nil {
		return "", fmt.Errorf("build Live Photo reconcile request: %w", err)
	}
	// The still existed before, so a lookup cannot tell whether this landed.
	return a.commitSerialized(ctx, serializedData, nil)
}

// findLanded looks hash up after a failed commit and returns its media key if
// the item is in the library anyway. Lookup errors count as "not found".
func (a *Api) findLanded(ctx context.Context, hash []byte) string {
	key, err := a.FindRemoteMediaByHash(ctx, hash)
	if err != nil {
		return ""
	}
	return key
}

// commitSerialized sends a create request, retrying as the policy allows.
// A failed commit (a 429 or 5xx, say) may still have created the item, so
// when landed is set it is asked before each retry and after the last
// failure; a key it returns is the commit's result. That avoids reporting a
// stored item as failed, and avoids creating it twice.
func (a *Api) commitSerialized(ctx context.Context, serializedData []byte, landed func(context.Context) string) (string, error) {
	retryConfig := a.retryConfig()
	if a.commitRetryConfig != nil {
		retryConfig = *a.commitRetryConfig
	}
	var lastErr error
	attempts := 0
	for attempt := 0; attempt <= retryConfig.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := waitRequest(ctx, CalculateBackoff(attempt-1, retryConfig)); err != nil {
				return "", err
			}
			if landed != nil {
				if key := landed(ctx); key != "" {
					return key, nil
				}
			}
		}
		mediaKey, retryable, err := a.doCommitRequest(ctx, serializedData)
		attempts++
		if err == nil {
			return mediaKey, nil
		}
		lastErr = err
		if !retryable {
			break
		}
	}
	if landed != nil && ctx.Err() == nil {
		if key := landed(ctx); key != "" {
			return key, nil
		}
	}
	return "", fmt.Errorf("commit failed after %d attempt(s): %w", attempts, lastErr)
}

func (a *Api) doCommitRequest(ctx context.Context, serializedData []byte) (mediaKey string, retryable bool, err error) {
	bearerToken, err := a.BearerToken(ctx)
	if err != nil {
		return "", true, fmt.Errorf("failed to get bearer token: %w", err)
	}

	headers := map[string]string{
		"accept-Encoding":          "gzip",
		"accept-Language":          a.language,
		"content-Type":             "application/x-protobuf",
		"user-Agent":               a.userAgent,
		"authorization":            "Bearer " + bearerToken,
		"x-goog-ext-173412678-bin": "CgcIAhClARgC",
		"x-goog-ext-174067345-bin": "CgIIAg==",
	}

	endpoint := a.commitEndpoint
	if endpoint == "" {
		endpoint = photosCreateMediaItemsEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(serializedData))
	if err != nil {
		return "", false, fmt.Errorf("failed to create request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := a.do(req)
	if err != nil {
		return "", ctx.Err() == nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ReadResponseBody(resp)
		statusErr := &httpStatusError{statusCode: resp.StatusCode, body: string(body)}
		return "", a.shouldRetry(statusErr), statusErr
	}

	bodyBytes, err := ReadResponseBody(resp)
	if err != nil {
		return "", false, fmt.Errorf("failed to read accepted response body: %w", err)
	}

	// HTTP success may mean the media item already exists even when the minimal
	// response schema cannot validate it. Do not retry and risk a duplicate commit.
	mediaKey, err = parseCreateMediaItemsResponse(bodyBytes)
	if err != nil {
		return "", false, fmt.Errorf("failed to parse accepted response: %w", err)
	}
	return mediaKey, false, nil
}

// parseCreateMediaItemsResponse decodes only the verified media-key path. The
// generated minimal schema leaves every other private response field unknown,
// so proto.Unmarshal preserves forward compatibility without guessing types.
func parseCreateMediaItemsResponse(responseBytes []byte) (string, error) {
	var response generated.CreateMediaItemsResponse
	if err := proto.Unmarshal(responseBytes, &response); err != nil {
		return "", fmt.Errorf("unmarshal create-media response: %w", err)
	}
	for _, item := range response.GetItem() {
		if mediaKey := item.GetResultItem().GetMediaKey(); mediaKey != "" {
			return mediaKey, nil
		}
	}
	return "", fmt.Errorf("upload rejected by API: media key is empty or missing")
}

// CreateAlbum creates a new album with the given name and initial media items.
// Returns the album media key for subsequent AddMediaToAlbum calls.
func (a *Api) CreateAlbum(ctx context.Context, albumName string, mediaKeys []string) (string, error) {
	// Build media keys structure
	protoMediaKeys := make([]*generated.CreateAlbumField4Type, len(mediaKeys))
	for i, key := range mediaKeys {
		protoMediaKeys[i] = &generated.CreateAlbumField4Type{
			Field1: &generated.CreateAlbumField4TypeField1Type{
				MediaKey: key,
			},
		}
	}

	// Create the protobuf message
	protoBody := generated.CreateAlbum{
		AlbumName: albumName,
		Timestamp: time.Now().Unix(),
		Field3:    1,
		MediaKeys: protoMediaKeys,
		Field6:    &generated.CreateAlbumField6Type{},
		Field7:    &generated.CreateAlbumField7Type{Field1: 3},
		DeviceInfo: &generated.CreateAlbumField8Type{
			Model:             a.model,
			Make:              a.make,
			AndroidApiVersion: a.androidAPIVersion,
		},
	}

	// Serialize the protobuf message
	serializedData, err := proto.Marshal(&protoBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal protobuf: %w", err)
	}

	// Get the bearer token
	bearerToken, err := a.BearerToken(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get bearer token: %w", err)
	}

	// Prepare headers
	headers := map[string]string{
		"Accept-Encoding":          "gzip",
		"Accept-Language":          a.language,
		"Content-Type":             "application/x-protobuf",
		"User-Agent":               a.userAgent,
		"Authorization":            "Bearer " + bearerToken,
		"x-goog-ext-173412678-bin": "CgcIAhClARgC",
		"x-goog-ext-174067345-bin": "CgIIAg==",
	}

	// Create the request
	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://photosdata-pa.googleapis.com/6439526531001121323/8386163679468898444",
		bytes.NewReader(serializedData),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Make the request
	resp, err := a.do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check for errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ReadResponseBody(resp)
		return "", &httpStatusError{statusCode: resp.StatusCode, body: string(body)}
	}

	// Parse the response body
	bodyBytes, err := ReadResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	var pbResp generated.CreateAlbumResponse
	if err := proto.Unmarshal(bodyBytes, &pbResp); err != nil {
		return "", fmt.Errorf("failed to unmarshal protobuf: %w", err)
	}

	// Get album media key from response
	if pbResp.GetField1() == nil {
		return "", fmt.Errorf("create album failed: invalid response structure")
	}

	albumMediaKey := pbResp.GetField1().GetAlbumMediaKey()
	if albumMediaKey == "" {
		return "", fmt.Errorf("create album failed: no album media key returned")
	}

	return albumMediaKey, nil
}

// AddMediaToAlbum adds media items to an existing album.
func (a *Api) AddMediaToAlbum(ctx context.Context, albumMediaKey string, mediaKeys []string) error {
	// Create the protobuf message
	protoBody := generated.AddMediaToAlbum{
		MediaKeys:     mediaKeys,
		AlbumMediaKey: albumMediaKey,
		Field5:        &generated.AddMediaToAlbumField5Type{Field1: 2},
		DeviceInfo: &generated.AddMediaToAlbumField6Type{
			Model:             a.model,
			Make:              a.make,
			AndroidApiVersion: a.androidAPIVersion,
		},
		Timestamp: time.Now().Unix(),
	}

	// Serialize the protobuf message
	serializedData, err := proto.Marshal(&protoBody)
	if err != nil {
		return fmt.Errorf("failed to marshal protobuf: %w", err)
	}

	// Get the bearer token
	bearerToken, err := a.BearerToken(ctx)
	if err != nil {
		return fmt.Errorf("failed to get bearer token: %w", err)
	}

	// Prepare headers
	headers := map[string]string{
		"Accept-Encoding":          "gzip",
		"Accept-Language":          a.language,
		"Content-Type":             "application/x-protobuf",
		"User-Agent":               a.userAgent,
		"Authorization":            "Bearer " + bearerToken,
		"x-goog-ext-173412678-bin": "CgcIAhClARgC",
		"x-goog-ext-174067345-bin": "CgIIAg==",
	}

	// Create the request
	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://photosdata-pa.googleapis.com/6439526531001121323/484917746253879292",
		bytes.NewReader(serializedData),
	)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Make the request
	resp, err := a.do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check for errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ReadResponseBody(resp)
		return &httpStatusError{statusCode: resp.StatusCode, body: string(body)}
	}

	return nil
}
