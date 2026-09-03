package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

const StartEffectTopic = "saga.start"
const CompletionEffectTopicPrefix = "saga.result."
const StepReceiptNamespace = "saga-step"
const WireVersion uint16 = 1

type startEffectPayload struct {
	Version uint16       `json:"version"`
	Start   StartRequest `json:"start"`
}

type completionEffectPayload struct {
	Version    uint16     `json:"version"`
	Completion Completion `json:"completion"`
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

// BindCommand makes this command identity part of the active Nest transaction.
// The caller supplies the validated retention deadline; Saga policy, not this
// helper, owns TTL selection.
func BindCommand(command Command, expiresAt time.Time, fences ...dataengine.LeaseFence) error {
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		return nest.ErrTransactionClosed
	}
	if expiresAt.IsZero() || command.Validate() != nil {
		return ErrInvalidRecord
	}
	raw, err := json.Marshal(command)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	for _, fence := range fences {
		receipt, err := dataengine.NewLeaseFenceReceipt(fence)
		if err != nil {
			return err
		}
		if err := tx.AddReceipt(receipt); err != nil {
			return err
		}
	}
	return tx.AddReceipt(dataengine.Receipt{
		Namespace: StepReceiptNamespace, ID: command.ID, Digest: digest[:], ExpiresAt: expiresAt.UnixNano(),
	})
}

func NewCompletionEffect(completion Completion) (nest.Effect, error) {
	if completion.CompletedAt.IsZero() {
		completion.CompletedAt = time.Now().UTC()
	}
	if err := completion.Validate(); err != nil {
		return nest.Effect{}, err
	}
	payload, err := json.Marshal(completionEffectPayload{Version: WireVersion, Completion: completion})
	if err != nil {
		return nest.Effect{}, err
	}
	return nest.Effect{
		ID: "saga-completion:" + completion.CommandID, Topic: CompletionEffectTopicPrefix + completion.SagaID,
		Key: completion.IdempotencyKey, Payload: payload, Headers: map[string]string{"saga-id": completion.SagaID},
	}, nil
}

// EmitCompletion attaches the replayable completion to the bound receipt and
// appends its broker delivery intent to the same Nest CommitRecord.
func EmitCompletion(completion Completion) error {
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		return nest.ErrTransactionClosed
	}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	if err := tx.SetReceiptPayload(StepReceiptNamespace, completion.CommandID, effect.Payload); err != nil {
		return fmt.Errorf("saga: bind completion receipt: %w", err)
	}
	return tx.Emit(effect)
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

func DecodeCompletionEffect(payload []byte) (Completion, error) {
	var envelope completionEffectPayload
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil || envelope.Version != WireVersion {
		return Completion{}, ErrInvalidRecord
	}
	if err := envelope.Completion.Validate(); err != nil {
		return Completion{}, err
	}
	return envelope.Completion, nil
}
