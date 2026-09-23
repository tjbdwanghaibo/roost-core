package policy

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// GroupConfig shapes a Group.
type GroupConfig struct {
	// Manager receives the subscriptions. Required.
	Manager *entitysync.Manager
	// Profile every member subscribes with. Zero is the default profile.
	Profile entity.SyncProfile
	// Session names a member's session. Nil means the session id IS the
	// member id.
	Session func(member int64) entitysync.SessionID
	// MaxSubjects and MaxMembers bound the group. Zero means unbounded (the
	// manager's own limits still apply).
	MaxSubjects int
	MaxMembers  int
}

var (
	ErrGroupClosed         = errors.New("policy: group is closed")
	ErrGroupSubjectLimit   = errors.New("policy: group subject limit reached")
	ErrGroupMemberLimit    = errors.New("policy: group member limit reached")
	ErrGroupSubjectPresent = errors.New("policy: subject is already in the group")
	ErrGroupMemberPresent  = errors.New("policy: member is already in the group")
	ErrGroupNotPresent     = errors.New("policy: not in the group")
)

// Group is the all-to-all policy: every member receives every subject in the
// room, wherever they are. A lobby, a party, an instance. It is not a lockstep battle room (that is lockstep.Room) and not a label on the wire. Subjects and
// members are separate sets — a spectator is a member and not a subject, a
// scripted actor is a subject and not a member — and a player is usually
// both, added twice.
type Group struct {
	mu       sync.Mutex
	config   GroupConfig
	session  func(int64) entitysync.SessionID
	subjects map[int64]struct{}
	members  map[int64]struct{}
	closed   bool
}

func NewGroup(config GroupConfig) (*Group, error) {
	if config.Manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	session := config.Session
	if session == nil {
		session = func(member int64) entitysync.SessionID { return entitysync.SessionID(member) }
	}
	return &Group{config: config, session: session, subjects: make(map[int64]struct{}), members: make(map[int64]struct{})}, nil
}

// AddSubject registers a subject with the manager and subscribes every
// current member to it.
func (r *Group) AddSubject(state *entity.SubjectSyncState) error {
	if state == nil {
		return entitysync.ErrSubjectInvalid
	}
	id := state.SubjectID()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrGroupClosed
	}
	if _, present := r.subjects[id]; present {
		return ErrGroupSubjectPresent
	}
	if r.config.MaxSubjects > 0 && len(r.subjects) >= r.config.MaxSubjects {
		return ErrGroupSubjectLimit
	}
	if err := r.config.Manager.Register(state); err != nil && !errors.Is(err, entitysync.ErrSubjectRegistered) {
		return err
	}
	r.subjects[id] = struct{}{}
	var errs []error
	for member := range r.members {
		if err := r.config.Manager.Subscribe(r.session(member), id, r.config.Profile); err != nil {
			errs = append(errs, fmt.Errorf("member %d: %w", member, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveSubject retires a subject: every member is owed a remove.
func (r *Group) RemoveSubject(id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.subjects[id]; !present {
		return ErrGroupNotPresent
	}
	delete(r.subjects, id)
	if err := r.config.Manager.Unregister(id); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
		return err
	}
	return nil
}

// Join makes a session a member: it receives every subject in the group. The
// session must already be open with the manager.
func (r *Group) Join(member int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrGroupClosed
	}
	if _, present := r.members[member]; present {
		return ErrGroupMemberPresent
	}
	if r.config.MaxMembers > 0 && len(r.members) >= r.config.MaxMembers {
		return ErrGroupMemberLimit
	}
	session := r.session(member)
	var errs []error
	for id := range r.subjects {
		if err := r.config.Manager.Subscribe(session, id, r.config.Profile); err != nil {
			errs = append(errs, fmt.Errorf("subject %d: %w", id, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		for id := range r.subjects {
			_ = r.config.Manager.Unsubscribe(session, id)
		}
		return err
	}
	r.members[member] = struct{}{}
	return nil
}

// Leave takes a member out: it stops receiving the group's subjects.
func (r *Group) Leave(member int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.members[member]; !present {
		return ErrGroupNotPresent
	}
	delete(r.members, member)
	session := r.session(member)
	var errs []error
	for id := range r.subjects {
		if err := r.config.Manager.Unsubscribe(session, id); err != nil && !errors.Is(err, entitysync.ErrSubscriptionNotFound) && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			errs = append(errs, fmt.Errorf("subject %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Close retires every subject and forgets every member. Members' sessions
// stay open — they are the transport's, not the group's.
func (r *Group) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var errs []error
	for id := range r.subjects {
		if err := r.config.Manager.Unregister(id); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			errs = append(errs, fmt.Errorf("subject %d: %w", id, err))
		}
	}
	clear(r.subjects)
	clear(r.members)
	return errors.Join(errs...)
}

// Members and Subjects report the group's size.
func (r *Group) Members() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.members)
}

func (r *Group) Subjects() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subjects)
}
