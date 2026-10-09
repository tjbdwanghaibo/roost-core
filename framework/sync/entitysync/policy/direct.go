package policy

import (
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	flog "github.com/tjbdwanghaibo/roost-core/infra/observe/log"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// Direct is the degenerate policy: one binding at a time, decided by the
// caller. A GM watching one player, a replay recorder, a test. It exists so
// the three organizations share a shape — every policy ends in Subscribe /
// Unsubscribe on the manager — not because it does anything the manager
// cannot.
//
// Direct 记着调用方当前的绑定（成功的 Bind 起，到 Unbind、会话关闭或业务 Unregister 该实体为止），只为一件事：
// 绑定被框架撤销（RR-59 退回 remove）、实体重新登记后，按调用方当前的绑定重新提交（RR-20260926-70）。会话关闭
// （CloseSession 或传输失败）与业务 Unregister 时 Manager 已丢掉订阅，绑定在下一次 Flush 的政策阶段随之删除，
// 表的大小落在活跃绑定数上（RR-20260926-79）；RR-59 撤销不删除绑定。每条绑定记着本来源最近一次成功订阅的戳，
// 释放通知只删除戳更早的——通知所指那一次会话打开 / 那一次登记里的绑定（RR-20260926-85）。
type Direct struct {
	subscriptions *entitysync.SubscriptionSource
	session       func(int64) entitysync.SessionID

	mu    sync.Mutex // 串行化 Bind / Unbind / resubmit / released，本来源的订阅与绑定表只在它下面变化
	bound map[directBinding]directBound
}

type directBinding struct {
	session entitysync.SessionID
	subject int64
}

type directBound struct {
	profile entity.SyncProfile
	stamp   entitysync.SubscriptionStamp // 本来源最近一次成功订阅这对 pair 的戳（Bind 或 resubmit）
}

func NewDirect(manager *entitysync.Manager, session func(observer int64) entitysync.SessionID) (*Direct, error) {
	if manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	if session == nil {
		session = func(observer int64) entitysync.SessionID { return entitysync.SessionID(observer) }
	}
	d := &Direct{session: session, bound: make(map[directBinding]directBound)}
	d.subscriptions = manager.NewSubscriptionSourceWithHooks(entitysync.SubscriptionSourceHooks{Resubmit: d.resubmit, Released: d.released})
	return d, nil
}

// Bind makes an observer receive a subject under a profile. A refused Bind
// leaves the previous binding, if any, as it was.
func (d *Direct) Bind(observer, subject int64, profile entity.SyncProfile) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	session := d.session(observer)
	stamp, err := d.subscriptions.SubscribeStamped(session, subject, profile)
	if err != nil {
		return err
	}
	d.bound[directBinding{session: session, subject: subject}] = directBound{profile: profile, stamp: stamp}
	return nil
}

// Unbind ends it.
func (d *Direct) Unbind(observer, subject int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	session := d.session(observer)
	delete(d.bound, directBinding{session: session, subject: subject})
	return d.subscriptions.Unsubscribe(session, subject)
}

// resubmit 是 Direct 的重新提交入口：框架撤销的绑定在实体重新登记后交还这里，仍绑定着的以调用方当前的视图重新订阅；
// 缺席期间 Unbind 过的不恢复。拒绝没有调用方可报告，记一条 Warn 后放弃（调用方可再次 Bind）。
func (d *Direct) resubmit(retracted []entitysync.RetractedSubscription) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, subscription := range retracted {
		key := directBinding{session: subscription.Session, subject: subscription.Subject}
		binding, bound := d.bound[key]
		if !bound {
			continue
		}
		stamp, err := d.subscriptions.SubscribeStamped(subscription.Session, subscription.Subject, binding.profile)
		if err != nil {
			flog.Warn("policy: direct binding could not be resubmitted after the framework retracted it", "session", subscription.Session, "subject", subscription.Subject, "err", err)
			continue
		}
		binding.stamp = stamp
		d.bound[key] = binding
	}
}

// released 是 Direct 的释放入口（RR-20260926-79）：会话关闭或业务 Unregister 后 Manager 丢掉了这些订阅，
// 对应的绑定不再有意义。旧实现只在 Unbind 时删除，表随历史绑定数单调增长。通知在下一次政策阶段才到，其间调用方
// 可能已在重开的会话或重新登记的实体上再次 Bind：绑定的戳晚于通知的是新的，保留。删除时一并 Unsubscribe，让 Manager
// 释放 RR-59 撤销后仍为这对 pair 保留的重新提交记录（此时本来源已不持有订阅，不影响其他来源）。
//
// 判定比较戳而不是 Holds（RR-20260926-85）：新绑定随后被 RR-59 撤销时 subject 退役或已被忘掉，Holds 为假，旧实现据此删掉
// 新绑定并 Unsubscribe，连它的 RR-70 撤销记录一起删掉，实体重新登记后不再恢复。戳早于通知的绑定一定属于通知所指的那次
// 打开 / 登记（或更早、同样已死的），本来源不会有更新的订阅或撤销记录，Unsubscribe 只清掉这次释放留下的记录。
func (d *Direct) released(pairs []entitysync.ReleasedSubscription) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, pair := range pairs {
		key := directBinding{session: pair.Session, subject: pair.Subject}
		if binding, bound := d.bound[key]; !bound || binding.stamp > pair.Stamp {
			continue
		}
		delete(d.bound, key)
		_ = d.subscriptions.Unsubscribe(pair.Session, pair.Subject)
	}
}
