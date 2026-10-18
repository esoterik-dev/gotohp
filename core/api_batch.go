package core

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// FindRemoteMediaByHashes looks up several SHA-1 hashes in one request. The
// result maps each answered hash (as a string of its bytes) to its match; a
// hash not in the library maps to an empty match. A hash the server did not
// answer is absent from the result, and callers must treat it as unknown,
// never as "not in the library".
func (a *Api) FindRemoteMediaByHashes(ctx context.Context, hashes [][]byte) (map[string]RemoteMediaMatch, error) {
	var matches map[string]RemoteMediaMatch
	err := a.withRetry(ctx, a.retryConfig(), func(int) error {
		body, err := a.postHashCheck(ctx, encodeHashBatch(hashes))
		if err != nil {
			return err
		}
		matches, err = decodeHashBatch(body)
		return err
	})
	if err == nil {
		found := 0
		for _, match := range matches {
			if match.PrimaryMediaKey != "" || match.RelatedMediaKey != "" {
				found++
			}
		}
		a.policy.log().Debug("hash lookup batch", "hashes", len(hashes), "found", found)
	}
	return matches, err
}

// encodeHashBatch builds a HashCheck request carrying several hashes:
// field 1 { repeated field 1 { bytes sha1 = 1 }, field 2 {} }. The endpoint
// answers with one entry per hash (checked against the live API on
// 2026-10-18).
func encodeHashBatch(hashes [][]byte) []byte {
	var inner []byte
	for _, h := range hashes {
		sha := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), h)
		inner = protowire.AppendTag(inner, 1, protowire.BytesType)
		inner = protowire.AppendBytes(inner, sha)
	}
	inner = protowire.AppendTag(inner, 2, protowire.BytesType)
	inner = protowire.AppendBytes(inner, nil)
	out := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(out, inner)
}

// decodeHashBatch reads a RemoteMatches response with one entry per hash:
// field 1 { repeated field 2 { field 1 { bytes sha1 = 1 }, field 2 {
// string media_key = 1, field 6 { string media_key = 1 } } } }.
func decodeHashBatch(body []byte) (map[string]RemoteMediaMatch, error) {
	matches := make(map[string]RemoteMediaMatch)
	err := eachField(body, func(num protowire.Number, v []byte) error {
		if num != 1 {
			return nil
		}
		return eachField(v, func(num protowire.Number, entry []byte) error {
			if num != 2 {
				return nil
			}
			var hash []byte
			var match RemoteMediaMatch
			err := eachField(entry, func(num protowire.Number, v []byte) error {
				switch num {
				case 1:
					return eachField(v, func(num protowire.Number, v []byte) error {
						if num == 1 {
							hash = v
						}
						return nil
					})
				case 2:
					return eachField(v, func(num protowire.Number, v []byte) error {
						switch num {
						case 1:
							match.PrimaryMediaKey = string(v)
						case 6:
							return eachField(v, func(num protowire.Number, v []byte) error {
								if num == 1 {
									match.RelatedMediaKey = string(v)
								}
								return nil
							})
						}
						return nil
					})
				}
				return nil
			})
			if err != nil {
				return err
			}
			if hash != nil {
				matches[string(hash)] = match
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("decode batch hash lookup: %w", err)
	}
	return matches, nil
}

// eachField calls fn for every length-delimited field of a protobuf message
// and skips other wire types.
func eachField(b []byte, fn func(protowire.Number, []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || !num.IsValid() {
			return fmt.Errorf("invalid tag")
		}
		b = b[n:]
		if typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return fmt.Errorf("invalid field %d", num)
			}
			b = b[m:]
			continue
		}
		v, m := protowire.ConsumeBytes(b)
		if m < 0 {
			return fmt.Errorf("invalid field %d", num)
		}
		b = b[m:]
		if err := fn(num, v); err != nil {
			return err
		}
	}
	return nil
}
