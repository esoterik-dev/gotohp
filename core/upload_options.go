package core

// UploadOptions fully describes how a single upload run behaves. The GUI
// derives them from its Preferences; the CLI builds them from flags.
type UploadOptions struct {
	// Api selects the account and upload policy used for every request.
	Api ApiOptions

	Recursive      bool
	ExcludePattern string
	// SkipFiles are files the run writes itself, such as the CLI's debug
	// log; they are never uploaded, even when a scanned folder holds them.
	SkipFiles []string
	Threads   int
	// HashThreads and UploadThreads split the work into two pools: hash
	// workers hash, stage and check for duplicates, upload workers transfer
	// and commit new files. When neither is set, Threads workers each handle
	// a file from start to finish, as before the split (normalized then sets
	// UploadThreads to 0). When one is set, the other defaults to Threads.
	HashThreads                   int
	UploadThreads                 int
	onePool                       bool // set by normalized; keeps it idempotent
	ForceUpload                   bool
	DeleteFromHost                bool
	DisableUnsupportedFilesFilter bool
	SetDateFromFilename           bool
	// StateFile records confirmed uploads so a rerun skips them without
	// hashing. UpdateState writes it; RevalidateState ignores its records.
	StateFile       string
	UpdateState     bool
	RevalidateState bool
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
	if o.onePool || (o.HashThreads < 1 && o.UploadThreads < 1) {
		o.onePool, o.HashThreads, o.UploadThreads = true, o.Threads, 0
	} else {
		if o.HashThreads < 1 {
			o.HashThreads = o.Threads
		}
		if o.UploadThreads < 1 {
			o.UploadThreads = o.Threads
		}
	}
	if o.AlbumAutoMode {
		o.AlbumName = ""
	}
	return o
}
