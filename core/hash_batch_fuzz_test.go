package core

import (
	"bytes"
	"maps"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// hashBatchReply builds a reply from raw: raw split at 0xff gives
// (hash, primary key, related key) triples, so the fuzzer explores
// well-formed replies too; an empty hash part leaves the entry's hash field
// out. It returns the reply and what decoding it must give: an entry without
// a hash answers nothing, and a repeated hash keeps its last entry.
func hashBatchReply(raw []byte) ([]byte, map[string]RemoteMediaMatch) {
	parts := bytes.Split(raw, []byte{0xff})
	want := make(map[string]RemoteMediaMatch)
	var entries [][]byte
	for i := 0; i+2 < len(parts); i += 3 {
		hash, primary, related := parts[i], parts[i+1], parts[i+2]
		var entry []byte
		if len(hash) > 0 {
			entry = field(1, field(1, hash))
		}
		if len(primary) > 0 || len(related) > 0 {
			entry = append(entry, field(2, msg(field(1, primary), field(6, field(1, related))))...)
		}
		entries = append(entries, field(2, entry))
		if len(hash) == 0 {
			continue
		}
		want[string(hash)] = RemoteMediaMatch{PrimaryMediaKey: string(primary), RelatedMediaKey: string(related)}
	}
	return field(1, msg(entries...)), want
}

// FuzzDecodeHashBatch checks that decoding a batch hash-lookup reply never
// panics or changes its input, returns nothing on error, and only reports
// hashes and media keys that are really in the reply, so a malformed reply
// cannot skip a file under a made-up key. A well-formed reply built from the
// same bytes must decode to exactly its entries.
func FuzzDecodeHashBatch(f *testing.F) {
	a, b, u := bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{2}, 20), bytes.Repeat([]byte{3}, 20)
	valid := batchResponse(map[string]string{string(a): "AF1QipA", string(b): "AF1QipB"}, [][]byte{a, b, u})
	varint := protowire.AppendTag(nil, 3, protowire.VarintType)
	varint = protowire.AppendVarint(varint, 7)

	for _, seed := range [][]byte{
		valid,
		batchResponse(nil, [][]byte{a}),       // not in the library
		append(bytes.Clone(valid), valid...),  // repeated field 1
		append(bytes.Clone(varint), valid...), // non-bytes field first
		valid[:len(valid)-1],                  // truncated
		valid[:2],                             // truncated after the outer tag
		field(1, field(2, field(2, field(1, []byte("k"))))), // match without a hash
		field(1, field(2, field(1, []byte{0x0a, 0x05}))),    // truncated hash
		msg(a, []byte{0xff}, []byte("AF1QipA"), []byte{0xff}, []byte("AF1QipR")),
		msg([]byte{0xff}, []byte("AF1QipA"), []byte{0xff}), // built entry without a hash
		{0x0a, 0x80, 0x80, 0x80, 0x80, 0x10},               // length beyond the body
		{0x0b, 0x0c},                                       // group start/end
		{0x00},                                             // field number 0
		{},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		input := bytes.Clone(raw)
		got, err := decodeHashBatch(raw)
		if !bytes.Equal(raw, input) {
			t.Fatal("decoder modified its input")
		}
		if err != nil {
			if got != nil {
				t.Fatalf("error %v with matches %+v", err, got)
			}
		} else {
			for hash, match := range got {
				for _, part := range []string{hash, match.PrimaryMediaKey, match.RelatedMediaKey} {
					if !bytes.Contains(raw, []byte(part)) {
						t.Fatalf("decoded %q, which is not in the reply %x", part, raw)
					}
				}
			}
		}

		reply, want := hashBatchReply(raw)
		got, err = decodeHashBatch(reply)
		if err != nil {
			t.Fatalf("well-formed reply %x: %v", reply, err)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("reply %x decoded to %+v, want %+v", reply, got, want)
		}
	})
}
