package core

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Placeholders the state fuzzer replaces with values only known at run time,
// so seeds and mutations can produce entries that really match the probe item.
const (
	fuzzStateKey   = "@KEY@"
	fuzzStatePath  = "@PATH@"
	fuzzStateMtime = 1700000000123456789
	fuzzStateSize  = 5
)

func fuzzStateLine(key, mediaKey, path string, size, mtime int64) string {
	return `{"key":"` + key + `","mediaKey":"` + mediaKey + `","files":[{"path":"` + path +
		`","size":` + strconv.FormatInt(size, 10) + `,"modifiedNanoseconds":` + strconv.FormatInt(mtime, 10) +
		`,"sha1":"0f7d0d088b6ea936fb25b477722d734706fe8b40"}]}` + "\n"
}

// FuzzUploadStateLoad feeds arbitrary state-file bytes, plus an arbitrary
// unterminated tail, through openUploadState. Properties:
//   - opening never panics, read-only or with compaction;
//   - when the file opens, appending a tail without a newline (a crash cut
//     off the last append) still opens and keeps every earlier key;
//   - compaction preserves the entry set exactly;
//   - lookup returns a media key only if some line of the file is a
//     well-formed entry for exactly that key, path, size and mtime.
func FuzzUploadStateLoad(f *testing.F) {
	header := `{"version":2}` + "\n"
	match := fuzzStateLine(fuzzStateKey, "AF1QipMatch", fuzzStatePath, fuzzStateSize, fuzzStateMtime)
	other := fuzzStateLine("6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b", "AF1QipOther", "/photos/IMG_0001.JPG", 2048, 1690000000000000000)
	for _, seed := range []struct{ data, tail string }{
		{header, ""},
		{header + match, ""},
		{header + other + match, `{"key":"cut off by a cra`},
		{header + match + other, `{"key":"` + fuzzStateKey + `","mediaKey":"AF1QipLater"`},
		{header + match, `{"key":"` + fuzzStateKey + `","mediaKey":"AF1QipLater","files":[]}`},
		{header + fuzzStateLine(fuzzStateKey, "AF1QipWrongSize", fuzzStatePath, fuzzStateSize+1, fuzzStateMtime), ""},
		{header + fuzzStateLine(fuzzStateKey, "AF1QipWrongTime", fuzzStatePath, fuzzStateSize, fuzzStateMtime+1), ""},
		{header + fuzzStateLine(fuzzStateKey, "AF1QipWrongPath", fuzzStatePath+".bak", fuzzStateSize, fuzzStateMtime), ""},
		{header + fuzzStateLine("other-key", "AF1QipWrongKey", fuzzStatePath, fuzzStateSize, fuzzStateMtime), ""},
		{header + `{"key":"` + fuzzStateKey + `","mediaKey":"AF1QipNoFiles","files":[]}` + "\n", ""},
		{header + `{"key":"` + fuzzStateKey + `","mediaKey":"","files":[]}` + "\n", ""},
		{header + "\n\n  \n" + match + "\n", "   "},
		{header + `{"key":""}` + "\n", ""},
		{header + `not json` + "\n" + match, ""},
		{header + match[:len(match)-1], ""}, // complete last entry without newline
		{`{"version":1}` + "\n" + match, ""},
		{`{"version":2`, ""},
		{"", ""},
		{"\xef\xbb\xbf" + header, ""},
		{header + `{"key":"` + fuzzStateKey + `","mediaKey":"dup","key":"x"}` + "\n", ""},
	} {
		f.Add([]byte(seed.data), []byte(seed.tail))
	}

	f.Fuzz(func(t *testing.T, data, tail []byte) {
		dir := t.TempDir()
		source := filepath.Join(dir, "IMG_0001.JPG")
		if err := os.WriteFile(source, []byte("photo"), 0600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Unix(0, fuzzStateMtime)
		if err := os.Chtimes(source, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		item := UploadWorkItem{Kind: UploadWorkSingle, Single: &SingleMedia{Path: source}}
		opts := UploadOptions{Api: ApiOptions{Account: "one@example.test"}, StateFile: filepath.Join(dir, "state.json")}
		probe := &uploadState{scope: stateDigest(struct {
			Account                                          string
			Saver, Quota, Date, Pair, Repair, IgnoreMetadata bool
		}{"one@example.test", false, false, false, false, false, false})}
		key := probe.key(item)
		absSource, _ := filepath.Abs(source)
		quotedPath, _ := json.Marshal(absSource)
		data = bytes.ReplaceAll(data, []byte(fuzzStateKey), []byte(key))
		data = bytes.ReplaceAll(data, []byte(`"`+fuzzStatePath+`"`), quotedPath)
		tail = bytes.ReplaceAll(tail, []byte(fuzzStateKey), []byte(key))
		tail = bytes.ReplaceAll(tail, []byte(`"`+fuzzStatePath+`"`), quotedPath)
		tail = bytes.ReplaceAll(tail, []byte("\n"), nil)

		open := func(t *testing.T, content []byte, update bool) (*uploadState, error) {
			t.Helper()
			if err := os.WriteFile(opts.StateFile, content, 0600); err != nil {
				t.Fatal(err)
			}
			o := opts
			o.UpdateState = update
			s, err := openUploadState(o)
			if err == nil {
				t.Cleanup(s.close)
				if s.scope != probe.scope {
					t.Fatal("probe scope out of sync with openUploadState")
				}
			}
			return s, err
		}

		readOnly, err := open(t, data, false)
		if err != nil {
			if _, err := open(t, data, true); err == nil {
				t.Fatal("writable open accepted a file the read-only open rejected")
			}
			return
		}
		checkLookup(t, readOnly, item, opts, data, key, absSource)

		if bytes.HasSuffix(data, []byte("\n")) {
			withTail := append(bytes.Clone(data), tail...)
			torn, err := open(t, withTail, false)
			if err != nil {
				t.Fatalf("unterminated tail %q made a valid state unreadable: %v", tail, err)
			}
			for k := range readOnly.entries {
				if _, ok := torn.entries[k]; !ok {
					t.Fatalf("tail %q lost confirmed entry %q", tail, k)
				}
			}
			checkLookup(t, torn, item, opts, withTail, key, absSource)
		}

		writable, err := open(t, data, true)
		if err != nil {
			t.Fatalf("writable open rejected a readable state: %v", err)
		}
		writable.close()
		compacted, err := os.ReadFile(opts.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := open(t, compacted, false)
		if err != nil {
			t.Fatalf("compacted state does not reopen: %v\n%s", err, compacted)
		}
		if len(reopened.entries) != len(readOnly.entries) {
			t.Fatalf("compaction changed entry count %d -> %d", len(readOnly.entries), len(reopened.entries))
		}
		for k, want := range readOnly.entries {
			got, ok := reopened.entries[k]
			if !ok || stateDigest(got) != stateDigest(want) {
				t.Fatalf("compaction changed entry %q: %+v -> %+v", k, want, got)
			}
		}
	})
}

// checkLookup verifies a lookup hit against the raw file, independently of
// the loader: some line must decode to an entry that matches the probe file.
func checkLookup(t *testing.T, s *uploadState, item UploadWorkItem, opts UploadOptions, data []byte, key, path string) {
	t.Helper()
	got := s.lookup(item, opts)
	if got == "" {
		return
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var entry stateEntry
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if entry.Key == key && entry.MediaKey == got && len(entry.Files) == 1 &&
			entry.Files[0].Path == path && entry.Files[0].Size == fuzzStateSize && entry.Files[0].Modified == fuzzStateMtime {
			return
		}
	}
	t.Fatalf("lookup returned %q with no matching entry in:\n%s", got, data)
}
