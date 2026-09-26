package policy

import (
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	flog "github.com/tjbdwanghaibo/roost-core/log"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// Direct is the degenerate policy: one binding at a time, decided by the
// caller. A GM watching one player, a replay recorder, a test. It exists so
// the three organizations share a shape — every policy ends in Subscribe /
// Unsubscribe on the manager — not because it does anything the manager
// cannot.
//
// Direct 记着调用方当前的绑定（成功的 Bind 到 Unbind 为止），只为一件事：绑定被框架撤销（RR-59 退回 remove）、
// 实体重新登记后，按调用方当前的绑定重新提交（RR-20260926-70）。
type Direct struct {
	subscriptions *entitysync.SubscriptionSource
	session       func(int64) entitysync.SessionID

	mu    sync.Mutex // 串行化 Bind / Unbind / resubmit，使重新提交看到的是调用方最新的绑定
	bound map[directBinding]entity.SyncProfile
}

type directBinding struct {
	session entitysync.SessionID
	subject int64
}

func NewDirect(manager *entitysync.Manager, session func(observer int64) entitysync.SessionID) (*Direct, error) {
	if manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	if session == nil {
		session = func(observer int64) entitysync.SessionID { return entitysync.SessionID(observer) }
	}
	d := &Direct{session: session, bound: make(map[directBinding]entity.SyncProfile)}
	d.subscriptions = manager.NewSubscriptionSourceWithResubmit(d.resubmit)
	return d, nil
}

// Bind makes an observer receive a subject under a profile. A refused Bind
// leaves the previous binding, if any, as it was.
func (d *Direct) Bind(observer, subject int64, profile entity.SyncProfile) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	session := d.session(observer)
	if err := d.subscriptions.Subscribe(session, subject, profile); err != nil {
		return err
	}
	d.bound[directBinding{session: session, subject: subject}] = profile
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
		profile, bound := d.bound[directBinding{session: subscription.Session, subject: subscription.Subject}]
		if !bound {
			continue
		}
		if err := d.subscriptions.Subscribe(subscription.Session, subscription.Subject, profile); err != nil {
			flog.Warn("policy: direct binding could not be resubmitted after the framework retracted it", "session", subscription.Session, "subject", subscription.Subject, "err", err)
		}
	}
}
