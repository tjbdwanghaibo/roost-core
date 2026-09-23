package policy

import (
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// Direct is the degenerate policy: one binding at a time, decided by the
// caller. A GM watching one player, a replay recorder, a test. It exists so
// the three organizations share a shape — every policy ends in Subscribe /
// Unsubscribe on the manager — not because it does anything the manager
// cannot.
type Direct struct {
	manager *entitysync.Manager
	session func(int64) entitysync.SessionID
}

func NewDirect(manager *entitysync.Manager, session func(observer int64) entitysync.SessionID) (*Direct, error) {
	if manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	if session == nil {
		session = func(observer int64) entitysync.SessionID { return entitysync.SessionID(observer) }
	}
	return &Direct{manager: manager, session: session}, nil
}

// Bind makes an observer receive a subject under a profile.
func (d *Direct) Bind(observer, subject int64, profile entity.SyncProfile) error {
	return d.manager.Subscribe(d.session(observer), subject, profile)
}

// Unbind ends it.
func (d *Direct) Unbind(observer, subject int64) error {
	return d.manager.Unsubscribe(d.session(observer), subject)
}
