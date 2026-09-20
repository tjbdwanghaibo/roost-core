package statslog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/worker"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/spf13/viper"
)

type ProviderFunc func() (any, error)

type providerEntry struct {
	id uint64
	fn ProviderFunc
}

type RuntimeStats struct {
	Goroutines     int    `json:"goroutines"`
	NumCPU         int    `json:"num_cpu"`
	GOMAXPROCS     int    `json:"gomaxprocs"`
	HeapAlloc      string `json:"heap_alloc"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapSys        string `json:"heap_sys"`
	HeapSysBytes   uint64 `json:"heap_sys_bytes"`
	Sys            string `json:"sys"`
	SysBytes       uint64 `json:"sys_bytes"`
	NumGC          uint32 `json:"num_gc"`
}

type EntityStats struct {
	Total      int            `json:"total"`
	ByCategory map[string]int `json:"by_category,omitempty"`
	ByKind     map[string]int `json:"by_kind,omitempty"`
}

type StatsRecord struct {
	Timestamp   string         `json:"timestamp"`
	TimestampMs int64          `json:"timestamp_ms"`
	Service     string         `json:"service"`
	Sid         int32          `json:"sid"`
	Runtime     RuntimeStats   `json:"runtime"`
	Entity      EntityStats    `json:"entity,omitempty"`
	Nest        NestStats      `json:"nest,omitempty"`
	Providers   map[string]any `json:"providers,omitempty"`
}

type NestStats struct {
	Main                   NestQueueStats `json:"main"`
	Broadcast              NestQueueStats `json:"broadcast"`
	Cost                   NestQueueStats `json:"cost"`
	WindowSeconds          float64        `json:"window_seconds"`
	ProcessedMessages      uint64         `json:"processed_messages"`
	Slow200msMessages      uint64         `json:"slow_200ms_messages"`
	ProcessedMessagesTotal uint64         `json:"processed_messages_total"`
	Slow200msMessagesTotal uint64         `json:"slow_200ms_messages_total"`
	DelayedMessages        int            `json:"delayed_messages"`
	Stopped                bool           `json:"stopped"`
}

type NestQueueStats struct {
	Name       string `json:"name,omitempty"`
	Workers    int    `json:"workers"`
	QueueLen   int    `json:"queue_len"`
	QueueCap   int    `json:"queue_cap"`
	QueueUsage string `json:"queue_usage"`
	Running    bool   `json:"running"`
	Stopped    bool   `json:"stopped"`
}

type StatsLogMod struct {
	enabled  bool
	service  string
	sid      int32
	dir      string
	filename string
	interval time.Duration
	metrics  *metrics.Registry

	mu             sync.Mutex
	file           *os.File
	providers      map[string]providerEntry
	nextProviderID uint64
	lastNestWork   nest.DispatcherWorkStats
	lastNestAt     time.Time
	registry       *app.Registry

	started  bool
	stopCh   chan struct{}
	doneCh   chan struct{}
	closedCh chan struct{}
	stopOnce sync.Once
}

func NewStatsLogMod() *StatsLogMod {
	return &StatsLogMod{
		interval:  time.Minute,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
		closedCh:  make(chan struct{}),
		providers: make(map[string]providerEntry),
	}
}

func (m *StatsLogMod) Name() app.ModName { return mods.ModStatsLog }

func (m *StatsLogMod) Init(cfg *viper.Viper) error {
	if cfg == nil {
		cfg = viper.New()
	}
	m.enabled = cfg.GetBool("stats_log.enabled")
	m.service = cfg.GetString("server_type")
	if m.service == "" {
		m.service = "roost"
	}
	m.sid = cfg.GetInt32("sid")
	m.dir = cfg.GetString("stats_log.dir")
	if m.dir == "" {
		m.dir = "log"
	}
	m.filename = cfg.GetString("stats_log.filename")
	if m.filename == "" {
		m.filename = fmt.Sprintf("%s-%d.stats.log", m.service, m.sid)
	}
	m.interval = cfg.GetDuration("stats_log.interval")
	if m.interval <= 0 {
		m.interval = time.Minute
	}
	return nil
}

func (m *StatsLogMod) Provide(r *app.Registry) error {
	if r == nil {
		return nil
	}
	m.registry = r
	if reg, ok := app.Lookup[*metrics.Registry](r, mods.ModMetrics); ok && reg != nil {
		m.metrics = reg
	}
	return r.Register(mods.ModStatsLog, m)
}

// CollectStats takes one observation now — the same record a tick writes to
// the JSONL file — for callers such as ops' /statsz. It is typed any so ops
// need not import this package.
func (m *StatsLogMod) CollectStats() any {
	if m == nil {
		return nil
	}
	return m.collect()
}

// publishGauges mirrors the record into the metrics registry, so what the
// JSONL file says is also what /metrics and the dashboards say: goroutines,
// heap, and how many entities of each category and kind this process holds.
// Memory is observed at process level (heap); an entity's own footprint is
// not attributable without allocation tracking, so the entity figures are
// counts.
func (m *StatsLogMod) publishGauges(record StatsRecord) {
	if m.metrics == nil {
		return
	}
	m.metrics.SetGauge("runtime.goroutines", nil, int64(record.Runtime.Goroutines))
	m.metrics.SetGauge("runtime.heap_alloc_bytes", nil, int64(record.Runtime.HeapAllocBytes))
	m.metrics.SetGauge("runtime.heap_sys_bytes", nil, int64(record.Runtime.HeapSysBytes))
	m.metrics.SetGauge("runtime.sys_bytes", nil, int64(record.Runtime.SysBytes))
	m.metrics.SetGauge("runtime.num_gc", nil, int64(record.Runtime.NumGC))
	m.metrics.SetGauge("entity.count", nil, int64(record.Entity.Total))
	for category, count := range record.Entity.ByCategory {
		m.metrics.SetGauge("entity.count_by_category", metrics.Labels{"category": category}, int64(count))
	}
	for kind, count := range record.Entity.ByKind {
		m.metrics.SetGauge("entity.count_by_kind", metrics.Labels{"kind": kind}, int64(count))
	}
}

func (m *StatsLogMod) Start() error {
	if !m.enabled {
		return nil
	}
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.mu.Unlock()

	go func() {
		defer close(m.doneCh)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = m.FlushOnce()
			case <-m.stopCh:
				return
			}
		}
	}()
	return nil
}

func (m *StatsLogMod) Stop() {
	if err := m.StopWithContext(fctx.BaseContext()); err != nil {
		return
	}
}

func (m *StatsLogMod) StopWithContext(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	m.mu.Lock()
	if m.closedCh == nil {
		m.closedCh = make(chan struct{})
	}
	m.mu.Unlock()
	m.stopOnce.Do(func() {
		m.mu.Lock()
		started := m.started
		m.mu.Unlock()
		go func() {
			if started {
				close(m.stopCh)
				<-m.doneCh
			}
			m.closeFile()
			close(m.closedCh)
		}()
	})
	select {
	case <-m.closedCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *StatsLogMod) RegisterProvider(name string, fn ProviderFunc) func() {
	if name == "" || fn == nil {
		return func() {}
	}
	m.mu.Lock()
	m.nextProviderID++
	id := m.nextProviderID
	m.providers[name] = providerEntry{id: id, fn: fn}
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if entry, ok := m.providers[name]; ok && entry.id == id {
			delete(m.providers, name)
		}
		m.mu.Unlock()
	}
}

func (m *StatsLogMod) FlushOnce() error {
	if m == nil || !m.enabled {
		return nil
	}
	record := m.collect()
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.file == nil {
		if err := os.MkdirAll(m.dir, 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(m.dir, m.filename), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		m.file = f
	}
	if _, err := m.file.Write(append(raw, '\n')); err != nil {
		return err
	}
	return m.file.Sync()
}

func (m *StatsLogMod) collect() StatsRecord {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	now := time.Now()
	record := StatsRecord{
		Timestamp:   now.Format(time.RFC3339Nano),
		TimestampMs: now.UnixMilli(),
		Service:     m.service,
		Sid:         m.sid,
		Runtime: RuntimeStats{
			Goroutines:     runtime.NumGoroutine(),
			NumCPU:         runtime.NumCPU(),
			GOMAXPROCS:     runtime.GOMAXPROCS(0),
			HeapAlloc:      formatBytes(ms.HeapAlloc),
			HeapAllocBytes: ms.HeapAlloc,
			HeapSys:        formatBytes(ms.HeapSys),
			HeapSysBytes:   ms.HeapSys,
			Sys:            formatBytes(ms.Sys),
			SysBytes:       ms.Sys,
			NumGC:          ms.NumGC,
		},
		Entity:    m.collectEntityStats(),
		Providers: m.collectProviders(),
	}
	if runtime, ok := app.Lookup[interface{ Stats() nest.DispatcherStats }](m.registry, mods.ModNest); ok && runtime != nil {
		record.Nest = m.formatNestStats(runtime.Stats(), now)
	}
	m.publishGauges(record)
	return record
}

func (m *StatsLogMod) collectProviders() map[string]any {
	m.mu.Lock()
	providers := make(map[string]ProviderFunc, len(m.providers))
	for name, entry := range m.providers {
		providers[name] = entry.fn
	}
	m.mu.Unlock()
	if len(providers) == 0 {
		return nil
	}
	out := make(map[string]any, len(providers))
	for name, fn := range providers {
		value, err := collectProvider(fn)
		if err != nil {
			out[name] = map[string]any{"error": err.Error()}
			continue
		}
		out[name] = value
	}
	return out
}

func collectProvider(fn ProviderFunc) (value any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

func (m *StatsLogMod) closeFile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.file != nil {
		_ = m.file.Close()
		m.file = nil
	}
}

func (m *StatsLogMod) collectEntityStats() EntityStats {
	stats := EntityStats{
		ByCategory: make(map[string]int),
		ByKind:     make(map[string]int),
	}
	runtime, ok := app.Lookup[interface {
		Len() int
		Range(func(entity.IThreadSafeEntity) bool)
	}](m.registry, mods.ModEntityRuntime)
	if !ok || runtime == nil {
		return stats
	}
	stats.Total = runtime.Len()
	runtime.Range(func(e entity.IThreadSafeEntity) bool {
		if e == nil {
			return true
		}
		stats.ByCategory[fmt.Sprint(e.GetEntityCategory())]++
		stats.ByKind[fmt.Sprint(e.GetEntityKind())]++
		return true
	})
	return stats
}

func (m *StatsLogMod) formatNestStats(stats nest.DispatcherStats, now time.Time) NestStats {
	m.mu.Lock()
	prev := m.lastNestWork
	prevAt := m.lastNestAt
	m.lastNestWork = stats.Work
	m.lastNestAt = now
	m.mu.Unlock()

	interval := time.Duration(0)
	if !prevAt.IsZero() {
		interval = now.Sub(prevAt)
	} else if m != nil {
		interval = m.interval
	}
	return formatNestStats(stats, nestWorkDelta(stats.Work, prev), interval)
}

func formatNestStats(stats nest.DispatcherStats, delta nest.DispatcherWorkStats, interval time.Duration) NestStats {
	return NestStats{
		Main:                   formatNestPoolStats(stats.Main),
		Broadcast:              formatNestPoolStats(stats.Heart),
		Cost:                   formatNestPoolStats(stats.Cost),
		WindowSeconds:          roundSeconds(interval),
		ProcessedMessages:      delta.ProcessedMessages,
		Slow200msMessages:      delta.Slow200msMessages,
		ProcessedMessagesTotal: stats.Work.ProcessedMessages,
		Slow200msMessagesTotal: stats.Work.Slow200msMessages,
		DelayedMessages:        stats.Delayed,
		Stopped:                stats.Stopped,
	}
}

func nestWorkDelta(cur nest.DispatcherWorkStats, prev nest.DispatcherWorkStats) nest.DispatcherWorkStats {
	return nest.DispatcherWorkStats{
		ProcessedMessages: subtractCounter(cur.ProcessedMessages, prev.ProcessedMessages),
		Slow200msMessages: subtractCounter(cur.Slow200msMessages, prev.Slow200msMessages),
	}
}

func subtractCounter(cur uint64, prev uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

func roundSeconds(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(d.Round(time.Millisecond)) / float64(time.Second)
}

func formatNestPoolStats(stats worker.PoolStats) NestQueueStats {
	return NestQueueStats{
		Name:       stats.Name,
		Workers:    stats.WorkerNum,
		QueueLen:   stats.QueueLen,
		QueueCap:   stats.QueueCap,
		QueueUsage: fmt.Sprintf("%d/%d", stats.QueueLen, stats.QueueCap),
		Running:    stats.Started && !stats.Stopped,
		Stopped:    stats.Stopped,
	}
}

func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KB", "MB", "GB", "TB", "PB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.2f EB", value/unit)
}
