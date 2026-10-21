package fctx

import (
	stdctx "context"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"sync"
	"time"
)

// Context is the per-goroutine request context.
// It carries request-scoped metadata for logging, timing, and config access.
type Context struct {
	Now         time.Time
	NowMilli    int64
	Config      any
	Base        stdctx.Context
	Frame       uint64
	Meta        RequestMeta
	Trace       TraceMeta
	SyncWait    time.Duration
	keyValue    map[any]any
	frozen      []frozenValue // 本次执行冻结的派生值（Frozen），与 Now 同生命期、不进 Snapshot
	longWorker  bool
	ioWorker    bool
	shortWorker bool // 当前 goroutine 的执行位置，不进入 Snapshot
}

// frozenValue 是 Frozen 记下的一个值。
type frozenValue struct {
	key   any
	value any
}

// ContextSnapshot 用于框架控制的同步交接。异步 Nest 只带 Meta/Trace，并在入队时
// 固定当前 RuntimeConfig；普通 worker 任务不传执行上下文。业务数据显式放入参数，
// Now/NowMilli 不传递，每次执行重新固定业务时间。
type ContextSnapshot struct {
	Valid    bool
	Config   any
	Base     stdctx.Context
	Frame    uint64
	Meta     RequestMeta
	Trace    TraceMeta
	SyncWait time.Duration
	KeyValue map[any]any
}

func (s ContextSnapshot) Clone() ContextSnapshot {
	s.Trace = s.Trace.Clone()
	s.KeyValue = cloneKeyValue(s.KeyValue)
	return s
}

type TraceMeta struct {
	TraceID string
	Enabled bool
	Reason  string
	Sampled bool
	Tags    map[string]string
}

func (t TraceMeta) Active() bool {
	return t.Enabled && t.TraceID != ""
}

func (t TraceMeta) Clone() TraceMeta {
	t.Tags = cloneTraceTags(t.Tags)
	return t
}

type RequestMeta struct {
	Source   string
	Handler  string
	PlayerID int64
	MsgID    uint32
	Seq      uint32
}

func InNestHandler() bool {
	c := CurrentContext()
	return c != nil && c.Meta.Source == "nest"
}

type Option func(*Context)

var contextPool = sync.Pool{
	New: func() interface{} {
		return &Context{}
	},
}

var (
	runtimeConfigMu sync.RWMutex
	runtimeConfig   any
)

// NewContext creates a request Context, binds it to the current goroutine, and returns a release function.
func NewContext(opts ...Option) (*Context, func()) {
	prev := CurrentContext()
	c := contextPool.Get().(*Context)
	c.init()
	c.shortWorker = prev != nil && prev.shortWorker
	c.longWorker = prev != nil && prev.longWorker
	c.ioWorker = prev != nil && prev.ioWorker
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	StoreContext(c)
	released := false
	return c, func() {
		if released {
			return
		}
		released = true
		c.Clear()
		if prev != nil {
			StoreContext(prev)
		} else {
			DeleteContext()
		}
		contextPool.Put(c)
	}
}

func (c *Context) init() {
	c.Now = clock.Now()
	c.NowMilli = c.Now.UnixMilli()
	c.Config = RuntimeConfig()
	c.Base = nil
	c.Frame = 0
	c.Meta = RequestMeta{}
	c.Trace = TraceMeta{}
	c.SyncWait = 0
	c.keyValue = nil
	c.clearFrozen()
}

// Now 是业务时间的唯一入口（DESIGN-TODO D22-1，维护者 2026-10-21）：在 Nest 消息、心跳拍、entitytimer 的 Tick
// 等一次执行里，返回这次执行开始时冻结的进程业务时钟（真实时间 + time.logic_offset），同一次执行里读多少次都是
// 同一个值，handler、心跳的 Beat.Now、生成分发包装填的 nowUnix / nowUnixMilli 因此是同一时刻。
//
// 冻结发生在请求上下文建立时（NewContext）：Nest 消息是业务执行开始时，交接（ContextSnapshot）不带它，换池续行
// 重新冻结。不在请求上下文里（服务 goroutine、启动阶段、admin 命令、驱动自己的循环）时现读进程业务时钟；那些
// 路径要的是 Registry 的业务时钟时照旧读 app.BusinessClock(registry)，两者在 App 里是同一份 time.logic_offset。
func Now() time.Time {
	if c := CurrentContext(); c != nil && !c.Now.IsZero() {
		return c.Now
	}
	return clock.Now()
}

// NowMilli 是 Now 的 UnixMilli，同样在一次执行里冻结。
func NowMilli() int64 {
	if c := CurrentContext(); c != nil && c.NowMilli != 0 {
		return c.NowMilli
	}
	return clock.UnixMilli()
}

// Frozen 返回本次执行为 key 冻结的值：第一次调用时用 compute 算出并记下，同一次执行（同一个请求上下文）里之后的
// 调用都返回这个值，不再调用 compute。生命期与 Now 相同：每次执行开始时清空，不进 Snapshot（交接后的新执行重新
// 冻结）。没有请求上下文时不缓存，每次都调用 compute。时间域（gameplay/timedomain）用它让一条消息里读到同一个
// 域时间，即使这期间域的倍率被别的 goroutine 发布了新值。key 用包内私有类型的值或指针，避免与别的包冲突。
func Frozen(key any, compute func() any) any {
	c := CurrentContext()
	if c == nil {
		return compute()
	}
	for _, f := range c.frozen {
		if f.key == key {
			return f.value
		}
	}
	value := compute()
	c.frozen = append(c.frozen, frozenValue{key: key, value: value})
	return value
}

// clearFrozen 清掉冻结值，保留底层数组给池里的下一次执行复用。
func (c *Context) clearFrozen() {
	clear(c.frozen)
	c.frozen = c.frozen[:0]
}

// Clear clears request-scoped state before the context is returned to pool.
func (c *Context) Clear() {
	c.shortWorker = false
	c.longWorker = false
	c.ioWorker = false
	c.Config = nil
	c.Base = nil
	c.Frame = 0
	c.Meta = RequestMeta{}
	c.Trace = TraceMeta{}
	c.SyncWait = 0
	c.keyValue = nil
	c.clearFrozen()
}

func SetRuntimeConfig(config any) {
	runtimeConfigMu.Lock()
	runtimeConfig = config
	runtimeConfigMu.Unlock()
}

func RuntimeConfig() any {
	runtimeConfigMu.RLock()
	defer runtimeConfigMu.RUnlock()
	return runtimeConfig
}

// BaseContext returns the request base context when a request Context is bound.
// It falls back to context.Background for framework paths that are detached
// from a direct request.
func BaseContext() stdctx.Context {
	if c := CurrentContext(); c != nil && c.Base != nil {
		return c.Base
	}
	return stdctx.Background()
}

// BindBase temporarily binds a standard context to the current request Context.
// If no request Context exists, it creates one and restores the previous state on
// release. This is useful at service boundaries that accept context.Context
// but delegate into framework code that reads the bound request Context.
func BindBase(base stdctx.Context) func() {
	if base == nil {
		return func() {}
	}
	if c := CurrentContext(); c != nil {
		old := c.Base
		c.Base = base
		return func() {
			c.Base = old
		}
	}
	_, release := NewContext(WithBase(base))
	return release
}

// CaptureSnapshot copies the current request Context for an explicit,
// framework-controlled handoff. It is not the business async API; handlers
// must use Nest effects or worker tasks with explicit data instead. A
// zero-value snapshot means no request context was active.
func CaptureSnapshot() ContextSnapshot {
	if c := CurrentContext(); c != nil {
		return c.Snapshot()
	}
	return ContextSnapshot{}
}

func (c *Context) Snapshot() ContextSnapshot {
	if c == nil {
		return ContextSnapshot{}
	}
	return ContextSnapshot{
		Valid:    true,
		Config:   c.Config,
		Base:     c.Base,
		Frame:    c.Frame,
		Meta:     c.Meta,
		Trace:    c.Trace.Clone(),
		SyncWait: c.SyncWait,
		KeyValue: cloneKeyValue(c.keyValue),
	}
}

func WithSnapshot(snapshot ContextSnapshot) Option {
	return func(c *Context) {
		c.ApplySnapshot(snapshot)
	}
}

func (c *Context) ApplySnapshot(snapshot ContextSnapshot) {
	if c == nil || !snapshot.Valid {
		return
	}
	c.Config = snapshot.Config
	c.Base = snapshot.Base
	c.Frame = snapshot.Frame
	c.Meta = snapshot.Meta
	c.Trace = snapshot.Trace.Clone()
	c.SyncWait = snapshot.SyncWait
	c.keyValue = cloneKeyValue(snapshot.KeyValue)
}

func WithConfig(config any) Option {
	return func(c *Context) {
		c.Config = config
	}
}

func WithBase(base stdctx.Context) Option {
	return func(c *Context) {
		c.Base = base
	}
}

func WithSyncWait(timeout time.Duration) Option {
	return func(c *Context) {
		c.SyncWait = timeout
	}
}

func (c *Context) Done() <-chan struct{} {
	if c == nil || c.Base == nil {
		return nil
	}
	return c.Base.Done()
}

func WithFrame(frame uint64) Option {
	return func(c *Context) {
		c.Frame = frame
	}
}

func WithSource(source string) Option {
	return func(c *Context) {
		c.Meta.Source = source
	}
}

func WithHandler(handler string) Option {
	return func(c *Context) {
		c.Meta.Handler = handler
	}
}

func WithMeta(meta RequestMeta) Option {
	return func(c *Context) {
		c.SetMeta(meta)
	}
}

func WithTrace(trace TraceMeta) Option {
	return func(c *Context) {
		c.Trace = trace.Clone()
	}
}

func WithPlayerProtocol(playerID int64, msgID uint32, seq uint32) Option {
	return func(c *Context) {
		c.MergeMeta(RequestMeta{
			Source:   "player_protocol",
			PlayerID: playerID,
			MsgID:    msgID,
			Seq:      seq,
		})
	}
}

func (c *Context) SetMeta(meta RequestMeta) {
	if c == nil {
		return
	}
	c.Meta = meta
}

func (c *Context) MergeMeta(meta RequestMeta) {
	if c == nil {
		return
	}
	if meta.Source != "" {
		c.Meta.Source = meta.Source
	}
	if meta.Handler != "" {
		c.Meta.Handler = meta.Handler
	}
	if meta.PlayerID != 0 {
		c.Meta.PlayerID = meta.PlayerID
	}
	if meta.MsgID != 0 {
		c.Meta.MsgID = meta.MsgID
	}
	if meta.Seq != 0 {
		c.Meta.Seq = meta.Seq
	}
}

// Set stores a key-value pair in the context.
func (c *Context) Set(key, value any) {
	if c.keyValue == nil {
		c.keyValue = make(map[any]any)
	}
	c.keyValue[key] = value
}

// Get retrieves a value by key.
func (c *Context) Get(key any) (any, bool) {
	if c.keyValue == nil {
		return nil, false
	}
	v, ok := c.keyValue[key]
	return v, ok
}

func cloneKeyValue(src map[any]any) map[any]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[any]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneTraceTags(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
