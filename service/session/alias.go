// Package session is the assembly half of the session service: the Session
// RPC interface, its generated transport, the Mod and the server run loop.
//
// The domain — idempotent Enter, one live run per owner, exactly-once
// resource release, the Admin operator surface, the stores, the errcode
// segment — lives in roost-core/service/session (M-08). Everything below is
// an alias to it, so existing callers keep compiling and errors.Is keeps
// working (the sentinels are the same pointers).
package session

import (
	core "github.com/tjbdwanghaibo/roost-core/service/session"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

type (
	State         = core.State
	Resource      = core.Resource
	Run           = core.Run
	Claim         = core.Claim
	LedgerEntry   = core.LedgerEntry
	EnterRequest  = core.EnterRequest
	Config        = core.Config
	Service       = core.Service
	Releaser      = core.Releaser
	ReleaserFunc  = core.ReleaserFunc
	RunStore      = core.RunStore
	ClaimStore    = core.ClaimStore
	RequestLedger = core.RequestLedger
	Admin         = core.Admin
	RedisConfig   = core.RedisConfig
	RedisStores   = core.RedisStores
)

const (
	StateOpen      = core.StateOpen
	StateSucceeded = core.StateSucceeded
	StateFailed    = core.StateFailed
	StateAbandoned = core.StateAbandoned
	StateExpired   = core.StateExpired

	DefaultTTL         = core.DefaultTTL
	MaxAdminNoteBytes  = core.MaxAdminNoteBytes
	MaxContextEntries  = core.MaxContextEntries
	MaxPageSize        = core.MaxPageSize
	MaxResourceEntries = core.MaxResourceEntries

	CodeOK                = core.CodeOK
	CodeAdminNoteRequired = core.CodeAdminNoteRequired
	CodeAlreadyAttached   = core.CodeAlreadyAttached
	CodeAlreadyRunning    = core.CodeAlreadyRunning
	CodeConflict          = core.CodeConflict
	CodeNotAttached       = core.CodeNotAttached
	CodeNotOwner          = core.CodeNotOwner
	CodeNotResolvable     = core.CodeNotResolvable
	CodeRangeInvalid      = core.CodeRangeInvalid
	CodeRequestInvalid    = core.CodeRequestInvalid
	CodeRunExpired        = core.CodeRunExpired
	CodeRunInvalid        = core.CodeRunInvalid
	CodeRunMissing        = core.CodeRunMissing
	CodeRunTerminal       = core.CodeRunTerminal
)

var (
	ErrAdminNoteRequired = core.ErrAdminNoteRequired
	ErrAlreadyAttached   = core.ErrAlreadyAttached
	ErrAlreadyRunning    = core.ErrAlreadyRunning
	ErrConflict          = core.ErrConflict
	ErrNotAttached       = core.ErrNotAttached
	ErrNotOwner          = core.ErrNotOwner
	ErrNotResolvable     = core.ErrNotResolvable
	ErrRangeInvalid      = core.ErrRangeInvalid
	ErrRequestInvalid    = core.ErrRequestInvalid
	ErrRunExpired        = core.ErrRunExpired
	ErrRunInvalid        = core.ErrRunInvalid
	ErrRunMissing        = core.ErrRunMissing
	ErrRunTerminal       = core.ErrRunTerminal
)

// New builds the session service; see roost-core/service/session.New.
func New(cfg Config) (*Service, error) { return core.New(cfg) }

// NewRedisStores builds the Redis-backed stores; see roost-core/service/session.NewRedisStores.
func NewRedisStores(client versionstore.RedisClient, cfg RedisConfig) (RedisStores, error) {
	return core.NewRedisStores(client, cfg)
}

// Error maps an error to the code and reason a client sees.
func Error(err error) (int32, string) { return core.Error(err) }

// Code returns the errcode carried by err.
func Code(err error) int32 { return core.Code(err) }
