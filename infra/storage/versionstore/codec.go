package versionstore

import "encoding/json"

// JSONCodec encodes values as JSON. It is the default choice; the version is
// framed outside the payload, so switching codecs — or changing the value's
// JSON shape — cannot affect version comparison.
type JSONCodec[T any] struct{}

func (JSONCodec[T]) Encode(value T) ([]byte, error) { return json.Marshal(value) }

func (JSONCodec[T]) Decode(raw []byte) (T, error) {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	return value, nil
}

var _ Codec[int] = JSONCodec[int]{}
