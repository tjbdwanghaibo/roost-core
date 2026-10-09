package bus

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

const rpcWireVersion uint8 = 1

// ErrRPCCapturedByJetStream reports that a lightweight RPC was answered by a
// JetStream PubAck: the target serves RPC over JetStream, whose request stream
// covers the lightweight RPC subjects, so the request was stored instead of
// reaching a handler. Align nats.rpc.transport on both sides (use
// CallReliable / CallToReliable). A server with RR-20261005-NC-92 refuses such
// a request without running it; an older server may still run it later.
var ErrRPCCapturedByJetStream = errors.New("bus: lightweight rpc was stored by a jetstream stream; the target serves rpc over jetstream")

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

// jetStreamPubAck is what JetStream answers a core request whose subject a
// stream stores. It is always JSON, whatever the Bus codec is.
type jetStreamPubAck struct {
	Stream string `json:"stream"`
	Seq    uint64 `json:"seq"`
}

// jetStreamCapture recognises a PubAck where an RPC response was expected
// (RR-20261005-NC-92); nil when data is something else.
func jetStreamCapture(data []byte) error {
	var ack jetStreamPubAck
	if json.Unmarshal(data, &ack) != nil || ack.Stream == "" {
		return nil
	}
	return fmt.Errorf("%w (stream %s, seq %d)", ErrRPCCapturedByJetStream, ack.Stream, ack.Seq)
}

func decodeRPCResponse(codec Codec, data []byte, target any) error {
	var envelope rpcResponseEnvelope
	if err := codec.Unmarshal(data, &envelope); err != nil {
		if captured := jetStreamCapture(data); captured != nil {
			return captured
		}
		return fmt.Errorf("bus: decode rpc response envelope: %w", err)
	}
	if envelope.Version != rpcWireVersion {
		if captured := jetStreamCapture(data); captured != nil {
			return captured
		}
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
		if captured := jetStreamCapture(data); captured != nil {
			return nil, captured
		}
		return nil, fmt.Errorf("bus: decode rpc response envelope: %w", err)
	}
	if envelope.Version != rpcWireVersion {
		if captured := jetStreamCapture(data); captured != nil {
			return nil, captured
		}
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
