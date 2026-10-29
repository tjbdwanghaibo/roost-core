//go:build integration

package saga

// 协调器在真实私有三节点集群（R3、文件存储）上的端到端：正向 / 补偿、发布结果未知、旧写者被隔离
// fail-stop、快照 + 清理 + 重放一致与快照不兼容拒绝启动、kill -9 协调器后重放、流满 fail-stop、10176 立即重发、准入信号过期
// 清除（LimitMarkerTTL）。参与方是用例里的简化实现：按操作记回执（同一操作只执行一次，之后回放结论），把结果经 Complete 发回分区。
//
// 环境：scripts/nats-cluster.sh 起的私有集群；ROOST_NATS_CLUSTER_URL 没设置就跳过。不读共享隔离环境的变量。
// 变异：ROOST_SAGA_MUTATE=nofence 让用例里的日志包装去掉期望序号，TestRealJetStreamCoordinatorFencesAStaleWriter 必须变红。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

var realJSSeq atomic.Int64

// realJSConn 是一条到私有集群的连接：日志原语与核心 NATS（与生产同一装配）。
type realJSConn struct {
	client *ndriver.Client
	log    *ndriver.JetStreamLogClient
}

func realJSURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROOST_NATS_CLUSTER_URL")
	if url == "" {
		t.Skip("ROOST_NATS_CLUSTER_URL is not set: scripts/nats-cluster.sh start && eval \"$(scripts/nats-cluster.sh env)\"")
	}
	return url
}

func dialRealJS(t *testing.T, url string) realJSConn {
	t.Helper()
	cfg := fnats.DefaultConfig(url)
	cfg.ReconnectWait = 100 * time.Millisecond
	client, err := ndriver.NewClient(cfg, ndriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	js, err := ndriver.NewJetStreamClient(client)
	if err != nil {
		t.Fatal(err)
	}
	log, err := ndriver.NewJetStreamLog(js)
	if err != nil {
		t.Fatal(err)
	}
	return realJSConn{client: client, log: log}
}

// realLayout 是用例私有的布局（唯一前缀与流名），用例结束删流与桶。
func realLayout(t *testing.T, url string, partitions, groups int) Layout {
	t.Helper()
	token := fmt.Sprintf("%d%d", time.Now().UnixNano()%1_000_000_000, realJSSeq.Add(1))
	layout := Layout{Prefix: "sjs" + token + ".saga", Stream: "SJS" + token, Partitions: partitions, Groups: groups}
	t.Cleanup(func() { deleteRealLayout(t, url, layout) })
	return layout
}

func deleteRealLayout(t *testing.T, url string, layout Layout) {
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Errorf("cleanup: connect: %v", err)
		return
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Errorf("cleanup: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, name := range layout.Streams() {
		if err := js.DeleteStream(ctx, name); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
			t.Errorf("cleanup: delete stream %s: %v", name, err)
		}
	}
	for _, bucket := range []string{layout.SnapshotBucket(), layout.LoadBucket()} {
		if err := js.DeleteKeyValue(ctx, bucket); err != nil && !errors.Is(err, gojs.ErrBucketNotFound) {
			t.Errorf("cleanup: delete bucket %s: %v", bucket, err)
		}
	}
	if err := js.DeleteObjectStore(ctx, layout.ObjectBucket()); err != nil && !errors.Is(err, gojs.ErrBucketNotFound) && !errors.Is(err, gojs.ErrStreamNotFound) {
		t.Errorf("cleanup: delete object store: %v", err)
	}
}

// giftDefinition 是端到端用的两步定义：debit → credit，短超时、3 次尝试。
func giftDefinition() Definition {
	step := func(name string) Step {
		return Step{Name: name, ForwardTopic: "gift." + name, CompensateTopic: "gift." + name + ".cancel", Timeout: 1500 * time.Millisecond, MaxAttempts: 4, BackoffMin: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond}
	}
	return Definition{Type: "gift", Version: 1, Steps: []Step{step("debit"), step("credit")}}
}

// realEngineOptions 是端到端的缺省选项（R3，短预算）。
func realEngineOptions(layout Layout, owned []int) Options {
	return Options{
		Layout: layout, Owned: owned, Replicas: 3, StreamMaxBytes: 256 << 20, DuplicateWindow: 40 * time.Second,
		PublishTimeout: 2 * time.Second, PublishUnknownBudget: 15 * time.Second, QueryTimeout: 2 * time.Second, StartVerdictTimeout: 2 * time.Second,
		SnapshotEveryEvents: 1 << 20, SnapshotInterval: time.Hour, TerminalRetention: time.Hour,
	}
}

func allPartitions(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// startRealEngine 建、登记、启动一个协调器，用例结束停掉。log 为空时用 conn 的日志。
func startRealEngine(t *testing.T, conn realJSConn, log fnats.JetStreamLog, options Options) *Engine {
	t.Helper()
	if log == nil {
		log = conn.log
	}
	e, err := NewEngine(log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Register(giftDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := e.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return e
}

// testParticipant 是用例里的参与方：跟随全部命令事件，按操作（IdempotencyKey）记回执——同一操作第一次执行业务，之后回放结论；
// 业务结论由 decide 给出。结果经 Complete 发回分区（失败重试到成功）。executed 计业务真正执行的次数（按操作）。
type testParticipant struct {
	t        *testing.T
	engine   *Engine
	layout   Layout
	decide   func(Command) Completion
	delay    time.Duration
	mu       sync.Mutex
	receipts map[string]Completion
	executed map[string]int
	seen     map[string]bool
	writers  map[string]map[string]bool // saga → 派发过它的命令的写者
	follows  []fnats.LogFollow
	wg       sync.WaitGroup
	stop     chan struct{}
}

func startParticipant(t *testing.T, log fnats.JetStreamLog, engine *Engine, layout Layout, decide func(Command) Completion) *testParticipant {
	t.Helper()
	p := &testParticipant{t: t, engine: engine, layout: layout, decide: decide, receipts: map[string]Completion{}, executed: map[string]int{}, seen: map[string]bool{}, writers: map[string]map[string]bool{}, stop: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, stream := range layout.Streams() {
		follow, err := log.Follow(ctx, stream, []string{layout.Prefix + ".*.ev.cmd.>"}, 0, p.onEvent)
		if err != nil {
			t.Fatal(err)
		}
		p.follows = append(p.follows, follow)
	}
	t.Cleanup(p.close)
	return p
}

func (p *testParticipant) close() {
	select {
	case <-p.stop:
		return
	default:
		close(p.stop)
	}
	for _, f := range p.follows {
		_ = f.Stop(context.Background())
	}
	p.wg.Wait()
}

func (p *testParticipant) onEvent(msg fnats.LogMsg) {
	rest := strings.TrimPrefix(msg.Subject, p.layout.Prefix+".")
	k, err := strconv.Atoi(rest[:strings.IndexByte(rest, '.')])
	if err != nil {
		return
	}
	ev, err := decodeEvent(msg.Data, p.layout, k)
	if err != nil || ev.Command == nil {
		p.t.Errorf("participant got an undecodable command event: %v", err)
		return
	}
	command := *ev.Command
	p.mu.Lock()
	if p.writers[command.SagaID] == nil {
		p.writers[command.SagaID] = map[string]bool{}
	}
	p.writers[command.SagaID][ev.Writer] = true
	if p.seen[command.ID] {
		p.mu.Unlock()
		return
	}
	p.seen[command.ID] = true
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if p.delay > 0 {
			select {
			case <-time.After(p.delay):
			case <-p.stop:
				return
			}
		}
		p.mu.Lock()
		receipt, ok := p.receipts[command.IdempotencyKey]
		if !ok {
			receipt = p.decide(command)
			p.receipts[command.IdempotencyKey] = receipt
			p.executed[command.IdempotencyKey]++
		}
		p.mu.Unlock()
		completion := receipt
		completion.CommandID, completion.IdempotencyKey, completion.SagaID = command.ID, command.IdempotencyKey, command.SagaID
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := p.engine.Complete(ctx, completion)
			cancel()
			if err == nil {
				return
			}
			select {
			case <-p.stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
}

// giftDecide：业务键以 refuse 开头的 saga 第二步（credit）被拒；其余都成功，Data 带步骤名。
func giftDecide(command Command) Completion {
	if command.Phase == PhaseForward && command.StepName == "credit" && strings.HasPrefix(command.BusinessKey, "refuse") {
		return Completion{Error: "credit refused"}
	}
	return Completion{Success: true, Data: []byte(command.StepName)}
}

func waitTerminal(t *testing.T, e *Engine, id string, budget time.Duration) Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	record, err := e.WaitTerminal(ctx, id)
	if err != nil {
		t.Fatalf("wait terminal %s: %v", id, err)
	}
	return record
}

// readPartitionEvents 读分区 k 现存的全部事件（按流序号）。
func readPartitionEvents(t *testing.T, log fnats.JetStreamLog, layout Layout, k int) []logEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	last, err := log.LastMsg(ctx, layout.StreamOf(k), layout.EventFilter(k))
	if errors.Is(err, fnats.ErrLogNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var events []logEvent
	err = log.ReadRange(ctx, layout.StreamOf(k), []string{layout.EventFilter(k)}, 1, last.Seq, func(msg fnats.LogMsg) error {
		ev, err := decodeEvent(msg.Data, layout, k)
		if err != nil {
			return err
		}
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// assertLinearChain 断言分区事件链没有分叉：同一 saga 的事件版本严格递增（late_alarm 只改 tombstone、不加版本，可以相等）。
func assertLinearChain(t *testing.T, events []logEvent) {
	t.Helper()
	last := map[string]uint64{}
	for i, ev := range events {
		id, version := ev.Saga.Record.ID, ev.Saga.Record.Version
		if prev, ok := last[id]; ok && (version < prev || (version == prev && ev.Kind != "late_alarm")) {
			t.Fatalf("event %d forks saga %s: version %d after %d (writer %s, kind %s)", i, id, version, prev, ev.Writer, ev.Kind)
		}
		last[id] = version
	}
}

func TestRealJetStreamCoordinatorForwardAndCompensation(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 4, 2)
	owner := startRealEngine(t, dialRealJS(t, url), nil, realEngineOptions(layout, allPartitions(4)))
	clientConn := dialRealJS(t, url)
	client := startRealEngine(t, clientConn, nil, realEngineOptions(layout, nil))
	participant := startParticipant(t, clientConn.log, client, layout, giftDecide)

	type started struct {
		bk     string
		record Record
	}
	var keys []string
	for i := range 12 {
		keys = append(keys, "ok-"+strconv.Itoa(i))
	}
	for i := range 6 {
		keys = append(keys, "refuse-"+strconv.Itoa(i))
	}
	results := make(chan started, len(keys))
	var wg sync.WaitGroup
	for _, bk := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			record, err := client.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: bk, DefinitionVersion: 1, Data: []byte("payload-" + bk)})
			if err != nil {
				t.Errorf("start %s: %v", bk, err)
				return
			}
			results <- started{bk, record}
		}()
	}
	wg.Wait()
	close(results)
	for s := range results {
		if s.record.ID != IDOf("gift", s.bk) || s.record.StartAttempt == "" || s.record.Version < 1 {
			t.Fatalf("start %s returned %+v", s.bk, s.record)
		}
		final := waitTerminal(t, client, s.record.ID, 30*time.Second)
		want := StatusCompleted
		if strings.HasPrefix(s.bk, "refuse") {
			want = StatusCompensated
		}
		if final.Status != want {
			t.Fatalf("%s ended %v (%s), want %v", s.bk, final.Status, final.LastError, want)
		}
		if final.StartAttempt != s.record.StartAttempt {
			t.Fatalf("%s: start attempt %q changed to %q", s.bk, s.record.StartAttempt, final.StartAttempt)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// 同一意图再启动 = 已存在（返回终态）；同业务键不同 Data = 身份冲突。
	again, err := client.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: "ok-0", DefinitionVersion: 1, Data: []byte("payload-ok-0")})
	if err != nil || again.Status != StatusCompleted {
		t.Fatalf("repeated start: %+v %v", again, err)
	}
	if _, err := client.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: "ok-0", DefinitionVersion: 1, Data: []byte("other")}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("conflicting start: %v", err)
	}
	if got, err := client.GetByBusinessKey(ctx, "gift", "refuse-1"); err != nil || got.Status != StatusCompensated {
		t.Fatalf("GetByBusinessKey: %+v %v", got, err)
	}
	if _, err := client.GetByBusinessKey(ctx, "gift", "never"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing saga: %v", err)
	}
	all, err := client.List(ctx, Query{Type: "gift", Limit: 1000})
	if err != nil || len(all) != len(keys) {
		t.Fatalf("List: %d records, %v", len(all), err)
	}
	compensated, err := client.List(ctx, Query{Statuses: []Status{StatusCompensated}, Limit: 1000})
	if err != nil || len(compensated) != 6 {
		t.Fatalf("List compensated: %d, %v", len(compensated), err)
	}
	// 每个操作只执行一次业务；协调器侧没有失败。
	participant.mu.Lock()
	for op, n := range participant.executed {
		if n != 1 {
			t.Errorf("operation %s executed %d times", op, n)
		}
	}
	participant.mu.Unlock()
	if s := owner.Stats(); s.FailStops != 0 || s.Completed != 12 || s.Compensated != 6 || s.Started != uint64(len(keys)) {
		t.Fatalf("owner stats = %+v", s)
	}
	for k := range layout.Partitions {
		assertLinearChain(t, readPartitionEvents(t, clientConn.log, layout, k))
	}
	// Resume / Compensate 走 in.op：Completed 不能补偿。
	if _, err := client.Compensate(ctx, IDOf("gift", "ok-1"), "operator", time.Time{}); !errors.Is(err, ErrSagaCompleted) {
		t.Fatalf("compensate a completed saga: %v", err)
	}
	if _, err := client.Resume(ctx, ResumeRequest{ID: IDOf("gift", "ok-1")}); err == nil || !strings.Contains(err.Error(), "cannot resume") {
		t.Fatalf("resume a completed saga: %v", err)
	}
}

// unknownLog 包装真实日志：按 drop 决定某次分区事件发布（带期望序号的）照常发出、但丢掉答复，返回 ErrLogOutcomeUnknown
// （实际已落库）；incomplete 决定某次发布不发出、直接返回 ErrLogBatchIncomplete（服务端丢批，确定没写入）。
// ROOST_SAGA_MUTATE=nofence 时去掉期望序号（变异：隔离必须失效）。
type unknownLog struct {
	fnats.JetStreamLog
	drop       func(msgs []fnats.LogMsg) bool
	incomplete func(msgs []fnats.LogMsg) bool
	dropped    atomic.Int64
	refused    atomic.Int64
}

func (l *unknownLog) PublishFenced(ctx context.Context, msgs []fnats.LogMsg, fence fnats.LogFence) (fnats.LogPublishAck, error) {
	if os.Getenv("ROOST_SAGA_MUTATE") == "nofence" {
		fence = fnats.LogFence{}
	}
	if fence.Filter != "" && l.incomplete != nil && l.incomplete(msgs) {
		l.refused.Add(1)
		return fnats.LogPublishAck{}, fmt.Errorf("%w: injected 10176", fnats.ErrLogBatchIncomplete)
	}
	ack, err := l.JetStreamLog.PublishFenced(ctx, msgs, fence)
	if err == nil && fence.Filter != "" && l.drop != nil && l.drop(msgs) {
		l.dropped.Add(1)
		return fnats.LogPublishAck{}, fmt.Errorf("%w: injected lost PubAck (written at %d)", fnats.ErrLogOutcomeUnknown, ack.Seq)
	}
	return ack, err
}

// 发布结果未知：单条重发得到 Duplicate、批重发得到 10201（或序号不符但最后一条是自己的）都判为已提交；10176 立即重发。事件链不分叉、
// 不 fail-stop，saga 照常完成。
func TestRealJetStreamCoordinatorResolvesUnknownOutcomesAndRepublishesIncompleteBatches(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 2, 1)
	conn := dialRealJS(t, url)
	var calls atomic.Int64
	seenSingle, seenBatch := atomic.Bool{}, atomic.Bool{}
	wrapped := &unknownLog{JetStreamLog: conn.log}
	// 每个 MsgID 只丢一次答复（重发时照常返回）：第一次单条与第一次多条一定丢，之后每 4 次丢一次。
	dropOnce := sync.Map{}
	wrapped.drop = func(msgs []fnats.LogMsg) bool {
		if _, loaded := dropOnce.LoadOrStore(msgs[0].MsgID, true); loaded {
			return false
		}
		if len(msgs) == 1 && !seenSingle.Swap(true) {
			return true
		}
		if len(msgs) > 1 && !seenBatch.Swap(true) {
			return true
		}
		return calls.Add(1)%4 == 0
	}
	// 每 5 次不同的发布注入一次 10176（不发出，确定没写入）：同一组消息的重发照常发出。
	var distinct atomic.Int64
	incompleteOnce := sync.Map{}
	wrapped.incomplete = func(msgs []fnats.LogMsg) bool {
		_, loaded := incompleteOnce.LoadOrStore(msgs[0].MsgID, true)
		return !loaded && distinct.Add(1)%5 == 0
	}
	owner := startRealEngine(t, conn, wrapped, realEngineOptions(layout, allPartitions(2)))
	clientConn := dialRealJS(t, url)
	client := startRealEngine(t, clientConn, nil, realEngineOptions(layout, nil))
	startParticipant(t, clientConn.log, client, layout, giftDecide)
	// 先顺序跑几笔（一轮多是单条事件），再并发（一轮多条，原子批量）。
	runOne := func(i int) {
		bk := "u-" + strconv.Itoa(i)
		if i%4 == 0 {
			bk = "refuse-" + strconv.Itoa(i)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		record, err := client.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: bk, DefinitionVersion: 1})
		if err != nil {
			t.Errorf("start %s: %v", bk, err)
			return
		}
		if final := waitTerminal(t, client, record.ID, 40*time.Second); final.Status != StatusCompleted && final.Status != StatusCompensated {
			t.Errorf("%s ended %v", bk, final.Status)
		}
	}
	for i := 100; i < 104; i++ {
		runOne(i)
	}
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runOne(i)
		}()
	}
	wg.Wait()
	// 最后一次被丢答复的发布可能还在重发（50ms 一次）：等每次丢的答复都判定完再读计数。
	for deadline := time.Now().Add(10 * time.Second); owner.Stats().PublishResolved < uint64(wrapped.dropped.Load()) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	s := owner.Stats()
	t.Logf("dropped acks %d, injected 10176 %d, stats %+v (single dropped %v, batch dropped %v)", wrapped.dropped.Load(), wrapped.refused.Load(), s, seenSingle.Load(), seenBatch.Load())
	// 每次被丢的答复都判为“已提交”（重发得到 Duplicate / 10201 / 最后一条是自己的），没有一次当成没写而再写一遍（事件链不分叉）。
	if s.FailStops != 0 || s.PublishUnknown < uint64(wrapped.dropped.Load()) || s.PublishResolved != uint64(wrapped.dropped.Load()) || s.BatchIncomplete == 0 {
		t.Fatalf("stats = %+v", s)
	}
	if !seenSingle.Load() || !seenBatch.Load() {
		t.Fatalf("both branches must be hit: single %v batch %v", seenSingle.Load(), seenBatch.Load())
	}
	for k := range layout.Partitions {
		assertLinearChain(t, readPartitionEvents(t, conn.log, layout, k))
	}
}

// 旧写者（冻住的协调器在别人接管后恢复）发布被期望序号拒绝 → ErrPartitionFenced → RuntimeFailure（整个进程 fail-stop）；事件链
// 不分叉，新写者照常推进。ROOST_SAGA_MUTATE=nofence 时这个用例必须变红。
func TestRealJetStreamCoordinatorFencesAStaleWriter(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 1, 1)
	connA := dialRealJS(t, url)
	failureA := app.NewRuntimeFailure()
	optionsA := realEngineOptions(layout, []int{0})
	optionsA.Writer, optionsA.Failure = "writer-a", failureA
	var freeze atomic.Bool
	frozen, release := make(chan struct{}, 1), make(chan struct{})
	engineA, err := NewEngine(&unknownLog{JetStreamLog: connA.log}, connA.client, optionsA)
	if err != nil {
		t.Fatal(err)
	}
	engineA.hooks.beforePublish = func(int) {
		if freeze.Load() {
			select {
			case frozen <- struct{}{}:
			default:
			}
			<-release
		}
	}
	if err := engineA.Register(giftDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := engineA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = engineA.Stop(stopCtx)
	}()
	clientConn := dialRealJS(t, url)
	client := startRealEngine(t, clientConn, nil, realEngineOptions(layout, nil))
	startParticipant(t, clientConn.log, client, layout, giftDecide)
	first, err := client.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: "before", DefinitionVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, client, first.ID, 30*time.Second)

	// 冻住 A 的下一次发布，再起同一分区的 B（崩溃重启的重叠）：B 重放、接着处理同一条输入并先写进去。
	freeze.Store(true)
	startCtx, startCancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer startCancel()
	secondErr := make(chan error, 1)
	go func() {
		_, err := client.StartSaga(startCtx, StartRequest{Type: "gift", BusinessKey: "after", DefinitionVersion: 1})
		secondErr <- err
	}()
	select {
	case <-frozen:
	case <-time.After(20 * time.Second):
		t.Fatal("writer A never tried to publish")
	}
	connB := dialRealJS(t, url)
	optionsB := realEngineOptions(layout, []int{0})
	optionsB.Writer = "writer-b"
	startRealEngine(t, connB, &unknownLog{JetStreamLog: connB.log}, optionsB)
	if err := <-secondErr; err != nil {
		t.Fatalf("start served by B: %v", err)
	}
	waitTerminal(t, client, IDOf("gift", "after"), 30*time.Second)
	close(release)
	select {
	case err := <-failureA.Done():
		if !errors.Is(err, ErrPartitionFenced) {
			t.Fatalf("A failed with %v, want ErrPartitionFenced", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("stale writer A did not fail-stop")
	}
	events := readPartitionEvents(t, clientConn.log, layout, 0)
	assertLinearChain(t, events)
	sawB := false
	for _, ev := range events {
		switch ev.Writer {
		case "writer-b":
			sawB = true
		case "writer-a":
			if sawB {
				t.Fatalf("writer A wrote after B took over: %+v", ev.Saga.Record)
			}
		}
	}
	if !sawB {
		t.Fatal("B never wrote")
	}
}

// 快照 + 清理 + 重放一致：停掉协调器、换一个新写者从快照与尾巴重放，状态逐笔相同；日志确实被清理过。快照的版本 / 分区 / 布局 / 摘要
// 任何一项不符都拒绝启动（ErrSnapshotIncompatible），不退回从头重放。
func TestRealJetStreamCoordinatorSnapshotPurgeAndReplayAgree(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 2, 2)
	conn := dialRealJS(t, url)
	options := realEngineOptions(layout, allPartitions(2))
	options.SnapshotEveryEvents = 8
	options.Writer = "first"
	ownerCtxConn := dialRealJS(t, url)
	owner, err := NewEngine(ownerCtxConn.log, ownerCtxConn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Register(giftDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := owner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	client := startRealEngine(t, conn, nil, realEngineOptions(layout, nil))
	startParticipant(t, conn.log, client, layout, giftDecide)
	for i := range 30 {
		bk := "s-" + strconv.Itoa(i)
		if i%5 == 0 {
			bk = "refuse-" + strconv.Itoa(i)
		}
		record, err := client.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: bk, DefinitionVersion: 1, Data: []byte(bk)})
		if err != nil {
			t.Fatal(err)
		}
		waitTerminal(t, client, record.ID, 30*time.Second)
	}
	deadline := time.Now().Add(20 * time.Second)
	for owner.Stats().Purges < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if s := owner.Stats(); s.Snapshots < 2 || s.Purges < 2 {
		t.Fatalf("stats = %+v, want snapshots and purges on both partitions", s)
	}
	before, err := client.List(ctx, Query{Limit: 1000})
	if err != nil || len(before) != 30 {
		t.Fatalf("list before: %d %v", len(before), err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := owner.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	stopCancel()
	purged := false
	for _, stream := range layout.Streams() {
		var firstSeq uint64
		_ = conn.log.ReadRange(ctx, stream, []string{layout.Prefix + ".>"}, 1, 0, func(msg fnats.LogMsg) error {
			firstSeq = msg.Seq
			return errStopRead
		})
		if firstSeq > 1 {
			purged = true
		}
	}
	if !purged {
		t.Fatal("no stream was purged below its snapshot")
	}
	options.Writer = "second"
	replayed := startRealEngine(t, dialRealJS(t, url), nil, options)
	after, err := client.List(ctx, Query{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("after replay %d records, before %d", len(after), len(before))
	}
	for i := range before {
		if !sameRecord(before[i], after[i]) {
			t.Fatalf("record %s differs after replay:\nbefore %+v\nafter  %+v", before[i].ID, before[i], after[i])
		}
	}
	if replayed.Stats().Replayed == 0 {
		t.Log("replay read no messages (everything was in the snapshots)")
	}
	stopCtx, stopCancel = context.WithTimeout(context.Background(), 20*time.Second)
	if err := replayed.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	stopCancel()

	// 篡改快照清单：版本、分区、布局、摘要各一例，新进程必须拒绝启动。
	kv, err := conn.log.KeyValue(ctx, fnats.LogKVConfig{Bucket: layout.SnapshotBucket(), Replicas: 3, History: 1})
	if err != nil {
		t.Fatal(err)
	}
	original, err := kv.Get(ctx, snapshotKey(0))
	if err != nil {
		t.Fatal(err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(original.Value, &manifest); err != nil {
		t.Fatal(err)
	}
	for name, tamper := range map[string]func(*snapshotManifest){
		"version":   func(m *snapshotManifest) { m.V = 1 },
		"partition": func(m *snapshotManifest) { m.Part = 1 },
		"layout":    func(m *snapshotManifest) { m.Layout.Partitions = 3 },
		"digest":    func(m *snapshotManifest) { m.SHA256 = strings.Repeat("0", 64) },
	} {
		m := manifest
		tamper(&m)
		if _, err := kv.Put(ctx, snapshotKey(0), mustJSON(t, m)); err != nil {
			t.Fatal(err)
		}
		bad, err := NewEngine(conn.log, conn.client, options)
		if err != nil {
			t.Fatal(err)
		}
		_ = bad.Register(giftDefinition())
		startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = bad.Start(startCtx)
		startCancel()
		if !errors.Is(err, ErrSnapshotIncompatible) {
			t.Fatalf("%s: Start = %v, want ErrSnapshotIncompatible", name, err)
		}
	}
}

var errStopRead = errors.New("stop")

func sameRecord(a, b Record) bool {
	a.UpdatedAt, b.UpdatedAt = a.UpdatedAt.UTC(), b.UpdatedAt.UTC()
	a.CreatedAt, b.CreatedAt = a.CreatedAt.UTC(), b.CreatedAt.UTC()
	a.NextRunAt, b.NextRunAt = a.NextRunAt.UTC(), b.NextRunAt.UTC()
	a.DeadlineAt, b.DeadlineAt = a.DeadlineAt.UTC(), b.DeadlineAt.UTC()
	return a.ID == b.ID && a.Version == b.Version && a.Status == b.Status && a.Phase == b.Phase && a.Step == b.Step &&
		a.CompletedSteps == b.CompletedSteps && a.Incarnation == b.Incarnation && string(a.Data) == string(b.Data) &&
		a.UpdatedAt.Equal(b.UpdatedAt) && a.StartAttempt == b.StartAttempt && a.StartDigest == b.StartDigest && a.LastError == b.LastError
}

// 流满（MaxBytes + Discard=New）：服务端明确拒绝分区事件（ErrLogRejected）→ ErrPartitionRejected，fail-stop，不当结果未知重试。
func TestRealJetStreamCoordinatorFailsStopWhenTheStreamIsFull(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 1, 1)
	conn := dialRealJS(t, url)
	failure := app.NewRuntimeFailure()
	options := realEngineOptions(layout, []int{0})
	options.StreamMaxBytes = 512 << 10
	options.Failure = failure
	owner := startRealEngine(t, conn, nil, options)
	clientConn := dialRealJS(t, url)
	clientOptions := realEngineOptions(layout, nil)
	clientOptions.StreamMaxBytes = options.StreamMaxBytes
	client := startRealEngine(t, clientConn, nil, clientOptions)
	payload := []byte(strings.Repeat("x", 30<<10))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() {
		for i := 0; ctx.Err() == nil; i++ {
			startCtx, startCancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := client.StartSaga(startCtx, StartRequest{Type: "gift", BusinessKey: "full-" + strconv.Itoa(i), DefinitionVersion: 1, Data: payload})
			startCancel()
			if err != nil && strings.Contains(err.Error(), "rejected") {
				return // 输入也写不进去了
			}
		}
	}()
	select {
	case err := <-failure.Done():
		if !errors.Is(err, ErrPartitionRejected) {
			t.Fatalf("fail-stop cause %v, want ErrPartitionRejected", err)
		}
	case <-ctx.Done():
		t.Fatalf("the coordinator did not fail-stop on a full stream; stats %+v", owner.Stats())
	}
	if s := owner.Stats(); s.FailStops != 1 || s.PublishUnknown != 0 {
		t.Fatalf("stats = %+v (a full stream is a definite refusal, not an unknown outcome)", s)
	}
}

// 准入信号过期清除：LoadBucket 开 LimitMarkerTTL，崩溃参与方的键过期时 Watch 收到删除，该参与方不再计入，准入重新打开。
// 停更期间按本机时钟判过期（stale）同样关闭。
func TestRealJetStreamCoordinatorAdmissionForgetsAnExpiredParticipant(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 1, 1)
	conn := dialRealJS(t, url)
	options := realEngineOptions(layout, []int{0})
	options.Admission = AdmissionOptions{Enabled: true, HighRecords: 100, LowRecords: 50, StaleAfter: 700 * time.Millisecond, SignalTTL: 2 * time.Second, HintEvery: 200 * time.Millisecond}
	owner := startRealEngine(t, conn, nil, options)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	loads, err := conn.log.KeyValue(ctx, loadBucketConfig(layout, 3, options.Admission.withDefaults()))
	if err != nil {
		t.Fatal(err)
	}
	waitAdmission := func(want bool, reason string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if s := owner.AdmissionState(); s.Open == want && (reason == "" || s.Reason == reason) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("admission never became open=%v %q: %+v", want, reason, owner.AdmissionState())
	}
	waitAdmission(true, "")
	if _, err := loads.Put(ctx, "part.game.1", mustJSON(t, participantLoad{V: WireVersion, Records: 500})); err != nil {
		t.Fatal(err)
	}
	waitAdmission(false, AdmissionReasonBacklog)
	if _, err := owner.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: "blocked", DefinitionVersion: 1}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("start while closed: %v", err)
	}
	// 参与方“崩溃”：不再写。先因停更关闭（stale），键过期（2s）时服务端写删除标记，准入重新打开。
	waitAdmission(false, AdmissionReasonStale)
	waitAdmission(true, "")
	// 发起方据 admit 提示快速失败；提示随所有者的判定在一次发布周期内跟上，这里给它 3s。
	var record Record
	for retry := time.Now().Add(3 * time.Second); ; {
		record, err = owner.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: "after-expiry", DefinitionVersion: 1})
		if !errors.Is(err, ErrBackpressure) || time.Now().After(retry) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || record.ID != IDOf("gift", "after-expiry") {
		t.Fatalf("start after the participant expired: %+v %v", record, err)
	}
}

// ---- kill -9 协调器 ----

const childRoleEnv = "ROOST_SAGA_JS_COORDINATOR_CHILD"

// TestRealJetStreamCoordinatorChild 是被 kill -9 用例拉起的子进程角色：按环境变量里的布局跑一个拥有全部分区的协调器，
// Start 成功后打印 READY，然后一直跑到被杀。直接运行时跳过。
func TestRealJetStreamCoordinatorChild(t *testing.T) {
	if os.Getenv(childRoleEnv) != "1" {
		t.Skip("child role of TestRealJetStreamCoordinatorKill9Replays")
	}
	partitions, _ := strconv.Atoi(os.Getenv("ROOST_SAGA_JS_PARTITIONS"))
	groups, _ := strconv.Atoi(os.Getenv("ROOST_SAGA_JS_GROUPS"))
	layout := Layout{Prefix: os.Getenv("ROOST_SAGA_JS_PREFIX"), Stream: os.Getenv("ROOST_SAGA_JS_STREAM"), Partitions: partitions, Groups: groups}
	conn := dialRealJS(t, os.Getenv("ROOST_NATS_CLUSTER_URL"))
	options := realEngineOptions(layout, allPartitions(partitions))
	options.Writer = "child-" + strconv.Itoa(os.Getpid())
	options.SnapshotEveryEvents = 16
	e, err := NewEngine(conn.log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Register(giftDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	fmt.Println("READY")
	select {}
}

type childProcess struct {
	cmd *exec.Cmd
}

func startCoordinatorChild(t *testing.T, layout Layout) *childProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRealJetStreamCoordinatorChild$", "-test.v", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), childRoleEnv+"=1",
		"ROOST_SAGA_JS_PREFIX="+layout.Prefix, "ROOST_SAGA_JS_STREAM="+layout.Stream,
		"ROOST_SAGA_JS_PARTITIONS="+strconv.Itoa(layout.Partitions), "ROOST_SAGA_JS_GROUPS="+strconv.Itoa(layout.Groups))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &childProcess{cmd: cmd}
	t.Cleanup(child.kill)
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "READY" {
				ready <- true
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("coordinator child exited before READY")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("coordinator child not ready")
	}
	return child
}

func (c *childProcess) kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGKILL)
		_, _ = c.cmd.Process.Wait()
	}
}

// kill -9 协调器后同布局重启：快照 + 尾巴重放、时间轮重建，在途 saga 按截止继续，全部到终态；每个操作只执行一次业务（参与方回执），
// 事件链不分叉。
func TestRealJetStreamCoordinatorKill9Replays(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 4, 2)
	conn := dialRealJS(t, url)
	// 发起方与参与方在本进程（客户端一侧，不拥有分区）；协调器在子进程。
	clientOptions := realEngineOptions(layout, nil)
	client := startRealEngine(t, conn, nil, clientOptions)
	participant := startParticipant(t, conn.log, client, layout, giftDecide)
	participant.delay = 150 * time.Millisecond
	child := startCoordinatorChild(t, layout)

	var ids []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	stopStarting := make(chan struct{})
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stopStarting:
					return
				default:
				}
				bk := fmt.Sprintf("k%d-%d", w, i)
				if i%5 == 0 {
					bk = fmt.Sprintf("refuse%d-%d", w, i)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := client.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: bk, DefinitionVersion: 1})
				cancel()
				if err == nil || errors.Is(err, ErrStartUnconfirmed) {
					mu.Lock()
					ids = append(ids, IDOf("gift", bk))
					mu.Unlock()
				} else {
					t.Errorf("start %s: %v", bk, err)
				}
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond)
	child.kill() // kill -9：在途的发布、未提交的轮、未写的快照都丢了
	// 长停机：ROOST_SAGA_JS_RESTART_DELAY=60s 让协调器停 60s 再拉起（与参与方 kill -9 用例同一个开关）；期间发起方的启动意图
	// 照样进日志、等不到判定，在途步骤的截止全部过去，重启后按到期重试。
	restartDelay := 500 * time.Millisecond
	if d, _ := time.ParseDuration(os.Getenv("ROOST_SAGA_JS_RESTART_DELAY")); d > 0 {
		restartDelay = d
	}
	time.Sleep(restartDelay)
	startCoordinatorChild(t, layout)
	time.Sleep(1500 * time.Millisecond)
	close(stopStarting)
	wg.Wait()
	mu.Lock()
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	mu.Unlock()
	if len(ids) < 10 {
		t.Fatalf("only %d sagas started", len(ids))
	}
	for _, id := range ids {
		final := waitTerminal(t, client, id, 60*time.Second)
		if final.Status != StatusCompleted && final.Status != StatusCompensated {
			t.Fatalf("saga %s ended %v (%s)", id, final.Status, final.LastError)
		}
	}
	participant.mu.Lock()
	for op, n := range participant.executed {
		if n != 1 {
			t.Errorf("operation %s executed %d times", op, n)
		}
	}
	participant.mu.Unlock()
	// 至少有 saga 在 kill 时在途：它的命令由两个不同的写者（被杀的子进程与重启后的子进程）派发。日志已按快照清理过，
	// 所以按参与方看到的命令事件统计，不读日志。
	for k := range layout.Partitions {
		assertLinearChain(t, readPartitionEvents(t, conn.log, layout, k))
	}
	crossed := 0
	participant.mu.Lock()
	for _, w := range participant.writers {
		if len(w) > 1 {
			crossed++
		}
	}
	participant.mu.Unlock()
	if crossed == 0 {
		t.Fatal("no saga was in flight across the kill -9; the replay path was not exercised")
	}
	t.Logf("%d sagas across a coordinator kill -9, %d of them in flight at the kill", len(ids), crossed)
}

// 参与方停掉一段时间（每笔都要按步骤超时重试好几次），回来后全部 saga 都要到终态：协调器的重试定时器一次都不能丢。
// 2026-10-28 第 3 批参与方 kill -9 用例里发现：时间轮按墙钟（UnixNano）落槽、到期，分区判定“是否已到期”却用带单调读数的
// time.Time 比较；步骤超时是 tick 的整数倍，判定那一轮的时刻正好落在截止附近，墙钟比单调钟快的那几微秒里条目被当作“没到期的
// 陈旧条目”丢掉，记录停在 waiting、再也不重试（修前 150 笔、1s 超时、停 10s：三次各卡住 8 / 10 / 17 笔，另一组三次 1 次红；
// 现在的参数让到期判定多三倍多）。
func TestRealJetStreamCoordinatorRetriesThroughAParticipantOutage(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 1, 1)
	conn := dialRealJS(t, url)
	owner := startRealEngine(t, conn, nil, realEngineOptions(layout, []int{0}))
	if err := owner.Register(Definition{Type: "outage", Version: 1, Steps: []Step{
		{Name: "only", ForwardTopic: "outage.only", Timeout: 500 * time.Millisecond, MaxAttempts: 80, BackoffMin: 20 * time.Millisecond, BackoffMax: 50 * time.Millisecond},
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const sagas = 300
	for i := range sagas {
		if _, err := owner.StartSaga(ctx, StartRequest{Type: "outage", BusinessKey: "o" + strconv.Itoa(i), DefinitionVersion: 1}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Second) // 参与方不在：每笔按 500ms 超时重试约 18 次，一共约 5000 次到期判定
	startParticipant(t, conn.log, owner, layout, func(Command) Completion { return Completion{Success: true} })
	var stuck []Record
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		stuck = stuck[:0]
		for i := range sagas {
			record, err := owner.Get(ctx, IDOf("outage", "o"+strconv.Itoa(i)))
			if err != nil {
				t.Fatal(err)
			}
			if record.Status != StatusCompleted {
				stuck = append(stuck, record)
			}
		}
		if len(stuck) == 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(stuck) > 0 {
		r := stuck[0]
		t.Fatalf("%d of %d sagas did not complete after the participant came back, e.g. %s %s attempt %d next run %s (%s ago); stats %+v",
			len(stuck), sagas, r.BusinessKey, r.Status, r.Attempt, r.NextRunAt.Format("15:04:05.000"), time.Since(r.NextRunAt).Round(time.Millisecond), owner.Stats())
	}
}

// commandWatch 跟随布局全部流上的命令事件，记下每条命令（用例自己决定答不答）。
type commandWatch struct {
	mu       sync.Mutex
	commands []Command
	follows  []fnats.LogFollow
}

func watchCommands(t *testing.T, log fnats.JetStreamLog, layout Layout) *commandWatch {
	t.Helper()
	w := &commandWatch{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, stream := range layout.Streams() {
		follow, err := log.Follow(ctx, stream, []string{layout.Prefix + ".*.ev.cmd.>"}, 0, func(msg fnats.LogMsg) {
			rest := strings.TrimPrefix(msg.Subject, layout.Prefix+".")
			k, err := strconv.Atoi(rest[:strings.IndexByte(rest, '.')])
			if err != nil {
				return
			}
			if ev, err := decodeEvent(msg.Data, layout, k); err == nil && ev.Command != nil {
				w.mu.Lock()
				w.commands = append(w.commands, *ev.Command)
				w.mu.Unlock()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		w.follows = append(w.follows, follow)
	}
	t.Cleanup(func() {
		for _, f := range w.follows {
			_ = f.Stop(context.Background())
		}
	})
	return w
}

// await 等第一条满足 match 的命令。
func (w *commandWatch) await(t *testing.T, match func(Command) bool) Command {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		for _, c := range w.commands {
			if match(c) {
				w.mu.Unlock()
				return c
			}
		}
		w.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("command did not arrive")
	return Command{}
}

// 放弃之后才到的正向成功（saga 方向 ④）在真实日志上：Failed 被重开、只补偿那一步，补偿命令带那份成功的 Data；同一成功再送达两次
// 只告警一次、只重开一次；补偿完回到 Compensated。重放（新协调器从日志恢复）后状态一致。
// 历史：TestRealMongoLateSuccessReopensACompensatedSagaOnce / TestRealMongoLateSuccessAlarmIsMarkedOnce（Mongo 冷副本上的协调器）。
func TestRealJetStreamCoordinatorCompensatesALateSuccessOnce(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 1, 1)
	conn := dialRealJS(t, url)
	options := realEngineOptions(layout, allPartitions(1))
	engine, err := NewEngine(conn.log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	quick := Definition{Type: "late", Version: 1, Steps: []Step{
		{Name: "debit", ForwardTopic: "late.debit", CompensateTopic: "late.debit.cancel", Timeout: 300 * time.Millisecond, MaxAttempts: 1, BackoffMin: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond},
		{Name: "credit", ForwardTopic: "late.credit", CompensateTopic: "late.credit.cancel", Timeout: time.Second, MaxAttempts: 1, BackoffMin: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond},
	}}
	if err := engine.Register(quick); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Stop(context.Background()) })
	commands := watchCommands(t, conn.log, layout)
	started, err := engine.StartSaga(ctx, StartRequest{Type: "late", BusinessKey: "late-1", DefinitionVersion: 1, Data: []byte(`{"gift":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	debit := commands.await(t, func(c Command) bool { return c.SagaID == started.ID && c.Phase == PhaseForward && c.Step == 0 })
	failed := waitTerminal(t, engine, started.ID, 10*time.Second) // 唯一一次尝试超时 → Failed
	if failed.Status != StatusFailed {
		t.Fatalf("want Failed after the only attempt timed out: %+v", failed)
	}
	late := Completion{CommandID: debit.ID, IdempotencyKey: debit.IdempotencyKey, SagaID: started.ID, Success: true, Data: debit.Payload}
	for range 3 { // 同一成功送达三次
		if err := engine.Complete(ctx, late); err != nil {
			t.Fatal(err)
		}
	}
	compensation := commands.await(t, func(c Command) bool { return c.SagaID == started.ID && c.Phase == PhaseCompensate && c.Step == 0 })
	if string(compensation.Payload) != string(debit.Payload) {
		t.Fatalf("compensation payload %q, want the late success's data %q", compensation.Payload, debit.Payload)
	}
	if err := engine.Complete(ctx, Completion{CommandID: compensation.ID, IdempotencyKey: compensation.IdempotencyKey, SagaID: started.ID, Success: true}); err != nil {
		t.Fatal(err)
	}
	final := waitTerminal(t, engine, started.ID, 10*time.Second)
	if final.Status != StatusCompensated || final.LateStep != 0 || final.Version <= failed.Version {
		t.Fatalf("after compensating the late step: %+v", final)
	}
	// 三次送达同一个结果：Complete 按 MsgID res.<CommandID> 发布，流在去重窗口内只留一条，分区只见到一次；窗口外的重复由分区按
	// 回执摘要判重复（单元 TestPartitionResultDuplicateAndIdentityConflict）。这里钉的是“只告警一次、只重开一次”。
	stats := engine.Stats()
	if stats.LateAfterAbandon != 1 || stats.Reopened != 1 {
		t.Fatalf("three deliveries of one late success: LateAfterAbandon=%d Reopened=%d, want 1, 1", stats.LateAfterAbandon, stats.Reopened)
	}
	// 新协调器从日志重放：同一份状态。
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	if err := engine.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	replayed := startRealEngineWith(t, dialRealJS(t, url), options, quick)
	got, err := replayed.Get(ctx, started.ID)
	if err != nil || got.Status != StatusCompensated || got.Version != final.Version {
		t.Fatalf("after replay: %+v %v, want %+v", got, err, final)
	}
}

// startRealEngineWith 与 startRealEngine 相同，登记给定的定义。
func startRealEngineWith(t *testing.T, conn realJSConn, options Options, definitions ...Definition) *Engine {
	t.Helper()
	e, err := NewEngine(conn.log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range definitions {
		if err := e.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	})
	return e
}

// 装配的生命周期在真实 JetStream 上：Start 先恢复协调器、再订阅启动中继；Nest 启动 effect 经中继变成 saga；Stop 先排空中继再停
// 协调器；中继订阅失败（效果流不存在）时 Start 返回错误并停掉已启动的协调器（可以再 Assemble 重来）。
func TestRealAssemblyStartsTheRelayAndStopsInOrder(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 2, 1)
	conn := dialRealJS(t, url)
	js, err := ndriver.NewJetStreamClient(conn.client)
	if err != nil {
		t.Fatal(err)
	}
	effects := layout.Stream + "_EFFECTS"
	prefix := strings.ToLower(layout.Stream) + ".effect"
	t.Cleanup(func() {
		nc, err := gonats.Connect(url)
		if err != nil {
			return
		}
		defer nc.Close()
		if gjs, err := gojs.New(nc); err == nil {
			_ = gjs.DeleteStream(context.Background(), effects)
		}
	})
	config := AssemblyConfig{Engine: realEngineOptions(layout, allPartitions(2)),
		Starts: NestStartConsumerConfig{Stream: effects, Durable: "starts", EffectPrefix: prefix, AckWait: 10 * time.Second, ProcessTimeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	missing, err := Assemble(conn.log, conn.client, js, config, giftDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Start(ctx); err == nil {
		t.Fatal("Start succeeded without the effect stream the relay subscribes to")
	}
	if missing.Running() || missing.Engine.ready.Load() {
		t.Fatal("a failed Start left the coordinator running")
	}

	if err := js.EnsureStream(ctx, fnats.JetStreamConfig{Name: effects, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile, Replicas: 1, Duplicates: time.Minute}); err != nil {
		t.Fatal(err)
	}
	asm, err := Assemble(conn.log, conn.client, js, config, giftDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if err := asm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !asm.Running() || asm.ConsumersClosed() {
		t.Fatal("a started assembly is not running with its relay")
	}
	effect, err := NewStartEffect(StartRequest{Type: "gift", DefinitionVersion: 1, BusinessKey: "relayed", Data: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
	if _, err := js.Publish(ctx, prefix+"."+StartEffectTopic, raw, fnats.JetStreamPublishOptions{MsgID: effect.ID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if record, err := asm.Engine.GetByBusinessKey(ctx, "gift", "relayed"); err == nil {
			if record.ID != IDOf("gift", "relayed") {
				t.Fatalf("relayed saga = %+v", record)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay did not turn the start effect into a saga")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := asm.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if asm.Running() || !asm.ConsumersClosed() || asm.Engine.ready.Load() {
		t.Fatal("Stop did not finish: relay or coordinator still up")
	}
}
