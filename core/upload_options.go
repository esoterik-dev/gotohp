package core

// UploadOptions fully describes how a single upload run behaves. The GUI
// derives them from its Preferences; the CLI builds them from flags.
type UploadOptions struct {
	// Api selects the account and upload policy used for every request.
	Api ApiOptions

	Recursive      bool
	ExcludePattern string
	Threads        int
	// HashThreads and UploadThreads size the hashing and network worker
	// pools; each defaults to Threads.
	HashThreads                   int
	UploadThreads                 int
	ForceUpload                   bool
	DeleteFromHost                bool
	DisableUnsupportedFilesFilter bool
	SetDateFromFilename           bool
	// StageLocal copies each upload work item to os.TempDir (TMPDIR on Unix)
	// before hashing and uploading. This is useful for FUSE/device sources.
	StageLocal bool

	PairLivePhotos             bool
	SkipIncompleteLivePhotos   bool
	UpdateExistingPhotosToLive bool
	// IgnoreAppleMetadata matches Live Photo pairs by filename stem instead of
	// Apple content identifiers. It is never persisted.
	IgnoreAppleMetadata bool

	// AlbumName is a name or album key to add uploads to. Ignored when
	// AlbumAutoMode is set, which creates one album per source directory.
	AlbumName     string
	AlbumAutoMode bool
}

// UploadOptions derives run options from GUI preferences.
func (c Preferences) UploadOptions() UploadOptions {
	return UploadOptions{
		Api: ApiOptions{
			Proxy:    c.Proxy,
			Saver:    c.Saver,
			UseQuota: c.UseQuota,
		},
		Recursive:                     c.Recursive,
		ExcludePattern:                c.ExcludePattern,
		Threads:                       c.UploadThreads,
		ForceUpload:                   c.ForceUpload,
		DeleteFromHost:                c.DeleteFromHost,
		DisableUnsupportedFilesFilter: c.DisableUnsupportedFilesFilter,
		SetDateFromFilename:           c.SetDateFromFilename,
		PairLivePhotos:                c.PairLivePhotos,
		SkipIncompleteLivePhotos:      c.SkipIncompleteLivePhotos,
		UpdateExistingPhotosToLive:    c.UpdateExistingPhotosToLive,
		AlbumName:                     c.AlbumName,
		AlbumAutoMode:                 c.AlbumAutoMode,
	}
}

func (o UploadOptions) normalized() UploadOptions {
	if o.Threads < 1 {
		o.Threads = 1
	}
	if o.HashThreads < 1 {
		o.HashThreads = o.Threads
	}
	if o.UploadThreads < 1 {
		o.UploadThreads = o.Threads
	}
	if o.AlbumAutoMode {
		o.AlbumName = ""
	}
	return o
}
