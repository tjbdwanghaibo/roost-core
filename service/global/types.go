// Package global is the cross-server coordination service: it owns which
// global group a game server belongs to.
//
// The business boundary document of the implementation this replaces already
// wrote down the invariant this package must hold — "route binding migration
// must use epoch CAS, not an in-process lock". It was a convention, and the
// implementation did not keep it: every store exposed both an unconditional
// SetXxx and a read-modify-write UpdateXxx. The Redis implementation's Update
// used compare-and-set; the DAO-backed implementation's Update read and then
// wrote unconditionally. Both satisfied the same interface, so the type system
// could not tell them apart and whichever was configured decided whether the
// documented invariant held.
//
// Here the invariant is held by the types. State goes through versionstore,
// whose contract has no unconditional write, so a non-CAS implementation
// cannot exist.
//
// The package also used to hold a liveness lease per game server
// (AcquireLease / RenewLease / ReleaseLease / Lease / LiveGames). Process
// liveness now belongs to the App — the singleton lock every service type
// takes through app.Singleton, read with app.SingletonLiveness.Live — so the
// lease API was removed rather than kept as a second, disagreeing answer to
// "is this game server alive".
package global

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// Error codes.
const (
	CodeOK int32 = 0

	CodeRouteInvalid   int32 = 570101
	CodeRouteMissing   int32 = 570102
	CodeRouteStale     int32 = 570103
	CodeRouteMigrating int32 = 570104
	// 570105 through 570109 are RETIRED. 570105-570108 were the lease codes
	// (invalid, missing, not holder, expired) and 570109 was the range code
	// whose only producer was LiveGames; they went with the lease API when
	// process liveness moved to the App's singleton lock. Like the hole
	// below, they are not reused.
	CodeConflict int32 = 570110
	// CodeRequestInvalid reports a request this service could not even read:
	// a wire frame that failed to decode. The generated transport needs one
	// coded error for that, and answering it with CodeInternal would report a
	// caller's malformed request as a server fault.
	//
	// It is 570125, not 570111 — the next free-LOOKING number — because
	// 570111 through 570124 are RETIRED rather than free. 570111 was a
	// catch-all "store failed" that was removed; 570112 through 570124 were
	// the activity codes, which moved to package activity's own segment when
	// that service was split out of this one. Every one of those numbers
	// meant something in a shipped release, and a code that comes back
	// meaning something else is worse than a hole a comment explains.
	CodeRequestInvalid int32 = 570125
)

var (
	ErrRouteInvalid = errcode.Define(CodeRouteInvalid, "global: route is invalid", "")
	ErrRouteMissing = errcode.Define(CodeRouteMissing, "global: route binding not found", "")
	// ErrRouteStale reports that a rebind presented an epoch that is no
	// longer current. It is the epoch CAS the boundary document required and
	// the implementation enforced with an in-process lock, which is no
	// guarantee across instances.
	ErrRouteStale     = errcode.Define(CodeRouteStale, "global: route epoch is stale", "")
	ErrRouteMigrating = errcode.Define(CodeRouteMigrating, "global: route binding is migrating", "")

	ErrConflict = errcode.Define(CodeConflict, "global: conflict", "")
	// ErrRequestInvalid reports a request that could not be decoded. The
	// generated transport returns it for a frame it cannot read, which is the
	// one refusal the transport itself has to be able to make.
	ErrRequestInvalid = errcode.Define(CodeRequestInvalid, "global: request is invalid", "")
)

// RouteState is where a binding is in its lifecycle.
//
//	active ──> migrating ──> active (at a new global sid)
//
// Migration is two-step on purpose: a binding that is moving is visible as
// moving, so a caller resolving it can wait rather than reach a server that
// is handing over.
type RouteState string

const (
	RouteActive    RouteState = "active"
	RouteMigrating RouteState = "migrating"
)

// RouteBinding says which global group and instance serve one game server.
type RouteBinding struct {
	// GameSID identifies the game server.
	GameSID int32 `json:"game_sid"`
	// GlobalGroupID is the coordination group it belongs to.
	GlobalGroupID string `json:"global_group_id"`
	// GlobalSID is the instance currently serving it.
	GlobalSID int32 `json:"global_sid"`
	// TargetGlobalSID is where it is moving, while State is migrating.
	TargetGlobalSID int32 `json:"target_global_sid,omitempty"`
	// Epoch increments on every accepted change. A rebind must present the
	// epoch it read, which is what makes concurrent migrations safe without a
	// lock — and a lock across instances is exactly what the boundary
	// document forbade.
	Epoch uint64     `json:"epoch"`
	State RouteState `json:"state"`
	// CompletedFromEpoch 保留最近一次完成操作的请求身份；Abort 不写它。
	// 下一次 Begin 会清除，旧完成请求不能越过新的迁移。
	CompletedFromEpoch uint64 `json:"completed_from_epoch,omitempty"`

	UpdatedAtUnix int64 `json:"updated_at_unix"`
}

func (b RouteBinding) Validate() error {
	if b.GameSID <= 0 {
		return fmt.Errorf("%w: game sid must be positive", ErrRouteInvalid)
	}
	if strings.TrimSpace(b.GlobalGroupID) == "" {
		return fmt.Errorf("%w: global group id is empty", ErrRouteInvalid)
	}
	if b.GlobalSID <= 0 {
		return fmt.Errorf("%w: global sid must be positive", ErrRouteInvalid)
	}
	return nil
}

// Error maps an error to the code and reason a client sees.
//
// It matches roost-kit's servicerpc.Error convention, which is what an RPC
// envelope is filled from.
//
// It is short because the sentinels carry their own codes: errcode.ClientError
// finds the code through any depth of fmt.Errorf wrapping, so there is no
// per-sentinel table here to keep in step with the one above. A hand-written
// switch over every sentinel is the shape this replaces, and it is a second
// list that a newly added error silently falls off.
//
// Two behaviours are relied on rather than incidental:
//
//   - When an error wraps two coded errors with "%w: %w", the FIRST one wins.
//     That is what makes a refusal which wraps a caller's own reason report
//     the refusal, which is what the client has to be told.
//   - An error this package cannot classify reports errcode.CodeInternal, not
//     a code of its own. Answering "the store failed" for an unclassified bug
//     is a guess presented as a diagnosis — and a catch-all code of that shape
//     is what the previous constant block had, with nothing able to produce it
//     deliberately.
func Error(err error) (int32, string) {
	if err == nil {
		return CodeOK, ""
	}
	// versionstore.ErrConflict is a FOREIGN sentinel: it belongs to roost-kit
	// and carries no code of this package's, so errcode.ClientError would
	// report it as CodeInternal. Compare-and-set exhaustion under contention
	// is a real, retryable outcome a caller can act on, and "server error" is
	// not an answer it can act on — so it is mapped deliberately here.
	//
	// This is the only kind of case a table is still needed for, and it is
	// why Error is a function rather than a bare call to errcode.
	if errors.Is(err, versionstore.ErrConflict) {
		return errcode.ClientError(ErrConflict)
	}
	return errcode.ClientError(err)
}

// Code is Error without the reason, for callers that only switch on the code.
func Code(err error) int32 {
	code, _ := Error(err)
	return code
}
