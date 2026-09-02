package room

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/health"
)

var (
	ErrRoomManagerNotRunning = errors.New("room: room manager is not running")
	ErrRoomManagerStopped    = errors.New("room: room manager is stopped")
	ErrRoomLimit             = errors.New("room: room capacity exceeded")
	ErrRoomAlreadyExists     = errors.New("room: room already exists")
	ErrRoomNotFound          = errors.New("room: room not found")
	ErrRoomClosing           = errors.New("room: room is closing")
)

type RoomManagerConfig struct {
	Downstream            ReliableRoomFrameSink
	MaxRooms              int
	MaxTotalSubjects      int
	MaxTotalSubscribers   int
	MaxSubjectsPerRoom    int
	MaxSubscribersPerRoom int
	ReplicationInterval   time.Duration
	IdleTTL               time.Duration
	SweepInterval         time.Duration
	CloseTimeout          time.Duration
}

type RoomManager struct {
	mu            sync.RWMutex
	closeMu       sync.Mutex
	config        RoomManagerConfig
	rooms         map[int64]*managedRoom
	budget        *roomResourceBudget
	running       bool
	stopped       bool
	cancel        context.CancelFunc
	done          chan struct{}
	closeComplete bool
	closeErr      error

	created     atomic.Uint64
	removed     atomic.Uint64
	idleExpired atomic.Uint64
}

type managedRoom struct {
	room         *RoomBroadcaster
	lastActivity atomic.Int64
	closing      bool
}

type RoomManagerStats struct {
	ActiveRooms       int
	ActiveSubjects    int64
	ActiveSubscribers int64
	Created           uint64
	Removed           uint64
	IdleExpired       uint64
	MaxRooms          int
	MaxSubjects       int64
	MaxSubscribers    int64
}

func NewRoomManager(config RoomManagerConfig) (*RoomManager, error) {
	if config.Downstream == nil {
		return nil, ErrRoomFrameSinkRequired
	}
	if config.MaxRooms <= 0 {
		config.MaxRooms = 1024
	}
	if config.MaxSubjectsPerRoom <= 0 {
		config.MaxSubjectsPerRoom = 100
	}
	if config.MaxSubscribersPerRoom <= 0 {
		config.MaxSubscribersPerRoom = 100
	}
	if config.MaxTotalSubjects <= 0 {
		var err error
		config.MaxTotalSubjects, err = roomCapacityProduct(config.MaxRooms, config.MaxSubjectsPerRoom)
		if err != nil {
			return nil, err
		}
	}
	if config.MaxTotalSubscribers <= 0 {
		var err error
		config.MaxTotalSubscribers, err = roomCapacityProduct(config.MaxRooms, config.MaxSubscribersPerRoom)
		if err != nil {
			return nil, err
		}
	}
	if config.ReplicationInterval <= 0 {
		config.ReplicationInterval = DefaultRoomBroadcastInterval
	}
	if config.IdleTTL <= 0 {
		config.IdleTTL = 5 * time.Minute
	}
	if config.SweepInterval <= 0 {
		config.SweepInterval = min(config.IdleTTL/2, 30*time.Second)
	}
	if config.CloseTimeout <= 0 {
		config.CloseTimeout = 5 * time.Second
	}
	return &RoomManager{
		config: config,
		rooms:  make(map[int64]*managedRoom),
		budget: newRoomResourceBudget(config.MaxTotalSubjects, config.MaxTotalSubscribers),
	}, nil
}

func (m *RoomManager) Start(ctx context.Context) error {
	if m == nil {
		return ErrRoomManagerNotRunning
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrRoomManagerStopped
	}
	if m.running {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	m.running = true
	go m.run(runCtx, m.done)
	return nil
}

func (m *RoomManager) Create(roomID int64) (*RoomBroadcaster, error) {
	if m == nil || roomID <= 0 {
		return nil, ErrRoomIDInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, ErrRoomManagerStopped
	}
	if !m.running {
		return nil, ErrRoomManagerNotRunning
	}
	if _, exists := m.rooms[roomID]; exists {
		return nil, ErrRoomAlreadyExists
	}
	if len(m.rooms) >= m.config.MaxRooms {
		return nil, ErrRoomLimit
	}
	entry := &managedRoom{}
	entry.lastActivity.Store(time.Now().UnixNano())
	room, err := NewRoomBroadcaster(roomID, m.config.Downstream, RoomBroadcasterConfig{
		MaxSubjects: m.config.MaxSubjectsPerRoom, MaxSubscribers: m.config.MaxSubscribersPerRoom,
		budget: m.budget, onActivity: func() { entry.lastActivity.Store(time.Now().UnixNano()) },
	})
	if err != nil {
		return nil, err
	}
	if err := room.Start(context.Background(), m.config.ReplicationInterval); err != nil {
		_ = room.Close(context.Background())
		return nil, err
	}
	entry.room = room
	m.rooms[roomID] = entry
	m.created.Add(1)
	return room, nil
}

func (m *RoomManager) Get(roomID int64) (*RoomBroadcaster, bool) {
	if m == nil || roomID <= 0 {
		return nil, false
	}
	m.mu.RLock()
	entry := m.rooms[roomID]
	if m.stopped || !m.running || entry != nil && (entry.closing || entry.room.isStopped()) {
		entry = nil
	}
	m.mu.RUnlock()
	if entry == nil {
		return nil, false
	}
	entry.lastActivity.Store(time.Now().UnixNano())
	return entry.room, true
}

func (m *RoomManager) GetOrCreate(roomID int64) (*RoomBroadcaster, error) {
	if room, ok := m.Get(roomID); ok {
		return room, nil
	}
	room, err := m.Create(roomID)
	if errors.Is(err, ErrRoomAlreadyExists) {
		if room, ok := m.Get(roomID); ok {
			return room, nil
		}
	}
	return room, err
}

func (m *RoomManager) Remove(ctx context.Context, roomID int64) error {
	if m == nil || roomID <= 0 {
		return ErrRoomIDInvalid
	}
	m.mu.Lock()
	entry := m.rooms[roomID]
	if entry != nil && entry.closing {
		m.mu.Unlock()
		return ErrRoomClosing
	}
	if entry != nil {
		entry.closing = true
	}
	m.mu.Unlock()
	if entry == nil {
		return ErrRoomNotFound
	}
	err := entry.room.Close(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		m.mu.Lock()
		if m.rooms[roomID] == entry {
			entry.closing = false
		}
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	removed := false
	if m.rooms[roomID] == entry {
		delete(m.rooms, roomID)
		removed = true
	}
	m.mu.Unlock()
	if removed {
		m.removed.Add(1)
	}
	return err
}

func (m *RoomManager) Close(ctx context.Context) (err error) {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.mu.Lock()
	if m.closeComplete {
		err = m.closeErr
		m.mu.Unlock()
		return err
	}
	m.stopped = true
	m.running = false
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	m.mu.RLock()
	ids := make([]int64, 0, len(m.rooms))
	for roomID := range m.rooms {
		ids = append(ids, roomID)
	}
	m.mu.RUnlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, roomID := range ids {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(m.closeErr, ctxErr)
		}
		m.mu.Lock()
		entry := m.rooms[roomID]
		if entry != nil {
			entry.closing = true
		}
		m.mu.Unlock()
		if entry == nil {
			continue
		}
		closeErr := entry.room.Close(ctx)
		if errors.Is(closeErr, context.Canceled) || errors.Is(closeErr, context.DeadlineExceeded) {
			m.mu.Lock()
			if m.rooms[roomID] == entry {
				entry.closing = false
			}
			m.mu.Unlock()
			return errors.Join(m.closeErr, closeErr)
		}
		m.mu.Lock()
		if m.rooms[roomID] == entry {
			delete(m.rooms, roomID)
			m.removed.Add(1)
		}
		m.closeErr = errors.Join(m.closeErr, closeErr)
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.closeComplete = len(m.rooms) == 0
	err = m.closeErr
	m.mu.Unlock()
	return err
}

func (m *RoomManager) Stats() RoomManagerStats {
	if m == nil {
		return RoomManagerStats{}
	}
	m.mu.RLock()
	rooms := len(m.rooms)
	m.mu.RUnlock()
	return RoomManagerStats{
		ActiveRooms: rooms, ActiveSubjects: m.budget.subjects.Load(), ActiveSubscribers: m.budget.subscribers.Load(),
		Created: m.created.Load(), Removed: m.removed.Load(), IdleExpired: m.idleExpired.Load(),
		MaxRooms: m.config.MaxRooms, MaxSubjects: m.budget.maxSubjects, MaxSubscribers: m.budget.maxSubscribers,
	}
}

func (m *RoomManager) CheckHealth(context.Context) health.Result {
	if m == nil {
		return health.Result{Status: health.StatusFail, Message: "room manager is nil"}
	}
	m.mu.RLock()
	running, stopped := m.running, m.stopped
	m.mu.RUnlock()
	stats := m.Stats()
	message := fmt.Sprintf("rooms=%d/%d subjects=%d/%d subscribers=%d/%d idle_expired=%d", stats.ActiveRooms, stats.MaxRooms, stats.ActiveSubjects, stats.MaxSubjects, stats.ActiveSubscribers, stats.MaxSubscribers, stats.IdleExpired)
	if stopped || !running {
		return health.Result{Status: health.StatusFail, Message: message}
	}
	if stats.ActiveRooms >= stats.MaxRooms || stats.ActiveSubjects >= stats.MaxSubjects || stats.ActiveSubscribers >= stats.MaxSubscribers {
		return health.Result{Status: health.StatusFail, Message: message}
	}
	if stats.ActiveRooms*10 >= stats.MaxRooms*8 || stats.ActiveSubjects*10 >= stats.MaxSubjects*8 || stats.ActiveSubscribers*10 >= stats.MaxSubscribers*8 {
		return health.Result{Status: health.StatusDegraded, Message: message}
	}
	return health.Result{Status: health.StatusOK, Message: message}
}

func (m *RoomManager) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(m.config.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.expireIdle(now)
		}
	}
}

func (m *RoomManager) expireIdle(now time.Time) {
	cutoff := now.Add(-m.config.IdleTTL).UnixNano()
	type candidate struct {
		id    int64
		entry *managedRoom
	}
	m.mu.Lock()
	ids := make([]int64, 0)
	for id, entry := range m.rooms {
		stats := entry.room.Stats()
		if !entry.closing && entry.lastActivity.Load() <= cutoff && stats.ActiveSubjects == 0 && stats.ActiveSubscribers == 0 && stats.PendingSubjects == 0 && stats.PendingRetirements == 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	entries := make([]candidate, 0, len(ids))
	for _, id := range ids {
		entry := m.rooms[id]
		entry.closing = true
		entries = append(entries, candidate{id: id, entry: entry})
	}
	m.mu.Unlock()
	for _, item := range entries {
		ctx, cancel := context.WithTimeout(context.Background(), m.config.CloseTimeout)
		err := item.entry.room.Close(ctx)
		cancel()
		m.mu.Lock()
		if m.rooms[item.id] == item.entry {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				item.entry.closing = false
				m.mu.Unlock()
				continue
			}
			delete(m.rooms, item.id)
		}
		m.mu.Unlock()
		m.idleExpired.Add(1)
		m.removed.Add(1)
	}
}

type roomResourceBudget struct {
	maxSubjects    int64
	maxSubscribers int64
	subjects       atomic.Int64
	subscribers    atomic.Int64
}

func newRoomResourceBudget(subjects, subscribers int) *roomResourceBudget {
	return &roomResourceBudget{maxSubjects: int64(subjects), maxSubscribers: int64(subscribers)}
}

func roomCapacityProduct(left, right int) (int, error) {
	maxInt := int(^uint(0) >> 1)
	if left <= 0 || right <= 0 || left > maxInt/right {
		return 0, fmt.Errorf("room: room capacity product overflows: %d * %d", left, right)
	}
	return left * right, nil
}

func reserveRoomResource(counter *atomic.Int64, limit int64) bool {
	for {
		current := counter.Load()
		if current >= limit {
			return false
		}
		if counter.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (b *roomResourceBudget) reserveSubject() bool {
	return reserveRoomResource(&b.subjects, b.maxSubjects)
}
func (b *roomResourceBudget) reserveSubscriber() bool {
	return reserveRoomResource(&b.subscribers, b.maxSubscribers)
}
func (b *roomResourceBudget) releaseSubject()    { b.releaseSubjects(1) }
func (b *roomResourceBudget) releaseSubscriber() { b.releaseSubscribers(1) }
func (b *roomResourceBudget) releaseSubjects(count int) {
	if count > 0 {
		b.subjects.Add(-int64(count))
	}
}
func (b *roomResourceBudget) releaseSubscribers(count int) {
	if count > 0 {
		b.subscribers.Add(-int64(count))
	}
}
