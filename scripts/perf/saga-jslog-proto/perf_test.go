package jslogproto

// 闭环压测（SAGAJS_PERF=1 才跑）：N 路发起者各自“发起 → 等终态 → 下一笔”，每档 SAGAJS_PERF_SECONDS 秒，稳态窗口
// [warmup, seconds)。协调器、参与方（真实 Nest + nestwal）、发起方同进程，NATS 是私有三节点集群（R3、文件存储）。
// 每档结束：排空在途 → 余额逐账户核对 → 新协调器实例强占全部分区重放、逐笔复读终态与发起方观察到的一致。
// 结果每档一行 JSON 写到 SAGAJS_PERF_OUT/<label>.jsonl。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

type perfResult struct {
	Label          string  `json:"label"`
	Scenario       string  `json:"scenario"`
	Durability     string  `json:"durability"`
	Partitions     int     `json:"partitions"`
	Streams        int     `json:"streams"`
	Concurrency    int     `json:"concurrency"`
	GOMAXPROCS     int     `json:"gomaxprocs"`
	Seconds        int     `json:"seconds"`
	SteadyTPS      float64 `json:"steady_tps"`
	Terminal       int     `json:"terminal"`
	Unexpected     int     `json:"unexpected"`
	ClientP50      float64 `json:"client_p50_ms"`
	ClientP99      float64 `json:"client_p99_ms"`
	ServerP50      float64 `json:"server_p50_ms"`
	ServerP99      float64 `json:"server_p99_ms"`
	Steps          uint64  `json:"steps"`
	JSMsgsStep     float64 `json:"js_msgs_per_step"`
	JSCallsStep    float64 `json:"js_publish_calls_per_step"`
	EvPerCall      float64 `json:"ev_per_publish_call"`
	FsyncStep      float64 `json:"wal_fsync_per_step"`
	BalancesOK     bool    `json:"balances_ok"`
	RereadOK       bool    `json:"reread_ok"`
	Projection     bool    `json:"projection"`
	ProjectionMode string  `json:"projection_mode"`
	NestAvgMs      float64 `json:"participant_nest_avg_ms"`
	ResultPubAvgMs float64 `json:"participant_result_pub_avg_ms"`
	CoordPubAvgMs  float64 `json:"coordinator_publish_avg_ms"`
	// 投影积压（投影打开时）：参与方 WAL 未确认记录、冷副本未投影事件的峰值，折算成 saga 笔数（÷ 每笔条数），
	// 与在途笔数（闭环并发）比较；停止加压后两者归零的耗时；冷副本与终态、Mongo 账户与内存的逐笔差异。
	PartBacklogPeak     uint64  `json:"participant_backlog_peak_records"`
	SagaBacklogPeak     uint64  `json:"saga_backlog_peak_events"`
	BacklogPeakSagas    float64 `json:"backlog_peak_sagas"`
	BacklogRatio        float64 `json:"backlog_peak_over_inflight"`
	CatchUpMs           float64 `json:"catch_up_ms"`
	Drained             bool    `json:"drained"`
	ProjectedDuringLoad uint64  `json:"participant_projected_during_load"`
	ProjRateDuringLoad  float64 `json:"participant_projection_rate_during_load"`
	PartBacklogAtStop   uint64  `json:"participant_backlog_at_stop"`
	PartBacklogLeft     uint64  `json:"participant_backlog_left_after_catchup"`
	CatchUpRate         float64 `json:"participant_projection_rate_catchup"`
	MongoCallsPerRecord float64 `json:"participant_mongo_calls_per_record"`
	MongoAcctChecked    bool    `json:"mongo_accounts_checked"`
	ColdDiffs           int     `json:"cold_copy_diffs"`
	MongoAcctDiffs      int     `json:"mongo_account_diffs"`
	RereadChecked       int     `json:"reread_checked"`
	RereadPruned        int     `json:"reread_pruned_by_retention"`
	SnapshotEvery       int     `json:"snapshot_every"`
	ReplayMsgs          uint64  `json:"replay_msgs"`
	ReplayMs            float64 `json:"replay_ms"`
	Snapshots           uint64  `json:"snapshots"`
	Purges              uint64  `json:"purges"`
	Unknown             uint64  `json:"publish_unknown"`
	LoadBefore          string  `json:"loadavg_before"`
	StartedAt           string  `json:"started_at"`
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

func loadavg() string {
	out, err := execOutput("sysctl", "-n", "vm.loadavg")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func execOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

func TestPerfClosedLoop(t *testing.T) {
	if os.Getenv("SAGAJS_PERF") != "1" {
		t.Skip("SAGAJS_PERF=1 才跑压测")
	}
	scenario := os.Getenv("SAGAJS_PERF_SCENARIO")
	if scenario == "" {
		scenario = "forward"
	}
	durability, durName := nest.DurabilityStrict, "strict"
	if os.Getenv("SAGAJS_PERF_DURABILITY") == "pipelined" {
		durability, durName = nest.DurabilityPipelined, "pipelined"
	}
	levels := strings.Fields(os.Getenv("SAGAJS_PERF_LEVELS"))
	if len(levels) == 0 {
		levels = []string{"1", "4", "16", "32", "64", "128"}
	}
	parts := envInt("SAGAJS_PERF_PARTS", 4)
	seconds := envInt("SAGAJS_PERF_SECONDS", 60)
	warmup := envInt("SAGAJS_PERF_WARMUP", 10)
	// 缺省不快照：快照保留期内的终态才在快照里（之后归投影），全量复读要求日志完整。快照的代价另跑一组量。
	snapEvery := envInt("SAGAJS_PERF_SNAPSHOT_EVERY", 0)
	retention := time.Duration(envInt("SAGAJS_PERF_RETENTION_MS", 2000)) * time.Millisecond
	label := os.Getenv("SAGAJS_PERF_LABEL")
	if label == "" {
		label = fmt.Sprintf("%s-%s-p%d", scenario, durName, parts)
	}
	outDir := os.Getenv("SAGAJS_PERF_OUT")
	walRoot := os.Getenv("SAGAJS_PERF_WAL_ROOT")
	if walRoot == "" {
		walRoot = t.TempDir()
	}
	for _, lv := range levels {
		conc, err := strconv.Atoi(lv)
		if err != nil {
			t.Fatal(err)
		}
		r := runPerfLevel(t, scenario, durability, parts, conc, seconds, warmup, snapEvery, retention, walRoot)
		r.Label, r.Durability = label, durName
		t.Logf("%s c%d diag: nest avg %.2f ms, result publish avg %.2f ms, coordinator publish avg %.2f ms", label, conc, r.NestAvgMs, r.ResultPubAvgMs, r.CoordPubAvgMs)
		t.Logf("%s c%d: TPS %.2f (terminal %d, unexpected %d) client P50/P99 %.1f/%.1f ms server %.1f/%.1f ms; per step: js msgs %.2f, publish calls %.2f, wal fsync %.3f; ev/call %.2f; balances=%v reread=%v (checked %d, pruned %d) replay %d msgs %.1f ms; snapshots %d purges %d unknown %d",
			label, conc, r.SteadyTPS, r.Terminal, r.Unexpected, r.ClientP50, r.ClientP99, r.ServerP50, r.ServerP99, r.JSMsgsStep, r.JSCallsStep, r.FsyncStep, r.EvPerCall,
			r.BalancesOK, r.RereadOK, r.RereadChecked, r.RereadPruned, r.ReplayMsgs, r.ReplayMs, r.Snapshots, r.Purges, r.Unknown)
		if outDir != "" {
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(outDir, label+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(r)
			_, _ = f.Write(append(raw, '\n'))
			_ = f.Close()
		}
		if r.Projection {
			t.Logf("%s c%d projection(%s): backlog peak participant %d records / saga %d events = %.1f sagas (%.2f × in-flight %d); drained=%v catch-up %.0f ms (left %d); projection rate load %.1f/s catch-up %.1f/s; mongo calls/record %.2f; cold diffs %d; mongo account diffs %d (checked=%v)",
				label, conc, r.ProjectionMode, r.PartBacklogPeak, r.SagaBacklogPeak, r.BacklogPeakSagas, r.BacklogRatio, conc, r.Drained, r.CatchUpMs, r.PartBacklogLeft,
				r.ProjRateDuringLoad, r.CatchUpRate, r.MongoCallsPerRecord, r.ColdDiffs, r.MongoAcctDiffs, r.MongoAcctChecked)
		}
		if !r.BalancesOK || !r.RereadOK || r.Unexpected > 0 || r.ColdDiffs > 0 || r.MongoAcctDiffs > 0 || (r.Projection && !r.Drained && os.Getenv("SAGAJS_PERF_ALLOW_UNDRAINED") != "1") {
			t.Errorf("%s c%d: verification failed (balances=%v reread=%v unexpected=%d)", label, conc, r.BalancesOK, r.RereadOK, r.Unexpected)
		}
	}
}

func runPerfLevel(t *testing.T, scenario string, durability nest.DurabilityPolicy, parts, conc, seconds, warmup, snapEvery int, retention time.Duration, walRoot string) perfResult {
	r := perfResult{Scenario: scenario, Partitions: parts, Concurrency: conc, GOMAXPROCS: runtime.GOMAXPROCS(0), Seconds: seconds, SnapshotEvery: snapEvery,
		LoadBefore: loadavg(), StartedAt: time.Now().Format(time.RFC3339)}
	e := newEnvGroups(t, parts, envInt("SAGAJS_PERF_STREAMS", 0))
	r.Streams = len(e.l.Streams())
	e.pairs = 256
	walDir := filepath.Join(walRoot, e.l.Prefix)
	defer os.RemoveAll(walDir)
	// SAGAJS_PERF_PROJECTION：part = 只开参与方投影（维护者 2026-10-27 第三轮定的缺省口径）；all（或 1）= 参与方 + saga 冷副本；
	// saga = 只开冷副本（定位用）；off / 0 / 空 = 都不开。
	mode := os.Getenv("SAGAJS_PERF_PROJECTION")
	partProj := mode == "part" || mode == "all" || mode == "1"
	coldProj := mode == "saga" || mode == "all" || mode == "1"
	projection := partProj || coldProj
	r.Projection = projection
	r.ProjectionMode = mode
	var sp *SagaProjector
	if projection {
		e.withMongo()
	}
	if coldProj {
		sp = e.sagaProjector()
	}
	var mongoCalls atomic.Uint64
	var partMongo fmongo.IMongo
	if partProj {
		partMongo = countingMongo{IMongo: e.mongo, calls: &mongoCalls}
	}
	part, err := NewParticipant(e.nc, e.js, ParticipantOptions{Layout: e.l, WALDir: walDir, Accounts: e.pairs * 2, InitialBal: initialBalance,
		Workers: 64, Durability: durability, Log: quietLog(), Mongo: partMongo, Database: e.db})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := part.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	coord := e.coordinator("A", func(o *CoordinatorOptions) { o.SnapshotEvery, o.Retention = snapEvery, retention })
	e.takeAll(coord, false)
	syncs0 := part.WALSyncs()
	projected0, calls0 := part.ProjectedRecords(), mongoCalls.Load()

	type sample struct {
		id         string
		start, end time.Time
	}
	var mu sync.Mutex
	var samples []sample
	var data sync.Map
	begin := time.Now()
	stopAt := begin.Add(time.Duration(seconds) * time.Second)
	var seq atomic.Int64
	var wg sync.WaitGroup
	runCtx, runCancel := context.WithTimeout(context.Background(), time.Duration(seconds+120)*time.Second)
	defer runCancel()
	// 投影积压采样（每 500ms）：参与方 WAL 未确认记录、冷副本未投影事件。
	samplerDone := make(chan struct{})
	samplerStop := make(chan struct{})
	if projection {
		go func() {
			defer close(samplerDone)
			tick := time.NewTicker(500 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-samplerStop:
					return
				case <-tick.C:
				}
				if n := part.ProjectionBacklog(); partProj && n > r.PartBacklogPeak {
					r.PartBacklogPeak = n
				}
				if coldProj {
					bctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					if n, err := sp.Backlog(bctx); err == nil && n > r.SagaBacklogPeak {
						r.SagaBacklogPeak = n
					}
					cancel()
				}
			}
		}()
	} else {
		close(samplerDone)
	}
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for time.Now().Before(stopAt) {
				i := int(seq.Add(1))
				id := fmt.Sprintf("s%d-%d", w, i)
				pair := w % e.pairs
				d := TransferData{From: int64(2 * pair), To: int64(2*pair + 1), Amount: 1, RefuseCredit: scenario == "compensate"}
				data.Store(id, d)
				started := time.Now()
				ch, err := e.client.Start(runCtx, id, d)
				if err != nil {
					t.Error(err)
					return
				}
				select {
				case <-ch:
				case <-runCtx.Done():
					t.Errorf("saga %s not terminal", id)
					return
				}
				mu.Lock()
				samples = append(samples, sample{id: id, start: started, end: time.Now()})
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	loadElapsed := time.Since(begin)
	if projection {
		close(samplerStop)
		<-samplerDone
		r.ProjectedDuringLoad = part.ProjectedRecords() - projected0
		r.ProjRateDuringLoad = float64(r.ProjectedDuringLoad) / loadElapsed.Seconds()
		r.PartBacklogAtStop = part.ProjectionBacklog()
		// 停止加压后两边积压归零的耗时（上限 SAGAJS_PERF_CATCHUP_MAX_S，缺省 60s；门槛 30s）。追不平时记剩余积压与这段时间的投影速率。
		catchMax := time.Duration(envInt("SAGAJS_PERF_CATCHUP_MAX_S", 60)) * time.Second
		catchBegin := time.Now()
		projectedAtStop := part.ProjectedRecords()
		for {
			drained := true
			if coldProj {
				bctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				n, err := sp.Backlog(bctx)
				cancel()
				drained = err == nil && n == 0
			}
			if partProj && part.ProjectionBacklog() != 0 {
				drained = false
			}
			if drained {
				r.Drained = true
				break
			}
			if time.Since(catchBegin) >= catchMax {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		r.CatchUpMs = float64(time.Since(catchBegin).Microseconds()) / 1000
		r.CatchUpRate = float64(part.ProjectedRecords()-projectedAtStop) / time.Since(catchBegin).Seconds()
		r.PartBacklogLeft = part.ProjectionBacklog()
		if projected := part.ProjectedRecords() - projected0; projected > 0 {
			r.MongoCallsPerRecord = float64(mongoCalls.Load()-calls0) / float64(projected)
		}
		recordsPerSaga, eventsPerSaga := 2.0, 3.0
		if scenario == "compensate" {
			recordsPerSaga, eventsPerSaga = 3.0, 4.0
		}
		r.BacklogPeakSagas = max(float64(r.PartBacklogPeak)/recordsPerSaga, float64(r.SagaBacklogPeak)/eventsPerSaga)
		r.BacklogRatio = r.BacklogPeakSagas / float64(conc)
	}
	time.Sleep(500 * time.Millisecond)
	final := e.client.Latest()
	want := StatusCompleted
	if scenario == "compensate" {
		want = StatusCompensated
	}
	var client, server []float64
	steady := 0
	lo, hi := begin.Add(time.Duration(warmup)*time.Second), stopAt
	var all []string
	for _, s := range samples {
		all = append(all, s.id)
		f := final[s.id]
		if f == nil || f.Status != want {
			r.Unexpected++
			continue
		}
		r.Terminal++
		if !s.end.Before(lo) && s.end.Before(hi) {
			steady++
			client = append(client, float64(s.end.Sub(s.start).Microseconds())/1000)
			server = append(server, float64(f.UpdatedAt-f.CreatedAt))
		}
	}
	sort.Float64s(client)
	sort.Float64s(server)
	r.SteadyTPS = float64(steady) / float64(seconds-warmup)
	r.ClientP50, r.ClientP99 = percentile(client, 0.5), percentile(client, 0.99)
	r.ServerP50, r.ServerP99 = percentile(server, 0.5), percentile(server, 0.99)
	executed := part.Stats.Executed.Load() + part.Stats.Refused.Load()
	r.Steps = executed + part.Stats.Replayed.Load()
	if r.Steps > 0 {
		msgs := coord.Stats.EvMsgs.Load() + part.Stats.ResultPubs.Load() + e.client.Pubs.Load()
		calls := coord.Stats.PubCalls.Load() + part.Stats.ResultPubs.Load() + e.client.Pubs.Load()
		r.JSMsgsStep = float64(msgs) / float64(r.Steps)
		r.JSCallsStep = float64(calls) / float64(r.Steps)
		r.FsyncStep = float64(part.WALSyncs()-syncs0) / float64(executed)
	}
	if n := part.Stats.ResultPubs.Load(); n > 0 {
		r.NestAvgMs = float64(part.Stats.NestNanos.Load()) / float64(r.Steps) / 1e6
		r.ResultPubAvgMs = float64(part.Stats.PubNanos.Load()) / float64(n) / 1e6
	}
	if c := coord.Stats.PubCalls.Load(); c > 0 {
		r.CoordPubAvgMs = float64(coord.Stats.PubNanos.Load()) / float64(c) / 1e6
		r.EvPerCall = float64(coord.Stats.EvMsgs.Load()) / float64(c)
	}
	r.Snapshots, r.Purges, r.Unknown = coord.Stats.Snapshots.Load(), coord.Stats.Purges.Load(), coord.Stats.Unknown.Load()

	// 余额核对（逐账户、守恒）。checkBalances 失败会 Fatal，这里先自己算一遍拿布尔结果。
	r.BalancesOK = func() bool {
		ok := true
		wantBal := map[int64]int64{}
		for _, id := range all {
			f := final[id]
			if f != nil && f.Status == StatusCompleted {
				wantBal[f.Data.From] -= f.Data.Amount
				wantBal[f.Data.To] += f.Data.Amount
			}
		}
		for n := int64(0); n < int64(e.pairs*2); n++ {
			bctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			got, err := part.Balance(bctx, n)
			cancel()
			if err != nil || got != initialBalance+wantBal[n] {
				ok = false
			}
		}
		return ok
	}()

	if projection && r.Drained {
		// 冷副本与观察到的终态逐笔核对；Mongo 账户与参与方内存逐账户核对。只在追平之后做（追不平如实记 drained=false）。
		cctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		if coldProj {
			cold, err := sp.ColdState(cctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range all {
				f, c := final[id], cold[id]
				if f != nil && (c.ID == "" || uint64(c.Version) != f.Version || c.Status != string(f.Status)) {
					r.ColdDiffs++
				}
			}
		}
		if partProj {
			mongoBal, err := part.MongoBalances(cctx)
			if err != nil {
				t.Fatal(err)
			}
			for n := int64(0); n < int64(e.pairs*2); n++ {
				mem, err := part.Balance(cctx, n)
				if err != nil || mongoBal[accountEntityID(n)] != mem {
					r.MongoAcctDiffs++
				}
			}
			r.MongoAcctChecked = true
		}
		cancel()
	}
	// 终态复读：停 A，新实例 B 强占全部分区从“快照 + 尾巴”重放，逐笔与发起方观察到的终态比较。
	coord.Stop()
	b := e.coordinator("B", nil)
	replayBegin := time.Now()
	e.takeAll(b, true)
	r.ReplayMs = float64(time.Since(replayBegin).Microseconds()) / 1000
	r.ReplayMsgs = b.Stats.ReplayMsgs.Load()
	b.Stop()
	// 复读：B 重建出的每一笔与发起方观察到的终态（版本、状态）一致；快照开着时，过了保留期的终态已从快照里剪掉
	// （生产由投影回答），只计数不判错；不快照时必须一笔不少。
	r.RereadOK = true
	rebuilt := map[string]*Saga{}
	for k := 0; k < parts; k++ {
		for id, s := range b.SnapshotState(k) {
			rebuilt[id] = s
		}
	}
	for _, id := range all {
		f := final[id]
		if f == nil {
			continue
		}
		s := rebuilt[id]
		switch {
		case s == nil && snapEvery > 0:
			r.RereadPruned++
		case s == nil || s.Version != f.Version || s.Status != f.Status:
			r.RereadOK = false
		default:
			r.RereadChecked++
		}
	}
	part.Stop()
	return r
}

// 重放时间与分区消息数的关系：合成一个分区的日志（每笔 saga 一条事件 + 一条结果，事件体与压测同形），
// 量没有快照时接管的重放时间（读 KV、读整段分区日志、建时间轮）。有快照时重放的是“快照解码 + 快照之后的尾巴”，
// 尾巴按同样的速率算。
func TestPerfReplayVsLogSize(t *testing.T) {
	if os.Getenv("SAGAJS_PERF") != "1" {
		t.Skip("SAGAJS_PERF=1 才跑压测")
	}
	sizes := strings.Fields(os.Getenv("SAGAJS_PERF_REPLAY_SIZES"))
	if len(sizes) == 0 {
		sizes = []string{"10000", "50000", "200000"}
	}
	for _, sz := range sizes {
		n, _ := strconv.Atoi(sz)
		e := newEnv(t, 1)
		m := machine{budget: DefaultBudget()}
		now := time.Now().UnixMilli()
		var futures []jetstream.PubAckFuture
		flush := func() {
			for _, f := range futures {
				select {
				case <-f.Ok():
				case err := <-f.Err():
					t.Fatal(err)
				}
			}
			futures = futures[:0]
		}
		for i := 0; i < n/2; i++ {
			s, cmd := m.start(fmt.Sprintf("r-%d", i), 0, TransferData{From: 1, To: 2, Amount: 1}, now)
			s.Version = 1
			raw, _ := json.Marshal(Event{V: WireVersion, Part: 0, Writer: "gen", At: now, Kind: "start", Saga: s, Cmd: cmd})
			f, err := e.js.PublishAsync(e.l.EvSubject(0, "acct"), raw)
			if err != nil {
				t.Fatal(err)
			}
			res, _ := json.Marshal(Completion{V: WireVersion, SagaID: s.ID, Op: cmd.Op, CmdID: cmd.CmdID, Phase: cmd.Phase, Step: 0, Success: true})
			f2, err := e.js.PublishAsync(e.l.ResultSubject(0), res)
			if err != nil {
				t.Fatal(err)
			}
			futures = append(futures, f, f2)
			if len(futures) >= 2000 {
				flush()
			}
		}
		flush()
		c := e.coordinator("R", func(o *CoordinatorOptions) { o.LeaseTTL = time.Hour })
		begin := time.Now()
		e.takeAll(c, true)
		full := time.Since(begin)
		// 结果都在 In 水位之后：接管后会被当作新输入处理（推进 n/2 笔），先停掉，只量重放。
		c.Stop()
		t.Logf("replay %d msgs (%d sagas) from an empty snapshot: %v (%.0f msgs/s)", n, n/2, full, float64(n)/full.Seconds())
	}
}
