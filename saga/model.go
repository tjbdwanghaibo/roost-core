// Package saga provides the storage-independent orchestration state machine
// for durable business operations spanning multiple transaction domains.
package saga

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

type Status uint8

const (
	StatusPending Status = iota + 1
	StatusWaiting
	StatusCompensating
	StatusCompleted
	StatusCompensated
	StatusFailed
	StatusManualRequired
)

func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusCompensated || s == StatusFailed || s == StatusManualRequired
}

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusWaiting:
		return "waiting"
	case StatusCompensating:
		return "compensating"
	case StatusCompleted:
		return "completed"
	case StatusCompensated:
		return "compensated"
	case StatusFailed:
		return "failed"
	case StatusManualRequired:
		return "manual_required"
	default:
		return "invalid"
	}
}

type Phase uint8

const (
	PhaseForward Phase = iota + 1
	PhaseCompensate
)

func (p Phase) String() string {
	if p == PhaseForward {
		return "forward"
	}
	if p == PhaseCompensate {
		return "compensate"
	}
	return "invalid"
}

type Step struct {
	Name            string
	ForwardTopic    string
	CompensateTopic string
	Timeout         time.Duration
	MaxAttempts     uint32
	BackoffMin      time.Duration
	BackoffMax      time.Duration
}

type Definition struct {
	Type    string
	Version uint32
	Steps   []Step
}

func (d Definition) Validate() error {
	if strings.TrimSpace(d.Type) != d.Type || d.Type == "" || len(d.Type) > 128 || d.Version == 0 || len(d.Steps) == 0 || len(d.Steps) > 256 {
		return ErrInvalidDefinition
	}
	seen := make(map[string]struct{}, len(d.Steps))
	for i := range d.Steps {
		s := d.Steps[i]
		if strings.TrimSpace(s.Name) != s.Name || s.Name == "" || len(s.Name) > 128 || !validSubject(s.ForwardTopic) || (s.CompensateTopic != "" && !validSubject(s.CompensateTopic)) || (s.CompensateTopic == "" && !validSubject(s.ForwardTopic+".compensate")) || s.Timeout <= 0 || s.MaxAttempts == 0 || s.MaxAttempts > 1000 || s.BackoffMin <= 0 || s.BackoffMax < s.BackoffMin {
			return fmt.Errorf("%w: step %d", ErrInvalidDefinition, i)
		}
		if _, exists := seen[s.Name]; exists {
			return fmt.Errorf("%w: duplicate step %q", ErrInvalidDefinition, s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

type Lease struct {
	Owner string
	Token uint64
	Until time.Time
}

type Record struct {
	ID                string
	Type              string
	DefinitionVersion uint32
	BusinessKey       string
	Status            Status
	Phase             Phase
	Step              int
	CompletedSteps    int
	Attempt           uint32
	Version           uint64
	Data              []byte
	LastError         string
	OperationKey      string
	CommandID         string
	NextRunAt         time.Time
	DeadlineAt        time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Lease             Lease
}

func (r Record) Clone() Record {
	r.Data = append([]byte(nil), r.Data...)
	return r
}

func (r Record) Validate() error {
	waitingFields := r.OperationKey != "" && len(r.OperationKey) <= 192 && r.CommandID != "" && len(r.CommandID) <= 192 && r.Attempt > 0
	leaseValid := (r.Lease.Owner == "" && r.Lease.Token == 0 && r.Lease.Until.IsZero()) || (strings.TrimSpace(r.Lease.Owner) != "" && len(r.Lease.Owner) <= 256 && r.Lease.Token > 0 && !r.Lease.Until.IsZero())
	if !validSubjectToken(r.ID, 128) || strings.TrimSpace(r.Type) != r.Type || r.Type == "" || len(r.Type) > 128 || r.DefinitionVersion == 0 || strings.TrimSpace(r.BusinessKey) != r.BusinessKey || r.BusinessKey == "" || len(r.BusinessKey) > 512 || len(r.Data) > 4<<20 || len(r.LastError) > 4096 || r.Status < StatusPending || r.Status > StatusManualRequired || r.Phase < PhaseForward || r.Phase > PhaseCompensate || r.Version == 0 || r.Step < 0 || r.CompletedSteps < 0 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || (!r.Status.Terminal() && r.NextRunAt.IsZero()) || (r.Status == StatusWaiting) != waitingFields || (r.Status != StatusWaiting && (r.OperationKey != "" || r.CommandID != "")) || (r.Status == StatusCompensating && (r.Phase != PhaseCompensate || r.CompletedSteps == 0)) || (r.Status == StatusCompensated && r.CompletedSteps != 0) || !leaseValid {
		return ErrInvalidRecord
	}
	return nil
}

type Command struct {
	ID                string    `json:"id"`
	IdempotencyKey    string    `json:"idempotency_key"`
	SagaID            string    `json:"saga_id"`
	SagaType          string    `json:"saga_type"`
	DefinitionVersion uint32    `json:"definition_version"`
	BusinessKey       string    `json:"business_key"`
	Step              int       `json:"step"`
	StepName          string    `json:"step_name"`
	Phase             Phase     `json:"phase"`
	Attempt           uint32    `json:"attempt"`
	Topic             string    `json:"topic"`
	Payload           []byte    `json:"payload,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at"`
	CreatedAt         time.Time `json:"created_at"`
}

func (c Command) Clone() Command {
	c.Payload = append([]byte(nil), c.Payload...)
	return c
}

func (c Command) Validate() error {
	if len(c.ID) > 192 || c.ID == "" || len(c.IdempotencyKey) > 192 || c.IdempotencyKey == "" || !validSubjectToken(c.SagaID, 128) || strings.TrimSpace(c.SagaType) != c.SagaType || len(c.SagaType) > 128 || c.SagaType == "" || c.DefinitionVersion == 0 || strings.TrimSpace(c.BusinessKey) != c.BusinessKey || len(c.BusinessKey) > 512 || c.BusinessKey == "" || c.Step < 0 || strings.TrimSpace(c.StepName) != c.StepName || len(c.StepName) > 128 || c.StepName == "" || c.Phase < PhaseForward || c.Phase > PhaseCompensate || c.Attempt == 0 || !validSubject(c.Topic) || len(c.Payload) > 4<<20 || c.DeadlineAt.IsZero() || c.CreatedAt.IsZero() {
		return ErrInvalidRecord
	}
	return nil
}

type Completion struct {
	CommandID      string    `json:"command_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	SagaID         string    `json:"saga_id"`
	Success        bool      `json:"success"`
	Retryable      bool      `json:"retryable,omitempty"`
	Data           []byte    `json:"data,omitempty"`
	Error          string    `json:"error,omitempty"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
}

func (c Completion) Validate() error {
	invalidOutcome := (c.Success && (c.Retryable || c.Error != "")) || (!c.Success && strings.TrimSpace(c.Error) == "")
	if c.CommandID == "" || len(c.CommandID) > 192 || c.IdempotencyKey == "" || len(c.IdempotencyKey) > 192 || !validSubjectToken(c.SagaID, 128) || len(c.Error) > 4096 || len(c.Data) > 4<<20 || invalidOutcome {
		return ErrInvalidRecord
	}
	return nil
}

func validSubject(subject string) bool {
	if subject == "" || len(subject) > 256 || strings.TrimSpace(subject) != subject {
		return false
	}
	for _, token := range strings.Split(subject, ".") {
		if !validSubjectToken(token, 128) {
			return false
		}
	}
	return true
}

func validSubjectToken(token string, maxLength int) bool {
	if token == "" || len(token) > maxLength || strings.ContainsAny(token, ".*> \t\r\n") {
		return false
	}
	return true
}

type OutboxRecord struct {
	Command       Command
	Attempt       uint32
	NextAttemptAt time.Time
	Lease         Lease
	CreatedAt     time.Time
}

func (o OutboxRecord) Clone() OutboxRecord {
	o.Command = o.Command.Clone()
	return o
}

var idState struct {
	prefix [8]byte
	seq    atomic.Uint64
}

func init() {
	if _, err := rand.Read(idState.prefix[:]); err != nil {
		binary.BigEndian.PutUint64(idState.prefix[:], uint64(time.Now().UnixNano()))
	}
}

func NewID() string {
	var raw [16]byte
	var encoded [32]byte
	copy(raw[:8], idState.prefix[:])
	binary.BigEndian.PutUint64(raw[8:], idState.seq.Add(1))
	hex.Encode(encoded[:], raw[:])
	return string(encoded[:])
}
