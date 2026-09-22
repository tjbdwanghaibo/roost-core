package policy

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/entitysync"
)

// RoomConfig shapes a Room.
type RoomConfig struct {
	// Manager receives the subscriptions. Required.
	Manager *entitysync.Manager
	// Profile every member subscribes with. Zero is the default profile.
	Profile entity.SyncProfile
	// Session names a member's session. Nil means the session id IS the
	// member id.
	Session func(member int64) entitysync.SessionID
	// MaxSubjects and MaxMembers bound the room. Zero means unbounded (the
	// manager's own limits still apply).
	MaxSubjects int
	MaxMembers  int
}

var (
	ErrRoomClosed         = errors.New("policy: room is closed")
	ErrRoomSubjectLimit   = errors.New("policy: room subject limit reached")
	ErrRoomMemberLimit    = errors.New("policy: room member limit reached")
	ErrRoomSubjectPresent = errors.New("policy: subject is already in the room")
	ErrRoomMemberPresent  = errors.New("policy: member is already in the room")
	ErrRoomNotPresent     = errors.New("policy: not in the room")
)

// Room is the all-to-all policy: every member receives every subject in the
// room, wherever they are. A lobby, a battle, an instance. Subjects and
// members are separate sets — a spectator is a member and not a subject, a
// scripted actor is a subject and not a member — and a player is usually
// both, added twice.
type Room struct {
	mu       sync.Mutex
	config   RoomConfig
	session  func(int64) entitysync.SessionID
	subjects map[int64]struct{}
	members  map[int64]struct{}
	closed   bool
}

func NewRoom(config RoomConfig) (*Room, error) {
	if config.Manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	session := config.Session
	if session == nil {
		session = func(member int64) entitysync.SessionID { return entitysync.SessionID(member) }
	}
	return &Room{config: config, session: session, subjects: make(map[int64]struct{}), members: make(map[int64]struct{})}, nil
}

// AddSubject registers a subject with the manager and subscribes every
// current member to it.
func (r *Room) AddSubject(state *entity.SubjectSyncState) error {
	if state == nil {
		return entitysync.ErrSubjectInvalid
	}
	id := state.SubjectID()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRoomClosed
	}
	if _, present := r.subjects[id]; present {
		return ErrRoomSubjectPresent
	}
	if r.config.MaxSubjects > 0 && len(r.subjects) >= r.config.MaxSubjects {
		return ErrRoomSubjectLimit
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
func (r *Room) RemoveSubject(id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.subjects[id]; !present {
		return ErrRoomNotPresent
	}
	delete(r.subjects, id)
	if err := r.config.Manager.Unregister(id); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
		return err
	}
	return nil
}

// Join makes a session a member: it receives every subject in the room. The
// session must already be open with the manager.
func (r *Room) Join(member int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRoomClosed
	}
	if _, present := r.members[member]; present {
		return ErrRoomMemberPresent
	}
	if r.config.MaxMembers > 0 && len(r.members) >= r.config.MaxMembers {
		return ErrRoomMemberLimit
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

// Leave takes a member out: it stops receiving the room's subjects.
func (r *Room) Leave(member int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.members[member]; !present {
		return ErrRoomNotPresent
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
// stay open — they are the transport's, not the room's.
func (r *Room) Close() error {
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

// Members and Subjects report the room's size.
func (r *Room) Members() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.members)
}

func (r *Room) Subjects() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subjects)
}
