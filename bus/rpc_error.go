package bus

import (
	"fmt"

	"github.com/tjbdwanghaibo/cube-core/errcode"
)

const rpcWireVersion uint8 = 1

type rpcErrorEnvelope struct {
	Code   int32  `json:"code"`
	Reason string `json:"reason"`
}

// rpcResponseEnvelope makes transport success distinct from business success.
// Payload is encoded separately with the configured Bus codec so callers never
// accidentally decode an error object into an unrelated zero-value response.
type rpcResponseEnvelope struct {
	Version uint8             `json:"version"`
	OK      bool              `json:"ok"`
	Payload []byte            `json:"payload,omitempty"`
	Error   *rpcErrorEnvelope `json:"error,omitempty"`
}

func rpcErrorResponse(err error) rpcErrorEnvelope {
	code, reason := errcode.ClientError(err)
	return rpcErrorEnvelope{Code: code, Reason: reason}
}

func encodeRPCSuccess(codec Codec, value any) ([]byte, error) {
	payload, err := codec.Marshal(value)
	if err != nil {
		return nil, err
	}
	return codec.Marshal(rpcResponseEnvelope{Version: rpcWireVersion, OK: true, Payload: payload})
}

func encodeRPCFailure(codec Codec, cause error) ([]byte, error) {
	wireErr := rpcErrorResponse(cause)
	return codec.Marshal(rpcResponseEnvelope{Version: rpcWireVersion, Error: &wireErr})
}

func decodeRPCResponse(codec Codec, data []byte, target any) error {
	var envelope rpcResponseEnvelope
	if err := codec.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("bus: decode rpc response envelope: %w", err)
	}
	if envelope.Version != rpcWireVersion {
		return fmt.Errorf("bus: unsupported rpc response version %d", envelope.Version)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return fmt.Errorf("bus: malformed rpc failure response")
		}
		return errcode.Remote(envelope.Error.Code, envelope.Error.Reason, "remote rpc failed")
	}
	if target == nil {
		return nil
	}
	if len(envelope.Payload) == 0 {
		return fmt.Errorf("bus: successful rpc response has no payload")
	}
	if err := codec.Unmarshal(envelope.Payload, target); err != nil {
		return fmt.Errorf("bus: decode rpc response payload: %w", err)
	}
	return nil
}

func decodeRPCResponseBytes(codec Codec, data []byte) ([]byte, error) {
	var envelope rpcResponseEnvelope
	if err := codec.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("bus: decode rpc response envelope: %w", err)
	}
	if envelope.Version != rpcWireVersion {
		return nil, fmt.Errorf("bus: unsupported rpc response version %d", envelope.Version)
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return nil, fmt.Errorf("bus: malformed rpc failure response")
		}
		return nil, errcode.Remote(envelope.Error.Code, envelope.Error.Reason, "remote rpc failed")
	}
	return envelope.Payload, nil
}
