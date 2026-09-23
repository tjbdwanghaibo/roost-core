package entitysync

import (
	"sort"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// subscriptionKind is where one session stands with respect to one subject.
type subscriptionKind uint8

const (
	// kindSnapshot: subscribed, has nothing yet; the next frame carries a
	// full snapshot (an ObjectCreate, or an ObjectUpdate carrying a full
	// payload when the session already holds the object under another
	// profile).
	kindSnapshot subscriptionKind = iota + 1
	// kindLive: holds baseVersion; the next frame carries a delta from it.
	kindLive
	// kindLeaving: unsubscribed or the subject is retiring; the next frame
	// carries an ObjectRemove and the entry is dropped.
	kindLeaving
)

// subscription is one session's standing with one subject. It is the whole
// of what used to be a coordinator row plus a sink row: the profile the
// session wants, the kind of frame it is owed, and the version it holds.
type subscription struct {
	profile     entity.SyncProfile
	kind        subscriptionKind
	baseVersion uint64
}

// subject is an entity's sync state plus its subscribers. The subscribers
// live HERE, not in an index somewhere else: the subject is the truth about
// who receives it (ARCH-10).
type subject struct {
	mu          sync.Mutex
	id          int64
	state       *entity.SubjectSyncState
	subscribers map[SessionID]*subscription
	// retiring: Unregister was called; every subscriber is leaving and the
	// subject is forgotten once the last remove has gone out.
	retiring bool
}

func newSubject(state *entity.SubjectSyncState) *subject {
	return &subject{id: state.SubjectID(), state: state, subscribers: make(map[SessionID]*subscription)}
}

// profilesLocked collects the distinct profiles of the subscribers in kinds,
// sorted so packers see a deterministic order.
func (s *subject) profilesLocked(kinds ...subscriptionKind) []entity.SyncProfile {
	unique := make(map[entity.SyncProfile]struct{})
	for _, sub := range s.subscribers {
		for _, kind := range kinds {
			if sub.kind == kind {
				unique[sub.profile.Normalize()] = struct{}{}
				break
			}
		}
	}
	if len(unique) == 0 {
		return nil
	}
	profiles := make([]entity.SyncProfile, 0, len(unique))
	for profile := range unique {
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool {
		a, b := profiles[i], profiles[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.LOD != b.LOD {
			return a.LOD < b.LOD
		}
		return a.SchemaVersion < b.SchemaVersion
	})
	return profiles
}

// sessionsLocked lists the subscribed sessions in ascending order.
func (s *subject) sessionsLocked() []SessionID {
	ids := make([]SessionID, 0, len(s.subscribers))
	for id := range s.subscribers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// updateFor picks the update prepared for this profile.
func updateFor(updates []entity.SubjectSyncUpdate, profile entity.SyncProfile) (entity.SubjectSyncUpdate, bool) {
	profile = profile.Normalize()
	for _, update := range updates {
		if update.Profile.Normalize() == profile {
			return update, true
		}
	}
	return entity.SubjectSyncUpdate{}, false
}
