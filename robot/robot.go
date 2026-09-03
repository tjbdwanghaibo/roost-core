// Package robot is a virtual-client framework with two jobs: simulating
// real client logic (integration-style bots) and load testing. It was
// generalized from the cube robot service — the battle-tested pieces
// (session multiplexing, blocking scenario trees, goroutine-per-bot
// scheduling, load-test control plane) are ported as-is, while the pain
// points are addressed in the framework: typed blackboard keys instead of
// hand-written accessors, a convention-driven generic call action instead
// of per-protocol boilerplate, per-message latency histograms, staged
// executors and SLO thresholds.
//
// Layering: transport (wire) → session (multiplexing) → Context (one bot)
// → action/scenario (behavior) → runner (scheduling) → loadtest (control
// plane). Everything is protocol-agnostic: business installs a codec, a
// dialer and its message ids.
package robot

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
)

// ActionRunner executes one named blocking action for a robot.
type ActionRunner func(context.Context, *Context, string, any) error

// AuthProvider builds the gateway handshake packet for one robot (the
// connect action sends it right after dialing when configured). Games plug
// their token-signing here; the framework never sees credentials.
type AuthProvider func(rb *Context) (*transport.Packet, error)

// Config contains the per-robot immutable runtime data.
type Config struct {
	// ID is the robot's ordinal inside one run (1-based).
	ID int
	// PlayerID is the business identity, produced by the runner's
	// IdentityProvider.
	PlayerID  int64
	Transport transport.Config
	Protocols *protocol.Registry
	// Auth optionally builds the post-connect gateway handshake packet.
	Auth AuthProvider
	// Seed is the per-robot deterministic random seed (runner derives it
	// from the run seed and robot id) — scenario Random uses it so a run is
	// reproducible.
	Seed int64
}

// Context is the runtime handle passed into scenarios and actions.
type Context struct {
	Config

	Blackboard *Blackboard
	RunAction  ActionRunner

	mu      sync.RWMutex
	session *session.Session
	onClose []func() error
}

func NewContext(cfg Config) *Context {
	return &Context{
		Config:     cfg,
		Blackboard: NewBlackboard(),
	}
}

// Do executes one named action — the single entry point scenarios use.
func (c *Context) Do(ctx context.Context, name string, param any) error {
	if c == nil {
		return errors.New("robot: context is nil")
	}
	if c.RunAction == nil {
		return errors.New("robot: action runner is nil")
	}
	return c.RunAction(ctx, c, name, param)
}

func (c *Context) Session() *session.Session {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	s := c.session
	c.mu.RUnlock()
	return s
}

// SetSession installs a session, closing any previous one (reconnect).
func (c *Context) SetSession(s *session.Session) {
	if c == nil {
		return
	}
	c.mu.Lock()
	old := c.session
	c.session = s
	c.mu.Unlock()
	if old != nil && old != s {
		_ = old.Close()
	}
}

// AddCloseHook registers cleanup executed by Close in LIFO order (push
// handler unregisters, coalescer shutdowns).
func (c *Context) AddCloseHook(fn func() error) {
	if c == nil || fn == nil {
		return
	}
	c.mu.Lock()
	c.onClose = append(c.onClose, fn)
	c.mu.Unlock()
}

func (c *Context) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	hooks := append([]func() error(nil), c.onClose...)
	c.onClose = nil
	s := c.session
	c.mu.Unlock()

	var err error
	for i := len(hooks) - 1; i >= 0; i-- {
		if hooks[i] != nil {
			err = errors.Join(err, hooks[i]())
		}
	}

	c.mu.Lock()
	if c.session == s {
		c.session = nil
	}
	c.mu.Unlock()
	if s != nil {
		err = errors.Join(err, s.Close())
	}
	return err
}

// EnsurePushCapture idempotently installs a standing push handler under a
// blackboard key: the first call registers it (and hooks the unregister
// into Close), repeated calls are no-ops. This replaces the hand-written
// ensureXxxCapture family from cube.
func (c *Context) EnsurePushCapture(key string, msgID uint32, handler session.PushHandler) error {
	if c == nil || key == "" || handler == nil {
		return errors.New("robot: capture key and handler are required")
	}
	marker := "capture:" + key
	if _, exists := c.Blackboard.Get(marker); exists {
		return nil
	}
	s := c.Session()
	if s == nil {
		return session.ErrClosed
	}
	unregister := s.RegisterPushHandler(msgID, handler)
	c.Blackboard.Set(marker, true)
	c.AddCloseHook(func() error {
		unregister()
		c.Blackboard.Delete(marker)
		return nil
	})
	return nil
}

// Key is a typed blackboard accessor: business declares its state keys once
// (`var WorldBossID = robot.Key[int64]("worldboss_id")`) and gets typed
// Get/Set for free — replacing cube's 35 hand-written accessor methods.
// Zero values are stored and readable like any other value; use Clear to
// express "unset" (the old "0 means unset" convention is gone on purpose).
type TypedKey[T any] struct {
	name string
}

func Key[T any](name string) TypedKey[T] {
	return TypedKey[T]{name: name}
}

func (k TypedKey[T]) Name() string { return k.name }

func (k TypedKey[T]) Get(rb *Context) (T, bool) {
	var zero T
	if rb == nil || rb.Blackboard == nil || k.name == "" {
		return zero, false
	}
	raw, ok := rb.Blackboard.Get(k.name)
	if !ok {
		return zero, false
	}
	value, ok := raw.(T)
	if !ok {
		return zero, false
	}
	return value, true
}

func (k TypedKey[T]) GetOr(rb *Context, fallback T) T {
	if value, ok := k.Get(rb); ok {
		return value
	}
	return fallback
}

func (k TypedKey[T]) Set(rb *Context, value T) {
	if rb == nil || rb.Blackboard == nil {
		return
	}
	rb.Blackboard.Set(k.name, value)
}

func (k TypedKey[T]) Clear(rb *Context) {
	if rb == nil || rb.Blackboard == nil {
		return
	}
	rb.Blackboard.Delete(k.name)
}

// Coalescer batches keyed acknowledgements: entries with the same key are
// deduplicated (keeping the highest sequence), then flushed together after
// the debounce window. Generalized from cube's entity-sync ACK coalescer —
// the mechanism that keeps thousands of bots from generating an ack storm.
type Coalescer[K comparable] struct {
	interval time.Duration
	flush    func(map[K]int64)

	mu      sync.Mutex
	pending map[K]int64
	running bool
	closed  bool
	wake    chan struct{}
	done    chan struct{}
}

// NewCoalescer builds a coalescer flushing at most every interval (<= 0
// selects 100ms). flush receives the drained batch (key -> highest seq).
func NewCoalescer[K comparable](interval time.Duration, flush func(map[K]int64)) *Coalescer[K] {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	return &Coalescer[K]{
		interval: interval,
		flush:    flush,
		pending:  make(map[K]int64),
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// Add records one acknowledgement; a higher seq for the same key wins.
func (c *Coalescer[K]) Add(key K, seq int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if existing, ok := c.pending[key]; !ok || seq > existing {
		c.pending[key] = seq
	}
	if !c.running {
		c.running = true
		go c.worker()
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Coalescer[K]) worker() {
	timer := time.NewTimer(c.interval)
	defer timer.Stop()
	for {
		select {
		case <-c.done:
			c.drain()
			return
		case <-c.wake:
			// keep accumulating until the debounce window elapses
		case <-timer.C:
			if !c.drain() {
				c.mu.Lock()
				if len(c.pending) == 0 {
					c.running = false
					c.mu.Unlock()
					return
				}
				c.mu.Unlock()
			}
			timer.Reset(c.interval)
		}
	}
}

func (c *Coalescer[K]) drain() bool {
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return false
	}
	batch := c.pending
	c.pending = make(map[K]int64)
	c.mu.Unlock()
	if c.flush != nil {
		c.flush(batch)
	}
	return true
}

// Close flushes any pending batch and stops the worker.
func (c *Coalescer[K]) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	running := c.running
	c.mu.Unlock()
	if running {
		close(c.done)
	} else {
		c.drain()
	}
	return nil
}

// BoundedQueue is a drop-oldest bounded queue: enqueue never blocks the
// producer (a session read loop must never stall on a slow consumer), the
// oldest entry is discarded when full. Generalized from cube's push-capture
// queue.
type BoundedQueue[T any] struct {
	ch chan T
}

func NewBoundedQueue[T any](capacity int) *BoundedQueue[T] {
	if capacity <= 0 {
		capacity = 64
	}
	return &BoundedQueue[T]{ch: make(chan T, capacity)}
}

// Push enqueues value, dropping the oldest entry when full. It reports
// whether an old entry was dropped.
func (q *BoundedQueue[T]) Push(value T) (dropped bool) {
	for {
		select {
		case q.ch <- value:
			return dropped
		default:
			select {
			case <-q.ch:
				dropped = true
			default:
			}
		}
	}
}

// Pop dequeues with context cancellation.
func (q *BoundedQueue[T]) Pop(ctx context.Context) (T, error) {
	var zero T
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case value := <-q.ch:
		return value, nil
	}
}

// TryPop dequeues without blocking.
func (q *BoundedQueue[T]) TryPop() (T, bool) {
	var zero T
	select {
	case value := <-q.ch:
		return value, true
	default:
		return zero, false
	}
}

// Len is the current queue depth.
func (q *BoundedQueue[T]) Len() int { return len(q.ch) }
