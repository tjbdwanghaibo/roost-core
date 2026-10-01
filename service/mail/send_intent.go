package mail

import (
	"bytes"
	"slices"
)

// sameSendIntent reports whether the envelope read back from the store is the
// one the ledger's Intent describes. It compares the complete persisted
// intent, including id and expiry: the recovery path must not deliver an
// envelope substituted under a reserved id.
//
// The comparison is field by field rather than reflect.DeepEqual because the
// Intent side always carries nil slices (clone keeps nil, and JSON omitempty
// round-trips nil as nil) while an EnvelopeStore is free to hand back an
// empty-but-non-nil Recipients or Attachment. Those are the same envelope;
// DeepEqual called them different and the same-request recovery of
// RR-20260929-16 then failed with ErrConflict on every retry (RR-20261001-02).
func sameSendIntent(a, b Envelope) bool {
	return a.ID == b.ID &&
		a.Audience == b.Audience &&
		a.Scope == b.Scope &&
		slices.Equal(a.Recipients, b.Recipients) &&
		a.Subject == b.Subject &&
		a.Body == b.Body &&
		bytes.Equal(a.Attachment, b.Attachment) &&
		a.SendRequestID == b.SendRequestID &&
		a.CreatedAtUnix == b.CreatedAtUnix &&
		a.ExpiresAtUnix == b.ExpiresAtUnix
}
