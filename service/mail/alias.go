// Package mail is the assembly half of the mail service: the Mail RPC
// interface, its generated transport, the Mod that reads configuration and
// wires Redis, and the server run loop.
//
// The domain — envelopes, the per-player mailbox state machine, the
// three-step claim, the stores, the errcode segment — lives in
// roost-core/service/mail (M-07). Everything below is an alias to it, so
// existing callers keep compiling and errors.Is keeps working (the sentinels
// are the same pointers).
package mail

import (
	core "github.com/tjbdwanghaibo/roost-core/service/mail"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

type (
	Audience      = core.Audience
	Status        = core.Status
	Envelope      = core.Envelope
	Item          = core.Item
	Entry         = core.Entry
	Mailbox       = core.Mailbox
	Claim         = core.Claim
	SettledClaim  = core.SettledClaim
	SendRequest   = core.SendRequest
	SentRecord    = core.SentRecord
	Page          = core.Page
	Summary       = core.Summary
	Config        = core.Config
	Service       = core.Service
	Deliverer     = core.Deliverer
	DelivererFunc = core.DelivererFunc
	EnvelopeStore = core.EnvelopeStore
	MailboxStore  = core.MailboxStore
	SendLedger    = core.SendLedger
	RedisConfig   = core.RedisConfig
	RedisStores   = core.RedisStores
)

const (
	AudienceDirect    = core.AudienceDirect
	AudienceBroadcast = core.AudienceBroadcast

	StatusUnread  = core.StatusUnread
	StatusRead    = core.StatusRead
	StatusClaimed = core.StatusClaimed
	StatusDeleted = core.StatusDeleted

	DefaultClaimLease  = core.DefaultClaimLease
	DefaultPageSize    = core.DefaultPageSize
	MaxAttachmentBytes = core.MaxAttachmentBytes
	MaxBodyBytes       = core.MaxBodyBytes
	MaxMailboxEntries  = core.MaxMailboxEntries
	MaxPageSize        = core.MaxPageSize
	MaxRecipients      = core.MaxRecipients
	MaxSettledClaims   = core.MaxSettledClaims
	MaxSubjectBytes    = core.MaxSubjectBytes

	CodeOK               = core.CodeOK
	CodeAlreadyClaimed   = core.CodeAlreadyClaimed
	CodeAudienceInvalid  = core.CodeAudienceInvalid
	CodeBodyInvalid      = core.CodeBodyInvalid
	CodeClaimHeld        = core.CodeClaimHeld
	CodeClaimHistoryFull = core.CodeClaimHistoryFull
	CodeClaimTokenWrong  = core.CodeClaimTokenWrong
	CodeConflict         = core.CodeConflict
	CodeExpired          = core.CodeExpired
	CodeMailInvalid      = core.CodeMailInvalid
	CodeMailMissing      = core.CodeMailMissing
	CodeMailboxFull      = core.CodeMailboxFull
	CodeNoAttachment     = core.CodeNoAttachment
	CodeNotRecipient     = core.CodeNotRecipient
	CodeRangeInvalid     = core.CodeRangeInvalid
	CodeRequestInvalid   = core.CodeRequestInvalid
)

var (
	ErrAlreadyClaimed   = core.ErrAlreadyClaimed
	ErrAudienceInvalid  = core.ErrAudienceInvalid
	ErrBodyInvalid      = core.ErrBodyInvalid
	ErrClaimHeld        = core.ErrClaimHeld
	ErrClaimHistoryFull = core.ErrClaimHistoryFull
	ErrClaimTokenWrong  = core.ErrClaimTokenWrong
	ErrConflict         = core.ErrConflict
	ErrExpired          = core.ErrExpired
	ErrMailInvalid      = core.ErrMailInvalid
	ErrMailMissing      = core.ErrMailMissing
	ErrMailboxFull      = core.ErrMailboxFull
	ErrNoAttachment     = core.ErrNoAttachment
	ErrNotRecipient     = core.ErrNotRecipient
	ErrRangeInvalid     = core.ErrRangeInvalid
	ErrRequestInvalid   = core.ErrRequestInvalid
)

// New builds the mail service; see roost-core/service/mail.New.
func New(cfg Config) (*Service, error) { return core.New(cfg) }

// NewRedisStores builds the Redis-backed stores; see roost-core/service/mail.NewRedisStores.
func NewRedisStores(client fredis.IRedis, cfg RedisConfig) (RedisStores, error) {
	return core.NewRedisStores(client, cfg)
}

// Error maps an error to the code and reason a client sees.
func Error(err error) (int32, string) { return core.Error(err) }

// Code returns the errcode carried by err.
func Code(err error) int32 { return core.Code(err) }
