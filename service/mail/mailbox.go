package mail

import (
	"fmt"
	"sort"
)

// Mailbox is one player's state for every mail delivered to them: read
// status, claim record, and the unread count.
//
// It is ONE versioned entry on purpose. The implementation this replaces kept
// per-mail state documents and derived the unread count with an aggregation
// over every active envelope, which meant:
//
//   - the count could disagree with the statuses it summarized, because they
//     were written separately; and
//   - one player opening their mailbox ran an unbounded `$lookup` over a
//     collection that holds every server-wide mail for every player.
//
// Here a status change and the count move in the same compare-and-set, so they
// cannot disagree, and reading the whole mailbox is one read.
type Mailbox struct {
	PlayerID int64 `json:"player_id"`
	// Entries is the per-mail state, keyed by envelope id. Bounded by
	// MaxMailboxEntries; see evict.
	Entries map[string]Entry `json:"entries,omitempty"`
	// Unread is maintained here rather than counted on read. It is the
	// authoritative count precisely because it moves with the statuses.
	Unread int32 `json:"unread"`
	// SettledClaims remembers mails whose ENTRY was dropped by retention after
	// they had been claimed. Display retention and claim identity are two
	// different bounds: an entry carries subject, status and timestamps and is
	// capped at MaxMailboxEntries so a mailbox stays readable, while "this mail
	// was already claimed" has to outlive the entry or a duplicate fanout
	// message resurrects the mail and a second token is minted for the same
	// attachment (RR-20260910-02). CommitClaim's own comment already says the
	// token "stays the delivery key for this mail forever"; eviction was
	// deleting it. Capped separately by MaxSettledClaims, oldest first.
	SettledClaims map[string]SettledClaim `json:"settled_claims,omitempty"`
	// Evicted counts entries retention dropped, so a mailbox that is losing
	// history says so instead of just being shorter than the client expects.
	Evicted uint64 `json:"evicted,omitempty"`

	UpdatedAtUnix int64 `json:"updated_at_unix"`
}

// Entry is one mail's state for one player.
type Entry struct {
	MailID string `json:"mail_id"`
	Status Status `json:"status"`

	// ClaimToken is the idempotency key for this mail's attachment delivery.
	//
	// It is minted by the service and is CONSTANT for the life of this entry:
	// a retried claim gets the same token back, never a new one. That is the
	// whole fix. The implementation this replaces let the CLIENT supply the
	// key and allowed a lapsed reservation to be taken over by a different
	// key, so a caller that retried after a lost response was handed the
	// attachment again under a key the delivery side had never seen.
	ClaimToken string `json:"claim_token,omitempty"`
	// ClaimDeadlineUnix is when an in-flight claim may be retried. It bounds
	// how long a crashed deliverer blocks the mail; it does NOT allow a
	// different token to take over, because the token does not change.
	ClaimDeadlineUnix int64 `json:"claim_deadline_unix,omitempty"`
	// ClaimEnvelopeExpiresAtUnix is when the envelope this claim belongs to
	// stops being claimable. It is copied into the settled-claim record when
	// retention drops the entry, which is what lets that record be kept for
	// exactly as long as the mail could still be re-claimed.
	ClaimEnvelopeExpiresAtUnix int64 `json:"claim_envelope_expires_at_unix,omitempty"`
	// ClaimAttempts counts reservations. It is not an idempotency key and is
	// not part of one — it exists so an operator can see a mail whose
	// delivery keeps failing.
	ClaimAttempts int32 `json:"claim_attempts,omitempty"`

	DeliveredAtUnix int64 `json:"delivered_at_unix"`
	UpdatedAtUnix   int64 `json:"updated_at_unix"`
}

// claimable reports whether a reservation may be taken now, and why not when
// it may not.
func (e Entry) claimable(nowUnix int64) error {
	switch {
	case e.Status == StatusClaimed:
		return fmt.Errorf("%w: mail %s", ErrAlreadyClaimed, e.MailID)
	case e.Status == StatusDeleted:
		// Deliberate: deleting a mail does not release its attachment. A
		// player who could delete-then-claim would claim twice.
		return fmt.Errorf("%w: mail %s was deleted", ErrMailMissing, e.MailID)
	case e.ClaimToken != "" && e.ClaimDeadlineUnix > nowUnix:
		return fmt.Errorf("%w: mail %s until %d", ErrClaimHeld, e.MailID, e.ClaimDeadlineUnix)
	default:
		return nil
	}
}

// SettledClaim is what remains of a claimed mail after its entry was evicted:
// the token that was the delivery key, and when the entry went.
type SettledClaim struct {
	Token         string `json:"token"`
	SettledAtUnix int64  `json:"settled_at_unix"`
	// EnvelopeExpiresAtUnix is when the envelope this claim protects stops
	// being claimable. Retention drops the record then and not before: a count
	// bound cannot express a time window, and the mail stays re-claimable for
	// as long as its envelope is valid (RR-20260911-01).
	EnvelopeExpiresAtUnix int64 `json:"envelope_expires_at_unix,omitempty"`
}

func (m *Mailbox) init(playerID int64) {
	m.PlayerID = playerID
	if m.Entries == nil {
		m.Entries = make(map[string]Entry)
	}
	if m.SettledClaims == nil {
		m.SettledClaims = make(map[string]SettledClaim)
	}
}

// settledClaim reports the claim that outlived its entry, if any.
func (m Mailbox) settledClaim(mailID string) (SettledClaim, bool) {
	settled, ok := m.SettledClaims[mailID]
	return settled, ok
}

// entry returns the state for one mail, and whether it was delivered.
func (m Mailbox) entry(mailID string) (Entry, bool) {
	entry, ok := m.Entries[mailID]
	return entry, ok
}

// setStatus moves one entry and keeps Unread in step.
//
// It is the ONLY expression in this package that writes a status, which is
// what makes the count trustworthy: there is no path that changes a status
// without passing through the counter.
func (m *Mailbox) setStatus(mailID string, next Status, nowUnix int64) (Entry, bool) {
	entry, ok := m.Entries[mailID]
	if !ok {
		return Entry{}, false
	}
	if entry.Status == next {
		return entry, false
	}
	if entry.Status == StatusUnread {
		m.Unread--
	}
	if next == StatusUnread {
		m.Unread++
	}
	entry.Status = next
	entry.UpdatedAtUnix = nowUnix
	m.Entries[mailID] = entry
	m.UpdatedAtUnix = nowUnix
	return entry, true
}

// deliver records a mail as delivered to this mailbox, idempotently.
func (m *Mailbox) deliver(mailID string, nowUnix int64) (Entry, bool) {
	if settled, ok := m.settledClaim(mailID); ok {
		// Already delivered and already claimed; the entry is gone only
		// because retention dropped it. Recreating it as unread is what let a
		// second token be minted for the same attachment, so this answers
		// like any other redelivery of a mail already in the mailbox.
		return Entry{
			MailID: mailID, Status: StatusClaimed,
			ClaimToken:      settled.Token,
			DeliveredAtUnix: settled.SettledAtUnix,
			UpdatedAtUnix:   settled.SettledAtUnix,
		}, false
	}
	if existing, ok := m.Entries[mailID]; ok {
		// Idempotent: a redelivered mail does not become unread again and
		// does not increment the count a second time. The transport is
		// at-least-once, so this path is the normal one, not the exception.
		return existing, false
	}
	entry := Entry{
		MailID: mailID, Status: StatusUnread,
		DeliveredAtUnix: nowUnix, UpdatedAtUnix: nowUnix,
	}
	m.Entries[mailID] = entry
	m.Unread++
	m.UpdatedAtUnix = nowUnix
	return entry, true
}

// evict brings the mailbox back within MaxMailboxEntries by dropping the
// oldest entries that are safe to drop, and counts what it dropped.
//
// Safe means terminal: deleted, or claimed. An unread mail is never evicted to
// make room for a newer one — silently discarding mail a player has not seen
// is the failure this bound exists to make visible, not to cause. A mailbox
// that cannot be brought under the limit refuses the delivery instead, which
// is an error the caller can act on rather than a loss nobody observes.
func (m *Mailbox) evict(nowUnix int64) {
	if len(m.Entries) <= MaxMailboxEntries {
		return
	}
	evictable := make([]Entry, 0, len(m.Entries))
	for _, entry := range m.Entries {
		if entry.Status == StatusDeleted || entry.Status == StatusClaimed {
			evictable = append(evictable, entry)
		}
	}
	sort.Slice(evictable, func(i, j int) bool {
		if evictable[i].UpdatedAtUnix == evictable[j].UpdatedAtUnix {
			return evictable[i].MailID < evictable[j].MailID
		}
		return evictable[i].UpdatedAtUnix < evictable[j].UpdatedAtUnix
	})
	for _, entry := range evictable {
		if len(m.Entries) <= MaxMailboxEntries {
			break
		}
		delete(m.Entries, entry.MailID)
		if entry.ClaimToken != "" {
			// Keyed on the token rather than on Status: a claimed mail the
			// player then deleted is evicted as StatusDeleted, and forgetting
			// its claim there would reopen the same hole from the other side.
			if m.SettledClaims == nil {
				m.SettledClaims = make(map[string]SettledClaim)
			}
			m.SettledClaims[entry.MailID] = SettledClaim{
				Token:                 entry.ClaimToken,
				SettledAtUnix:         nowUnix,
				EnvelopeExpiresAtUnix: entry.ClaimEnvelopeExpiresAtUnix,
			}
		}
		m.Evicted++
		m.UpdatedAtUnix = nowUnix
	}
	m.evictSettledClaims(nowUnix)
}

// evictSettledClaims drops the settled claims that have nothing left to
// protect, which is a TIME question, not a count one.
//
// U-0165 bounded these by count and argued that was enough because
// ReserveClaim refuses an expired envelope. That argument does not hold: a
// count bound cannot express a time window, so an envelope with a week left
// lost its claim identity as soon as MaxSettledClaims newer claims were
// settled, and the mail became re-claimable under a fresh token
// (RR-20260911-01). A record is now kept until the envelope it protects can no
// longer be claimed.
//
// Records written before the expiry was recorded carry no window and cannot be
// aged out. Only those are dropped by count, oldest first, and only when the
// mailbox is over the bound — that bounds an upgraded mailbox without ever
// dropping a record that knows what it is protecting.
func (m *Mailbox) evictSettledClaims(nowUnix int64) {
	for mailID, settled := range m.SettledClaims {
		if settled.EnvelopeExpiresAtUnix > 0 && nowUnix >= settled.EnvelopeExpiresAtUnix {
			delete(m.SettledClaims, mailID)
		}
	}
	if len(m.SettledClaims) <= MaxSettledClaims {
		return
	}
	legacy := make([]string, 0, len(m.SettledClaims))
	for mailID, settled := range m.SettledClaims {
		if settled.EnvelopeExpiresAtUnix <= 0 {
			legacy = append(legacy, mailID)
		}
	}
	sort.Slice(legacy, func(i, j int) bool {
		left, right := m.SettledClaims[legacy[i]], m.SettledClaims[legacy[j]]
		if left.SettledAtUnix == right.SettledAtUnix {
			return legacy[i] < legacy[j]
		}
		return left.SettledAtUnix < right.SettledAtUnix
	})
	for _, mailID := range legacy {
		if len(m.SettledClaims) <= MaxSettledClaims {
			break
		}
		delete(m.SettledClaims, mailID)
	}
}

// settledClaimsOverflow reports that retention could not get the settled
// claims under their bound without forgetting an identity that is still
// protecting a claimable envelope. Delivery refuses instead: silently dropping
// one is how the same attachment gets a second token, and an operator has to
// see this the way they see a mailbox that cannot be brought under its bound.
func (m Mailbox) settledClaimsOverflow() bool {
	return len(m.SettledClaims) > MaxSettledClaims
}

// full reports whether the mailbox is at its bound with nothing evictable, so
// a delivery must be refused rather than silently dropped.
func (m Mailbox) full() bool {
	if len(m.Entries) < MaxMailboxEntries {
		return false
	}
	for _, entry := range m.Entries {
		if entry.Status == StatusDeleted || entry.Status == StatusClaimed {
			return false
		}
	}
	return true
}

func (m Mailbox) clone() Mailbox {
	out := m
	if m.Entries != nil {
		out.Entries = make(map[string]Entry, len(m.Entries))
		for id, entry := range m.Entries {
			out.Entries[id] = entry
		}
	}
	// Every mutable collection, not just Entries: `out := m` copies the map
	// HEADER, so a caller mutating the returned snapshot was reaching straight
	// into the stored settled claims and deleting a claim identity without
	// going through Store.Update (RR-20260911-02).
	if m.SettledClaims != nil {
		out.SettledClaims = make(map[string]SettledClaim, len(m.SettledClaims))
		for id, settled := range m.SettledClaims {
			out.SettledClaims[id] = settled
		}
	}
	return out
}
