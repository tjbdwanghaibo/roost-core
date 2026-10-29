package saga

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// admission.go 是 JetStream 协调器按参与方投影积压做的准入（设计 §6，原型稿 §12a 迁移）：
//
//   - 信号：装了原生步骤的进程定期把 DataEngine 投影器的 WALUnacked（记录数）写进 LoadBucket 的 part.<service>.<sid>
//     （发布者 LoadPublisher，wiring/saga 的 Steps 在有原生步骤时启动）。桶有 MaxAge，并开 LimitMarkerTTL：崩溃进程的键过期时服务端写删除标记，Watch 看得到，
//     该参与方不再计入（不开的话过期是静默的，那个参与方会一直被判为信号停更、准入一直关）。
//   - 判定：每个协调器进程一份 admission，全部分区共用。积压 = 全部参与方 WALUnacked 的最大值；≥ HighRecords 关闭，
//     关闭后 ≤ LowRecords 才重开（滞回）。
//   - 过期：按本机单调时钟记“最后一次收到该键更新”的时刻，超过 StaleAfter → 关闭（backpressure_stale_signal）。不读参与方
//     写入的时刻（跨机依赖时钟，原型的做法）。
//   - 初始化 fail-closed（RV-20261028-03）：Watch 的初始快照读完之前关闭（backpressure_not_ready）；读完且没有任何信号 → 打开。
//   - 只作用于新启动：分区处理 in.start 时判一次（权威），关闭 → 判定 rejected，不写事件。在途推进、重试、补偿、Resume 不受影响。
//   - 提示：协调器把状态写进同桶 admit 键（变化即写，否则每 HintEvery），StartSaga 据它快速失败，不写意图。

// 准入关闭的原因（都以 backpressure 开头，都可重试）。
const (
	AdmissionReasonBacklog  = "backpressure"
	AdmissionReasonStale    = "backpressure_stale_signal"
	AdmissionReasonNotReady = "backpressure_not_ready"
)

const (
	admitHintKey         = "admit"
	participantKeyPrefix = "part."
)

// AdmissionOptions 是准入参数。零值字段在 NewEngine 里取缺省（原型第五轮推荐值换算成记录数）。
type AdmissionOptions struct {
	// Enabled 关闭时新启动一律准入（不建 watch、不发布提示）。
	Enabled bool
	// HighRecords / LowRecords：积压（全部参与方 WALUnacked 的最大值）≥ High 关闭，关闭后 ≤ Low 才重开。缺省 5000 / 2500。
	HighRecords uint64
	LowRecords  uint64
	// StaleAfter：一个参与方的信号多久没收到更新算停更（本机单调时钟）。缺省 2s。
	StaleAfter time.Duration
	// SignalTTL 是 LoadBucket 的 MaxAge 与 LimitMarkerTTL：崩溃参与方的键最多留这么久，过期时 Watch 收到删除。缺省 30s。
	SignalTTL time.Duration
	// HintEvery：准入状态没变时多久重写一次 admit 提示。缺省 1s。
	HintEvery time.Duration
	// HintFreshness：发起方只信这么久之内收到的提示。缺省 3s。
	HintFreshness time.Duration
}

func (o AdmissionOptions) withDefaults() AdmissionOptions {
	if o.HighRecords == 0 {
		o.HighRecords = 5000
	}
	if o.LowRecords == 0 {
		o.LowRecords = o.HighRecords / 2
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 2 * time.Second
	}
	if o.SignalTTL <= 0 {
		o.SignalTTL = 30 * time.Second
	}
	if o.HintEvery <= 0 {
		o.HintEvery = time.Second
	}
	if o.HintFreshness <= 0 {
		o.HintFreshness = 3 * time.Second
	}
	return o
}

func (o AdmissionOptions) validate() error {
	if !o.Enabled {
		return nil
	}
	if o.LowRecords >= o.HighRecords {
		return fmt.Errorf("%w: admission low_records %d must be < high_records %d", ErrInvalidDefinition, o.LowRecords, o.HighRecords)
	}
	if o.SignalTTL < time.Second || o.StaleAfter >= o.SignalTTL {
		return fmt.Errorf("%w: admission signal TTL %v must be >= 1s and > stale_after %v", ErrInvalidDefinition, o.SignalTTL, o.StaleAfter)
	}
	return nil
}

// AdmissionState 是一次准入判定：打开与否、关闭原因、当前积压（记录数）与判定时刻。
type AdmissionState struct {
	Open           bool
	Reason         string
	BacklogRecords uint64
	At             time.Time
}

// admission 是协调器侧的准入闸门。观察（Watch 回调）与判定（分区循环）在不同 goroutine，由 mu 保护；判定只读内存、不做 I/O，
// 分区循环可以每个启动意图调一次。
type admission struct {
	opts AdmissionOptions
	now  func() time.Time // 本机时钟；带单调读数时过期判定按单调时钟

	mu    sync.Mutex
	loads map[string]loadSeen
	ready bool
	open  bool
	why   string
}

type loadSeen struct {
	records uint64
	seenAt  time.Time
}

func newAdmission(opts AdmissionOptions, now func() time.Time) *admission {
	return &admission{opts: opts, now: now, loads: map[string]loadSeen{}, why: AdmissionReasonNotReady}
}

// observe 记一条 Watch 交来的条目：参与方信号的新值（刷新最后收到的时刻），或删除 / 过期清除（该参与方下线，不再计入）。
// admit 提示与无法解码的值忽略。
func (a *admission) observe(entry fnats.LogKVEntry) {
	if !strings.HasPrefix(entry.Key, participantKeyPrefix) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry.Deleted {
		delete(a.loads, entry.Key)
		return
	}
	var load participantLoad
	if json.Unmarshal(entry.Value, &load) != nil || load.V != WireVersion {
		slog.Warn("saga: ignored an undecodable admission signal", "key", entry.Key)
		return
	}
	a.loads[entry.Key] = loadSeen{records: load.Records, seenAt: a.now()}
}

// markReady 在 Watch 的初始快照交完后调用：从此按信号判定。
func (a *admission) markReady() {
	a.mu.Lock()
	a.ready = true
	a.mu.Unlock()
}

// evaluate 按当前信号与滞回重算准入。
func (a *admission) evaluate() AdmissionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	var backlog uint64
	stale := false
	for _, load := range a.loads {
		if now.Sub(load.seenAt) > a.opts.StaleAfter {
			stale = true
		}
		backlog = max(backlog, load.records)
	}
	if !a.ready {
		a.open, a.why = false, AdmissionReasonNotReady
		return AdmissionState{Open: false, Reason: a.why, BacklogRecords: backlog, At: now}
	}
	if a.why == AdmissionReasonNotReady {
		// 初始快照刚读完：从“打开”起按信号判定（与从没关过一样，不经过低水位滞回）。
		a.open, a.why = true, ""
	}
	switch {
	case stale:
		a.open, a.why = false, AdmissionReasonStale
	case a.open && backlog >= a.opts.HighRecords:
		a.open, a.why = false, AdmissionReasonBacklog
	case !a.open && backlog <= a.opts.LowRecords:
		a.open, a.why = true, ""
	case !a.open && a.why == AdmissionReasonStale:
		// 信号恢复新鲜、积压在滞回区间：按积压判定（没到高水位就打开）。
		if backlog >= a.opts.HighRecords {
			a.why = AdmissionReasonBacklog
		} else {
			a.open, a.why = true, ""
		}
	}
	return AdmissionState{Open: a.open, Reason: a.why, BacklogRecords: backlog, At: now}
}

// run 建 Watch（只看 part.* 信号）并启动提示发布；返回的停止函数按三步停机（在 ctx 内等两个 goroutine 退出）。
func (a *admission) run(kv fnats.LogKV) (func(context.Context) error, error) {
	setup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	follow, err := kv.Watch(setup, participantKeyPrefix+">", a.observe, a.markReady)
	if err != nil {
		return nil, fmt.Errorf("saga: admission watch: %w", err)
	}
	stopHints := make(chan struct{})
	hintsDone := make(chan struct{})
	go func() {
		defer close(hintsDone)
		a.publishHints(kv, stopHints)
	}()
	var once sync.Once
	return func(ctx context.Context) error {
		once.Do(func() { close(stopHints) })
		if err := follow.Stop(ctx); err != nil {
			return err
		}
		select {
		case <-hintsDone:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("saga: admission hints still publishing: %w", ctx.Err())
		}
	}, nil
}

// publishHints 定期判定，状态变化时立即、否则每 HintEvery 把提示写进 admit 键。写失败只计数（提示不是权威）。
func (a *admission) publishHints(kv fnats.LogKV, stop <-chan struct{}) {
	tick := time.NewTicker(min(100*time.Millisecond, a.opts.HintEvery))
	defer tick.Stop()
	var last admissionHint
	var lastAt time.Time
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		state := a.evaluate()
		hint := admissionHint{V: WireVersion, Open: state.Open, Reason: state.Reason, BacklogRecords: state.BacklogRecords}
		if hint.Open == last.Open && hint.Reason == last.Reason && time.Since(lastAt) < a.opts.HintEvery {
			continue
		}
		raw, _ := json.Marshal(hint)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := kv.Put(ctx, admitHintKey, raw)
		cancel()
		if err != nil {
			metrics.IncCounter("saga.admission.hint_failures_total", nil, 1)
			continue
		}
		last, lastAt = hint, time.Now()
	}
}

// admissionHints 是发起方一侧看到的准入提示（admit 键）。收到时刻按本机单调时钟记，只信 HintFreshness 之内的。
type admissionHints struct {
	freshness time.Duration
	mu        sync.Mutex
	hint      admissionHint
	seenAt    time.Time
}

func (h *admissionHints) observe(entry fnats.LogKVEntry) {
	if entry.Key != admitHintKey || entry.Deleted {
		return
	}
	var hint admissionHint
	if json.Unmarshal(entry.Value, &hint) != nil || hint.V != WireVersion {
		return
	}
	h.mu.Lock()
	h.hint, h.seenAt = hint, time.Now()
	h.mu.Unlock()
}

// closed 报告最近一次（新鲜度之内）收到的提示是否关闭，以及原因。没有提示、提示过期都按“不知道”放行：权威判定在所有者。
func (h *admissionHints) closed() (bool, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seenAt.IsZero() || h.hint.Open || time.Since(h.seenAt) > h.freshness {
		return false, ""
	}
	return true, h.hint.Reason
}

// loadBucketConfig 是 LoadBucket 的配置：MaxAge 与 LimitMarkerTTL 都是 SignalTTL（见 admission.go 文件注释）。
func loadBucketConfig(layout Layout, replicas int, opts AdmissionOptions) fnats.LogKVConfig {
	return fnats.LogKVConfig{Bucket: layout.LoadBucket(), Replicas: replicas, History: 1, MaxAge: opts.SignalTTL, LimitMarkerTTL: opts.SignalTTL}
}

// OpenLoadBucket 打开（没有时按同一配置建出）准入信号桶 LoadBucket：参与方的 LoadPublisher 用，桶配置与协调器相同
// （MaxAge 与 LimitMarkerTTL 都取 admission.SignalTTL，零值取缺省）。
func OpenLoadBucket(ctx context.Context, log fnats.JetStreamLog, layout Layout, replicas int, admission AdmissionOptions) (fnats.LogKV, error) {
	if log == nil {
		return nil, fmt.Errorf("saga: JetStream log is required")
	}
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	return log.KeyValue(ctx, loadBucketConfig(layout, max(replicas, 1), admission.withDefaults()))
}

// errAdmissionClosed 包装准入关闭的原因。
func errAdmissionClosed(reason string) error {
	if reason == "" {
		reason = AdmissionReasonBacklog
	}
	return fmt.Errorf("%w (%s)", ErrBackpressure, reason)
}

// LoadPublisher 是参与方一侧的准入信号（设计 §6）：装了原生步骤的进程每 interval 把本进程 DataEngine 投影器的积压
// （ProjectorStats.WALUnacked，记录数）写进 LoadBucket 的 part.<name>。协调器按“最后一次收到更新”的本机时刻判过期，
// 所以发布要比 AdmissionOptions.StaleAfter 勤（缺省 200ms 对 2s）；写失败只计数，下一拍重写。
//
// 生命周期：Run 阻塞到 ctx 结束；优雅下线时 Run 返回后调用 Remove 删掉自己的键（不算过期，协调器立即不再计入）；
// 崩溃进程的键由桶的 MaxAge / LimitMarkerTTL 清除。没有原生步骤的进程不发布（Mongo 步骤没有投影积压）。
type LoadPublisher struct {
	kv       fnats.LogKV
	key      string
	interval time.Duration
	read     func() uint64
}

// NewLoadPublisher 构造信号发布者。name 是参与方在桶里的名字（一般 <service>.<sid>），只能含字母、数字、'_'、'-' 与 '.'；
// interval ≤ 0 取 200ms。kv 是 LoadBucket（与协调器同一配置：loadBucketConfig）。
func NewLoadPublisher(kv fnats.LogKV, name string, interval time.Duration, read func() uint64) (*LoadPublisher, error) {
	if kv == nil || read == nil || !validLoadName(name) {
		return nil, fmt.Errorf("%w: load publisher name %q", ErrInvalidDefinition, name)
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	return &LoadPublisher{kv: kv, key: participantKeyPrefix + name, interval: interval, read: read}, nil
}

func validLoadName(name string) bool {
	if name == "" || len(name) > 200 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// Run 立即发布一次，之后每 interval 一次，直到 ctx 结束。
func (p *LoadPublisher) Run(ctx context.Context) {
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		p.publish(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (p *LoadPublisher) publish(ctx context.Context) {
	raw, _ := json.Marshal(participantLoad{V: WireVersion, Records: p.read()})
	putCtx, cancel := context.WithTimeout(ctx, max(p.interval, time.Second))
	defer cancel()
	if _, err := p.kv.Put(putCtx, p.key, raw); err != nil && ctx.Err() == nil {
		metrics.IncCounter("saga.admission.signal_failures_total", nil, 1)
	}
}

// Remove 删掉本参与方的信号键（优雅下线）。
func (p *LoadPublisher) Remove(ctx context.Context) error {
	return p.kv.Delete(ctx, p.key)
}
