//go:build integration && (darwin || linux)

package remoteentity

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	syncdriver "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// outbox 发布归属的真实进程演练（REMOTE_ENTITY“outbox 发布归属”、KNOWN-LIMITS ②③）。
//
// 两个子进程（本测试二进制重入）各自是一个完整的 Remote 写者：正式 Assembly（真实 Mongo 提交存储、真实 Redis L2、
// 真实 JetStream 总线）、真实 WAL + DataEngine 投影器、延迟收尾 finalizer。写者 W（sid 1000）连 NATS 经私有
// toxiproxy 的代理，父进程在它的下行链路上加延迟，让 W 的发布（JetStream 发布确认）变慢；接管方 T（sid 2000）直连
// NATS，持续写自己的实体（每次写都会唤醒它的 outbox 扫描，和线上一样），并对 W 的实体续约快照兴趣（W 的快照因此
// 真的要经 NATS 发出）。时钟偏差经发布归属的时钟观察缝 outboxNow 注入 T（只偏移“宽限期是否已过”的判定，
// 进程的其他时钟不变；对宽限期判定而言与写者时钟反向偏移等价）。
//
// 时长按比例缩小：缺省 finalize_projection_timeout = outbox_takeover_grace = 3 s（线上缺省 30 s 的 1/10），
// strict 等待 1 s（线上 Nest 请求期限 3 s；取 1 s 让“一次 strict 等待的剩余”与收尾期限可区分），
// 收尾重试间隔取正式缺省 500 ms。
//
// 只在 ROOST_OUTBOX_DRILL=1 时运行（约 2 分钟）：scripts/mirror-local.sh 的私有环境或隔离环境，需要 toxiproxy：
//
//	ROOST_OUTBOX_DRILL=1 go test -tags integration -run '^TestRealOutboxTakeover' -v ./framework/remoteentity
//
// ROOST_OUTBOX_DRILL_OUT=<目录> 时把每个场景的事件与指标写成 JSON 证据。

const outboxDrillKind entity.EntityKind = 137

// outboxDrillChildConfig 经环境变量交给子进程。
type outboxDrillChildConfig struct {
	SID           int32
	Database      string
	NATSURL       string
	ViaProxy      bool
	BusPrefix     string
	L2Prefix      string
	WALDir        string
	GraceMS       int64
	ProjectionMS  int64
	RetryMS       int64
	ClockOffsetMS int64
	Entities      []int64
	LoopEntity    int64
	LoopPeriodMS  int64
	InterestKeys  []int64
}

// outboxDrillEvent 是子进程报告的一个事件（stdout 一行 "DRILL <json>"）。时刻都是同一台机器的真实时间（UnixNano）。
type outboxDrillEvent struct {
	Ev          string           `json:"ev"`
	SID         int32            `json:"sid"`
	Tag         string           `json:"tag,omitempty"`
	TX          string           `json:"tx,omitempty"`
	Err         string           `json:"err,omitempty"`
	GateTimeout bool             `json:"gate_timeout,omitempty"`
	Start       int64            `json:"start,omitempty"`
	At          int64            `json:"at,omitempty"`
	Already     bool             `json:"already,omitempty"`
	OK          bool             `json:"ok,omitempty"`
	Writes      int64            `json:"writes,omitempty"`
	Errors      int64            `json:"errors,omitempty"`
	Counters    map[string]int64 `json:"counters,omitempty"`
}

// drillOutboxStorage 记下本进程每次标记已发布的时刻，以及这次标记是否真的把记录由 Applied 改成了 Committed。
// Already 取条件更新的结果，而不是标记前读一次状态：两个进程几乎同时标记时，先读后标记会让两边都读到 Applied、
// 都被算成第一个发布者（2026-11-06 发版前全量验证在“接管方时钟快 2.7 s”一格出现过“x0 moved … 2 times”）。
type drillOutboxStorage struct {
	*MongoCommitter
	emit func(outboxDrillEvent)
}

func (s *drillOutboxStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	transitioned, err := s.MongoCommitter.markRemoteCommitPublished(ctx, id)
	event := outboxDrillEvent{Ev: "mark", TX: id.String(), At: time.Now().UnixNano(), Already: err == nil && !transitioned}
	if err != nil {
		event.Err = err.Error()
	}
	s.emit(event)
	return err
}

func TestRemoteOutboxDrillProcessHelper(t *testing.T) {
	raw := os.Getenv("ROOST_OUTBOX_DRILL_CHILD")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var cfg outboxDrillChildConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.Database, "roost_outboxdrill_") {
		t.Fatal("isolated database required")
	}
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: outboxDrillKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	var outMu sync.Mutex
	emit := func(event outboxDrillEvent) {
		event.SID = cfg.SID
		data, _ := json.Marshal(event)
		outMu.Lock()
		fmt.Fprintf(os.Stdout, "DRILL %s\n", data)
		outMu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	storage := &drillOutboxStorage{MongoCommitter: newBoundMongoCommitter(mongo, cfg.Database, cfg.SID, 0), emit: emit}
	loader := newRemoteTestLoader()
	entities := make(map[int64]*testRemoteEntity)
	for _, short := range cfg.Entities {
		live := newTestRemoteEntity(short, 1, outboxDrillKind)
		entities[short] = live
		loader.add(live)
	}
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	remoteCfg := DefaultConfig()
	remoteCfg.SnapshotL2KeyPrefix = cfg.L2Prefix
	remoteCfg.FinalizeProjectionTimeout = time.Duration(cfg.ProjectionMS) * time.Millisecond
	remoteCfg.OutboxTakeoverGrace = time.Duration(cfg.GraceMS) * time.Millisecond
	remoteCfg.FinalizeRetryInterval = time.Duration(cfg.RetryMS) * time.Millisecond
	assembly, err := Assemble(AssemblyDeps{Redis: realRemoteRedis(t), Backend: backend}, remoteCfg, cfg.SID, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	mgr := assembly.Manager
	if cfg.ClockOffsetMS != 0 {
		offset := time.Duration(cfg.ClockOffsetMS) * time.Millisecond
		mgr.remote.outboxNow = func() time.Time { return time.Now().Add(offset) }
	}
	mgr.remote.finalizeTrace = func(id entity.RemoteTransactionID, step string) {
		if step == "released" {
			emit(outboxDrillEvent{Ev: "released", TX: id.String(), At: time.Now().UnixNano()})
		}
	}
	client, err := natsdriver.NewClient(fnats.DefaultConfig(cfg.NATSURL), natsdriver.ClientOptions{IgnoreDiscoveredServers: cfg.ViaProxy})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	js, err := natsdriver.NewJetStreamClient(client)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := syncdriver.NewJetStreamSyncBus(ctx, js, syncdriver.JetStreamSyncConfig{
		LocalSid: cfg.SID, Prefix: cfg.BusPrefix, Storage: fnats.JetStreamStorageMemory, StreamMaxAge: 10 * time.Minute, AckWait: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.Start(ctx, bus); err != nil {
		t.Fatal(err)
	}
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{Databases: storage.databases.Load(), DefaultDatabase: cfg.Database, ServerID: cfg.SID, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(storage, mgr); err != nil {
		t.Fatal(err)
	}
	walOptions := nestwal.DefaultOptions(cfg.WALDir)
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 5 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// strictWrite 走 Nest strict 写的 Remote 部分：写准入（写门在 deadline 内排队）、WAL 追加、投影器投影为 Applied、
	// 在 deadline 内等发布确认，然后 Close（超时则转延迟收尾，写门留给 finalizer 释放）。
	var writeMu sync.Mutex // 只保护同一实体的 dirty 标志与提交内容，不串行不同实体
	strictWrite := func(short int64, deadline time.Duration, tag string) outboxDrillEvent {
		event := outboxDrillEvent{Ev: "commit", Tag: tag, Start: time.Now().UnixNano()}
		live := entities[short]
		writeCtx, cancelWrite := context.WithTimeout(ctx, deadline)
		defer cancelWrite()
		batch, err := mgr.PrepareRemoteWriteBatch(writeCtx, []int64{live.GUId()})
		if err != nil {
			event.Err, event.GateTimeout, event.At = err.Error(), errors.Is(err, context.DeadlineExceeded), time.Now().UnixNano()
			return event
		}
		var tx entity.RemoteTransactionID
		_, _ = crand.Read(tx[:])
		event.TX = tx.String()
		writeMu.Lock()
		live.dirty.set(true)
		err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "outbox-drill", "", true, uint8(nest.DurabilityStrict.Record())))
		writeMu.Unlock()
		if err == nil {
			err = projector.Commit(writeCtx, remoteCommitRecord(nest.DurabilityStrict.Record(), batch.Commits()))
		}
		if err != nil {
			_ = batch.Abort(context.Background(), err)
			_ = batch.Close(context.Background())
			event.Err, event.At = err.Error(), time.Now().UnixNano()
			return event
		}
		projector.TransactionReleased(coredata.TransactionID(tx))
		_, err = batch.Commit(writeCtx)
		event.At = time.Now().UnixNano()
		if err != nil {
			event.Err = err.Error()
		}
		_ = batch.Close(context.Background())
		return event
	}

	var loopWrites, loopErrors int64
	var loopMu sync.Mutex
	loopDone := make(chan struct{})
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	go func() {
		defer close(loopDone)
		if cfg.LoopEntity == 0 {
			return
		}
		ticker := time.NewTicker(time.Duration(cfg.LoopPeriodMS) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			}
			event := strictWrite(cfg.LoopEntity, time.Second, "loop")
			loopMu.Lock()
			loopWrites++
			if event.Err != "" {
				loopErrors++
			}
			loopMu.Unlock()
		}
	}()
	go func() {
		// 兴趣按 2 s 续约（兴趣租约远长于此），W 的快照因此一直要经 NATS 发布。
		for {
			for _, short := range cfg.InterestKeys {
				key := entity.RemoteSnapshotKey{EntityID: testRemoteFullIDWithKind(short, 1, outboxDrillKind), Kind: outboxDrillKind, Scope: 1}
				_ = mgr.RenewRemoteSnapshotInterest(ctx, key)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	emit(outboxDrillEvent{Ev: "ready", At: time.Now().UnixNano()})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "write": // write <short> <deadline ms> <tag>
			short, _ := strconv.ParseInt(fields[1], 10, 64)
			ms, _ := strconv.ParseInt(fields[2], 10, 64)
			go func(tag string) { emit(strictWrite(short, time.Duration(ms)*time.Millisecond, tag)) }(fields[3])
		case "interested": // interested <short>
			short, _ := strconv.ParseInt(fields[1], 10, 64)
			key := entity.RemoteSnapshotKey{EntityID: testRemoteFullIDWithKind(short, 1, outboxDrillKind), Kind: outboxDrillKind, Scope: 1}
			emit(outboxDrillEvent{Ev: "interested", Tag: fields[1], OK: mgr.snapshots.interests.interested(key)})
		case "metrics":
			stopLoop()
			<-loopDone
			counters := make(map[string]int64)
			for _, metric := range metrics.Snapshot() {
				if metric.Kind != metrics.KindCounter || !strings.HasPrefix(metric.Name, "remote_entity") {
					continue
				}
				name := metric.Name
				if len(metric.Labels) > 0 {
					keys := make([]string, 0, len(metric.Labels))
					for key := range metric.Labels {
						keys = append(keys, key+"="+metric.Labels[key])
					}
					slices.Sort(keys)
					name += "{" + strings.Join(keys, ",") + "}"
				}
				counters[name] += metric.Value
			}
			loopMu.Lock()
			emit(outboxDrillEvent{Ev: "metrics", Counters: counters, Writes: loopWrites, Errors: loopErrors})
			loopMu.Unlock()
		case "quit":
			stopLoop()
			<-loopDone
			stop, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
			_ = assembly.Stop(stop)
			_ = projector.Close(stop)
			cancelStop()
			emit(outboxDrillEvent{Ev: "bye"})
			return
		}
	}
}

// ---- 父进程 ----

type outboxDrillChild struct {
	t      *testing.T
	sid    int32
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan outboxDrillEvent
	stderr bytes.Buffer
	mu     sync.Mutex
	log    []outboxDrillEvent
	exited chan struct{}
}

func startOutboxDrillChild(t *testing.T, cfg outboxDrillChildConfig) *outboxDrillChild {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemoteOutboxDrillProcessHelper$", "-test.v", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), "ROOST_OUTBOX_DRILL_CHILD="+string(raw))
	child := &outboxDrillChild{t: t, sid: cfg.SID, cmd: cmd, events: make(chan outboxDrillEvent, 4096), exited: make(chan struct{})}
	cmd.Stderr = &child.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if child.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(child.exited)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			line, found := strings.CutPrefix(scanner.Text(), "DRILL ")
			if !found {
				continue
			}
			var event outboxDrillEvent
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			child.mu.Lock()
			child.log = append(child.log, event)
			child.mu.Unlock()
			child.events <- event
		}
		_ = cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-child.exited
		if t.Failed() {
			t.Logf("child %d stderr:\n%s", cfg.SID, child.stderr.String())
		}
	})
	child.await(t, "ready", "", 60*time.Second)
	return child
}

func (c *outboxDrillChild) send(t *testing.T, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(c.stdin, line); err != nil {
		t.Fatalf("child %d: %v", c.sid, err)
	}
}

// await 等第一个符合 ev / tag 的新事件（之前收到的不算），其余事件留在日志里。
func (c *outboxDrillChild) await(t *testing.T, ev, tag string, within time.Duration) outboxDrillEvent {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case event := <-c.events:
			if event.Ev == ev && (tag == "" || event.Tag == tag) {
				return event
			}
		case <-c.exited:
			t.Fatalf("child %d exited while waiting for %s %s; stderr:\n%s", c.sid, ev, tag, c.stderr.String())
		case <-timer.C:
			t.Fatalf("child %d: no %s %s within %v", c.sid, ev, tag, within)
		}
	}
}

// find 在已收到的事件里找（不消费通道）。
func (c *outboxDrillChild) find(match func(outboxDrillEvent) bool) []outboxDrillEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []outboxDrillEvent
	for _, event := range c.log {
		if match(event) {
			out = append(out, event)
		}
	}
	return out
}

// waitFor 轮询已收到的事件，直到有符合 match 的。
func (c *outboxDrillChild) waitFor(t *testing.T, what string, within time.Duration, match func(outboxDrillEvent) bool) outboxDrillEvent {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if found := c.find(match); len(found) > 0 {
			return found[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d: %s not seen within %v", c.sid, what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *outboxDrillChild) metrics(t *testing.T) outboxDrillEvent {
	t.Helper()
	c.send(t, "metrics")
	return c.await(t, "metrics", "", 30*time.Second)
}

// outboxDrillEnv 是一个场景共用的隔离资源：库、总线前缀、L2 前缀都按场景唯一，结束删除；W 的 NATS 代理也是场景自己的。
type outboxDrillEnv struct {
	database, busPrefix, l2Prefix string
	mongo                         fmongo.IMongo
	status                        *MongoCommitter
	proxyAPI, proxyName           string
	proxyURL                      string
	natsURL                       string
	walRoot                       string
}

func newOutboxDrillEnv(t *testing.T) *outboxDrillEnv {
	t.Helper()
	if os.Getenv("ROOST_OUTBOX_DRILL") != "1" {
		t.Skip("set ROOST_OUTBOX_DRILL=1 (about two minutes) in an isolated environment with toxiproxy")
	}
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with the isolated environment")
	}
	api, natsURL := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"), os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if api == "" || natsURL == "" || os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI") == "" || os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR") == "" {
		t.Fatal("the drill needs the isolated Mongo, NATS, Redis and toxiproxy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	jet := newStep4JetStream(t, ctx) // 只用它的唯一前缀与结束删流
	env := &outboxDrillEnv{
		database: fmt.Sprintf("roost_outboxdrill_%d", time.Now().UnixNano()), busPrefix: jet.prefix,
		l2Prefix: "outboxdrill" + strconv.FormatInt(time.Now().UnixNano(), 36), proxyAPI: api, natsURL: natsURL, walRoot: t.TempDir(),
	}
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	env.mongo = mongo
	env.status = newBoundMongoCommitter(mongo, env.database, 3000, 0)
	redis := realRemoteRedis(t)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mongo.Database(env.database).Drop(cleanup); err != nil {
			t.Errorf("drop %s: %v", env.database, err)
		}
		_ = mongo.Close(cleanup)
		for _, short := range []int64{outboxDrillE1, outboxDrillE2, outboxDrillE3} {
			key := entity.RemoteSnapshotKey{EntityID: testRemoteFullIDWithKind(short, 1, outboxDrillKind), Kind: outboxDrillKind, Scope: 1}
			_, _ = redis.Del(cleanup, env.l2Prefix+":"+remoteSnapshotL2Key(key))
		}
	})
	// W 自己的 NATS 代理：上游是第一个 NATS 节点，故障只加在这一条链路上。
	first := strings.Split(natsURL, ",")[0]
	parsed, err := url.Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	env.proxyName = "outboxdrill-w-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	created := proxyRequest(t, http.MethodPost, api+"/proxies", map[string]any{"name": env.proxyName, "listen": "127.0.0.1:0", "upstream": parsed.Host, "enabled": true})
	var proxy struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(created, &proxy); err != nil || proxy.Listen == "" || strings.HasSuffix(proxy.Listen, ":0") {
		t.Fatalf("proxy did not allocate a listen address: %s (%v)", created, err)
	}
	env.proxyURL = "nats://" + proxy.Listen
	t.Cleanup(func() { proxyRequest(t, http.MethodDelete, api+"/proxies/"+env.proxyName, nil) })
	return env
}

func (e *outboxDrillEnv) slowWriterNATS(t *testing.T, latency time.Duration) {
	t.Helper()
	proxyRequest(t, http.MethodPost, e.proxyAPI+"/proxies/"+e.proxyName+"/toxics", map[string]any{
		"name": "slow-ack", "type": "latency", "stream": "downstream", "toxicity": 1,
		"attributes": map[string]any{"latency": latency.Milliseconds(), "jitter": 0},
	})
}

func (e *outboxDrillEnv) healWriterNATS(t *testing.T) {
	t.Helper()
	proxyRequest(t, http.MethodDelete, e.proxyAPI+"/proxies/"+e.proxyName+"/toxics/slow-ack", nil)
}

func (e *outboxDrillEnv) createdAt(t *testing.T, tx string) (time.Time, entity.RemoteCommitState) {
	t.Helper()
	raw, err := hex.DecodeString(tx)
	var id entity.RemoteTransactionID
	if err != nil || len(raw) != len(id) {
		t.Fatalf("transaction id %q: %v", tx, err)
	}
	copy(id[:], raw)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := e.status.CommitStatus(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return status.CreatedAt, status.State
}

const (
	outboxDrillE1 int64 = 7601 // W 的实体：被代发、之后验证写门
	outboxDrillE2 int64 = 7602 // W 的实体：先写，让 W 的发布卡在它上面
	outboxDrillE3 int64 = 7701 // T 自己持续写的实体
)

type outboxDrillTiming struct {
	grace, projection, retry, strict time.Duration
}

func (e *outboxDrillEnv) start(t *testing.T, timing outboxDrillTiming, takerOffset time.Duration) (writer, taker *outboxDrillChild) {
	t.Helper()
	base := outboxDrillChildConfig{
		Database: e.database, BusPrefix: e.busPrefix, L2Prefix: e.l2Prefix,
		GraceMS: timing.grace.Milliseconds(), ProjectionMS: timing.projection.Milliseconds(), RetryMS: timing.retry.Milliseconds(),
	}
	takerCfg := base
	takerCfg.SID, takerCfg.NATSURL, takerCfg.WALDir = 2000, e.natsURL, filepath.Join(e.walRoot, "taker")
	takerCfg.ClockOffsetMS = takerOffset.Milliseconds()
	takerCfg.Entities, takerCfg.LoopEntity, takerCfg.LoopPeriodMS = []int64{outboxDrillE3}, outboxDrillE3, 100
	takerCfg.InterestKeys = []int64{outboxDrillE1, outboxDrillE2}
	writerCfg := base
	writerCfg.SID, writerCfg.NATSURL, writerCfg.ViaProxy, writerCfg.WALDir = 1000, e.proxyURL, true, filepath.Join(e.walRoot, "writer")
	writerCfg.Entities = []int64{outboxDrillE1, outboxDrillE2}
	writer = startOutboxDrillChild(t, writerCfg)
	taker = startOutboxDrillChild(t, takerCfg)
	// T 的兴趣经 NATS 到达 W 之后，W 的快照才真的要经（被注入故障的）NATS 发布。
	for _, short := range []int64{outboxDrillE1, outboxDrillE2} {
		deadline := time.Now().Add(15 * time.Second)
		for {
			writer.send(t, "interested "+strconv.FormatInt(short, 10))
			if writer.await(t, "interested", strconv.FormatInt(short, 10), 5*time.Second).OK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the taker's interest in %d never reached the writer", short)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// 预热：两个实体各写一次（冷加载、首写认领），正常路径下 W 自己发布、在期限内确认。
	for i, short := range []int64{outboxDrillE1, outboxDrillE2} {
		tag := "warm" + strconv.Itoa(i)
		writer.send(t, fmt.Sprintf("write %d %d %s", short, (3*time.Second).Milliseconds(), tag))
		if event := writer.await(t, "commit", tag, 10*time.Second); event.Err != "" {
			t.Fatalf("warm-up write on %d failed: %s", short, event.Err)
		}
	}
	return writer, taker
}

func outboxDrillMarks(children []*outboxDrillChild, tx string) (first *outboxDrillEvent, all []outboxDrillEvent) {
	for _, child := range children {
		all = append(all, child.find(func(event outboxDrillEvent) bool { return event.Ev == "mark" && event.TX == tx })...)
	}
	for i := range all {
		if !all[i].Already && all[i].Err == "" && (first == nil || all[i].At < first.At) {
			first = &all[i]
		}
	}
	return first, all
}

func outboxDrillEvidence(t *testing.T, name string, value any) {
	t.Helper()
	dir := os.Getenv("ROOST_OUTBOX_DRILL_OUT")
	if dir == "" {
		return
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + "ms" }

// 写者活着但发布卡住、超过宽限期仍未发布，被另一个进程代发（KNOWN-LIMITS ③）。
//
// W 先写 E2（x0），它的发布卡在被延迟的 NATS 确认上（W 的协调者是唯一的，一页没发完不读下一页）；随后写 E1（x1），
// x1 持久为 Applied 后 W 的协调者在宽限期内一直没读到它。T 过了宽限期代发两笔；W 的协调者回来时 x1 已不在 outbox
// 里——它的 tracker 没人通知，strict 写在期限内以“结果未知”答复，延迟收尾持写门到期限回源读到 Committed 才释放。
// 期间同实体的写在写门上超时；释放后同实体的写正常。
//
// grace_is_projection：宽限期 = 收尾期限（缺省关系），释放应在代发后“一次 strict 等待的剩余”之内；
// grace_beyond_projection：宽限期 = 2 × 收尾期限，收尾期限先到、读到 Applied 后按退避重试，释放应在代发后一次重试间隔之内。
func TestRealOutboxTakeoverOfALiveStalledWriter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		timing outboxDrillTiming
		stall  time.Duration
	}{
		{"grace_is_projection", outboxDrillTiming{grace: 3 * time.Second, projection: 3 * time.Second, retry: 500 * time.Millisecond, strict: time.Second}, 3500 * time.Millisecond},
		{"grace_beyond_projection", outboxDrillTiming{grace: 3 * time.Second, projection: 1500 * time.Millisecond, retry: 500 * time.Millisecond, strict: time.Second}, 3500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOutboxDrillEnv(t)
			writer, taker := env.start(t, tc.timing, 0)
			strict := tc.timing.strict
			env.slowWriterNATS(t, tc.stall)
			writer.send(t, fmt.Sprintf("write %d %d x0", outboxDrillE2, strict.Milliseconds()))
			time.Sleep(150 * time.Millisecond)
			writer.send(t, fmt.Sprintf("write %d %d x1", outboxDrillE1, strict.Milliseconds()))
			x1 := writer.await(t, "commit", "x1", strict+5*time.Second)
			// 写门仍被 x1 的延迟收尾持有：同实体的写在自己的期限内拿不到写门。
			writer.send(t, fmt.Sprintf("write %d %d probe", outboxDrillE1, strict.Milliseconds()))
			probe := writer.await(t, "commit", "probe", strict+5*time.Second)
			x0 := writer.waitFor(t, "x0 commit", 10*time.Second, func(e outboxDrillEvent) bool { return e.Ev == "commit" && e.Tag == "x0" })
			released := writer.waitFor(t, "x1 release", 30*time.Second, func(e outboxDrillEvent) bool { return e.Ev == "released" && e.TX == x1.TX })
			writer.waitFor(t, "x0 release", 30*time.Second, func(e outboxDrillEvent) bool { return e.Ev == "released" && e.TX == x0.TX })
			env.healWriterNATS(t)
			writer.send(t, fmt.Sprintf("write %d %d after", outboxDrillE1, strict.Milliseconds()))
			after := writer.await(t, "commit", "after", strict+5*time.Second)
			wm, tm := writer.metrics(t), taker.metrics(t)

			children := []*outboxDrillChild{writer, taker}
			report := map[string]any{"timing": map[string]string{"grace": ms(tc.timing.grace), "projection": ms(tc.timing.projection), "retry": ms(tc.timing.retry), "strict": ms(strict), "writer_nats_ack_latency": ms(tc.stall)}}
			for _, item := range []struct {
				name string
				ev   outboxDrillEvent
			}{{"x0", x0}, {"x1", x1}} {
				created, state := env.createdAt(t, item.ev.TX)
				first, marks := outboxDrillMarks(children, item.ev.TX)
				if state != entity.RemoteCommitCommitted || first == nil {
					t.Fatalf("%s: state=%v first mark=%v marks=%+v", item.name, state, first, marks)
				}
				if first.SID != taker.sid {
					t.Fatalf("%s was first published by %d, want a takeover by the taker (marks=%+v)", item.name, first.SID, marks)
				}
				if late := time.Unix(0, first.At).Sub(created); late < tc.timing.grace-50*time.Millisecond {
					t.Fatalf("%s taken over %v after it was written, inside the grace %v", item.name, late, tc.timing.grace)
				}
				firsts := 0
				for _, mark := range marks {
					if !mark.Already && mark.Err == "" {
						firsts++
					}
				}
				if firsts != 1 {
					t.Fatalf("%s moved from Applied to Committed %d times (marks=%+v)", item.name, firsts, marks)
				}
				report[item.name] = map[string]any{"created_to_takeover": ms(time.Unix(0, first.At).Sub(created)), "marks": marks, "commit_err": item.ev.Err,
					"commit_after": ms(time.Duration(item.ev.At - item.ev.Start))}
			}
			first, marks := outboxDrillMarks(children, x1.TX)
			// 慢发布转后台（outboxStragglerWait）之后，写者的协调者不再被 x0 的慢发布挡在页屏障上，可能也去发 x1；
			// 但它的发布确认同样被拖慢，只能在接管方代发之后才标记，读到的是“已是 Committed”（Already）——这正是
			// “写者被抢先后自己标记、tracker 照常结束”的路径。写者若抢在接管方之前标记了 x1，说明卡顿没有超过宽限期，场景没搭起来。
			for _, mark := range marks {
				if mark.SID == writer.sid && !mark.Already {
					t.Fatalf("the writer published x1 itself before the takeover (marks=%+v): the stall did not outlast the grace, scenario not set up", marks)
				}
			}
			if x1.Err == "" || !strings.Contains(x1.Err, entity.ErrRemotePersistenceIndeterminate.Error()) {
				t.Fatalf("x1 strict answer=%q, want the indeterminate answer at its deadline", x1.Err)
			}
			if answered := time.Duration(x1.At - x1.Start); answered > strict+500*time.Millisecond {
				t.Fatalf("x1 strict write answered after %v, deadline %v", answered, strict)
			}
			holdAfterTakeover := time.Duration(released.At - first.At)
			// 文档上界：宽限期 = 收尾期限时，代发后最多再等一次 strict 等待的剩余；宽限期更长时收尾期限先到，之后按退避
			// 重试回源，代发后最多再等一次重试间隔（上限 5 s）。
			bound := strict
			if tc.timing.grace > tc.timing.projection {
				bound = 5 * time.Second
			}
			if holdAfterTakeover > bound+500*time.Millisecond {
				t.Fatalf("x1 released %v after the takeover, want within %v (+500ms)", holdAfterTakeover, bound)
			}
			if !probe.GateTimeout {
				t.Fatalf("probe write on the held entity: %+v, want a write-gate timeout", probe)
			}
			if after.Err != "" {
				t.Fatalf("write after the release failed: %s", after.Err)
			}
			if got := tm.Counters["remote_entity.outbox.takeover_total{reason=grace}"]; got != 2 {
				t.Fatalf("taker takeover_total{grace}=%d, want 2 (counters %v)", got, tm.Counters)
			}
			if got := wm.Counters["remote_entity.outbox.takeover_total{reason=grace}"] + wm.Counters["remote_entity.outbox.takeover_total{reason=successor}"]; got != 0 {
				t.Fatalf("writer took over %d commits", got)
			}
			if tm.Errors != 0 {
				t.Fatalf("taker's own writes failed %d/%d times", tm.Errors, tm.Writes)
			}
			report["x1_created_to_release"] = ms(time.Duration(released.At) - time.Duration(mustCreated(t, env, x1.TX).UnixNano()))
			report["x1_release_after_takeover"] = ms(holdAfterTakeover)
			report["probe"], report["after"] = probe, after
			report["writer_counters"], report["taker_counters"] = wm.Counters, tm.Counters
			report["taker_loop"] = map[string]int64{"writes": tm.Writes, "errors": tm.Errors}
			outboxDrillEvidence(t, "live-stalled-writer-"+tc.name, report)
			t.Logf("x1: answered %v after write (%s); taken over %v after created; released %v after takeover (bound %v); probe gate timeout=%v; after write %v",
				time.Duration(x1.At-x1.Start), x1.Err, time.Unix(0, first.At).Sub(mustCreated(t, env, x1.TX)), holdAfterTakeover, bound, probe.GateTimeout,
				time.Duration(after.At-after.Start))
			t.Logf("writer counters: finalize_projection_timeout=%d write_gate_timeout=%d finalize_status_read=%d finalize_retry=%d; taker takeover{grace}=%d loop writes=%d errors=%d",
				wm.Counters["remote_entity.finalize_projection_timeout_total"], wm.Counters["remote_entity_write_gate_timeout_total"],
				wm.Counters["remote_entity.finalize_status_read_total"], wm.Counters["remote_entity.finalize_retry_total"],
				tm.Counters["remote_entity.outbox.takeover_total{reason=grace}"], tm.Writes, tm.Errors)
		})
	}
}

func mustCreated(t *testing.T, env *outboxDrillEnv, tx string) time.Time {
	created, _ := env.createdAt(t, tx)
	return created
}

// 写者崩溃后的接管时延随接管方时钟偏差变化（KNOWN-LIMITS ①②）：W 的 strict 写持久为 Applied、发布卡在被延迟的 NATS
// 确认上时 SIGKILL W；T 的时钟（发布归属判定用）偏移 offset。预期 T 在写入后 max(grace − offset, 0)（加一次扫描）代发，
// 恰好一次。
func TestRealOutboxTakeoverAfterWriterCrashUnderClockSkew(t *testing.T) {
	timing := outboxDrillTiming{grace: 3 * time.Second, projection: 3 * time.Second, retry: 500 * time.Millisecond, strict: time.Second}
	report := map[string]any{"timing": map[string]string{"grace": ms(timing.grace), "projection": ms(timing.projection), "strict": ms(timing.strict)}}
	for _, offset := range []time.Duration{-6 * time.Second, -3 * time.Second, -time.Second, 0, time.Second, 2500 * time.Millisecond, 3 * time.Second, 6 * time.Second} {
		t.Run("taker_clock_"+offset.String(), func(t *testing.T) {
			env := newOutboxDrillEnv(t)
			writer, taker := env.start(t, timing, offset)
			env.slowWriterNATS(t, 30*time.Second)
			writer.send(t, fmt.Sprintf("write %d %d crash", outboxDrillE1, (10*time.Second).Milliseconds()))
			var tx string
			deadline := time.Now().Add(10 * time.Second)
			for tx == "" {
				// W 的 strict 写还在等发布确认（不会有 commit 事件）；从 W 的标记之外找到这笔提交：读 outbox 里 W 写的 Applied 记录。
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				pending, err := env.status.PendingRemoteCommits(ctx, 10)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				for _, status := range pending {
					if status.WriterSID == writer.sid {
						tx = status.TransactionID.String()
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("the writer's commit never became Applied")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := writer.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			<-writer.exited
			killed := time.Now()
			created, _ := env.createdAt(t, tx)
			want := max(timing.grace-offset, 0)
			mark := taker.waitFor(t, "takeover", want+15*time.Second, func(e outboxDrillEvent) bool { return e.Ev == "mark" && e.TX == tx })
			tm := taker.metrics(t)
			first, marks := outboxDrillMarks([]*outboxDrillChild{writer, taker}, tx)
			if first == nil || first.SID != taker.sid || mark.Already {
				t.Fatalf("takeover marks=%+v", marks)
			}
			delay := time.Unix(0, first.At).Sub(created)
			if delay < want-50*time.Millisecond || delay > want+time.Second {
				t.Fatalf("taken over %v after the write, want about %v (grace %v, taker clock %+v)", delay, want, timing.grace, offset)
			}
			// 崩溃的那笔只被代发一次由上面的标记判断（first 是 T、不是 Already）。计数器还会计入 T 判为“要代发”的其他提交：
			// 实际宽限期 grace − offset ≤ 0 时 T 对 W 留下的任何 Applied 提交都立即代发，包括 W 正在自己发布的准备写
			// （T 标记时读到已是 Committed），这是文档写明的“代发方时钟快过宽限期即退化为立即代发”，不是重复发布。
			takeovers := tm.Counters["remote_entity.outbox.takeover_total{reason=grace}"]
			if takeovers < 1 || (timing.grace-offset > 0 && takeovers != 1) {
				t.Fatalf("takeover_total{grace}=%d, want 1 (at least 1 when the effective grace %v is not positive; taker marks %+v; crash commit %s)",
					takeovers, timing.grace-offset, taker.find(func(event outboxDrillEvent) bool { return event.Ev == "mark" }), tx)
			}
			report[offset.String()] = map[string]any{"created_to_takeover": ms(delay), "expected": ms(want), "kill_after_created": ms(killed.Sub(created)), "marks": marks, "taker_counters": tm.Counters}
			t.Logf("taker clock %+v: taken over %v after the write (expected %v), once", offset, delay, want)
		})
	}
	outboxDrillEvidence(t, "crash-takeover-clock-skew", report)
}

// 接管方时钟快（等价于写者时钟慢）对活着的写者的影响：W 的发布只慢 300 ms（远小于宽限期）。时钟一致时 W 自己发布，
// strict 写在期限内确认、没有代发；偏差达到宽限期时 T 在 W 的协调者读到 x1 之前就代发，x1 的 tracker 没人通知，退化为
// 修复前的“strict 超时、写门持有到收尾期限”。只影响时延与同实体可用性，不重复提交、不卡住。
func TestRealOutboxFastTakerClockAgainstALiveWriter(t *testing.T) {
	timing := outboxDrillTiming{grace: 3 * time.Second, projection: 3 * time.Second, retry: 500 * time.Millisecond, strict: time.Second}
	report := map[string]any{"timing": map[string]string{"grace": ms(timing.grace), "projection": ms(timing.projection), "strict": ms(timing.strict), "writer_nats_ack_latency": "300ms"}}
	for _, offset := range []time.Duration{0, time.Second, 2 * time.Second, 2700 * time.Millisecond, 3 * time.Second, 6 * time.Second} {
		t.Run("taker_clock_+"+offset.String(), func(t *testing.T) {
			env := newOutboxDrillEnv(t)
			writer, taker := env.start(t, timing, offset)
			env.slowWriterNATS(t, 300*time.Millisecond)
			writer.send(t, fmt.Sprintf("write %d %d x0", outboxDrillE2, timing.strict.Milliseconds()))
			time.Sleep(100 * time.Millisecond)
			writer.send(t, fmt.Sprintf("write %d %d x1", outboxDrillE1, timing.strict.Milliseconds()))
			x1 := writer.await(t, "commit", "x1", timing.strict+5*time.Second)
			if x1.Err != "" {
				writer.waitFor(t, "x1 release", 30*time.Second, func(e outboxDrillEvent) bool { return e.Ev == "released" && e.TX == x1.TX })
			}
			x0 := writer.waitFor(t, "x0 commit", 10*time.Second, func(e outboxDrillEvent) bool { return e.Ev == "commit" && e.Tag == "x0" })
			if x0.Err != "" {
				writer.waitFor(t, "x0 release", 30*time.Second, func(e outboxDrillEvent) bool { return e.Ev == "released" && e.TX == x0.TX })
			}
			env.healWriterNATS(t)
			writer.send(t, fmt.Sprintf("write %d %d after", outboxDrillE1, timing.strict.Milliseconds()))
			after := writer.await(t, "commit", "after", timing.strict+5*time.Second)
			wm, tm := writer.metrics(t), taker.metrics(t)
			children := []*outboxDrillChild{writer, taker}
			result := map[string]any{}
			for _, item := range []struct {
				name string
				ev   outboxDrillEvent
			}{{"x0", x0}, {"x1", x1}} {
				first, marks := outboxDrillMarks(children, item.ev.TX)
				if first == nil {
					t.Fatalf("%s never published: %+v", item.name, marks)
				}
				firsts := 0
				for _, mark := range marks {
					if !mark.Already && mark.Err == "" {
						firsts++
					}
				}
				if firsts != 1 {
					t.Fatalf("%s moved from Applied to Committed %d times", item.name, firsts)
				}
				entry := map[string]any{"first_publisher": first.SID, "commit_after": ms(time.Duration(item.ev.At - item.ev.Start)), "commit_err": item.ev.Err, "marks": marks}
				if released := writer.find(func(e outboxDrillEvent) bool { return e.Ev == "released" && e.TX == item.ev.TX }); len(released) > 0 {
					entry["write_gate_held"] = ms(time.Duration(released[0].At - item.ev.Start))
				}
				result[item.name] = entry
			}
			if offset == 0 {
				if x1.Err != "" || tm.Counters["remote_entity.outbox.takeover_total{reason=grace}"] != 0 {
					t.Fatalf("with synchronized clocks the writer must publish its own commits: x1=%q takeovers=%d", x1.Err, tm.Counters["remote_entity.outbox.takeover_total{reason=grace}"])
				}
			}
			if offset >= timing.grace {
				if first, _ := outboxDrillMarks(children, x1.TX); first.SID != taker.sid {
					t.Fatalf("taker clock +%v ≥ grace: x1 first published by %d, want an immediate takeover", offset, first.SID)
				}
			}
			if after.Err != "" {
				t.Fatalf("write after the episode failed: %s", after.Err)
			}
			result["after"] = ms(time.Duration(after.At - after.Start))
			result["writer_counters"], result["taker_counters"] = wm.Counters, tm.Counters
			report["+"+offset.String()] = result
			t.Logf("taker clock +%v: x1 %s after %v (first publisher %v); takeover{grace}=%d; writer finalize_projection_timeout=%d write_gate_timeout=%d",
				offset, map[bool]string{true: "confirmed", false: "indeterminate"}[x1.Err == ""], time.Duration(x1.At-x1.Start), result["x1"].(map[string]any)["first_publisher"],
				tm.Counters["remote_entity.outbox.takeover_total{reason=grace}"], wm.Counters["remote_entity.finalize_projection_timeout_total"], wm.Counters["remote_entity_write_gate_timeout_total"])
		})
	}
	outboxDrillEvidence(t, "fast-taker-clock-live-writer", report)
}
