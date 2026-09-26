package entitysync

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

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
	update    *capturedUpdate
}

// encodedFrame 保存这一帧准入后的会话状态。一次 tick 拆成多帧时，
// 每个成功的 Push 都有独立提交点，后续帧失败不能撤回已交付的时钟和引用。
type encodedFrame struct {
	payload                   []byte
	creates, updates, removes uint64
	next                      *session
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
type sessionLifetime struct {
	traceID uint64
	// subjects 是订阅意图的反向索引，包含待快照与待删除项。
	// 仅 Manager.mu 保护；编码副本共享生命周期，但不能读写此表。
	subjects map[int64]*subject
}

type session struct {
	lifetime *sessionLifetime
	id       SessionID
	epoch    uint32
	tick     uint32

	objects     map[int64]frame.ObjectRef // subject → the ref this session knows it by
	generations map[uint16]uint16
	free        []uint16
	next        uint16

	// held: open, subscribing, but not yet receiving. Nothing is encoded for
	// a held session; the policy says ReadySession when the client can take
	// frames (it has installed its decoder), and the first frame after that
	// is a FrameFull in a fresh epoch.
	held bool

	framesSent    uint64
	snapshotAfter [2]int64 // 新入场/恢复分别轮转，避免另一类改变公平顺序
	// 仅编码副本可写；已发布的引用表始终只读。
	referencesOwned bool
}

// clone 复制会话供编码使用；准入失败保留最后成功帧的状态，未交付的后缀不生效。
func (s *session) clone() *session {
	next := *s
	next.referencesOwned = false
	return &next
}

// copyReferences 在首次 create/remove 前复制。纯 update 只推进独立的会话时钟。
func (s *session) copyReferences() {
	if s.referencesOwned {
		return
	}
	s.objects = maps.Clone(s.objects)
	s.generations = maps.Clone(s.generations)
	s.free = append([]uint16(nil), s.free...)
	s.referencesOwned = true
}

func newSession(id SessionID) *session {
	return &session{lifetime: &sessionLifetime{subjects: make(map[int64]*subject)}, id: id, epoch: 1, next: 1, objects: make(map[int64]frame.ObjectRef), generations: make(map[uint16]uint16)}
}

func (s *session) allocate(subjectID int64, maxObjects int) (frame.ObjectRef, error) {
	if len(s.objects) >= maxObjects {
		return frame.ObjectRef{}, frame.ErrObjectLimit
	}
	s.copyReferences()
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
	s.copyReferences()
	delete(s.objects, subjectID)
	s.free = append(s.free, ref.ID)
}

// encode 按完整实体包边界组帧；对象数或字节容量不足就开启下一帧，
// 不切开实体 payload。每帧独立推进会话时钟。 The first frame a session ever gets is a FrameFull (it can
// only contain creates: nothing is live yet); every later frame is a
// FrameDelta based on the previous tick.
//
// 编码失败可能是引用表不一致或单实体包超硬上限；关闭受影响会话，
// 由业务修正配置/数据并重新订阅，不能截断内容继续发送。
func (s *session) encode(entries []frameEntry, cfg wireConfig) ([]encodedFrame, error) {
	// 先释放退出视野的对象，再为新对象分配引用。MaxObjects 限制持有量，
	// 不能让同一 tick 的合法替换因为新对象 ID 更小而暂时超额。
	slices.SortFunc(entries, func(a, b frameEntry) int {
		if (a.kind == entryRemove) != (b.kind == entryRemove) {
			if a.kind == entryRemove {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.subjectID, b.subjectID)
	})
	perFrame := cfg.limits.MaxObjects
	if perFrame <= 0 {
		perFrame = frame.DefaultLimits().MaxObjects
	}
	maxBytes := cfg.limits.MaxFrameBytes
	if maxBytes <= 0 {
		maxBytes = frame.DefaultLimits().MaxFrameBytes
	}
	// 工作区只在本次 encode 内复用；Encode 返回的字节和逐帧会话状态独立。
	capacity := min(len(entries), perFrame)
	objects := make([]frame.ObjectDelta, 0, capacity)
	components := make([]frame.ComponentDelta, capacity)
	var frames []encodedFrame
	counts := encodedFrame{}
	size := 32 // frame 协议头
	finish := func() error {
		if len(objects) == 0 {
			return nil
		}
		out := frame.Frame{
			SnapshotMeta: frame.SnapshotMeta{RoomID: wireStream, Epoch: s.epoch, Tick: s.tick + 1, SchemaVersion: cfg.schemaVersion},
			Kind:         frame.Delta, BaseTick: s.tick, Objects: objects,
		}
		if s.tick == 0 {
			out.Kind, out.BaseTick = frame.Full, 0
		}
		data, err := frame.Encode(out, cfg.limits)
		if err != nil {
			return err
		}
		s.tick++
		s.framesSent++
		counts.payload, counts.next = data, s
		frames = append(frames, counts)
		return nil
	}
	for _, entry := range entries {
		if entry.kind == entryRemove {
			if _, exists := s.objects[entry.subjectID]; !exists {
				continue
			}
		}
		entryBytes := 9 // 完整 ObjectDelta 的协议头
		if entry.kind != entryRemove {
			data, err := entry.update.encode(cfg.limits.MaxComponentBytes)
			if err != nil {
				return nil, err
			}
			entryBytes += 9 + len(data)
		}
		if entryBytes > maxBytes-32 {
			return nil, fmt.Errorf("%w: complete entity %d needs %d bytes", frame.ErrFrameTooLarge, entry.subjectID, entryBytes+32)
		}
		if len(objects) > 0 && (len(objects) == perFrame || entryBytes > maxBytes-size) {
			if err := finish(); err != nil {
				return nil, err
			}
			// 下一帧不能改写前一帧已经保存的引用表。
			s = s.clone()
			clear(objects)
			clear(components)
			objects, size, counts = objects[:0], 32, encodedFrame{}
		}
		object, ok, err := s.objectFor(entry, cfg, components[len(objects):len(objects)+1])
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if s.tick == 0 && object.Operation != frame.ObjectCreate {
			return nil, fmt.Errorf("%w: session %d owes a %v before its baseline", ErrWireFrame, s.id, entry.kind)
		}
		switch object.Operation {
		case frame.ObjectCreate:
			counts.creates++
		case frame.ObjectUpdate:
			counts.updates++
		case frame.ObjectRemove:
			counts.removes++
		}
		objects = append(objects, object)
		size += entryBytes
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return frames, nil
}

// objectFor renders one entry against this session's reference table. A
// remove for an object the session never held is nothing (ok=false).
func (s *session) objectFor(entry frameEntry, cfg wireConfig, component []frame.ComponentDelta) (frame.ObjectDelta, bool, error) {
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
		data, err := entry.update.encode(cfg.limits.MaxComponentBytes)
		if err != nil {
			return frame.ObjectDelta{}, false, err
		}
		component[0] = frame.ComponentDelta{Operation: frame.ComponentSet, TypeID: cfg.componentType, SchemaVersion: cfg.componentSchema, Data: data}
		return frame.ObjectDelta{Operation: operation, Ref: ref, Archetype: cfg.archetype, Components: component}, true, nil
	case entryUpdate:
		if !exists {
			return frame.ObjectDelta{}, false, fmt.Errorf("%w: session %d has no baseline for subject %d", ErrWireFrame, s.id, entry.subjectID)
		}
		data, err := entry.update.encode(cfg.limits.MaxComponentBytes)
		if err != nil {
			return frame.ObjectDelta{}, false, err
		}
		component[0] = frame.ComponentDelta{Operation: frame.ComponentSet, TypeID: cfg.componentType, SchemaVersion: cfg.componentSchema, Data: data}
		return frame.ObjectDelta{Operation: frame.ObjectUpdate, Ref: ref, Components: component}, true, nil
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
