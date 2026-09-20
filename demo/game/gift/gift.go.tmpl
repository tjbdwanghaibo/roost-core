package gift

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/saga"
)

// Package gift is the game's side of the gift saga: what a gift is, how it
// travels as saga state, and how one gift is named.
//
// A gift moves an item from one player's Bag into another player's mailbox.
// Those are two transaction domains — the Bag is a Nest transaction on the
// sender's Player, the mail is a call into the mail service — so the whole
// thing is a saga (saga/gift_item): step debit takes the item out of the
// Bag, step deliver sends the mail with the item attached, and if deliver
// refuses (the recipient has never entered the game, the mailbox is full)
// the coordinator runs debit's compensation, which puts the item back. The
// steps live in internal/service/<game>/gift_saga.go.

const (
	// Deadline bounds the whole saga. Past it the coordinator stops issuing
	// step commands and compensates what already ran; the demo's steps take
	// well under a second on a developer machine.
	Deadline = 2 * time.Minute
	// MailExpiresIn is how long the gift mail stays claimable, in seconds.
	MailExpiresIn = 7 * 24 * 60 * 60
	// sagaIDSessionChars is how much of the session id goes into the saga
	// id; enough to tell two sessions of one player apart.
	sagaIDSessionChars = 16
)

// NativeStep is what a saga step needs in order to commit its identity and
// its result in the SAME Nest transaction as its business.
//
// This is the difference between the two step shapes the framework offers.
// `SubscribeMongoStep` runs the handler inside a Mongo transaction that
// reserves the command id, then publishes the completion afterwards — so a
// step whose business is a Nest transaction has two commits and a window
// between them: the process can die after the Nest commit and before the
// inbox's, and the redelivery runs the business again.
// `SubscribeDataEngineStep` closes it: the step binds the command's receipt
// and emits its completion INSIDE the Nest transaction, so "this command ran"
// and "what it did" are one durable record. The consumer publishes nothing —
// it waits for that receipt to be projected and then acknowledges.
//
// The price is that the step's business must BE a Nest transaction. The gift
// saga's debit and its compensation are; its deliver step is a mail call, so
// that one stays on the Mongo inbox.
type NativeStep struct {
	Inbox       *saga.DataEngineStepInbox
	Command     saga.Command
	Reservation saga.Reservation
}

// Complete records the command's receipt and its outcome in the open Nest
// transaction. Both have to be in it: the receipt is what makes a redelivery
// a replay instead of a second execution, and the completion is what the
// coordinator waits for. A business refusal commits too — with no mutation,
// just the receipt and a failed completion — because a refusal the
// coordinator never hears is a saga that waits out its deadline.
func (step NativeStep) Complete(success bool, reason string) error {
	if step.Inbox == nil {
		return fmt.Errorf("gift: native step has no inbox")
	}
	if err := step.Inbox.Bind(step.Command, step.Reservation); err != nil {
		return err
	}
	completion := saga.Completion{
		CommandID:      step.Command.ID,
		IdempotencyKey: step.Command.IdempotencyKey,
		SagaID:         step.Command.SagaID,
		Success:        success,
	}
	if !success {
		completion.Error = reason
		if completion.Error == "" {
			completion.Error = "gift: step refused"
		}
	}
	return saga.EmitCompletion(completion)
}

// State is the saga's Data: written once at start, read by every step and
// compensation. It is JSON because the saga runtime stores and ships it as
// bytes and an operator reads it from the saga record.
type State struct {
	From   int64 `json:"from"`
	To     int64 `json:"to"`
	ItemID int64 `json:"item_id"`
	Count  int32 `json:"count"`
}

// Validate is what every step trusts about the state it decodes.
func (state State) Validate() error {
	if state.From <= 0 || state.To <= 0 || state.ItemID <= 0 || state.Count <= 0 {
		return fmt.Errorf("gift: state %+v is not a gift", state)
	}
	return nil
}

// Encode renders the state for StartRequest.Data.
func Encode(state State) ([]byte, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(state)
}

// Decode reads a step command's payload back into a State.
func Decode(raw []byte) (State, error) {
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, fmt.Errorf("gift: decode state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

// SagaID names one gift: the sender, the session and the frame sequence. A
// client retrying the same frame therefore starts the same saga (StartSaga
// is idempotent for one id with the same data), and a new frame after a
// gift starts a new one. It doubles as the business key. The saga runtime
// wants a subject token — no '.', '*', '>' or whitespace — so the session
// id is reduced to its token-safe prefix.
func SagaID(from int64, sessionID string, seq uint32) string {
	var token strings.Builder
	for _, r := range sessionID {
		if token.Len() >= sagaIDSessionChars {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			token.WriteRune(r)
		default:
			token.WriteByte('-')
		}
	}
	return fmt.Sprintf("gift-%d-%s-%d", from, token.String(), seq)
}

// Subject and Body are the gift mail's text.
func Subject(state State) string { return fmt.Sprintf("A gift from player %d", state.From) }

func Body(state State) string {
	return fmt.Sprintf("Player %d sent you %d x item %d. Claim the attachment to take it.", state.From, state.Count, state.ItemID)
}
