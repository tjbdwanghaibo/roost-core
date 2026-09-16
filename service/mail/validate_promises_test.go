package mail

import (
	"errors"
	"strings"
	"testing"
)

func validDirect() Envelope {
	return Envelope{ID: "m-1", Audience: AudienceDirect, Recipients: []int64{7}, Subject: "hi", Body: "body", CreatedAtUnix: 100, ExpiresAtUnix: 200}
}

// Envelope.Validate is the shape gate for everything the mail service stores
// and fans out. Every rule is pinned by its sentinel and message; a dropped
// rule turns exactly one case red.
func TestEnvelopeValidateRefusesEachBrokenField(t *testing.T) {
	if err := validDirect().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	broadcast := Envelope{ID: "m-2", Audience: AudienceBroadcast, Subject: "all", ExpiresAtUnix: 200}
	if err := broadcast.Validate(); err != nil {
		t.Fatalf("broadcast baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		mutate   func(*Envelope)
		sentinel error
		text     string
	}{
		{"blank id", func(e *Envelope) { e.ID = " " }, ErrMailInvalid, "id is empty"},
		{"direct without recipients", func(e *Envelope) { e.Recipients = nil }, ErrAudienceInvalid, "a direct mail has no recipients"},
		{"direct with too many recipients", func(e *Envelope) {
			e.Recipients = make([]int64, MaxRecipients+1)
			for i := range e.Recipients {
				e.Recipients[i] = int64(i + 1)
			}
		}, ErrAudienceInvalid, "recipients, limit"},
		{"recipient that is not a player", func(e *Envelope) { e.Recipients = []int64{7, 0} }, ErrAudienceInvalid, "recipient 0 is not a player"},
		{"broadcast carrying recipients", func(e *Envelope) { e.Audience = AudienceBroadcast }, ErrAudienceInvalid, "a broadcast mail must not carry a recipient list"},
		{"unknown audience", func(e *Envelope) { e.Audience = Audience("carrier-pigeon") }, ErrAudienceInvalid, `unknown audience "carrier-pigeon"`},
		{"blank subject", func(e *Envelope) { e.Subject = "  " }, ErrBodyInvalid, "subject is empty"},
		{"subject too long", func(e *Envelope) { e.Subject = strings.Repeat("s", MaxSubjectBytes+1) }, ErrBodyInvalid, "subject is"},
		{"body too long", func(e *Envelope) { e.Body = strings.Repeat("b", MaxBodyBytes+1) }, ErrBodyInvalid, "body is"},
		{"attachment too long", func(e *Envelope) { e.Attachment = make([]byte, MaxAttachmentBytes+1) }, ErrBodyInvalid, "attachment is"},
		{"never expires", func(e *Envelope) { e.ExpiresAtUnix = 0 }, ErrMailInvalid, "a mail must expire"},
		{"expires before creation", func(e *Envelope) { e.ExpiresAtUnix = e.CreatedAtUnix }, ErrMailInvalid, "is not after creation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validDirect()
			tc.mutate(&e)
			err := e.Validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}
