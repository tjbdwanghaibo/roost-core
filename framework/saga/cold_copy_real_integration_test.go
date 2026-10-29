//go:build integration

package saga

// 冷副本（设计 §8，第 6 批）在真实依赖上的用例：
//
//   - TestRealMongoColdCopyVersionGate：真实 Mongo 上的管道 upsert 版本门（同一生版本只升；剪掉之后同业务键重开的一生盖住旧一生；
//     重投、乱序、旧一生迟到都不回退），以及记录的全部字段往返、List 的过滤 / 顺序 / 分页。只要 Mongo。
//   - TestRealJetStreamColdCopyKill9Converges：协调器（带冷副本投影，选项 C-A）在子进程里，发起方与参与方在本进程；跑一批
//     saga 时 kill -9 协调器、同布局重启，全部到终态后冷副本逐笔与权威终态（分区内存里的记录）一致：被杀进程取走没确认的终态事件
//     在 AckWait 后重投给重启的进程。
//   - TestRealJetStreamColdCopyKeepsUpWithoutBlockingThePurge：正式装配（Assemble + 冷副本）下快照频繁、冷副本跟得上时分区照常
//     清理；冷副本 durable 被删掉（模拟 R1 内存消费者随服务端重启消失）时清理停住、投影器重建 durable 后清理恢复、冷副本补齐。
//
// 环境：ROOST_NATS_CLUSTER_URL（scripts/nats-cluster.sh 起的私有集群）与 ROOST_DATAENGINE_IT_MONGO_URI（私有 mongod 副本集或
// 共享隔离环境；库名 roost_sagajs6_<pid>_<ns>，用后删除；地址只经环境变量传给子进程，不打印）。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func coldMongo(t *testing.T) (fmongo.IMongo, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set")
	}
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_sagajs6_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
		_ = client.Close(context.Background())
	})
	return client, database
}

func TestRealMongoColdCopyVersionGate(t *testing.T) {
	client, database := coldMongo(t)
	cold, err := NewColdCopy(client, database, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := cold.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 123456789, time.UTC)
	full := coldRecord("full", 5, t0, StatusCompensating)
	full.Phase, full.Step, full.CompletedSteps, full.Attempt, full.Incarnation = PhaseCompensate, 1, 1, 0, 2
	full.LateStep, full.LateData, full.Data, full.LastError = 2, []byte("late"), []byte("data"), "credit refused"
	full.StartDigest, full.StartAttempt, full.DeadlineAt = "digest", "inst.7", t0.Add(time.Hour)
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
	write := func(records ...Record) {
		t.Helper()
		if err := cold.write(ctx, records); err != nil {
			t.Fatal(err)
		}
	}
	get := func(id string) Record {
		t.Helper()
		r, err := cold.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	write(full)
	got := get(full.ID)
	want := full
	want.CreatedAt, want.UpdatedAt, want.NextRunAt, want.DeadlineAt = t0.Truncate(time.Millisecond), full.UpdatedAt.Truncate(time.Millisecond), full.NextRunAt.Truncate(time.Millisecond), full.DeadlineAt.Truncate(time.Millisecond)
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// 同一生：版本只升。v9 之后再来 v6、重投 v9，都不变。
	a9, a6 := coldRecord("a", 9, t0, StatusCompleted), coldRecord("a", 6, t0, StatusFailed)
	write(a9)
	write(a9, a6) // 同一批里旧版本排在后面
	write(a6)
	if r := get(a9.ID); r.Version != 9 || r.Status != StatusCompleted {
		t.Fatalf("same life after an older version: v%d %s, want v9 completed", r.Version, r.Status)
	}
	// 剪掉之后同业务键重开的一生（更晚 created_at，版本从头来）盖住旧一生；旧一生迟到的事件不回退。
	later := t0.Add(2 * time.Hour)
	a2 := coldRecord("a", 2, later, StatusCompensated)
	write(a2)
	write(a9)
	if r := get(a9.ID); r.Version != 2 || !r.CreatedAt.Equal(later.Truncate(time.Millisecond)) || r.Status != StatusCompensated {
		t.Fatalf("new life: v%d created %s %s, want v2 of the later life", r.Version, r.CreatedAt, r.Status)
	}

	// List：过滤、（updated_at, _id）升序、分页。
	var batch []Record
	for i := range 5 {
		batch = append(batch, coldRecord(fmt.Sprintf("l%d", i), uint64(i+1), t0.Add(time.Duration(i)*time.Minute), StatusCompleted))
	}
	write(batch...)
	page, err := cold.List(ctx, Query{Statuses: []Status{StatusCompleted}, Limit: 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	var all []Record
	for _, r := range append(append([]Record(nil), batch...), a9) {
		if r.Status == StatusCompleted && r.ID != a9.ID {
			all = append(all, r)
		}
	}
	sortRecords(all)
	if len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
		t.Fatalf("List page 2 = %v, want %s %s", ids(page), all[2].ID, all[3].ID)
	}
	if none, err := cold.List(ctx, Query{Type: "other", Limit: 10}, 0); err != nil || len(none) != 0 {
		t.Fatalf("List(type other) = %v %v", none, err)
	}
	if _, err := cold.Get(ctx, IDOf("rally", "missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func ids(records []Record) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.ID
	}
	return out
}

// ---- kill -9 协调器后最终一致 ----

const coldChildEnv = "ROOST_SAGA_JS_COLD_CHILD"

// TestRealJetStreamColdCopyCoordinatorChild 是 kill -9 用例的子进程：拥有全部分区的协调器 + 冷副本投影（与 Assemble 同一组装：
// Options.ColdCopy + ColdCopyProjector），打印 READY 后一直跑到被杀。直接运行时跳过。
func TestRealJetStreamColdCopyCoordinatorChild(t *testing.T) {
	if os.Getenv(coldChildEnv) != "1" {
		t.Skip("child role of TestRealJetStreamColdCopyKill9Converges")
	}
	partitions, _ := strconv.Atoi(os.Getenv("ROOST_SAGA_JS_PARTITIONS"))
	groups, _ := strconv.Atoi(os.Getenv("ROOST_SAGA_JS_GROUPS"))
	layout := Layout{Prefix: os.Getenv("ROOST_SAGA_JS_PREFIX"), Stream: os.Getenv("ROOST_SAGA_JS_STREAM"), Partitions: partitions, Groups: groups}
	conn := dialRealJS(t, os.Getenv("ROOST_NATS_CLUSTER_URL"))
	js, err := ndriver.NewJetStreamClient(conn.client)
	if err != nil {
		t.Fatal(err)
	}
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	cold, err := NewColdCopy(client, os.Getenv("ROOST_SAGA_JS_DB"), "")
	if err != nil {
		t.Fatal(err)
	}
	options := realEngineOptions(layout, allPartitions(partitions))
	options.Writer = "cold-child-" + strconv.Itoa(os.Getpid())
	options.SnapshotEveryEvents = 16
	options.ColdCopy = cold
	e, err := NewEngine(conn.log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Register(giftDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	projector, err := NewColdCopyProjector(cold, js, conn.log, layout, ColdCopySettings{TerminalOnly: true, BatchSize: 16, MaxWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := projector.Start(ctx); err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	select {}
}

func startColdChild(t *testing.T, layout Layout, database string) *childProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRealJetStreamColdCopyCoordinatorChild$", "-test.v", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), coldChildEnv+"=1", "ROOST_SAGA_JS_DB="+database,
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
			t.Fatal("cold copy coordinator child exited before READY")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("cold copy coordinator child not ready")
	}
	return child
}

// awaitColdCopy 等冷副本里每笔都与 want 的版本、状态一致（最终一致），返回用时。
func awaitColdCopy(t *testing.T, cold *ColdCopy, want map[string]Record, budget time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var behind []string
		for id, final := range want {
			got, err := cold.Get(ctx, id)
			if err != nil || got.Version != final.Version || got.Status != final.Status || !got.CreatedAt.Equal(final.CreatedAt.Truncate(time.Millisecond)) {
				behind = append(behind, fmt.Sprintf("%s(want v%d %s, got v%d %s %v)", id, final.Version, final.Status, got.Version, got.Status, err))
			}
		}
		cancel()
		if len(behind) == 0 {
			return time.Since(start)
		}
		if time.Since(start) > budget {
			slices.Sort(behind)
			t.Fatalf("cold copy not consistent after %s: %d of %d behind, e.g. %v", budget, len(behind), len(want), behind[:min(len(behind), 5)])
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestRealJetStreamColdCopyKill9Converges(t *testing.T) {
	url := realJSURL(t)
	client, database := coldMongo(t)
	layout := realLayout(t, url, 4, 2)
	conn := dialRealJS(t, url)
	cold, err := NewColdCopy(client, database, "")
	if err != nil {
		t.Fatal(err)
	}
	// 发起方与参与方在本进程（不拥有分区，也不开冷副本回落：核对直接读冷副本与权威内存）；协调器 + 投影在子进程。
	clientEngine := startRealEngine(t, conn, nil, realEngineOptions(layout, nil))
	participant := startParticipant(t, conn.log, clientEngine, layout, giftDecide)
	participant.delay = 100 * time.Millisecond
	child := startColdChild(t, layout, database)

	var mu sync.Mutex
	var started []string
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
				bk := fmt.Sprintf("c%d-%d", w, i)
				if i%4 == 0 {
					bk = fmt.Sprintf("refuse%d-%d", w, i)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, err := clientEngine.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: bk, DefinitionVersion: 1})
				cancel()
				if err == nil || errors.Is(err, ErrStartUnconfirmed) {
					mu.Lock()
					started = append(started, IDOf("gift", bk))
					mu.Unlock()
				} else {
					t.Errorf("start %s: %v", bk, err)
				}
			}
		}()
	}
	time.Sleep(2 * time.Second)
	child.kill() // kill -9：投影器取走、没确认的终态事件在 AckWait 后重投给重启的进程
	time.Sleep(500 * time.Millisecond)
	startColdChild(t, layout, database)
	time.Sleep(1500 * time.Millisecond)
	close(stopStarting)
	wg.Wait()
	mu.Lock()
	sagaIDs := slices.Compact(slices.Sorted(slices.Values(started)))
	mu.Unlock()
	if len(sagaIDs) < 20 {
		t.Fatalf("only %d sagas started", len(sagaIDs))
	}
	finals := map[string]Record{}
	var compensated int
	for _, id := range sagaIDs {
		final := waitTerminal(t, clientEngine, id, 90*time.Second)
		if final.Status != StatusCompleted && final.Status != StatusCompensated {
			t.Fatalf("saga %s ended %v (%s)", id, final.Status, final.LastError)
		}
		if final.Status == StatusCompensated {
			compensated++
		}
		finals[id] = final
	}
	// 冷副本的“权威对照”是分区内存里的终态（Get 走所有者内存；这个发起方没有开冷副本回落）。
	took := awaitColdCopy(t, cold, finals, 90*time.Second)
	t.Logf("%d sagas (%d compensated) across a coordinator kill -9; the cold copy matched every terminal state %s after the last one", len(finals), compensated, took.Round(time.Millisecond))
}

// ---- 跟得上时不挡清理；durable 丢了清理停、重建后恢复 ----

func TestRealJetStreamColdCopyKeepsUpWithoutBlockingThePurge(t *testing.T) {
	url := realJSURL(t)
	client, database := coldMongo(t)
	layout := realLayout(t, url, 2, 1)
	conn := dialRealJS(t, url)
	js, err := ndriver.NewJetStreamClient(conn.client)
	if err != nil {
		t.Fatal(err)
	}
	effects := layout.Stream + "_EFFECTS"
	prefix := strings.ToLower(layout.Stream) + ".effect"
	nc, err := gonats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	admin, err := gojs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.DeleteStream(context.Background(), effects) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := js.EnsureStream(ctx, fnats.JetStreamConfig{Name: effects, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile, Replicas: 1, Duplicates: time.Minute}); err != nil {
		t.Fatal(err)
	}
	options := realEngineOptions(layout, allPartitions(2))
	options.SnapshotEveryEvents = 8
	options.SnapshotInterval = time.Second
	asm, err := Assemble(conn.log, conn.client, js, AssemblyConfig{Engine: options,
		Starts:   NestStartConsumerConfig{Stream: effects, Durable: "starts", EffectPrefix: prefix, AckWait: 10 * time.Second, ProcessTimeout: 5 * time.Second},
		ColdCopy: &ColdCopySettings{Client: client, Database: database, TerminalOnly: true}}, giftDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if err := asm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		if err := asm.Stop(stopCtx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	engine := asm.Engine
	cold := engine.opts.ColdCopy
	startParticipant(t, conn.log, engine, layout, giftDecide)
	run := func(prefix string, n int) map[string]Record {
		t.Helper()
		finals := map[string]Record{}
		for i := range n {
			bk := fmt.Sprintf("%s%d", prefix, i)
			if _, err := engine.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: bk, DefinitionVersion: 1}); err != nil {
				t.Fatal(err)
			}
			finals[IDOf("gift", bk)] = Record{}
		}
		for id := range finals {
			finals[id] = waitTerminal(t, engine, id, 30*time.Second)
		}
		return finals
	}
	firstSeq := func() uint64 {
		t.Helper()
		info, err := admin.Stream(ctx, layout.StreamOf(0))
		if err != nil {
			t.Fatal(err)
		}
		state, err := info.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return state.State.FirstSeq
	}
	// 冷副本跟得上：清理照常推进。
	awaitColdCopy(t, cold, run("keep", 40), 30*time.Second)
	awaitPurges := func(after uint64, budget time.Duration) uint64 {
		t.Helper()
		deadline := time.Now().Add(budget)
		for engine.Stats().Purges <= after {
			if time.Now().After(deadline) {
				t.Fatalf("no purge after %d within %s (first seq %d)", after, budget, firstSeq())
			}
			time.Sleep(100 * time.Millisecond)
		}
		return engine.Stats().Purges
	}
	purges := awaitPurges(0, 15*time.Second)
	if seq := firstSeq(); seq <= 1 {
		t.Fatalf("the stream still starts at %d with the cold copy keeping up", seq)
	}
	// 冷副本 durable 没了（R1 内存消费者随服务端重启消失的同一后果）：清理停住，直到投影器发现并重建它。
	durable := ColdCopyDurable(layout.StreamOf(0))
	if err := admin.DeleteConsumer(ctx, layout.StreamOf(0), durable); err != nil {
		t.Fatal(err)
	}
	before := firstSeq()
	gone := run("gone", 10)
	time.Sleep(1500 * time.Millisecond) // 至少一轮快照 + 清理
	floors, err := conn.log.DurableAckFloors(ctx, layout.StreamOf(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, back := floors[durable]; !back {
		if seq := firstSeq(); seq != before {
			t.Fatalf("purged from %d to %d while the cold copy durable was missing", before, seq)
		}
	}
	// 投影器在 coldCopyCheckEvery 内重建 durable，重投流里还在的事件，冷副本补齐，清理恢复。
	awaitColdCopy(t, cold, gone, 30*time.Second)
	run("back", 10)
	awaitPurges(engine.Stats().Purges, 15*time.Second)
	if engine.Stats().Purges <= purges {
		t.Fatal("purge did not resume after the cold copy durable was recreated")
	}
	if _, err := admin.Consumer(ctx, layout.StreamOf(0), durable); err != nil {
		t.Fatalf("cold copy durable was not recreated: %v", err)
	}
	info, err := admin.Consumer(ctx, layout.StreamOf(0), durable)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := info.CachedInfo().Config; cfg.Replicas != 1 || !cfg.MemoryStorage {
		t.Fatalf("cold copy durable state: replicas %d memory %v, want R1 memory", cfg.Replicas, cfg.MemoryStorage)
	}
	// 回落读：剪掉之前也能经 Engine 读到（内存优先），冷副本里的同一条与内存一致。
	for id, final := range gone {
		got, err := engine.Get(ctx, id)
		if err != nil || got.Version != final.Version {
			t.Fatalf("Get(%s) = v%d %v, want v%d", id, got.Version, err, final.Version)
		}
	}
	var count int64
	if count, err = client.Database(database).Collection(DefaultColdCopyCollection).CountDocuments(ctx, bson.M{}); err != nil || count != 60 {
		t.Fatalf("cold copy holds %d documents (%v), want 60", count, err)
	}
}
