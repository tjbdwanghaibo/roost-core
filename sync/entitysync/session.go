package entitysync

import (
	"fmt"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// entryKind is what one subject contributes to one session's frame.
type entryKind uint8

const (
	entryCreate entryKind = iota + 1 // a full snapshot: ObjectCreate, or ObjectUpdate when the object is already held
	entryUpdate                      // a delta from the version the session holds
	entryRemove                      // the subject left the session's view
)

type frameEntry struct {
	subjectID int64
	kind      entryKind
	update    entity.SubjectSyncUpdate
}

// wireConfig is the fixed part of every frame.
type wireConfig struct {
	limits          frame.Limits
	schemaVersion   uint16
	archetype       uint16
	componentType   uint16
	componentSchema uint16
}

// session is everything the manager knows about one receiver: its wire clock
// and the object references it has been given. Nothing here refers to a room
// or to any other session — a session's frames are its own (ARCH-10).
type session struct {
	id    SessionID
	epoch uint32
	tick  uint32

	objects     map[int64]frame.ObjectRef // subject → the ref this session knows it by
	generations map[uint16]uint16
	free        []uint16
	next        uint16

	// held: open, subscribing, but not yet receiving. Nothing is encoded for
	// a held session; the policy says ReadySession when the client can take
	// frames (it has installed its decoder), and the first frame after that
	// is a FrameFull in a fresh epoch.
	held bool

	framesSent uint64
}

// clone copies the session so a tick can encode against a scratch copy and
// keep it only once the frames were admitted: a transport that asks for a
// retry must find the session exactly as it was.
func (s *session) clone() *session {
	next := &session{id: s.id, epoch: s.epoch, tick: s.tick, next: s.next, framesSent: s.framesSent, held: s.held,
		objects: make(map[int64]frame.ObjectRef, len(s.objects)), generations: make(map[uint16]uint16, len(s.generations)),
		free: append([]uint16(nil), s.free...)}
	for k, v := range s.objects {
		next.objects[k] = v
	}
	for k, v := range s.generations {
		next.generations[k] = v
	}
	return next
}

func newSession(id SessionID) *session {
	return &session{id: id, epoch: 1, next: 1, objects: make(map[int64]frame.ObjectRef), generations: make(map[uint16]uint16)}
}

func (s *session) allocate(subjectID int64, maxObjects int) (frame.ObjectRef, error) {
	if len(s.objects) >= maxObjects {
		return frame.ObjectRef{}, frame.ErrObjectLimit
	}
	var id uint16
	if len(s.free) != 0 {
		id = s.free[len(s.free)-1]
		s.free = s.free[:len(s.free)-1]
	} else {
		id = s.next
		if id == 0 {
			return frame.ObjectRef{}, frame.ErrObjectLimit
		}
		s.next++
	}
	generation := s.generations[id] + 1
	if generation == 0 {
		generation = 1
	}
	s.generations[id] = generation
	ref := frame.ObjectRef{ID: id, Generation: generation}
	s.objects[subjectID] = ref
	return ref, nil
}

func (s *session) release(subjectID int64) {
	ref, exists := s.objects[subjectID]
	if !exists {
		return
	}
	delete(s.objects, subjectID)
	s.free = append(s.free, ref.ID)
}

// encode turns this tick's entries into frames — one, or several when there
// are more objects than a frame may carry — and advances the session's clock
// once per frame. The first frame a session ever gets is a FrameFull (it can
// only contain creates: nothing is live yet); every later frame is a
// FrameDelta based on the previous tick.
//
// An error here is the session's: its reference table is inconsistent with
// what the manager believes it was sent, and the only safe answer is to close
// it and let the policy subscribe it afresh.
func (s *session) encode(entries []frameEntry, cfg wireConfig) ([][]byte, error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].subjectID < entries[j].subjectID })
	perFrame := cfg.limits.MaxObjects
	if perFrame <= 0 {
		perFrame = frame.DefaultLimits().MaxObjects
	}
	var frames [][]byte
	for start := 0; start < len(entries); start += perFrame {
		chunk := entries[start:min(start+perFrame, len(entries))]
		tick := s.tick + 1
		out := frame.Frame{
			SnapshotMeta: frame.SnapshotMeta{RoomID: wireStream, Epoch: s.epoch, Tick: tick, SchemaVersion: cfg.schemaVersion},
			Kind:         frame.Delta, BaseTick: s.tick,
			Objects: make([]frame.ObjectDelta, 0, len(chunk)),
		}
		if s.tick == 0 {
			out.Kind, out.BaseTick = frame.Full, 0
		}
		for _, entry := range chunk {
			object, ok, err := s.objectFor(entry, cfg)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if out.Kind == frame.Full && object.Operation != frame.ObjectCreate {
				return nil, fmt.Errorf("%w: session %d owes a %v for subject %d before it has anything", ErrWireFrame, s.id, entry.kind, entry.subjectID)
			}
			out.Objects = append(out.Objects, object)
		}
		if len(out.Objects) == 0 {
			continue
		}
		data, err := frame.Encode(out, cfg.limits)
		if err != nil {
			return nil, err
		}
		s.tick = tick
		s.framesSent++
		frames = append(frames, data)
	}
	return frames, nil
}

// objectFor renders one entry against this session's reference table. A
// remove for an object the session never held is nothing (ok=false).
func (s *session) objectFor(entry frameEntry, cfg wireConfig) (frame.ObjectDelta, bool, error) {
	ref, exists := s.objects[entry.subjectID]
	switch entry.kind {
	case entryCreate:
		operation := frame.ObjectUpdate
		if !exists {
			var err error
			ref, err = s.allocate(entry.subjectID, cfg.limits.MaxObjects)
			if err != nil {
				return frame.ObjectDelta{}, false, err
			}
			operation = frame.ObjectCreate
		}
		data, err := EncodeSubjectUpdate(entry.update, cfg.limits.MaxComponentBytes)
		if err != nil {
			return frame.ObjectDelta{}, false, err
		}
		return frame.ObjectDelta{Operation: operation, Ref: ref, Archetype: cfg.archetype, Components: []frame.ComponentDelta{{
			Operation: frame.ComponentSet, TypeID: cfg.componentType, SchemaVersion: cfg.componentSchema, Data: data,
		}}}, true, nil
	case entryUpdate:
		if !exists {
			return frame.ObjectDelta{}, false, fmt.Errorf("%w: session %d has no baseline for subject %d", ErrWireFrame, s.id, entry.subjectID)
		}
		data, err := EncodeSubjectUpdate(entry.update, cfg.limits.MaxComponentBytes)
		if err != nil {
			return frame.ObjectDelta{}, false, err
		}
		return frame.ObjectDelta{Operation: frame.ObjectUpdate, Ref: ref, Components: []frame.ComponentDelta{{
			Operation: frame.ComponentSet, TypeID: cfg.componentType, SchemaVersion: cfg.componentSchema, Data: data,
		}}}, true, nil
	case entryRemove:
		if !exists {
			return frame.ObjectDelta{}, false, nil
		}
		s.release(entry.subjectID)
		return frame.ObjectDelta{Operation: frame.ObjectRemove, Ref: ref}, true, nil
	}
	return frame.ObjectDelta{}, false, fmt.Errorf("%w: unknown entry kind %d", ErrWireFrame, entry.kind)
}

func (k entryKind) String() string {
	switch k {
	case entryCreate:
		return "create"
	case entryUpdate:
		return "update"
	case entryRemove:
		return "remove"
	}
	return fmt.Sprintf("entryKind(%d)", uint8(k))
}
