package lobby

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

// RoomStore 是房间目录。
//
// 每次写入都把记录的寿命重置为 room TTL：relay 的报告（心跳）就是靠写入续期的；不再有人写，记录到期自己消失。
// 列表用的是按 game/mode 的索引，索引项与记录在同一次写入里增删（不在列表里的房间——已开始、满员、已关、
// relay 还没报告——写入时就从索引撤下）。记录过期不会撤索引项，List 读到时顺手清掉。
type RoomStore interface {
	// Create 存一个新房间；房间号已被占用时返回 false。
	Create(ctx context.Context, room Room) (bool, error)
	// Get 读一个房间。
	Get(ctx context.Context, id string) (Room, bool, error)
	// Update 在 compare-and-set 下修改一个房间：mutate 拿到的是当前值的副本，返回 save=false 时不写。
	// 房间不存在时返回 ErrRoomMissing。mutate 在冲突重试时会被再调用，必须只依赖它的参数。
	Update(ctx context.Context, id string, mutate func(room *Room) (save bool, err error)) (Room, error)
	// List 返回 game/mode 索引里的房间，新建的在前，最多 limit 个。返回的房间仍要由调用方按当前时刻再筛一次。
	List(ctx context.Context, game, mode string, limit int) ([]Room, error)
}

// copyRoom 让 mutate 改的是副本：Members 是切片，就地修改会改到存储读出来的那一份。
func copyRoom(room Room) Room {
	room.Members = slices.Clone(room.Members)
	room.Attrs = slices.Clone(room.Attrs)
	return room
}

// MemoryRooms 是进程内的 RoomStore，只用于单测与单进程调试：TTL 按注入的时钟算。
type MemoryRooms struct {
	mu    sync.Mutex
	ttl   time.Duration
	now   func() time.Time
	rooms map[string]memoryRoom
}

type memoryRoom struct {
	room      Room
	expiresAt time.Time
}

// NewMemoryRooms 返回内存房间目录。ttl 必须为正；now 为 nil 时取 time.Now。
func NewMemoryRooms(ttl time.Duration, now func() time.Time) (*MemoryRooms, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("lobby: room ttl must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &MemoryRooms{ttl: ttl, now: now, rooms: map[string]memoryRoom{}}, nil
}

func (m *MemoryRooms) live(id string) (memoryRoom, bool) {
	entry, ok := m.rooms[id]
	if ok && !m.now().Before(entry.expiresAt) {
		delete(m.rooms, id)
		return memoryRoom{}, false
	}
	return entry, ok
}

func (m *MemoryRooms) Create(_ context.Context, room Room) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, taken := m.live(room.ID); taken {
		return false, nil
	}
	m.rooms[room.ID] = memoryRoom{room: copyRoom(room), expiresAt: m.now().Add(m.ttl)}
	return true, nil
}

func (m *MemoryRooms) Get(_ context.Context, id string) (Room, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.live(id)
	return copyRoom(entry.room), ok, nil
}

func (m *MemoryRooms) Update(_ context.Context, id string, mutate func(*Room) (bool, error)) (Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.live(id)
	if !ok {
		return Room{}, ErrRoomMissing
	}
	next := copyRoom(entry.room)
	save, err := mutate(&next)
	if err != nil {
		return Room{}, err
	}
	if !save {
		return copyRoom(entry.room), nil
	}
	m.rooms[id] = memoryRoom{room: copyRoom(next), expiresAt: m.now().Add(m.ttl)}
	return next, nil
}

func (m *MemoryRooms) List(_ context.Context, game, mode string, limit int) ([]Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Room
	for id := range m.rooms {
		entry, ok := m.live(id)
		if ok && entry.room.Game == game && entry.room.Mode == mode && entry.room.listable() {
			out = append(out, copyRoom(entry.room))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAtUnix != out[j].CreatedAtUnix {
			return out[i].CreatedAtUnix > out[j].CreatedAtUnix
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var _ RoomStore = (*MemoryRooms)(nil)
