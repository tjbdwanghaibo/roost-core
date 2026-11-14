package remoteflow

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	syncdriver "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// KNOWN-LIMITS“多写者本地副本”：两个真实进程（sid 1000 / 1001，各自一套正式装配：Remote Manager、DataEngine Runtime、
// 文件 WAL、Nest）交替写同一个共享模式的 Remote 托管实体。契约（REMOTE_ENTITY“多写者：本地副本与权威”）：每个写者进程
// 内存里各有一份实例，别的进程的提交不会改它；写准入取得分布式锁与写许可后，本地实例落后于权威就仅内存卸载、从权威
// 重载，再执行 handler。所以每一笔写看到的都是前面全部写的结果，没有丢失更新，权威版本逐笔加一。
//
// 编排 = 本测试进程；写者 = 用同一个测试二进制起的两个子进程（ROOST_REMOTE_WRITER_SID），经 stdin / stdout 行协议收发：
// CREATE（Nest 之外新建并认领、进入共享）、WRITE <n>（经 Nest 把 balance 加 n、items 加 1，回复写后的本地值）、QUIT。
// 两个写者用同一个 Mongo 库、同一个 Redis 键前缀（共享模式的分布式锁）与同一个 NATS 主题前缀。
const (
	sharedWriterSIDEnv    = "ROOST_REMOTE_WRITER_SID"
	sharedWriterIDEnv     = "ROOST_REMOTE_WRITER_ID"
	sharedWriterPrefixEnv = "ROOST_REMOTE_WRITER_PREFIX"
	sharedWriterReply     = "WRITER "
)

var sharedWriterAdd = nest.NewHandlerName("remote_shared_writer_add")

func TestGeneratedRemoteSharedEntityTwoWriterProcesses(t *testing.T) {
	if sid := os.Getenv(sharedWriterSIDEnv); sid != "" {
		runSharedWriter(t, sid)
		return
	}
	ctx, mongo, _, database := rejectTestEnv(t) // 结束时删库：两个子进程都已退出
	id, err := entity.BuildEntityID(9850, EntityKindSyncedVault)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("%s.writers.%d", database, time.Now().UnixNano())
	a := startSharedWriter(t, ctx, 1000, id, prefix)
	b := startSharedWriter(t, ctx, 1001, id, prefix)
	a.call(t, "CREATE")

	sum, writes := int64(0), int64(0)
	for round, n := range []int64{1, 2, 4, 8, 16, 32, 64, 128} {
		writer := a
		if round%2 == 1 {
			writer = b
		}
		sum += n
		writes++
		got := writer.call(t, fmt.Sprintf("WRITE %d", n))
		// 写后的本地值就是前面全部写的累计：写者进程里那份实例在这笔写之前已按权威对齐（上一笔是另一个进程写的）。
		if want := fmt.Sprintf("%d %d", sum, writes); got != want {
			t.Fatalf("write %d (+%d) on sid %d left the local copy at balance/items %q, want %q: the writer did not see the other process's commits", writes, n, writer.sid, got, want)
		}
	}
	a.quit(t)
	b.quit(t)
	version, balance, items, found := storedSharedVault(t, ctx, mongo, database, id)
	if !found || version != uint64(writes) || balance != sum || items != writes {
		t.Fatalf("Mongo after %d alternating writes: version=%d balance=%d items=%d found=%v, want version %d balance %d items %d",
			writes, version, balance, items, found, writes, sum, writes)
	}
	t.Logf("two writer processes (sid 1000 / 1001) alternated %d writes on shared entity %d: authority version %d, balance %d, items %d", writes, id, version, balance, items)
}

// sharedWriterProcess 是编排端持有的一个写者子进程。
type sharedWriterProcess struct {
	sid     int32
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies chan string
	done    chan error
	exited  bool
	mu      sync.Mutex
	output  strings.Builder // 子进程除回复之外的输出，编排结束时整段打进用例日志
}

func startSharedWriter(t *testing.T, ctx context.Context, sid int32, id int64, prefix string) *sharedWriterProcess {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGeneratedRemoteSharedEntityTwoWriterProcesses$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", sharedWriterSIDEnv, sid), fmt.Sprintf("%s=%d", sharedWriterIDEnv, id), sharedWriterPrefixEnv+"="+prefix)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &sharedWriterProcess{sid: sid, cmd: cmd, stdin: stdin, replies: make(chan string, 16), done: make(chan error, 1)}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if reply, ok := strings.CutPrefix(line, sharedWriterReply); ok {
				p.replies <- reply
				continue
			}
			p.mu.Lock()
			p.output.WriteString(line + "\n")
			p.mu.Unlock()
		}
		p.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if !p.exited {
			_ = cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(10 * time.Second):
			}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		t.Logf("writer %d output:\n%s", sid, p.output.String())
	})
	return p
}

// call 发一条命令，等回复；回复以 OK 开头时返回其余部分，否则令用例失败。
func (p *sharedWriterProcess) call(t *testing.T, command string) string {
	t.Helper()
	if _, err := fmt.Fprintln(p.stdin, command); err != nil {
		t.Fatalf("writer %d %s: %v", p.sid, command, err)
	}
	select {
	case reply := <-p.replies:
		rest, ok := strings.CutPrefix(reply, "OK")
		if !ok {
			t.Fatalf("writer %d %s: %s", p.sid, command, reply)
		}
		return strings.TrimSpace(rest)
	case err := <-p.done:
		p.exited = true
		t.Fatalf("writer %d exited during %s: %v", p.sid, command, err)
	case <-time.After(60 * time.Second):
		t.Fatalf("writer %d did not answer %s within 60s", p.sid, command)
	}
	return ""
}

func (p *sharedWriterProcess) quit(t *testing.T) {
	t.Helper()
	p.call(t, "QUIT")
	_ = p.stdin.Close()
	select {
	case err := <-p.done:
		p.exited = true
		if err != nil {
			t.Fatalf("writer %d exited with %v", p.sid, err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("writer %d did not exit", p.sid)
	}
}

// runSharedWriter 是写者子进程：装配一套正式链路，按 stdin 的命令写，回复以 sharedWriterReply 开头的一行。
func runSharedWriter(t *testing.T, rawSID string) {
	sid64, err := strconv.ParseInt(rawSID, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	sid := int32(sid64)
	id, err := strconv.ParseInt(os.Getenv(sharedWriterIDEnv), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	prefix := os.Getenv(sharedWriterPrefixEnv)
	database := NewBalanceDao().DbName()
	if !strings.HasPrefix(database, "roost_remote_generated_") || database == "roost_remote_generated_placeholder" || prefix == "" {
		t.Fatal("isolated database and key prefix required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// 不用 rejectTestEnv：它在结束时删库，库由编排进程在两个写者都退出后删。
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mongo.Close(context.Background()) })
	redis, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = redis.Close() })
	RegisterEntity()
	w := newSharedWriter(t, ctx, mongo, redis, database, sid, prefix)
	reply := func(format string, args ...any) { fmt.Printf(sharedWriterReply+format+"\n", args...) }
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		command := strings.Fields(scanner.Text())
		switch {
		case len(command) == 1 && command[0] == "CREATE":
			// Nest 之外新建（生成 lifecycle 的 GetOrCreate 同此），认领并进入共享；第一笔声明它的批次写齐每个 DAO。
			if _, err := w.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindSyncedVault, Id: id}); err != nil {
				reply("ERR create: %v", err)
				continue
			}
			prepareRemoteOwnership(t, ctx, w.manager, []int64{id})
			reply("OK")
		case len(command) == 2 && command[0] == "WRITE":
			n, err := strconv.ParseInt(command[1], 10, 64)
			if err != nil {
				reply("ERR %v", err)
				continue
			}
			result, err := w.write(ctx, id, n)
			if err != nil {
				reply("ERR write: %v", err)
				continue
			}
			reply("OK %s", result)
		case len(command) == 1 && command[0] == "QUIT":
			if err := w.runtime.Flush(ctx); err != nil {
				reply("ERR flush: %v", err)
				continue
			}
			reply("OK")
			return
		default:
			reply("ERR unknown command %q", scanner.Text())
		}
	}
}

type sharedWriter struct {
	access    *entity.ManagerAccess
	manager   *remoteentity.Manager
	runtime   *engine.Runtime
	scheduler *nest.NestMgr
}

// write 经 Nest 写一笔；只重试重载窗口的可重试哨兵（写准入换实例时撞上卸载收尾，REMOTE_ENTITY“多写者”）。
func (w *sharedWriter) write(ctx context.Context, id, n int64) (string, error) {
	for attempt := 0; ; attempt++ {
		result, err := w.scheduler.Request(ctx, sharedWriterAdd, id, nest.Params{n})
		if err == nil {
			return result.(string), nil
		}
		if !errors.Is(err, entity.ErrRemoteEntityReloading) || attempt == 50 {
			return "", err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newSharedWriter 装配一个写者进程的正式链路（与 newRejectRig 相同的装配，sid 可选，没有故障注入与 Sync）。
func newSharedWriter(t *testing.T, ctx context.Context, mongo fmongo.IMongo, redis fredis.IRedis, database string, sid int32, prefix string) *sharedWriter {
	t.Helper()
	scoped := &scopedRedis{IRedis: redis, prefix: prefix + ":"}
	w := &sharedWriter{access: entity.NewManagerAccess(entity.NewEntityManager())}
	backend, err := remoteentity.NewBackend(w.access, remoteentity.NewMongoCommitter(mongo, database, sid, 0))
	if err != nil {
		t.Fatal(err)
	}
	databases := fixtureDatabases(t)
	if err := backend.BindDatabaseMap(databases); err != nil {
		t.Fatal(err)
	}
	cfg := remoteentity.DefaultConfig()
	cfg.LockTTL = 3 * time.Second
	assembly, err := remoteentity.Assemble(remoteentity.AssemblyDeps{Redis: scoped, Backend: backend}, cfg, sid, remoteentity.MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	w.manager = assembly.Manager
	natsClient, err := natsdriver.NewClient(fnats.DefaultConfig(remoteNatsURL()), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(natsClient.Close)
	if err := assembly.Start(ctx, syncdriver.NewNatsSyncBus(natsClient, sid, prefix)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.Stop(context.Background()); err != nil {
			t.Errorf("Remote stop: %v", err)
		}
	})
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: database, Databases: databases})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRemoteProjection(backend, assembly.Manager); err != nil {
		t.Fatal(err)
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, &effectRecorder{}, engine.OutboxWorkerOptions{Owner: fmt.Sprintf("remote-writer-%d", sid)})
	if err != nil {
		t.Fatal(err)
	}
	onFatal := func(err error) { t.Errorf("runtime fatal on sid %d: %v", sid, err) }
	w.runtime, err = engine.NewRuntime(store, wal, projector, outbox, w.access, assembly.Manager, onFatal, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.runtime.Shutdown(context.Background()); err != nil {
			t.Errorf("runtime shutdown: %v", err)
		}
	})
	options := append(w.runtime.NestOptions(), nest.NestOptionWithGetter(w.access), nest.NestOptionWithRemoteEntityManager(assembly.Manager),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{Workers: 4, QueueCap: 64}))
	w.scheduler = nest.NewEngine(options...)
	w.scheduler.MustRegisterHandlerWithMeta(sharedWriterAdd, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		vault := es[0].(*SyncedVault)
		vault.balance.SetValue(vault.balance.GetValue() + params[0].(int64))
		vault.items.SetValue(vault.items.GetValue() + 1)
		return fmt.Sprintf("%d %d", vault.balance.GetValue(), vault.items.GetValue()), nil
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict})
	if err := w.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.scheduler.Shutdown(context.Background()) })
	return w
}

// storedSharedVault 读 Mongo 里该实体两个 DAO 的 Remote 信封：版本（两个 DAO 必须一致）与 value。
func storedSharedVault(t *testing.T, ctx context.Context, mongo fmongo.IMongo, database string, id int64) (version uint64, balance, items int64, found bool) {
	t.Helper()
	values := make([]int64, 0, 2)
	for _, collection := range []string{"remote_balances", "remote_items"} {
		var doc struct {
			Version uint64 `bson:"_ver"`
			Data    []byte `bson:"data"`
		}
		err := mongo.Database(database).Collection(collection).FindOne(ctx, bson.M{"_id": id}, &doc)
		if errors.Is(err, fmongo.ErrNotFound) {
			return 0, 0, 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		var data struct {
			Value int64 `bson:"value"`
		}
		if err := bson.Unmarshal(doc.Data, &data); err != nil {
			t.Fatal(err)
		}
		if len(values) > 0 && doc.Version != version {
			t.Fatalf("Mongo DAO versions diverge for %d: %d vs %d", id, version, doc.Version)
		}
		version = doc.Version
		values = append(values, data.Value)
	}
	return version, values[0], values[1], true
}
