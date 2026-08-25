package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/cube-core/nest"
)

const StartEffectTopic = "saga.start"
const WireVersion uint16 = 1

type startEffectPayload struct {
	Version uint16       `json:"version"`
	Start   StartRequest `json:"start"`
}

// NewStartEffect creates a stable Nest transactional outbox intent. The
// enclosing handler's entity mutations and this intent share one WAL record.
func NewStartEffect(request StartRequest) (nest.Effect, error) {
	request.Type = strings.TrimSpace(request.Type)
	request.BusinessKey = strings.TrimSpace(request.BusinessKey)
	request.ID = strings.TrimSpace(request.ID)
	if request.Type == "" || len(request.Type) > 128 || request.DefinitionVersion == 0 || request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > 4<<20 || (request.ID != "" && !validSubjectToken(request.ID, 128)) {
		return nest.Effect{}, ErrInvalidRecord
	}
	// Now is an in-process test/recovery hook, not part of a durable business
	// intent. Delivery time determines the initial scheduling timestamp.
	request.Now = time.Time{}
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	payload, err := json.Marshal(startEffectPayload{Version: WireVersion, Start: request})
	if err != nil {
		return nest.Effect{}, err
	}
	// Include the full canonical intent. Exact WAL replay keeps the same ID,
	// while a programming error which reuses a business key with another
	// payload is not hidden by JetStream's MsgID de-duplication.
	digest := sha256.Sum256(payload)
	return nest.Effect{ID: "saga-start:" + hex.EncodeToString(digest[:16]), Topic: StartEffectTopic, Key: request.BusinessKey, Payload: payload, Headers: map[string]string{"saga-type": request.Type}}, nil
}

// EmitStart appends a Saga start intent to the active Nest transaction.
func EmitStart(request StartRequest) error {
	effect, err := NewStartEffect(request)
	if err != nil {
		return err
	}
	return nest.Emit(effect)
}

func DecodeStartEffect(payload []byte) (StartRequest, error) {
	var envelope startEffectPayload
	if len(payload) == 0 {
		return StartRequest{}, ErrInvalidRecord
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return StartRequest{}, err
	}
	if envelope.Version != WireVersion {
		return StartRequest{}, ErrInvalidRecord
	}
	request := envelope.Start
	request.Type = strings.TrimSpace(request.Type)
	request.BusinessKey = strings.TrimSpace(request.BusinessKey)
	request.ID = strings.TrimSpace(request.ID)
	request.Now = time.Time{}
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	if request.Type == "" || len(request.Type) > 128 || request.DefinitionVersion == 0 || request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > 4<<20 || (request.ID != "" && !validSubjectToken(request.ID, 128)) {
		return StartRequest{}, ErrInvalidRecord
	}
	return request, nil
}
