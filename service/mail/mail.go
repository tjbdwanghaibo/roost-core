package mail

import "context"

// The transport for Mail is generated from the interface below: the wire
// types, the handler table and the typed client. Regenerate after changing
// the interface.
//
//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir . -emit transport

// Mail is the cross-process contract: what ANOTHER process may ask of the
// mail service.
//
// It exists so a caller does not know, and cannot depend on, whether mail runs
// in its own process or in the caller's. Both the local implementation
// (*Service) and the remote client (*BusClient) satisfy it, and a Mod
// registers whichever one this process has under the same capability name. The
// business logic that consumes it is then identical in a merged deployment and
// a split one — which is what "which services share a process is a deployment
// decision" has to mean concretely, and it did not mean anything concrete while
// the registry held a *Service.
//
// It is DELIBERATELY SMALLER than *Service. Three of the service's methods are
// not here, and each omission is a decision:
//
//   - Deliver is a step of broadcast fanout. Whatever strategy a deployment
//     picks runs beside the mailboxes it writes; exposing it across the bus
//     would let any process insert an envelope reference into any mailbox.
//   - Mailbox reads a player's whole state, unbounded by a page. It is for an
//     operator, through admin, not for a peer service.
//   - There is no sweep to expose: envelopes expire by key ttl and mailboxes
//     evict inline. See the note on Server.
//
// Every method takes the caller's identity as a PARAMETER rather than reading
// it from a request struct, and that survives the bus on purpose. The defect
// this replaces was not that a player id travelled on the wire — between
// server processes that is ordinary — but that it travelled as an OPTIONAL
// LOOKING FIELD whose zero value compiled fine. Here the compiler still makes
// a caller say whose identity it is; the wire struct is the client's
// implementation detail.
// Results are NAMED, and every parameter is named too. That is not decoration:
// the servicerpc generator derives the wire structs from these names, so a
// response field is called what the method calls it and a request field is
// called what the parameter is called. An unnamed result has no field name to
// generate, so the generator refuses one.
//
//roost:rpc service_type=mail capability=service.mail
type Mail interface {
	// Send stores an envelope and delivers it. Idempotent per RequestID.
	Send(ctx context.Context, req SendRequest) (envelope Envelope, err error)
	// List returns one page of a player's mailbox, newest first.
	List(ctx context.Context, playerID int64, cursor string, limit int) (page Page, err error)
	// Summary reads a player's unread count.
	Summary(ctx context.Context, playerID int64) (summary Summary, err error)
	// MarkRead moves one mail to read. Idempotent.
	MarkRead(ctx context.Context, playerID int64, mailID string) (entry Entry, err error)
	// Delete moves one mail to deleted. It does not release the mail's claim
	// record; see Service.Delete.
	Delete(ctx context.Context, playerID int64, mailID string) (entry Entry, err error)
	// ReserveClaim takes the delivery reservation for an attachment and
	// returns the payload with the key to deliver it under.
	ReserveClaim(ctx context.Context, playerID int64, mailID string, scope string) (claim Claim, err error)
	// CommitClaim marks an attachment claimed. The token is required.
	CommitClaim(ctx context.Context, playerID int64, mailID string, token string) (entry Entry, err error)
	// CancelClaim releases an in-flight reservation. It reports whether it
	// released anything, which is why it returns a bool rather than only an
	// error: a cancel that found nothing to release is not the same outcome as
	// one that did.
	CancelClaim(ctx context.Context, playerID int64, mailID string, token string) (released bool, err error)
}

var _ Mail = (*Service)(nil)
