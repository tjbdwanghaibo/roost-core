// The match domain — types, error codes, the ticket state machine, the Redis
// store and the Grouping policies — lives in roost-core/service/match since
// M-06 (ARCH-01: core implements, kit assembles). This file re-exports it
// under the import path every existing consumer uses: the generated RPC
// surface in this package, the Mod, roost-codegen's generated projects and the
// game-demo matchmaker. The names on both sides denote the same types and the
// same sentinel values, so errors.Is and type assertions are unaffected.
// New code may import roost-core/service/match directly.
package match

import (
	core "github.com/tjbdwanghaibo/roost-core/service/match"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// Types.
type (
	Queue               = core.Queue
	Subject             = core.Subject
	SubjectKind         = core.SubjectKind
	Ticket              = core.Ticket
	TicketState         = core.TicketState
	Match               = core.Match
	Store               = core.Store
	Config              = core.Config
	Grouping            = core.Grouping
	FirstComeGrouping   = core.FirstComeGrouping
	ScoreWindowGrouping = core.ScoreWindowGrouping
)

// Ticket states.
const (
	TicketWaiting   = core.TicketWaiting
	TicketMatched   = core.TicketMatched
	TicketCancelled = core.TicketCancelled
	TicketExpired   = core.TicketExpired
)

// Limits and defaults.
const (
	DefaultTicketTTL = core.DefaultTicketTTL
	MaxGroupSize     = core.MaxGroupSize
	MaxPageSize      = core.MaxPageSize
	MaxPayloadBytes  = core.MaxPayloadBytes
	MaxQueueLength   = core.MaxQueueLength
)

// Error codes (segment 550101–550199).
const (
	CodeOK             = core.CodeOK
	CodeQueueInvalid   = core.CodeQueueInvalid
	CodeSubjectInvalid = core.CodeSubjectInvalid
	CodeTicketInvalid  = core.CodeTicketInvalid
	CodeTicketMissing  = core.CodeTicketMissing
	CodeAlreadyQueued  = core.CodeAlreadyQueued
	CodeNotPermitted   = core.CodeNotPermitted
	CodeTicketMatched  = core.CodeTicketMatched
	CodeConflict       = core.CodeConflict
	CodeRequestInvalid = core.CodeRequestInvalid
)

// Sentinels: the same pointers as core's, so errors.Is works across the two
// import paths.
var (
	ErrQueueInvalid   = core.ErrQueueInvalid
	ErrSubjectInvalid = core.ErrSubjectInvalid
	ErrTicketInvalid  = core.ErrTicketInvalid
	ErrTicketMissing  = core.ErrTicketMissing
	ErrAlreadyQueued  = core.ErrAlreadyQueued
	ErrNotPermitted   = core.ErrNotPermitted
	ErrTicketMatched  = core.ErrTicketMatched
	ErrConflict       = core.ErrConflict
	ErrRequestInvalid = core.ErrRequestInvalid
)

// NewStore is not re-exported: its state parameter is core's unexported
// queue type, so nothing outside core could ever have called it. Use
// NewMemoryStore (tests, single-process tools) or NewRedisStore.

// NewMemoryStore is a Store over an in-process versionstore.
func NewMemoryStore(cfg Config) (Store, error) { return core.NewMemoryStore(cfg) }

// NewRedisStore builds the Store every deployment uses.
func NewRedisStore(client versionstore.RedisClient, prefix string, cfg Config) (Store, error) {
	return core.NewRedisStore(client, prefix, cfg)
}

// Error maps an error to the code and reason a client sees.
func Error(err error) (int32, string) { return core.Error(err) }

// Code is the client code of err.
func Code(err error) int32 { return core.Code(err) }
