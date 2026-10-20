package core

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// README: hash workers check whether each file or Live Photo pair is
// already in the library; only new pairs are queued for the upload workers.
func TestHashWorkersSkipKnownLivePhotoPairs(t *testing.T) {
	instantRechecks(t)
	modes := map[string]UploadOptions{
		"one pool":          {Threads: 2},
		"split":             {HashThreads: 3, UploadThreads: 1},
		"split stage-local": {HashThreads: 3, UploadThreads: 1, StageLocal: true},
	}
	for name, opts := range modes {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			lib := newFakeLibrary()
			var paths []string
			for i := range 3 {
				still := filepath.Join(dir, fmt.Sprintf("IMG_%d.HEIC", i))
				video := filepath.Join(dir, fmt.Sprintf("IMG_%d.MOV", i))
				if err := os.WriteFile(still, []byte(still), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(video, liveVideoFixture(video), 0600); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, still, video)
				if i < 2 {
					lib.add(t, still)
				}
			}
			opts.PairLivePhotos, opts.IgnoreAppleMetadata = true, true
			_, rec := startRun(lib, paths, opts)
			rec.wait(t)

			// The new pair fails only because fakeLibrary cannot commit
			// Live Photos; it still shows the pair reached an upload worker.
			ok, skipped, failed := countResults(rec.results)
			if ok != 0 || skipped != 2 || failed != 1 {
				t.Fatalf("succeeded=%d skipped=%d failed=%d", ok, skipped, failed)
			}
			// 2 lookups per pair, plus the recheck of the new pair; an upload
			// worker must not look the pair up again.
			if lib.lookups != 10 {
				t.Fatalf("lookups=%d, want 10", lib.lookups)
			}
			if lib.uploads != 2 {
				t.Fatalf("uploads=%d, want only the new pair's 2 files", lib.uploads)
			}
			if opts.UploadThreads > 0 {
				for id := range rec.workersWithStatus("skipped") {
					if id < opts.UploadThreads {
						t.Errorf("upload worker %d skipped a known pair", id)
					}
				}
			}
		})
	}
}

func fixtureBox(typ string, payload ...[]byte) []byte {
	size := 8
	for _, p := range payload {
		size += len(p)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(size))
	out = append(out, typ...)
	for _, p := range payload {
		out = append(out, p...)
	}
	return out
}

func u32(v ...uint32) []byte {
	var out []byte
	for _, x := range v {
		out = binary.BigEndian.AppendUint32(out, x)
	}
	return out
}

// liveVideoFixture is the smallest MOV the parser accepts as a Live Photo
// video: one timed-metadata track whose first sample is still-image-time.
// tag makes each file's hash distinct.
func liveVideoFixture(tag string) []byte {
	sample := append(u32(9, 1), 0xff)
	build := func(offset uint32) []byte {
		entry := fixtureBox("mebx", []byte("com.apple.quicktime.still-image-time"))
		stbl := fixtureBox("stbl",
			fixtureBox("stsd", u32(0, 1), entry),
			fixtureBox("stsz", u32(0, 9, 1)),
			fixtureBox("stco", u32(0, 1, offset)))
		moov := fixtureBox("moov", fixtureBox("trak", fixtureBox("mdia", fixtureBox("minf", stbl))))
		return append(append(moov, fixtureBox("free", []byte(tag))...), sample...)
	}
	n := len(build(0)) - len(sample)
	return build(uint32(n))
}
