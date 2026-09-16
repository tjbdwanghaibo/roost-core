package mail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/servicemetrics"
)

// Config wires a Service.
type Config struct {
	// Envelopes, Mailboxes and Sends hold the durable state.
	Envelopes EnvelopeStore
	Mailboxes MailboxStore
	Sends     SendLedger

	// Broadcast fans broadcast envelopes out. Required only if the caller
	// sends broadcast mail; a nil Broadcast refuses AudienceBroadcast at send
	// time rather than accepting it and delivering to nobody.
	Broadcast Deliverer

	// ClaimLease is how long a reservation blocks a retry. Zero selects
	// DefaultClaimLease; it must be positive, because a zero lease would let
	// two deliverers hold the same mail at once.
	//
	// It bounds how long a crashed deliverer blocks one mail. It is NOT what
	// makes claiming safe — the constant claim token is. A lease that is too
	// short costs a duplicate DELIVERY ATTEMPT under the same key, which the
	// delivery side dedupes; in the implementation this replaces a lapsed
	// lease could be taken over under a DIFFERENT key, and that cost a
	// duplicate grant.
	ClaimLease time.Duration

	// NewMailID mints envelope ids; nil means a 128-bit random id. They must
	// not be guessable or sequential: the implementation this replaces used a
	// single-document counter, which made every send an extra round trip
	// serialized on one document, and made ids enumerable.
	NewMailID func() (string, error)
	// NewClaimToken mints claim tokens; nil means a 128-bit random token.
	// They must be unguessable: the token authorizes a commit.
	NewClaimToken func() (string, error)

	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	Metrics servicemetrics.Reporter
}

// DefaultClaimLease is how long a reservation blocks a retry.
const DefaultClaimLease = 30 * time.Second

// Service is the mailbox service.
type Service struct {
	cfg    Config
	report servicemetrics.Sink
}

// New validates the configuration and returns a Service.
func New(cfg Config) (*Service, error) {
	missing := []string{}
	if cfg.Envelopes == nil {
		missing = append(missing, "envelope store")
	}
	if cfg.Mailboxes == nil {
		missing = append(missing, "mailbox store")
	}
	if cfg.Sends == nil {
		missing = append(missing, "send ledger")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("mail: incomplete configuration: %s", strings.Join(missing, ", "))
	}
	if cfg.ClaimLease < 0 {
		return nil, fmt.Errorf("mail: claim lease must not be negative")
	}
	if cfg.ClaimLease == 0 {
		cfg.ClaimLease = DefaultClaimLease
	}
	if cfg.NewMailID == nil {
		cfg.NewMailID = randomID
	}
	if cfg.NewClaimToken == nil {
		cfg.NewClaimToken = randomID
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mail: mint id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SendRequest describes one mail to send.
//
// It carries no sender identity field. Who is allowed to send mail is decided
// by the transport that reaches this service — a mail service is an operator
// and system-event surface, not a player-facing write — and a field here
// would be a field a client could set.
type SendRequest struct {
	Audience   Audience
	Scope      string
	Recipients []int64

	Subject    string
	Body       string
	Attachment []byte

	// ExpiresInSeconds is how long the mail stays readable. Required and
	// positive.
	//
	// Seconds rather than a time.Duration, and the unit is in the name. A
	// Duration is the better type inside a process, but this request crosses a
	// bus: it marshals as an integer nanosecond count, which is unreadable in
	// a packet capture and unproducible by a non-Go caller. The type now says
	// what the unit is, so no layer between here and the wire has to remember
	// a conversion — an earlier version did that conversion inside the client,
	// where nothing in the type system mentioned it.
	ExpiresInSeconds int64
	// RequestID is the send idempotency key. Required: the transports this
	// service is reached over are at-least-once, and a send without a key is
	// a duplicate mail per redelivery.
	RequestID string
}

// Send stores an envelope and, for a broadcast, hands it to the Deliverer.
//
// It is idempotent per RequestID: a retried send returns the envelope it
// already produced. The implementation this replaces had a send ledger keyed
// on a request id too, but its claim path did not, which is the asymmetry
// this package removes.
func (s *Service) Send(ctx context.Context, req SendRequest) (Envelope, error) {
	if strings.TrimSpace(req.RequestID) == "" {
		return Envelope{}, fmt.Errorf("%w: a send idempotency key is required", ErrRequestInvalid)
	}
	if req.ExpiresInSeconds <= 0 {
		return Envelope{}, fmt.Errorf("%w: expires_in_seconds must be positive", ErrMailInvalid)
	}
	if req.Audience == AudienceBroadcast && s.cfg.Broadcast == nil {
		// Honest refusal beats accepting a broadcast and delivering it to
		// nobody. A service that answers OK for mail no player will ever see
		// is the silent-drop pattern with extra steps.
		s.report.Refused("send", "no_deliverer")
		return Envelope{}, fmt.Errorf("%w: no broadcast deliverer is configured", ErrAudienceInvalid)
	}

	// The ledger is consulted first, so a replay never reaches the id minter
	// or the envelope store.
	if existing, found, err := s.cfg.Sends.Get(ctx, req.RequestID); err != nil {
		return Envelope{}, err
	} else if found {
		return s.replaySend(ctx, existing.Value)
	}

	now := s.cfg.Now()
	id, err := s.cfg.NewMailID()
	if err != nil {
		return Envelope{}, err
	}
	envelope := Envelope{
		ID: id, Audience: req.Audience, Scope: req.Scope,
		Recipients:    append([]int64(nil), req.Recipients...),
		Subject:       strings.TrimSpace(req.Subject),
		Body:          req.Body,
		Attachment:    append([]byte(nil), req.Attachment...),
		SendRequestID: req.RequestID,
		CreatedAtUnix: now.Unix(),
		ExpiresAtUnix: now.Add(time.Duration(req.ExpiresInSeconds) * time.Second).Unix(),
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}

	// The ledger claim comes first and is insert-only, so two concurrent
	// sends with one request id cannot both proceed to create an envelope.
	// The implementation this replaces checked a ledger and then wrote,
	// which two racers both pass.
	_, claimed, err := s.cfg.Sends.Create(ctx, req.RequestID, SentRecord{
		RequestID: req.RequestID, MailID: id, CreatedAtUnix: now.Unix(),
	})
	if err != nil {
		return Envelope{}, err
	}
	if !claimed {
		// A racer won. Read back what it produced rather than creating a
		// second envelope, and finish its delivery if it has not yet — the
		// step is idempotent per mailbox, so two racers delivering is safe.
		existing, found, err := s.cfg.Sends.Get(ctx, req.RequestID)
		if err != nil {
			return Envelope{}, err
		}
		if !found {
			return Envelope{}, fmt.Errorf("%w: send %q vanished during create", ErrConflict, req.RequestID)
		}
		return s.replaySend(ctx, existing.Value)
	}

	created, err := s.cfg.Envelopes.Create(ctx, envelope)
	if err != nil {
		return Envelope{}, err
	}
	if !created {
		s.report.Conflict("send")
		return Envelope{}, fmt.Errorf("%w: mail id %s is already in use", ErrConflict, id)
	}

	if err := s.deliverAndRecord(ctx, req.RequestID, envelope); err != nil {
		// The envelope exists and the ledger names it, so a retry is a replay
		// — and replaySend re-attempts the delivery, because the ledger entry
		// carries no delivery stamp. Reporting the error lets the caller drive
		// that; swallowing it is how a mail nobody receives looks like a
		// successful send.
		return envelope, err
	}
	s.report.Accepted("send")
	return envelope, nil
}

// deliverAndRecord runs an envelope's delivery and, once it has succeeded,
// stamps the ledger so a replay knows there is nothing left to do.
//
// Direct delivery surfaces the mailbox store's own error — a full mailbox is
// the recipient's condition and the caller must see it as such; a broadcast
// deliverer's failure is wrapped as ErrConflict. In both cases the envelope
// and the ledger stay: a ledger entry with no delivery stamp is exactly what
// tells the replay to try again.
func (s *Service) deliverAndRecord(ctx context.Context, requestID string, envelope Envelope) error {
	if envelope.Audience == AudienceDirect {
		if err := s.deliverDirect(ctx, envelope); err != nil {
			s.report.Refused("send", "delivery_failed")
			return err
		}
	} else {
		if s.cfg.Broadcast == nil {
			s.report.Refused("send", "no_deliverer")
			return fmt.Errorf("%w: no broadcast deliverer is configured", ErrAudienceInvalid)
		}
		if err := s.cfg.Broadcast.Deliver(ctx, envelope); err != nil {
			s.report.Refused("send", "delivery_failed")
			return fmt.Errorf("%w: %s", ErrConflict, err)
		}
	}
	nowUnix := s.cfg.Now().Unix()
	_, _, err := s.cfg.Sends.Update(ctx, requestID, func(current SentRecord, found bool) (SentRecord, bool, error) {
		if !found || current.DeliveredAtUnix != 0 {
			return current, false, nil
		}
		current.DeliveredAtUnix = nowUnix
		return current, true, nil
	})
	if err != nil {
		// The mail is delivered; only the stamp is missing. That costs one
		// redundant, idempotent delivery attempt on the next replay, so it is
		// counted rather than turned into a failed send.
		s.report.Dropped("send.delivery_stamp_failed", 1)
	}
	return nil
}

// replaySend answers a send whose request id the ledger already holds. It
// returns the envelope the first attempt produced — and if that attempt never
// finished delivering, it delivers now. Send's contract is "idempotent per
// RequestID", and idempotent has to mean the same OUTCOME, not the same
// envelope with delivery left to whoever tried first. Before this, a broadcast
// whose fanout failed once was recorded as sent forever and every retry
// answered success.
func (s *Service) replaySend(ctx context.Context, record SentRecord) (Envelope, error) {
	envelope, ok, err := s.cfg.Envelopes.Get(ctx, record.MailID)
	if err != nil {
		return Envelope{}, err
	}
	if !ok {
		// The ledger names a mail that is not there. Answering "sent"
		// would be a lie and answering "not sent" would risk a second
		// send; report the inconsistency instead.
		return Envelope{}, fmt.Errorf("%w: send %q names missing mail %s",
			ErrConflict, record.RequestID, record.MailID)
	}
	if record.DeliveredAtUnix == 0 {
		if err := s.deliverAndRecord(ctx, record.RequestID, envelope); err != nil {
			return envelope, err
		}
	}
	s.report.Replayed("send")
	return envelope, nil
}

// deliverDirect writes one envelope into each named mailbox.
//
// Bounded by MaxRecipients, checked at validation, so this loop has a
// compile-time-visible ceiling rather than being "however many the caller
// asked for".
func (s *Service) deliverDirect(ctx context.Context, envelope Envelope) error {
	nowUnix := s.cfg.Now().Unix()
	for _, playerID := range envelope.Recipients {
		if err := s.Deliver(ctx, playerID, envelope.ID, nowUnix); err != nil {
			return err
		}
	}
	return nil
}

// Deliver records one envelope in one mailbox, idempotently.
//
// It is exported so a Deliverer implementation can use it: whatever fanout
// strategy a caller picks, this is the step that has to be idempotent, and
// reimplementing it would be reimplementing the unread counter.
func (s *Service) Deliver(ctx context.Context, playerID int64, mailID string, nowUnix int64) error {
	if playerID <= 0 {
		return fmt.Errorf("%w: player id must be positive", ErrRequestInvalid)
	}
	if strings.TrimSpace(mailID) == "" {
		return fmt.Errorf("%w: mail id is empty", ErrMailInvalid)
	}
	if nowUnix <= 0 {
		nowUnix = s.cfg.Now().Unix()
	}
	var (
		delivered      bool
		refused        bool
		refusedSettled bool
	)
	_, _, err := s.cfg.Mailboxes.Update(ctx, playerID, func(current Mailbox, _ bool) (Mailbox, bool, error) {
		// Work on a private copy. The store hands the callback a struct
		// copy whose maps still alias the stored value, so mutating them
		// and then returning save=false left the store changed anyway —
		// a refused delivery had already inserted its entry and its
		// tombstone while Unread and Version kept their old values
		// (RR-20260911-05). match's callbacks have always cloned first.
		current = current.clone()
		current.init(playerID)
		delivered, refused, refusedSettled = false, false, false
		_, settled := current.settledClaim(mailID)
		if _, exists := current.entry(mailID); !exists && !settled && current.full() {
			// Refused rather than dropped. A mailbox at its bound with
			// nothing evictable is a situation an operator has to see; a
			// silent discard is the loss this bound exists to prevent.
			refused = true
			return current, false, fmt.Errorf("%w: player %d holds %d unread mails",
				ErrMailboxFull, playerID, len(current.Entries))
		}
		_, added := current.deliver(mailID, nowUnix)
		delivered = added
		// Eviction runs AFTER the insert, so the bound holds on what is
		// stored rather than on what was stored a moment ago. Evicting first
		// leaves the mailbox one over the limit on every delivery — which is
		// what it did until a test counted the entries instead of trusting
		// the comment.
		if added {
			current.evict(nowUnix)
		}
		if current.settledClaimsOverflow() {
			refusedSettled = true
			return current, false, fmt.Errorf("%w: player %d holds %d settled claims whose mails are still claimable",
				ErrClaimHistoryFull, playerID, len(current.SettledClaims))
		}
		return current, added, nil
	})
	if err != nil {
		switch {
		case refused:
			s.report.Refused("deliver", "mailbox_full")
		case refusedSettled:
			s.report.Refused("deliver", "claim_history_full")
		}
		return err
	}
	if delivered {
		s.report.Accepted("deliver")
	} else {
		s.report.Replayed("deliver")
	}
	return nil
}

// Page is one page of a player's mailbox.
type Page struct {
	Items []Item
	// NextCursor continues the listing; empty means the end.
	NextCursor string
	// Unread is the count from the mailbox, not a count of this page.
	Unread int32
	// Evicted is how many entries retention has dropped from this mailbox in
	// total. A client that sees it rise knows it lost history, rather than
	// silently getting a shorter list than it expected.
	Evicted uint64
}

// Item is one mail plus this player's state for it.
type Item struct {
	Envelope Envelope
	Status   Status
	// Claimable reports whether Reserve would succeed right now. It saves the
	// client guessing from Status, which cannot express "held by an in-flight
	// delivery".
	Claimable bool
}

// List returns one page of a player's mailbox, newest first.
//
// The work is bounded, not just the output. This is exactly two reads — the
// mailbox, then one batched envelope fetch for at most `limit` ids — where the
// implementation this replaces clamped how many items it returned and then
// looped issuing one read per envelope until the page filled, with no bound on
// the iterations. A player with many deleted mails turned one packet into an
// unbounded number of database calls.
func (s *Service) List(ctx context.Context, playerID int64, cursor string, limit int) (Page, error) {
	if playerID <= 0 {
		return Page{}, fmt.Errorf("%w: player id must be positive", ErrRequestInvalid)
	}
	if limit < 0 {
		return Page{}, fmt.Errorf("%w: limit must not be negative, got %d", ErrRangeInvalid, limit)
	}
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	stored, found, err := s.cfg.Mailboxes.Get(ctx, playerID)
	if err != nil {
		return Page{}, err
	}
	if !found {
		return Page{}, nil
	}
	mailbox := stored.Value

	// Order is decided here, from delivery time and then id, so it does not
	// depend on map iteration and does not depend on a wall clock read at
	// render time. The implementation this replaces sorted by the publishing
	// process's clock, so history order changed with clock skew between
	// replicas.
	entries := make([]Entry, 0, len(mailbox.Entries))
	for _, entry := range mailbox.Entries {
		if entry.Status == StatusDeleted {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].DeliveredAtUnix == entries[j].DeliveredAtUnix {
			return entries[i].MailID > entries[j].MailID
		}
		return entries[i].DeliveredAtUnix > entries[j].DeliveredAtUnix
	})

	start := 0
	if cursor != "" {
		start = resumeIndex(entries, cursor)
	}
	if start >= len(entries) {
		return Page{Unread: mailbox.Unread, Evicted: mailbox.Evicted}, nil
	}
	end := start + limit
	if end > len(entries) {
		end = len(entries)
	}
	window := entries[start:end]

	ids := make([]string, 0, len(window))
	for _, entry := range window {
		ids = append(ids, entry.MailID)
	}
	envelopes, err := s.cfg.Envelopes.GetMany(ctx, ids)
	if err != nil {
		return Page{}, err
	}

	nowUnix := s.cfg.Now().Unix()
	page := Page{Items: make([]Item, 0, len(window)), Unread: mailbox.Unread, Evicted: mailbox.Evicted}
	for _, entry := range window {
		envelope, ok := envelopes[entry.MailID]
		if !ok {
			// The mailbox names a mail the envelope store does not have —
			// the envelope expired out from under it. Skipping it silently is
			// what made the implementation this replaces return short pages
			// while reporting a full count, so it is counted.
			s.report.Dropped("list.missing_envelope", 1)
			continue
		}
		if envelope.Expired(nowUnix) {
			continue
		}
		page.Items = append(page.Items, Item{
			Envelope:  envelope,
			Status:    entry.Status,
			Claimable: envelope.HasAttachment() && entry.claimable(nowUnix) == nil,
		})
	}
	if end < len(entries) {
		page.NextCursor = encodeCursor(window[len(window)-1])
	}
	s.report.Accepted("list")
	return page, nil
}

// encodeCursor renders a page boundary as the POSITION of its last entry in
// the listing order — delivery time and id — rather than as the id alone. An
// id-only cursor stops working the moment that mail is deleted: the next page
// cannot find it, answers empty with no NextCursor, and the client believes it
// has seen the whole mailbox. Encoding the position lets the next page resume
// from "everything after here" whether or not that mail still exists.
func encodeCursor(entry Entry) string {
	return strconv.FormatInt(entry.DeliveredAtUnix, 10) + "|" + entry.MailID
}

// resumeIndex returns the index of the first entry strictly after cursor in
// the listing order (delivery time descending, then id descending). entries
// must already be in that order.
//
// A cursor without a position — the id-only form issued before this encoding
// existed — resumes after that id when it is present. If that mail is gone the
// listing ends, which is the behaviour it always had; every page issued now
// carries a position, so this concerns only cursors handed out before the
// upgrade and still in a client's hand.
func resumeIndex(entries []Entry, cursor string) int {
	if rawAt, mailID, ok := strings.Cut(cursor, "|"); ok {
		if deliveredAt, err := strconv.ParseInt(rawAt, 10, 64); err == nil {
			return sort.Search(len(entries), func(i int) bool {
				if entries[i].DeliveredAtUnix != deliveredAt {
					return entries[i].DeliveredAtUnix < deliveredAt
				}
				return entries[i].MailID < mailID
			})
		}
	}
	for index, entry := range entries {
		if entry.MailID == cursor {
			return index + 1
		}
	}
	return len(entries)
}

// MarkRead moves one mail to read. Idempotent.
func (s *Service) MarkRead(ctx context.Context, playerID int64, mailID string) (Entry, error) {
	return s.transition(ctx, playerID, mailID, "mark_read", func(entry Entry) (Status, error) {
		if entry.Status == StatusUnread {
			return StatusRead, nil
		}
		// Already read, claimed or deleted: leave it where it is. Moving a
		// claimed mail back to read would lose the claim record.
		return entry.Status, nil
	})
}

// Delete moves one mail to deleted.
//
// It does NOT release the mail's claim record: a player who deleted a claimed
// mail must not be able to claim it again, and a player who deletes a mail
// with an unclaimed attachment gives it up. Both are deliberate, and both are
// only safe because Status and the claim token live in the same entry.
func (s *Service) Delete(ctx context.Context, playerID int64, mailID string) (Entry, error) {
	return s.transition(ctx, playerID, mailID, "delete", func(Entry) (Status, error) {
		return StatusDeleted, nil
	})
}

// transition is the single path through which a status changes, so every one
// of them goes through the unread counter.
func (s *Service) transition(
	ctx context.Context,
	playerID int64,
	mailID string,
	op string,
	next func(Entry) (Status, error),
) (Entry, error) {
	if playerID <= 0 {
		return Entry{}, fmt.Errorf("%w: player id must be positive", ErrRequestInvalid)
	}
	if strings.TrimSpace(mailID) == "" {
		return Entry{}, fmt.Errorf("%w: mail id is empty", ErrMailInvalid)
	}
	nowUnix := s.cfg.Now().Unix()
	var (
		result  Entry
		changed bool
	)
	_, _, err := s.cfg.Mailboxes.Update(ctx, playerID, func(current Mailbox, found bool) (Mailbox, bool, error) {
		// Work on a private copy. The store hands the callback a struct
		// copy whose maps still alias the stored value, so mutating them
		// and then returning save=false left the store changed anyway —
		// a refused delivery had already inserted its entry and its
		// tombstone while Unread and Version kept their old values
		// (RR-20260911-05). match's callbacks have always cloned first.
		current = current.clone()
		changed = false
		if !found {
			return current, false, fmt.Errorf("%w: player %d has no mailbox", ErrMailMissing, playerID)
		}
		current.init(playerID)
		entry, ok := current.entry(mailID)
		if !ok {
			return current, false, fmt.Errorf("%w: mail %s was not delivered to player %d",
				ErrMailMissing, mailID, playerID)
		}
		target, err := next(entry)
		if err != nil {
			return current, false, err
		}
		updated, moved := current.setStatus(mailID, target, nowUnix)
		result, changed = updated, moved
		if !moved {
			result = entry
		}
		return current, moved, nil
	})
	if err != nil {
		return Entry{}, err
	}
	if changed {
		s.report.Accepted(op)
	} else {
		s.report.Replayed(op)
	}
	return result, nil
}

// Claim is a reserved attachment: the payload plus the key its delivery must
// be deduped under.
type Claim struct {
	MailID string
	// Token is the delivery idempotency key. It is CONSTANT for this
	// (player, mail) pair across every reservation, which is the entire
	// reason this type exists: a caller that retries — with a new connection,
	// after a crash, an hour later — presents the same key, so a delivery
	// side that dedupes on it cannot grant twice.
	Token string
	// Attachment is the opaque payload to deliver.
	Attachment []byte
	// Attempts is which reservation this is. It is NOT part of the key; it is
	// there so a mail whose delivery keeps failing is visible.
	Attempts int32
	// DeadlineUnix is when another reservation may be taken.
	DeadlineUnix int64
}

// ReserveClaim takes the delivery reservation for one mail's attachment and
// returns the payload with the key to deliver it under.
//
// The order matters and is the fix. The token is minted once per (player,
// mail) and stored in the same entry as the status, so:
//
//   - a retry after a lost commit gets the SAME token back, and the delivery
//     side's dedupe on that key holds; and
//   - a lapsed reservation can be re-taken, but only under that same token,
//     so re-taking costs a duplicate ATTEMPT, never a duplicate grant.
//
// In the implementation this replaces the key came from the client and a
// lapsed reservation could be taken over under a different one, so a caller
// that retried with a fresh id after the lease elapsed was handed the
// attachment again under a key the delivery side had never seen.
func (s *Service) ReserveClaim(ctx context.Context, playerID int64, mailID string, scope string) (Claim, error) {
	if playerID <= 0 {
		return Claim{}, fmt.Errorf("%w: player id must be positive", ErrRequestInvalid)
	}
	envelope, ok, err := s.cfg.Envelopes.Get(ctx, mailID)
	if err != nil {
		return Claim{}, err
	}
	if !ok {
		return Claim{}, fmt.Errorf("%w: %s", ErrMailMissing, mailID)
	}
	// Authority: the envelope has to address this caller. The caller identity
	// is an argument, not a request field.
	if !envelope.Addresses(playerID, scope) {
		s.report.Refused("reserve_claim", "not_recipient")
		return Claim{}, fmt.Errorf("%w: mail %s does not address player %d", ErrNotRecipient, mailID, playerID)
	}
	nowUnix := s.cfg.Now().Unix()
	if envelope.Expired(nowUnix) {
		s.report.Refused("reserve_claim", "expired")
		return Claim{}, fmt.Errorf("%w: mail %s", ErrExpired, mailID)
	}
	if !envelope.HasAttachment() {
		s.report.Refused("reserve_claim", "no_attachment")
		return Claim{}, fmt.Errorf("%w: mail %s", ErrNoAttachment, mailID)
	}

	// The token is minted only once the reservation is known to be takeable,
	// and only once across compare-and-set retries, because Update may call
	// the callback more than once.
	token := ""
	mintToken := func() (string, error) {
		if token != "" {
			return token, nil
		}
		minted, err := s.cfg.NewClaimToken()
		if err != nil {
			return "", err
		}
		token = minted
		return minted, nil
	}

	var (
		result  Claim
		refusal string
	)
	_, _, err = s.cfg.Mailboxes.Update(ctx, playerID, func(current Mailbox, found bool) (Mailbox, bool, error) {
		// Work on a private copy. The store hands the callback a struct
		// copy whose maps still alias the stored value, so mutating them
		// and then returning save=false left the store changed anyway —
		// a refused delivery had already inserted its entry and its
		// tombstone while Unread and Version kept their old values
		// (RR-20260911-05). match's callbacks have always cloned first.
		current = current.clone()
		refusal = ""
		if !found {
			return current, false, fmt.Errorf("%w: player %d has no mailbox", ErrMailMissing, playerID)
		}
		current.init(playerID)
		entry, ok := current.entry(mailID)
		if !ok {
			if _, settled := current.settledClaim(mailID); settled {
				// The entry is gone because retention dropped it, but the
				// claim it recorded is not: minting a second token here is
				// exactly what let the same attachment be handed out twice
				// (RR-20260910-02).
				refusal = "already_claimed"
				return current, false, fmt.Errorf("%w: mail %s", ErrAlreadyClaimed, mailID)
			}
			return current, false, fmt.Errorf("%w: mail %s was not delivered to player %d",
				ErrMailMissing, mailID, playerID)
		}
		if err := entry.claimable(nowUnix); err != nil {
			switch {
			case errors.Is(err, ErrAlreadyClaimed):
				refusal = "already_claimed"
			case errors.Is(err, ErrClaimHeld):
				refusal = "held"
			default:
				refusal = "unavailable"
			}
			return current, false, err
		}
		// Reuse the stored token when there is one. This is the invariant the
		// whole design rests on, so it is a reuse and not a re-mint.
		reserved := entry.ClaimToken
		if reserved == "" {
			minted, err := mintToken()
			if err != nil {
				return current, false, err
			}
			reserved = minted
		}
		entry.ClaimToken = reserved
		// Remember how long this mail stays claimable, so the settled-claim
		// record that outlives the entry can be kept for exactly that long
		// (RR-20260911-01).
		entry.ClaimEnvelopeExpiresAtUnix = envelope.ExpiresAtUnix
		entry.ClaimDeadlineUnix = nowUnix + int64(s.cfg.ClaimLease.Seconds())
		entry.ClaimAttempts++
		entry.UpdatedAtUnix = nowUnix
		current.Entries[mailID] = entry
		current.UpdatedAtUnix = nowUnix
		result = Claim{
			MailID: mailID, Token: reserved,
			Attachment:   append([]byte(nil), envelope.Attachment...),
			Attempts:     entry.ClaimAttempts,
			DeadlineUnix: entry.ClaimDeadlineUnix,
		}
		return current, true, nil
	})
	if err != nil {
		if refusal != "" {
			s.report.Refused("reserve_claim", refusal)
		}
		return Claim{}, err
	}
	if result.Attempts > 1 {
		// A re-reservation under the same token. Counted separately from a
		// first attempt so a mail whose delivery keeps failing is visible.
		s.report.Replayed("reserve_claim")
	} else {
		s.report.Accepted("reserve_claim")
	}
	return result, nil
}

// CommitClaim marks an attachment claimed. The token is required and must be
// the one this mailbox holds.
//
// Idempotent: a retried commit on an already-claimed mail succeeds if the
// token matches, so a client retry after a lost response does not fail. A
// commit with the WRONG token is refused rather than accepted, which is the
// check that makes the token worth carrying.
func (s *Service) CommitClaim(ctx context.Context, playerID int64, mailID string, token string) (Entry, error) {
	if strings.TrimSpace(token) == "" {
		return Entry{}, fmt.Errorf("%w: a claim token is required", ErrRequestInvalid)
	}
	nowUnix := s.cfg.Now().Unix()
	var (
		result   Entry
		replayed bool
		refusal  string
	)
	_, _, err := s.cfg.Mailboxes.Update(ctx, playerID, func(current Mailbox, found bool) (Mailbox, bool, error) {
		// Work on a private copy. The store hands the callback a struct
		// copy whose maps still alias the stored value, so mutating them
		// and then returning save=false left the store changed anyway —
		// a refused delivery had already inserted its entry and its
		// tombstone while Unread and Version kept their old values
		// (RR-20260911-05). match's callbacks have always cloned first.
		current = current.clone()
		replayed, refusal = false, ""
		if !found {
			return current, false, fmt.Errorf("%w: player %d has no mailbox", ErrMailMissing, playerID)
		}
		current.init(playerID)
		entry, ok := current.entry(mailID)
		if !ok {
			if settled, ok := current.settledClaim(mailID); ok {
				// A late retry of the commit that already succeeded. The
				// tombstone kept the token, so this is still answerable as
				// the replay it is instead of "never delivered".
				if settled.Token != token {
					refusal = "token_mismatch"
					return current, false, fmt.Errorf("%w: mail %s", ErrClaimTokenWrong, mailID)
				}
				result = Entry{
					MailID: mailID, Status: StatusClaimed,
					ClaimToken:      settled.Token,
					DeliveredAtUnix: settled.SettledAtUnix,
					UpdatedAtUnix:   settled.SettledAtUnix,
				}
				replayed = true
				return current, false, nil
			}
			return current, false, fmt.Errorf("%w: mail %s was not delivered to player %d",
				ErrMailMissing, mailID, playerID)
		}
		if entry.ClaimToken != token {
			refusal = "token_mismatch"
			// Deliberately says nothing about the expected token.
			return current, false, fmt.Errorf("%w: mail %s", ErrClaimTokenWrong, mailID)
		}
		if entry.Status == StatusClaimed {
			result, replayed = entry, true
			return current, false, nil
		}
		if entry.Status == StatusDeleted {
			refusal = "deleted"
			return current, false, fmt.Errorf("%w: mail %s was deleted", ErrMailMissing, mailID)
		}
		updated, _ := current.setStatus(mailID, StatusClaimed, nowUnix)
		// The deadline is cleared but the token is kept: it stays the
		// delivery key for this mail forever, so a late duplicate delivery
		// attempt still dedupes.
		updated.ClaimDeadlineUnix = 0
		current.Entries[mailID] = updated
		result = updated
		return current, true, nil
	})
	if err != nil {
		if refusal != "" {
			s.report.Refused("commit_claim", refusal)
		}
		return Entry{}, err
	}
	if replayed {
		s.report.Replayed("commit_claim")
	} else {
		s.report.Accepted("commit_claim")
	}
	return result, nil
}

// CancelClaim releases an in-flight reservation so it can be retried sooner
// than its deadline.
//
// It keeps the token — the token is this mail's permanent delivery key — and
// only clears the deadline. It requires the token, so one caller cannot
// release another's reservation.
//
// A cancel that finds nothing to release reports that it did nothing, rather
// than answering OK either way. The implementation this replaces had two
// return branches with identical values, so a caller could not tell a
// released reservation from a no-op, and neither was counted.
func (s *Service) CancelClaim(ctx context.Context, playerID int64, mailID string, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, fmt.Errorf("%w: a claim token is required", ErrRequestInvalid)
	}
	nowUnix := s.cfg.Now().Unix()
	var released bool
	_, _, err := s.cfg.Mailboxes.Update(ctx, playerID, func(current Mailbox, found bool) (Mailbox, bool, error) {
		// Work on a private copy. The store hands the callback a struct
		// copy whose maps still alias the stored value, so mutating them
		// and then returning save=false left the store changed anyway —
		// a refused delivery had already inserted its entry and its
		// tombstone while Unread and Version kept their old values
		// (RR-20260911-05). match's callbacks have always cloned first.
		current = current.clone()
		released = false
		if !found {
			return current, false, nil
		}
		current.init(playerID)
		entry, ok := current.entry(mailID)
		if !ok {
			return current, false, nil
		}
		if entry.ClaimToken != token {
			// Not ours. Clearing the deadline here would release another
			// caller's reservation.
			return current, false, nil
		}
		if entry.Status == StatusClaimed || entry.ClaimDeadlineUnix == 0 {
			return current, false, nil
		}
		entry.ClaimDeadlineUnix = 0
		entry.UpdatedAtUnix = nowUnix
		current.Entries[mailID] = entry
		current.UpdatedAtUnix = nowUnix
		released = true
		return current, true, nil
	})
	if err != nil {
		return false, err
	}
	if released {
		s.report.Accepted("cancel_claim")
	} else {
		s.report.Dropped("cancel_claim.nothing_to_release", 1)
	}
	return released, nil
}

// Summary is a player's unread count, read in one round trip.
type Summary struct {
	PlayerID int64
	Unread   int32
	Evicted  uint64
}

// Summary reads the unread count.
//
// One read of one entry. The implementation this replaces ran a `$lookup` /
// `$unwind` / `$group` over every active envelope for the player on every
// call, and server-wide mail lives in that collection for every player.
func (s *Service) Summary(ctx context.Context, playerID int64) (Summary, error) {
	if playerID <= 0 {
		return Summary{}, fmt.Errorf("%w: player id must be positive", ErrRequestInvalid)
	}
	stored, found, err := s.cfg.Mailboxes.Get(ctx, playerID)
	if err != nil {
		return Summary{}, err
	}
	if !found {
		return Summary{PlayerID: playerID}, nil
	}
	return Summary{PlayerID: playerID, Unread: stored.Value.Unread, Evicted: stored.Value.Evicted}, nil
}

// Mailbox reads a player's whole mailbox state. Operator-facing.
func (s *Service) Mailbox(ctx context.Context, playerID int64) (Mailbox, bool, error) {
	stored, found, err := s.cfg.Mailboxes.Get(ctx, playerID)
	if err != nil || !found {
		return Mailbox{}, false, err
	}
	return stored.Value.clone(), true, nil
}
