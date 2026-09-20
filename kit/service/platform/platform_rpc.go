package platform

import "context"

// The transport for Platform is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir .

// Platform is the cross-process contract: what ANOTHER process may ask of the
// platform service.
//
// Two of the five Service methods are deliberately not here, and both
// omissions say something:
//
//   - ValidateSession takes no context.Context. That is not an oversight in
//     its signature — it does no I/O at all: it recomputes a MAC over the
//     player id with a secret this process already holds. A method that
//     touches nothing has no business being a round trip, and exposing it as
//     one would mean every session check in the fleet queues behind one
//     process. Callers that must validate a token verify it locally with the
//     same secret; that the secret has to be shared for this is a real
//     deployment fact, and it is the reason the check is cheap.
//
//   - AttemptDelivery is the owning process's own retry work. It is the second
//     half of a delivery that already failed once, and the cadence it runs at
//     is a property of the deployment, not a request another service makes.
//     It lives in the Server's run hook.
//
// HandleCallback IS here, and that is the load-bearing inclusion. A provider
// webhook arrives over public HTTPS, which some ingress or gateway process
// terminates — not this one. Forwarding the raw body and its signature over
// the bus keeps the gateway a dumb pipe: it does not verify, does not decode,
// and cannot be trusted to have done either, because the signature check, the
// 16 KiB bound, and the insert-only order reservation all happen HERE, at the
// owner. The alternative — a gateway that parses and forwards a decoded order
// — makes every gateway a place where an order can be minted.
//
// No method carries an affinity key, and that is a decision rather than an
// omission. The contention key for a delivery is the order id, and for the one
// method that writes an order the id is inside the signed payload, so it is
// not available as a routing key at all. It is also not needed: two concurrent
// arrivals of one order id are separated by the insert-only reservation, not
// by having been routed to the same process. Affinity here would be a
// correctness claim the transport cannot keep.
//
//roost:rpc service_type=platform capability=service.platform
type Platform interface {
	// AuthSession turns a channel credential into a session. The credential
	// is checked with the channel; the player id is resolved from the
	// verified identity and never read from the request.
	AuthSession(ctx context.Context, credential Credential) (session Session, err error)

	// HandleCallback records a provider callback and attempts delivery once.
	// It is idempotent per order id: a replay is answered as a replay rather
	// than delivered a second time.
	HandleCallback(ctx context.Context, raw []byte, signature string) (receipt Receipt, err error)

	// Order reads one order record. It is the answer to "did this payment
	// actually deliver", which is a question a game process has to be able to
	// ask without reading platform's storage directly.
	Order(ctx context.Context, orderID string) (order Order, found bool, err error)
}

var _ Platform = (*Service)(nil)
