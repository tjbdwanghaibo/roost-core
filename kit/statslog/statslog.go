package statslog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/worker"

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
	observedAt  time.Time      // 仅给写日志提交窗口使用，不导出到 JSON。
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
	Fast                   NestQueueStats `json:"fast"`
	Slow                   NestQueueStats `json:"slow"`
	FastContinuations      int            `json:"fast_continuations"`
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

	flushMu        sync.Mutex // 串行写文件及其窗口推进；只读 CollectStats 不改变基线。
	mu             sync.Mutex
	file           *os.File
	providers      map[string]providerEntry
	nextProviderID uint64
	lastNestWork   nest.DispatcherWorkStats
	lastNestAt     time.Time
	registry       *app.Registry
	// writeErr 是上一次打开 / 写文件的错误文本（空表示上一次成功），只用来给
	// reportWrite 去重告警（RR-20260928-04）。受 mu 保护。
	writeErr string

	// gaugeMu 保护 publishedCategories / publishedKinds：发布过 gauge 的实体分类与 kind。
	// 某个键在本次采集里不再出现（实体全部卸载）时要把它的 gauge 写回 0（RR-20261005-NC-164）。
	// 键的数量受实体 kind / category 定义个数约束，不随实体数增长。
	gaugeMu             sync.Mutex
	publishedCategories map[string]struct{}
	publishedKinds      map[string]struct{}

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

// config 是 stats_log.* 的声明（维护者决定 A4 ①）。
type config struct {
	app.ServiceIdentity
	Enabled  bool          `config:"stats_log.enabled" example:"true" help:"按 interval 把指标快照写进 stats 日志"`
	Dir      string        `config:"stats_log.dir" default:"log" example:"log" help:"stats 日志目录（相对进程工作目录，部署要给它可写的挂载）"`
	Filename string        `config:"stats_log.filename" help:"文件名，不写取 <server_type>-<sid>.stats.log"`
	Interval time.Duration `config:"stats_log.interval" default:"1m" min:"1ns" example:"1m"`
}

// ConfigSchema 声明 stats_log.*。
func (m *StatsLogMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *StatsLogMod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("stats_log: %w", err)
	}
	m.enabled = settings.Enabled
	m.service = settings.ServerType
	if m.service == "" {
		m.service = "roost"
	}
	m.sid = settings.Sid
	m.dir = settings.Dir
	m.filename = settings.Filename
	if m.filename == "" {
		m.filename = fmt.Sprintf("%s-%d.stats.log", m.service, m.sid)
	}
	m.interval = settings.Interval
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
// the JSONL file — without advancing its window, for callers such as ops' /statsz. It is typed any so ops
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
	// 记录里只有当前还有实体的键；之前发布过、这次缺席的键写 0，否则 gauge 停在最后一次的
	// 非零值，与 JSONL（缺席即 0）不一致（RR-20261005-NC-164）。
	m.gaugeMu.Lock()
	defer m.gaugeMu.Unlock()
	m.publishedCategories = m.publishCounts("entity.count_by_category", "category", record.Entity.ByCategory, m.publishedCategories)
	m.publishedKinds = m.publishCounts("entity.count_by_kind", "kind", record.Entity.ByKind, m.publishedKinds)
}

// publishCounts 写出 counts 中的每个键，并把 published 里本次缺席的键写成 0；返回更新后的键集合。
// 调用方持有 gaugeMu。
func (m *StatsLogMod) publishCounts(name, label string, counts map[string]int, published map[string]struct{}) map[string]struct{} {
	if published == nil {
		published = make(map[string]struct{}, len(counts))
	}
	for key := range published {
		if _, present := counts[key]; !present {
			m.metrics.SetGauge(name, metrics.Labels{label: key}, 0)
		}
	}
	for key, count := range counts {
		m.metrics.SetGauge(name, metrics.Labels{label: key}, int64(count))
		published[key] = struct{}{}
	}
	return published
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

	// RR-20260928-04：启动时先把文件打开一次，目录不可写在启动日志里就能看到，
	// 不用等第一个 interval（默认 1 分钟）。之前 Start 不碰文件，周期循环又把错误吞掉，
	// 生成的容器（只读根文件系统上的相对目录 /app/log）从不落盘，也没有一行告警。
	// 不因此让 Start 失败：统计文件是旁路观测，同一份数据每次采集都已发布到 metrics
	// gauge 与 ops /statsz；为它拒绝启动会让已部署、但还没合并新部署物的服务在升级后
	// 反复重启，代价比丢一份统计文件大。
	m.reportWrite(m.openFile())
	go func() {
		defer close(m.doneCh)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.reportWrite(m.FlushOnce())
			case <-m.stopCh:
				return
			}
		}
	}()
	return nil
}

// path is where the records go: stats_log.dir is resolved against the process
// working directory when relative.
func (m *StatsLogMod) path() string { return filepath.Join(m.dir, m.filename) }

// reportWrite makes a failed open or write visible (RR-20260928-04): every
// failure counts in stats_log.write_failures (no labels), and a WARN is logged
// when writing starts failing or the error changes, so a tick every few
// seconds does not flood the log with one fact. The first success after a
// failure is logged at INFO.
func (m *StatsLogMod) reportWrite(err error) {
	m.mu.Lock()
	previous := m.writeErr
	m.writeErr = ""
	if err != nil {
		m.writeErr = err.Error()
	}
	m.mu.Unlock()
	if err == nil {
		if previous != "" {
			slog.Info("stats_log: writing the stats file again", "path", m.path())
		}
		return
	}
	if m.metrics != nil {
		m.metrics.IncCounter("stats_log.write_failures", nil, 1)
	}
	if err.Error() != previous {
		slog.Warn("stats_log: cannot write the stats file; records are not persisted until it is writable (further failures with the same error only count in stats_log.write_failures)",
			"path", m.path(), "err", err)
	}
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
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	record := m.collect()
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.openFileLocked(); err != nil {
		return err
	}
	if _, err := m.file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := m.file.Sync(); err != nil {
		return err
	}
	// 只有持久记录成功后才推进累计计数基线，探针和失败写入都不能消耗窗口。
	m.lastNestWork = nest.DispatcherWorkStats{ProcessedMessages: record.Nest.ProcessedMessagesTotal, Slow200msMessages: record.Nest.Slow200msMessagesTotal}
	m.lastNestAt = record.observedAt
	return nil
}

func (m *StatsLogMod) openFile() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openFileLocked()
}

// openFileLocked opens the stats file for append, creating its directory; a
// failure leaves m.file nil so the next flush tries again. Caller holds mu.
func (m *StatsLogMod) openFileLocked() error {
	if m.file != nil {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.path(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	m.file = f
	return nil
}

func (m *StatsLogMod) collect() StatsRecord {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	now := time.Now()
	record := StatsRecord{
		observedAt:  now,
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
		Fast:                   formatNestPoolStats(stats.Fast),
		Slow:                   formatNestPoolStats(stats.Slow),
		FastContinuations:      stats.FastContinuations,
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
