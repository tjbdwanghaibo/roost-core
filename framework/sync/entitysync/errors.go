// Package entitysync replicates entity state to sessions.
//
// It is one mechanism, not three: a subject (an entity's SubjectSyncState)
// holds the sessions subscribed to it; one Manager per process owns every
// subject and every session, turns dirty subjects into frames on a tick and
// hands each session ITS frame; the policies that decide who subscribes to
// whom (a room, an area of interest, a direct binding) live outside and only
// call Subscribe / Unsubscribe.
//
// 阅读主线：subject.go 保存订阅意图，Manager.Flush 捕获内容并按会话分发，
// session.go 维护已交付的帧时钟与对象引用。内容版本、订阅意图、客户端实际持有
// 的对象是三个不同事实，失败重试和退订时不能互相代替。
package entitysync

import (
	"errors"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

var (
	ErrManagerStopping      = errors.New("entitysync: manager is stopping")
	ErrManagerClosed        = errors.New("entitysync: manager is closed")
	ErrTransportRequired    = errors.New("entitysync: transport is required")
	ErrSubjectInvalid       = errors.New("entitysync: subject is invalid")
	ErrSubjectRegistered    = errors.New("entitysync: subject is already registered")
	ErrSubjectNotRegistered = errors.New("entitysync: subject is not registered")
	ErrSubjectRetiring      = errors.New("entitysync: subject is retiring")
	// ErrSubjectRegisteredElsewhere：这个内容状态（SubjectSyncState）已登记在另一个 Manager。一个状态只喂一个
	// Manager——脏通知器、冻结内容与在途捕获在状态上各只有一份，第二个登记会抢走第一个的变化通知。错误信息点名
	// 实体、命名空间与已登记它的 Manager（ManagerConfig.Name）。在那个 Manager 注销、退役完成（remove 已交付或
	// 订阅者会话已关闭）之后才能登记到别的 Manager；同一个 Manager 重复登记仍是空操作。
	ErrSubjectRegisteredElsewhere = errors.New("entitysync: subject sync state is registered with another manager")
	ErrSubjectLimit               = errors.New("entitysync: subject limit reached")
	ErrSessionInvalid             = errors.New("entitysync: session is invalid")
	ErrSessionUnknown             = errors.New("entitysync: session is not open")
	ErrSessionLimit               = errors.New("entitysync: session limit reached")
	ErrSubscriberLimit            = errors.New("entitysync: subscriber limit reached for this subject")
	ErrSubscriptionNotFound       = errors.New("entitysync: subscription not found")
	ErrWireFrame                  = errors.New("entitysync: malformed wire frame")

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

	// ErrRegistrationCancelled 是 RegisterAfterRetirement 排队的登记被取消时交给 done 的错误：
	// subject 在退役完成前再次 Unregister、另一次排队替换了它，或排队的状态已经关闭。
	ErrRegistrationCancelled = errors.New("entitysync: queued registration was cancelled")
)

// SessionOpenRetryable 报告 OpenSession / OpenHeldSession 的错误是否只是“这次没有创建会话，
// 稍后重试即可”：ErrSessionClosing（同 ID 的旧 lifetime 仍在传输层退出）或 ErrSessionOpening
// （同 ID 的另一次打开尚未确认）。其他错误（会话上限、Manager 关闭、传输拒绝）重试无益。
// 重试的等待由调用方安排在业务池之外。
func SessionOpenRetryable(err error) bool {
	return errors.Is(err, ErrSessionClosing) || errors.Is(err, ErrSessionOpening)
}

// SessionLossReason 是 Manager 自己关闭一个会话（ManagerConfig.SessionLost）的原因分类，取值固定、低基数，
// 可以直接作指标标签。接入层据此决定连接怎么办：
//
//   - SessionLossPushRefused：传输拒收了这一帧（Transport.Push 返回非 ErrRetryLater 的错误）。玩家接入层的
//     传输在拒收时已经关闭了该玩家的连接，或玩家本来就没有连接（wiring/selfsync.PushPlayerFrame）。
//   - 其余都是“连接没有拒收任何东西”的原因——Manager 在编码这一轮的帧时就失败了，帧没有交给传输：
//     SessionLossObjectLimit（会话持有的对象超过 Limits.MaxObjects，frame.ErrObjectLimit）、
//     SessionLossTooLarge（单个实体包超过 MaxFrameBytes / MaxComponentBytes）、SessionLossEncodeFailed（其他编码错误）。
//     连接仍然活着，同步会话已经没了，所以接入层断开该玩家（wiring/selfsync.DisconnectLostPlayer），
//     客户端重连、重开会话、从新 epoch 的全量开始。
type SessionLossReason string

const (
	SessionLossPushRefused  SessionLossReason = "push_refused"
	SessionLossObjectLimit  SessionLossReason = "object_limit"
	SessionLossTooLarge     SessionLossReason = "too_large"
	SessionLossEncodeFailed SessionLossReason = "encode_failed"
)

// SessionLoss 是 Manager 交给 SessionLost 回调的原因：Reason 是分类，Err 是原错误（errors.Is / As 经 Unwrap
// 照常看得到传输或 frame 包的错误）。
type SessionLoss struct {
	Reason SessionLossReason
	Err    error
}

func (l *SessionLoss) Error() string {
	if l.Err == nil {
		return "entitysync: session lost (" + string(l.Reason) + ")"
	}
	return "entitysync: session lost (" + string(l.Reason) + "): " + l.Err.Error()
}

func (l *SessionLoss) Unwrap() error { return l.Err }

// LossReason 取 SessionLost 回调原因里的分类；cause 不是 Manager 给出的 SessionLoss 时返回空串。
func LossReason(cause error) SessionLossReason {
	var loss *SessionLoss
	if errors.As(cause, &loss) {
		return loss.Reason
	}
	return ""
}

// encodeLoss 把一次编码失败归类：分类只看 frame 包的哨兵错误，不看错误文本。
func encodeLoss(err error) *SessionLoss {
	reason := SessionLossEncodeFailed
	switch {
	case errors.Is(err, frame.ErrObjectLimit):
		reason = SessionLossObjectLimit
	case errors.Is(err, frame.ErrFrameTooLarge), errors.Is(err, frame.ErrComponentTooLarge):
		reason = SessionLossTooLarge
	}
	return &SessionLoss{Reason: reason, Err: err}
}
