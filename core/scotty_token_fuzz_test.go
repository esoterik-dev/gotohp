package core

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func scottyTokenSeed(version uint64, opaque []byte) []byte {
	raw := protowire.AppendTag(nil, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, version)
	raw = protowire.AppendTag(raw, 2, protowire.BytesType)
	return protowire.AppendBytes(raw, opaque)
}

// FuzzParseScottyFinalizeToken checks that parsing never panics, that an
// accepted token keeps the input byte-for-byte in an owned copy and re-parses
// to the same Raw, and that the legacy commit conversion agrees with the
// parser about which envelopes are valid.
func FuzzParseScottyFinalizeToken(f *testing.F) {
	valid := scottyTokenSeed(scottyFinalizeTokenVersion, []byte("opaque"))
	long := scottyTokenSeed(scottyFinalizeTokenVersion, bytes.Repeat([]byte{0xa5, 0x01, 0x00, 0xff}, 64))
	reversed := protowire.AppendTag(nil, 2, protowire.BytesType)
	reversed = protowire.AppendBytes(reversed, []byte("upload"))
	reversed = protowire.AppendTag(reversed, 1, protowire.VarintType)
	reversed = protowire.AppendVarint(reversed, scottyFinalizeTokenVersion)
	unknown := protowire.AppendTag(bytes.Clone(valid), 7, protowire.Fixed32Type)
	unknown = protowire.AppendFixed32(unknown, 0xdeadbeef)
	wrongType1 := protowire.AppendTag(nil, 1, protowire.BytesType)
	wrongType1 = protowire.AppendBytes(wrongType1, []byte{2})
	wrongType1 = append(wrongType1, valid[2:]...)
	wrongType2 := protowire.AppendTag(valid[:2:2], 2, protowire.VarintType)
	wrongType2 = protowire.AppendVarint(wrongType2, 6)
	// A non-minimal varint for version 2 (0x82 0x00) still decodes to 2.
	paddedVersion := append([]byte{0x08, 0x82, 0x00}, valid[2:]...)

	for _, seed := range [][]byte{
		valid,
		long,
		reversed,
		unknown,
		valid[:len(valid)-1],                     // truncated opaque bytes
		valid[:3],                                // truncated after the field-2 tag
		valid[:1],                                // truncated version varint
		append(bytes.Clone(valid), valid...),     // duplicated fields
		append(bytes.Clone(valid), valid[:2]...), // duplicated field 1
		scottyTokenSeed(1, []byte("opaque")),     // wrong version
		scottyTokenSeed(scottyFinalizeTokenVersion, nil), // empty field 2
		wrongType1,
		wrongType2,
		paddedVersion,
		{0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, // overlong varint
		{0x0b, 0x0c}, // group start/end
		{0x00},       // field number 0
		{},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		input := bytes.Clone(raw)
		token, err := ParseScottyFinalizeToken(raw)
		_, legacyErr := ScottyFinalizeToken{Raw: raw}.legacyCommitToken()
		if !bytes.Equal(raw, input) {
			t.Fatal("parser modified its input")
		}
		if err != nil {
			if token.Raw != nil {
				t.Fatalf("rejected token returned Raw %x", token.Raw)
			}
			if legacyErr == nil {
				t.Fatalf("legacyCommitToken accepted %x rejected by the parser: %v", raw, err)
			}
			return
		}
		if !bytes.Equal(token.Raw, raw) {
			t.Fatalf("Raw %x differs from input %x", token.Raw, raw)
		}
		if len(raw) > 0 && &token.Raw[0] == &raw[0] {
			t.Fatal("Raw aliases the caller's buffer")
		}
		again, err := ParseScottyFinalizeToken(token.Raw)
		if err != nil {
			t.Fatalf("accepted token %x does not re-parse: %v", token.Raw, err)
		}
		if !bytes.Equal(again.Raw, token.Raw) {
			t.Fatalf("re-parse changed Raw: %x -> %x", token.Raw, again.Raw)
		}
		if _, err := token.legacyCommitToken(); err != nil {
			// Unknown fields may still conflict with CommitToken's schema only
			// if they reuse field 1 or 2, which the parser already validated.
			t.Fatalf("accepted token %x fails legacy conversion: %v", raw, err)
		}
	})
}
