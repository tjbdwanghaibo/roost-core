package global

import "context"

// The transport for Routing is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir .

// Routing is the cross-process contract: what ANOTHER process may ask of the
// routing service.
//
// Every method the Service has is here, which is unusual in this repository —
// every other service holds something back — so it is worth saying why none of
// these needs to.
//
// Bind and the three migration steps are the privileged operations one would
// expect to keep local. They are exposed because each carries its own fence in
// the STATE rather than in who called:
//
//   - Bind is insert-only. A second Bind for a game server loses the Create
//     and is refused with ErrConflict, so it cannot move a live game — it can
//     only establish a binding that does not exist. That collision IS the
//     fence.
//   - Each migration step takes expectEpoch, checked inside the
//     compare-and-set.
//
// Two further things make exposing them sound:
//
//   - A caller that did not read the current binding first cannot construct a
//     migration call that lands, so a stale or duplicated migration driver is
//     refused rather than obeyed.
//   - The driver is not this process. A migration is orchestrated — by an
//     operator tool, by a control-plane process watching capacity — and
//     orchestration in the same process as the state it moves is how a
//     control plane ends up unable to restart without stopping the data
//     plane.
//
// No method carries an affinity key, and here that follows from the data
// rather than from an absence of options: a route is one versioned entry PER
// GAME SERVER, so contention is already per key and two game servers never
// touch the same one. Routing by game id would move traffic around without
// removing any contention.
//
// There is no liveness here. The lease methods this contract used to carry
// (AcquireLease, RenewLease, ReleaseLease, Lease, LiveGames) were removed:
// whether a game server is alive is answered by the App's singleton lock,
// through app.SingletonLiveness.Live.
//
//roost:rpc service_type=global capability=service.global
type Routing interface {
	// Bind establishes the first binding for a game server. It is
	// insert-only: rebinding a game that already has one is refused, and a
	// move goes through the migration steps instead.
	Bind(ctx context.Context, gameSID int32, groupID string, globalSID int32) (binding RouteBinding, err error)

	// Resolve reports where a game server belongs. It is the first call a
	// game process makes and the one it repeats after a migration.
	Resolve(ctx context.Context, gameSID int32) (binding RouteBinding, err error)

	// BeginMigration marks a binding as migrating toward a target, fenced on
	// the epoch the caller last read.
	BeginMigration(ctx context.Context, gameSID int32, targetGlobalSID int32, expectEpoch uint64) (binding RouteBinding, err error)

	// CompleteMigration finishes a migration, fenced the same way.
	CompleteMigration(ctx context.Context, gameSID int32, expectEpoch uint64) (binding RouteBinding, err error)

	// AbortMigration returns a migrating binding to its origin.
	AbortMigration(ctx context.Context, gameSID int32, expectEpoch uint64) (binding RouteBinding, err error)
}

var _ Routing = (*Service)(nil)
