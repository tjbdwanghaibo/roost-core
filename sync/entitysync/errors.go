// Package entitysync replicates entity state to sessions.
//
// It is one mechanism, not three: a subject (an entity's SubjectSyncState)
// holds the sessions subscribed to it; one Manager per process owns every
// subject and every session, turns dirty subjects into frames on a tick and
// hands each session ITS frame; the policies that decide who subscribes to
// whom (a room, an area of interest, a direct binding) live outside and only
// call Subscribe / Unsubscribe (ARCH-10).
//
// 阅读主线：subject.go 保存订阅意图，Manager.Flush 捕获内容并按会话分发，
// session.go 维护已交付的帧时钟与对象引用。内容版本、订阅意图、客户端实际持有
// 的对象是三个不同事实，失败重试和退订时不能互相代替。
package entitysync

import "errors"

var (
	ErrManagerStopping      = errors.New("entitysync: manager is stopping")
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
	// 已准入帧不会回滚；部分成功的订阅在重试时改发全量以恢复内容基线。
	ErrRetryLater = errors.New("entitysync: transport cannot take frames now; retry the tick")

	// ErrDurabilityDeferred marks a subject whose newest commit the durable
	// watermark has not reached. Nothing is sent for it this tick; its dirty
	// state and pending subscriptions are kept and the next tick tries again.
	ErrDurabilityDeferred = errors.New("entitysync: content is not durable yet; retry after the watermark advances")

	// ErrSessionClosing 表示同一 SessionID 的上一个 lifetime 仍在传输层退出（旧发送未结束），
	// 本次 OpenSession 没有创建会话。旧发送退出后重试，或为新连接分配新 SessionID。
	// SessionLifecycle 实现可包装它，表达“ID 仍被旧 lifetime 占用”。
	ErrSessionClosing = errors.New("entitysync: previous session with this id is still closing; retry OpenSession later")

	// ErrSessionOpening 表示同一 SessionID 的另一次 OpenSession 正在等待传输确认、结果未定，
	// 本次调用没有创建会话。稍后重试：那次打开成功则得到 nil，失败则重新尝试打开。
	ErrSessionOpening = errors.New("entitysync: session with this id is being opened; retry OpenSession later")
)
