package core

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// The state file is a JSON Lines log: a header line, then one entry per
// confirmed upload. A later entry for the same key replaces an earlier one.
// Each confirmation appends and syncs one line, so the cost per item stays
// constant however large the library is. Opening a writable state compacts
// the log to one entry per key.
const uploadStateVersion = 2

type stateHeader struct {
	Version int `json:"version"`
}

type stateFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modifiedNanoseconds"`
	SHA1     string `json:"sha1"`
}

// stateEntry confirms that the files of one work item are in the library as
// MediaKey. Key covers the account, upload policy, item kind and paths.
type stateEntry struct {
	Key      string      `json:"key"`
	MediaKey string      `json:"mediaKey"`
	Files    []stateFile `json:"files"`
}

type uploadState struct {
	mu      sync.Mutex
	scope   string
	entries map[string]stateEntry
	log     *os.File // append handle; nil when the state is read-only
	lock    *os.File // holds the writer lock until close
}

func stateDigest(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// openUploadState opens opts.StateFile, or returns nil when no state file is
// used. With UpdateState it takes the writer lock and compacts the log.
func openUploadState(opts UploadOptions) (*uploadState, error) {
	if opts.StateFile == "" {
		return nil, nil
	}
	account := opts.Api.Account
	if account == "" {
		account = currentConfig().Account.Selected
	}
	s := &uploadState{entries: make(map[string]stateEntry)}
	s.scope = stateDigest(struct {
		Account                                          string
		Saver, Quota, Date, Pair, Repair, IgnoreMetadata bool
	}{strings.ToLower(account), opts.Api.Saver, opts.Api.UseQuota, opts.SetDateFromFilename, opts.PairLivePhotos, opts.UpdateExistingPhotosToLive, opts.IgnoreAppleMetadata})

	if opts.UpdateState {
		lock, err := os.OpenFile(opts.StateFile+".lock", os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			return nil, fmt.Errorf("open upload state lock: %w", err)
		}
		if err := tryLockFile(lock); err != nil {
			_ = lock.Close()
			return nil, fmt.Errorf("upload state %q is in use by another gotohp run: %w", opts.StateFile, err)
		}
		s.lock = lock
	}

	err := s.load(opts.StateFile)
	if errors.Is(err, os.ErrNotExist) && opts.UpdateState {
		err = nil
	}
	if err == nil && opts.UpdateState {
		err = s.compact(opts.StateFile)
	}
	if err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *uploadState) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read upload state: %w", err)
	}
	lines := bytes.Split(data, []byte("\n"))
	var header stateHeader
	if err := json.Unmarshal(lines[0], &header); err != nil || header.Version != uploadStateVersion {
		return fmt.Errorf("unsupported or invalid upload state %q", path)
	}
	for i, line := range lines[1:] {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry stateEntry
		if err := json.Unmarshal(line, &entry); err != nil || entry.Key == "" {
			if i == len(lines)-2 && !bytes.HasSuffix(data, []byte("\n")) {
				break // A crash cut off the last append; that item was not confirmed.
			}
			return fmt.Errorf("invalid upload state %q at line %d", path, i+2)
		}
		s.entries[entry.Key] = entry
	}
	return nil
}

// compact atomically rewrites the log with one entry per key, then opens it
// for appending.
func (s *uploadState) compact(path string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".gotohp-state-*")
	if err != nil {
		return fmt.Errorf("write upload state: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	w := bufio.NewWriter(file)
	enc := json.NewEncoder(w)
	err = file.Chmod(0600)
	if err == nil {
		err = enc.Encode(stateHeader{Version: uploadStateVersion})
	}
	for _, entry := range s.entries {
		if err == nil {
			err = enc.Encode(entry)
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		err = syncDir(filepath.Dir(path))
	}
	if err != nil {
		return fmt.Errorf("write upload state: %w", err)
	}
	s.log, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open upload state: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	if closeErr := d.Close(); syncErr == nil {
		syncErr = closeErr
	}
	return syncErr
}

// close releases the writer lock. The lock file itself stays: the lock lives
// in the kernel and is released on exit even after a crash, so a leftover
// file never blocks the next run.
func (s *uploadState) close() {
	if s == nil {
		return
	}
	if s.log != nil {
		_ = s.log.Close()
		s.log = nil
	}
	if s.lock != nil {
		_ = s.lock.Close()
		s.lock = nil
	}
}

// writable reports whether confirmations are saved, and therefore whether
// deletions wait for them.
func (s *uploadState) writable() bool {
	return s != nil && s.log != nil
}

func (s *uploadState) key(item UploadWorkItem) string {
	paths := uploadWorkPaths(item)
	for i := range paths {
		paths[i], _ = filepath.Abs(paths[i])
	}
	return stateDigest([]any{s.scope, item.Kind, paths})
}

// lookup returns the confirmed media key for item when every file still has
// the recorded path, size and modification time; it reads no file contents.
func (s *uploadState) lookup(item UploadWorkItem, opts UploadOptions) string {
	if s == nil || opts.ForceUpload || opts.RevalidateState {
		return ""
	}
	s.mu.Lock()
	entry, ok := s.entries[s.key(item)]
	s.mu.Unlock()
	if !ok || len(entry.Files) != len(uploadWorkPaths(item)) {
		return ""
	}
	for _, file := range entry.Files {
		info, err := os.Stat(file.Path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size || info.ModTime().UnixNano() != file.Modified {
			return ""
		}
	}
	return entry.MediaKey
}

// save durably records a confirmed upload. It refuses if a source changed
// since it was hashed, so a checkpoint never vouches for different bytes.
// Incomplete Live Photo skips are not saved.
func (s *uploadState) save(prepared preparedUpload, mediaKey string, skipped bool) error {
	if !s.writable() || mediaKey == "" || (prepared.item.Kind == UploadWorkLivePhoto && skipped) {
		return nil
	}
	entry := stateEntry{Key: s.key(prepared.item), MediaKey: mediaKey}
	for _, file := range prepared.files {
		if err := file.unchanged(); err != nil {
			return fmt.Errorf("source changed before state checkpoint: %w", err)
		}
		path, err := filepath.Abs(file.source)
		if err != nil {
			return err
		}
		entry.Files = append(entry.Files, stateFile{
			Path:     path,
			Size:     file.info.Size(),
			Modified: file.info.ModTime().UnixNano(),
			SHA1:     hex.EncodeToString(file.hash),
		})
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	end, err := s.log.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = s.log.Write(append(line, '\n'))
		if err == nil {
			err = s.log.Sync()
		}
		if err != nil {
			_ = s.log.Truncate(end) // Never leave a partial line before the next append.
		}
	}
	if err != nil {
		return fmt.Errorf("write upload state: %w", err)
	}
	s.entries[entry.Key] = entry
	return nil
}
