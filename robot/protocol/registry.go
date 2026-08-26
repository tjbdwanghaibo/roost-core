// Package protocol maps message ids to encoders and decoders. The registry
// is byte-oriented (any <-> []byte) and codec-agnostic: protobuf, JSON or
// anything else plugs in through the Codec, so neither this package nor the
// rest of the robot framework depends on a serialization library. Ported
// from the cube robot service.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrEncoderNotFound = errors.New("robot protocol: encoder not found")
	ErrDecoderNotFound = errors.New("robot protocol: decoder not found")
	ErrCodecRequired   = errors.New("robot protocol: registry has no codec")
)

type Encoder func(any) ([]byte, error)
type Decoder func([]byte) (any, error)

// Codec is the pluggable serialization pair used by the message-level
// helpers (RegisterMessage / action.RegisterCall). Games typically install a
// protobuf codec in three lines; JSONCodec serves tests and JSON protocols.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

// CodecFuncs adapts two functions (e.g. proto.Marshal / proto.Unmarshal
// wrapped for any) into a Codec.
type CodecFuncs struct {
	MarshalFunc   func(any) ([]byte, error)
	UnmarshalFunc func([]byte, any) error
}

func (c CodecFuncs) Marshal(v any) ([]byte, error)      { return c.MarshalFunc(v) }
func (c CodecFuncs) Unmarshal(data []byte, v any) error { return c.UnmarshalFunc(data, v) }

// JSONCodec is the stdlib JSON codec.
type JSONCodec struct{}

func (JSONCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (JSONCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

type Registry struct {
	codec    Codec
	mu       sync.RWMutex
	encoders map[uint32]Encoder
	decoders map[uint32]Decoder
}

// NewRegistry builds a registry. codec may be nil when every message is
// registered through explicit RegisterEncoder/RegisterDecoder pairs.
func NewRegistry(codec Codec) *Registry {
	return &Registry{
		codec:    codec,
		encoders: make(map[uint32]Encoder),
		decoders: make(map[uint32]Decoder),
	}
}

// Codec returns the registry's default codec (nil when none installed).
func (r *Registry) Codec() Codec {
	if r == nil {
		return nil
	}
	return r.codec
}

func (r *Registry) RegisterEncoder(msgID uint32, encoder Encoder) error {
	if r == nil {
		return errors.New("robot protocol: registry is nil")
	}
	if msgID == 0 {
		return errors.New("robot protocol: msg id is required")
	}
	if encoder == nil {
		return fmt.Errorf("robot protocol: encoder is required for msg %d", msgID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.encoders[msgID]; ok {
		return fmt.Errorf("robot protocol: duplicate encoder msg %d", msgID)
	}
	r.encoders[msgID] = encoder
	return nil
}

func (r *Registry) MustRegisterEncoder(msgID uint32, encoder Encoder) {
	if err := r.RegisterEncoder(msgID, encoder); err != nil {
		panic(err)
	}
}

func (r *Registry) RegisterDecoder(msgID uint32, decoder Decoder) error {
	if r == nil {
		return errors.New("robot protocol: registry is nil")
	}
	if msgID == 0 {
		return errors.New("robot protocol: msg id is required")
	}
	if decoder == nil {
		return fmt.Errorf("robot protocol: decoder is required for msg %d", msgID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.decoders[msgID]; ok {
		return fmt.Errorf("robot protocol: duplicate decoder msg %d", msgID)
	}
	r.decoders[msgID] = decoder
	return nil
}

func (r *Registry) MustRegisterDecoder(msgID uint32, decoder Decoder) {
	if err := r.RegisterDecoder(msgID, decoder); err != nil {
		panic(err)
	}
}

func (r *Registry) Encode(msgID uint32, value any) ([]byte, error) {
	if r == nil {
		return nil, errors.New("robot protocol: registry is nil")
	}
	r.mu.RLock()
	encoder, ok := r.encoders[msgID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: msg %d", ErrEncoderNotFound, msgID)
	}
	return encoder(value)
}

func (r *Registry) Decode(msgID uint32, payload []byte) (any, error) {
	if r == nil {
		return nil, errors.New("robot protocol: registry is nil")
	}
	r.mu.RLock()
	decoder, ok := r.decoders[msgID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: msg %d", ErrDecoderNotFound, msgID)
	}
	return decoder(payload)
}

// EnsureEncoder installs a codec-backed encoder for msgID if none exists
// (idempotent — generated codec tables win). Outbound values marshal with
// the registry codec.
func EnsureEncoder(r *Registry, msgID uint32) error {
	if r == nil {
		return errors.New("robot protocol: registry is nil")
	}
	if r.codec == nil {
		return fmt.Errorf("%w (install one via NewRegistry)", ErrCodecRequired)
	}
	if msgID == 0 {
		return errors.New("robot protocol: msg id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.encoders[msgID]; !ok {
		codec := r.codec
		r.encoders[msgID] = func(value any) ([]byte, error) {
			return codec.Marshal(value)
		}
	}
	return nil
}

// EnsureDecoder installs a codec-backed decoder producing *T for msgID if
// none exists (idempotent).
func EnsureDecoder[T any](r *Registry, msgID uint32) error {
	if r == nil {
		return errors.New("robot protocol: registry is nil")
	}
	if r.codec == nil {
		return fmt.Errorf("%w (install one via NewRegistry)", ErrCodecRequired)
	}
	if msgID == 0 {
		return errors.New("robot protocol: msg id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.decoders[msgID]; !ok {
		codec := r.codec
		r.decoders[msgID] = func(payload []byte) (any, error) {
			value := new(T)
			if err := codec.Unmarshal(payload, value); err != nil {
				return nil, fmt.Errorf("robot protocol: decode msg %d: %w", msgID, err)
			}
			return value, nil
		}
	}
	return nil
}

// RegisterMessage installs both directions for one message type — for
// symmetric messages (notifies, echoes). Request/response pairs sharing one
// msg id must use EnsureEncoder for the request and EnsureDecoder for the
// response instead (action.RegisterCall does exactly that).
func RegisterMessage[T any](r *Registry, msgID uint32) error {
	if err := EnsureEncoder(r, msgID); err != nil {
		return err
	}
	return EnsureDecoder[T](r, msgID)
}

// EncodeTyped adapts a typed encode function into an Encoder (the bridge
// generated codec tables use).
func EncodeTyped[T any](msgID uint32, encode func(T) ([]byte, error)) Encoder {
	return func(value any) ([]byte, error) {
		typed, ok := value.(T)
		if !ok {
			return nil, fmt.Errorf("robot protocol: message type mismatch for msg %d", msgID)
		}
		return encode(typed)
	}
}

// DecodeTyped adapts a typed decode function into a Decoder.
func DecodeTyped[T any](msgID uint32, decode func([]byte) (T, error)) Decoder {
	return func(payload []byte) (any, error) {
		msg, err := decode(payload)
		if err != nil {
			return nil, fmt.Errorf("robot protocol: decode msg %d: %w", msgID, err)
		}
		return msg, nil
	}
}
