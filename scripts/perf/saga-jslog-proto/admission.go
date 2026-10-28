package jslogproto

// 按投影积压准入（维护者 2026-10-27 选第三轮 §6 的 A，设计稿“积压准入”一节）：
//
//   - 信号：每个参与方定期把自己 DataEngine 投影器的 WALUnacked（已准入、尚未投影确认的记录数）写进 KV 桶 <STREAM>_LOAD
//     的 part.<参与方名>，带写入时刻。桶有 MaxAge，进程死掉后条目最终消失；判过期靠写入时刻，不靠 MaxAge。
//   - 协调器 watch 这个桶，按“记录数 ÷ 每笔记录数”折算笔数，取全部参与方的最大值，带滞回判定：
//     笔数 ≥ HighSagas 关闭，≤ LowSagas 才重新打开；任何一个见过的参与方信号超过 StaleAfter 没更新 → 关闭（原因 stale）。
//     初始化 fail-closed（RV-20261028-03）：watch 的初始快照（桶里现有的全部信号）读完之前关闭（原因 not_ready）——
//     此时空的信号表只说明“还没读到”，不说明“没有积压”；读完且确实没有任何参与方信号时才打开（没有要保护的投影）。
//   - 准入只作用于**新 saga 的启动**：关闭时协调器把启动意图写成终态 Rejected（原因 backpressure，可重试），不派发任何步骤；
//     在途 saga 的推进、补偿、重试一律不受影响，否则积压永远消不掉。
//   - 协调器把当前准入状态写进同一个桶的 admit 键（状态变化时立即写，否则每秒刷新），发起方据此快速失败（ErrBackpressure，
//     不写启动意图）；它只是提示，协调器处理启动意图时再判一次才是权威。
//   - 参与方侧 ProjectorOptions.MaxUnackedRecords 是最后一道硬上限（DataEngine 在 WAL 准入前拒绝，ErrProjectionBackpressure）：
//     步骤事务被拒 → 命令 Nak 重投，协调器按步骤预算重试；硬上限要远高于 HighSagas × 每笔记录数 + 在途笔数 × 每笔记录数，
//     步骤预算（Timeout × MaxAttempts）要覆盖积压从硬上限回落的时间，否则在途 saga 会用尽重试走补偿（补偿同样要写参与方）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// ErrBackpressure：准入关闭（投影积压超过阈值或积压信号过期）。可重试：稍后用同一个 saga ID 再发起。
var ErrBackpressure = errors.New("jslogproto: saga admission closed (projection backlog); retry later")

// StatusRejected 是启动被准入拒绝的终态（Saga.Reason 给出原因）；同一个 ID 可以再次发起。
const StatusRejected Status = "rejected"

const (
	admitKey       = "admit"
	partLoadPrefix = "part."
	ReasonBacklog  = "backpressure"
	ReasonStale    = "backpressure_stale_signal"
	// ReasonNotReady：协调器还没读完积压信号的初始快照（刚启动 / 刚接管）。单独一个原因而不沿用 stale：
	// stale 表示某个参与方的信号停更，是要排查的故障；not_ready 是启动期的正常短暂状态。两者都以 backpressure 开头、同样可重试。
	ReasonNotReady  = "backpressure_not_ready"
	defaultLoadTTL  = 30 * time.Second
	defaultLoadTick = 200 * time.Millisecond
)

// LoadBucket 是参与方积压信号与准入状态的 KV 桶。
func (l Layout) LoadBucket() string { return "SAGAJS_" + strings.ToUpper(l.Prefix) + "_LOAD" }

// PartLoad 是参与方发布的积压信号。
type PartLoad struct {
	Records uint64 `json:"records"` // DataEngine ProjectorStats.WALUnacked
	At      int64  `json:"at"`      // unix ms
}

// AdmitState 是协调器发布的准入状态（发起方快速失败用）。
type AdmitState struct {
	Open   bool    `json:"open"`
	Reason string  `json:"reason,omitempty"`
	Sagas  float64 `json:"sagas"`
	At     int64   `json:"at"`
}

type AdmissionOptions struct {
	HighSagas      float64       // 积压折算笔数 ≥ 它关闭准入
	LowSagas       float64       // 关闭后降到 ≤ 它才重新打开（滞回）
	RecordsPerSaga float64       // 每笔 saga 在参与方产生的记录数（两步正向 2）
	StaleAfter     time.Duration // 参与方信号多久没更新算过期
	Tick           time.Duration // 重新判定与发布 admit 的间隔
}

func (o *AdmissionOptions) defaults() {
	if o.RecordsPerSaga == 0 {
		o.RecordsPerSaga = 2
	}
	if o.StaleAfter == 0 {
		o.StaleAfter = 2 * time.Second
	}
	if o.Tick == 0 {
		o.Tick = 100 * time.Millisecond
	}
	if o.LowSagas == 0 {
		o.LowSagas = o.HighSagas / 2
	}
}

// admission 是协调器侧的准入闸门（一个协调器实例一份，全部分区共用）。
type admission struct {
	opts AdmissionOptions
	now  func() int64

	mu     sync.Mutex
	loads  map[string]PartLoad
	open   bool
	reason string
	sagas  float64
	ready  bool // watch 的初始快照已读完；之前一律关闭（ReasonNotReady）
	// readyGate 非空时，初始快照读完后等它关闭才置就绪（测试用：受控地拖住初始快照）。
	readyGate <-chan struct{}
}

func newAdmission(opts AdmissionOptions, now func() int64) *admission {
	opts.defaults()
	return &admission{opts: opts, now: now, loads: map[string]PartLoad{}, open: false, reason: ReasonNotReady}
}

// evaluate 按当前信号与滞回重算准入，返回（是否打开，原因，折算笔数）。
func (a *admission) evaluate() (bool, string, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	var worst float64
	stale := false
	for _, l := range a.loads {
		if time.Duration(now-l.At)*time.Millisecond > a.opts.StaleAfter {
			stale = true
		}
		if s := float64(l.Records) / a.opts.RecordsPerSaga; s > worst {
			worst = s
		}
	}
	a.sagas = worst
	if !a.ready {
		a.open, a.reason = false, ReasonNotReady
		return a.open, a.reason, a.sagas
	}
	if a.reason == ReasonNotReady {
		// 初始快照刚读完：从“打开”起按信号判定（与从没关过一样，不经过低水位滞回）。
		a.open, a.reason = true, ""
	}
	switch {
	case stale:
		a.open, a.reason = false, ReasonStale
	case a.open && worst >= a.opts.HighSagas:
		a.open, a.reason = false, ReasonBacklog
	case !a.open && worst <= a.opts.LowSagas:
		a.open, a.reason = true, ""
	case !a.open && a.reason == ReasonStale:
		// 信号恢复新鲜但积压仍在滞回区间：按积压判定。
		if worst >= a.opts.HighSagas {
			a.reason = ReasonBacklog
		} else {
			a.open, a.reason = true, ""
		}
	}
	return a.open, a.reason, a.sagas
}

func (a *admission) set(key string, l PartLoad) {
	a.mu.Lock()
	a.loads[key] = l
	a.mu.Unlock()
}

func (a *admission) markReady() {
	a.mu.Lock()
	a.ready = true
	a.mu.Unlock()
}

func (a *admission) drop(key string) {
	a.mu.Lock()
	delete(a.loads, key)
	a.mu.Unlock()
}

// run watch 积压信号、定期判定并发布 admit 状态，直到 ctx 结束。
func (a *admission) run(ctx context.Context, kv jetstream.KeyValue) error {
	w, err := kv.WatchAll(ctx)
	if err != nil {
		return err
	}
	go func() {
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-w.Updates():
				if !ok {
					return
				}
				if e == nil {
					if a.readyGate != nil {
						go func() {
							select {
							case <-a.readyGate:
								a.markReady()
							case <-ctx.Done():
							}
						}()
						continue
					}
					a.markReady()
					continue
				}
				if !strings.HasPrefix(e.Key(), partLoadPrefix) {
					continue
				}
				if e.Operation() != jetstream.KeyValuePut {
					// 删除 / 过期清除：不再计入（进程正常下线）。信号停更但条目还在时按 StaleAfter 判过期。
					a.drop(e.Key())
					continue
				}
				var l PartLoad
				if json.Unmarshal(e.Value(), &l) == nil {
					a.set(e.Key(), l)
				}
			}
		}
	}()
	go func() {
		tick := time.NewTicker(a.opts.Tick)
		defer tick.Stop()
		var lastOpen bool = true
		var lastReason string
		var lastPub time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			open, reason, sagas := a.evaluate()
			if open != lastOpen || reason != lastReason || time.Since(lastPub) >= time.Second {
				raw, _ := json.Marshal(AdmitState{Open: open, Reason: reason, Sagas: sagas, At: a.now()})
				pctx, cancel := context.WithTimeout(ctx, time.Second)
				if _, err := kv.Put(pctx, admitKey, raw); err == nil {
					lastOpen, lastReason, lastPub = open, reason, time.Now()
				}
				cancel()
			}
		}
	}()
	return nil
}

// ensureLoadBucket 建（或复用）积压信号桶。
func ensureLoadBucket(ctx context.Context, js jetstream.JetStream, l Layout, replicas int) (jetstream.KeyValue, error) {
	return js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: l.LoadBucket(), Storage: jetstream.FileStorage, Replicas: replicas, History: 1, TTL: defaultLoadTTL,
	})
}

// loadPublisher 是参与方侧的积压信号发布：每 tick 把 WALUnacked 写进 part.<name>。
type loadPublisher struct {
	kv     jetstream.KeyValue
	key    string
	tick   time.Duration
	read   func() uint64
	now    func() int64
	paused func() bool
}

func (lp *loadPublisher) run(ctx context.Context) {
	t := time.NewTicker(lp.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if lp.paused != nil && lp.paused() {
			continue
		}
		raw, _ := json.Marshal(PartLoad{Records: lp.read(), At: lp.now()})
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		_, _ = lp.kv.Put(pctx, lp.key, raw)
		cancel()
	}
}

func partLoadKey(name string) string { return fmt.Sprintf("%s%s", partLoadPrefix, name) }
