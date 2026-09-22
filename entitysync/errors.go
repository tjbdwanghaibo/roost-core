// Package entitysync replicates entity state to sessions.
//
// It is one mechanism, not three: a subject (an entity's SubjectSyncState)
// holds the sessions subscribed to it; one Manager per process owns every
// subject and every session, turns dirty subjects into frames on a tick and
// hands each session ITS frame; the policies that decide who subscribes to
// whom (a room, an area of interest, a direct binding) live outside and only
// call Subscribe / Unsubscribe (ARCH-10).
package entitysync

import "errors"

var (
	ErrManagerClosed        = errors.New("entitysync: manager is closed")
	ErrTransportRequired    = errors.New("entitysync: transport is required")
	ErrSubjectInvalid       = errors.New("entitysync: subject is invalid")
	ErrSubjectRegistered    = errors.New("entitysync: subject is already registered")
	ErrSubjectNotRegistered = errors.New("entitysync: subject is not registered")
	ErrSubjectRetiring      = errors.New("entitysync: subject is retiring")
	ErrSubjectLimit         = errors.New("entitysync: subject limit reached")
	ErrSessionInvalid       = errors.New("entitysync: session is invalid")
	ErrSessionUnknown       = errors.New("entitysync: session is not open")
	ErrSessionLimit         = errors.New("entitysync: session limit reached")
	ErrSubscriberLimit      = errors.New("entitysync: subscriber limit reached for this subject")
	ErrSubscriptionNotFound = errors.New("entitysync: subscription not found")
	ErrWireFrame            = errors.New("entitysync: malformed wire frame")

	// ErrRetryLater is what a Transport returns when it cannot take frames
	// from ANYBODY right now (starting up, shutting down). The tick is
	// abandoned whole — every subject keeps its dirty state — and no session
	// is blamed. Any other push error is that session's alone: it is closed.
	ErrRetryLater = errors.New("entitysync: transport cannot take frames now; retry the tick")

	// ErrDurabilityDeferred marks a subject whose newest commit the durable
	// watermark has not reached. Nothing is sent for it this tick; its dirty
	// state and pending subscriptions are kept and the next tick tries again.
	ErrDurabilityDeferred = errors.New("entitysync: content is not durable yet; retry after the watermark advances")
)
