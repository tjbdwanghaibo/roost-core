//go:build integration

package level

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// KNOWN-LIMITS“LevelComponent”：持久模式的关卡状态在真实 Mongo 副本集 + 文件 WAL 上经进程崩溃恢复。
//
// 写进程（子进程）：正式 DataEngine Runtime（Mongo 投影、文件 WAL、新建准入门）+ Nest，实体挂 LevelDAO 与 LevelComponent，
// 用 waves 定义 Initialize / Start（第一波：spawn_wave 待办 + 超时计时器）、击杀、回执，推进到第二波；然后把投影拦住，
// 再写一笔（第二波的一次击杀）——这笔只在 WAL 里。打印最后的状态后等编排进程 SIGKILL。
// 恢复进程（另一个子进程）：同一个 WAL 目录与库，Runtime 启动回放 WAL；冷加载出的组件状态必须与崩溃前最后一笔
// 提交后的状态完全一致（阶段 generation、变量、待办动作、计时器），之后能继续回执、推进到终局并关闭。
//
// 运行：source 隔离环境的 env.sh 后
//
//	GOWORK=off go test -race -tags=integration -run '^TestRealMongoLevelComponentRecoversFromACrashFromTheWAL$' -count=1 ./gameplay/level
const (
	levelCrashRoleEnv     = "ROOST_LEVEL_CRASH_ROLE"
	levelCrashDatabaseEnv = "ROOST_LEVEL_CRASH_DB"
	levelCrashWALEnv      = "ROOST_LEVEL_CRASH_WAL"
	levelCrashKind        = entity.EntityKind(237) // 本包的其他用例用 238
	levelCrashReply       = "LEVEL "
)

var (
	levelCrashCommand = nest.NewHandlerName("level.crash.command")
	levelCrashOnce    sync.Once
)

func TestRealMongoLevelComponentRecoversFromACrashFromTheWAL(t *testing.T) {
	switch os.Getenv(levelCrashRoleEnv) {
	case "write":
		runLevelCrashWriter(t)
		return
	case "recover":
		runLevelCrashRecovery(t)
		return
	}
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("source the isolated environment (ROOST_DATAENGINE_IT_MONGO_URI)")
	}
	registerLevelCrashKind(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database := fmt.Sprintf("roost_level_crash_%d_%d", os.Getpid(), time.Now().UnixNano())
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Database(database).Drop(context.Background())
		_ = client.Close(context.Background())
	})
	walDir := t.TempDir()
	env := []string{levelCrashDatabaseEnv + "=" + database, levelCrashWALEnv + "=" + walDir}

	writer := startLevelCrashChild(t, ctx, "write", env)
	written := writer.await(t, "STATE")
	writer.await(t, "HELD")
	stored := storedLevelVersion(t, ctx, client, database)
	if err := writer.cmd.Process.Kill(); err != nil { // SIGKILL：不经停机收尾
		t.Fatal(err)
	}
	writer.wait(t, false)
	if after := storedLevelVersion(t, ctx, client, database); after != stored {
		t.Fatalf("Mongo moved from version %d to %d after the writer was killed", stored, after)
	}

	recovery := startLevelCrashChild(t, ctx, "recover", env)
	recovered := recovery.await(t, "STATE")
	var before, after State
	if err := json.Unmarshal([]byte(written), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(recovered), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("state recovered after the crash differs from the last committed state:\nbefore %s\nafter  %s", written, recovered)
	}
	replayed := recovery.await(t, "REPLAYED")
	finished := recovery.await(t, "FINISHED")
	recovery.wait(t, true)
	final := storedLevelVersion(t, ctx, client, database)
	if want, _ := strconv.ParseUint(finished, 10, 64); final != want || final <= stored {
		t.Fatalf("stored version after recovery %d (held at the crash %d, replayed to %s), want the recovery's final %s", final, stored, replayed, finished)
	}
	t.Logf("Mongo held version %d at the SIGKILL; WAL replay brought it to %s; the recovered component finished the level and stored version %d", stored, replayed, final)
}

// ---------------- 编排 ----------------

type levelCrashChild struct {
	role    string
	cmd     *exec.Cmd
	replies chan [2]string
	done    chan error
	exited  bool
	mu      sync.Mutex
	output  strings.Builder
}

func startLevelCrashChild(t *testing.T, ctx context.Context, role string, env []string) *levelCrashChild {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRealMongoLevelComponentRecoversFromACrashFromTheWAL$", "-test.count=1", "-test.v")
	cmd.Env = append(append(os.Environ(), levelCrashRoleEnv+"="+role), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &levelCrashChild{role: role, cmd: cmd, replies: make(chan [2]string, 8), done: make(chan error, 1)}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if reply, ok := strings.CutPrefix(line, levelCrashReply); ok {
				key, value, _ := strings.Cut(reply, " ")
				c.replies <- [2]string{key, value}
				continue
			}
			c.mu.Lock()
			c.output.WriteString(line + "\n")
			c.mu.Unlock()
		}
		c.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if !c.exited {
			_ = cmd.Process.Kill()
			select {
			case <-c.done:
			case <-time.After(10 * time.Second):
			}
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		t.Logf("%s process output:\n%s", role, c.output.String())
	})
	return c
}

// await 等子进程报告 key；它报 FAIL 或先退出时令用例失败。
func (c *levelCrashChild) await(t *testing.T, key string) string {
	t.Helper()
	select {
	case reply := <-c.replies:
		if reply[0] != key {
			t.Fatalf("%s process reported %s %s, want %s", c.role, reply[0], reply[1], key)
		}
		return reply[1]
	case err := <-c.done:
		c.exited = true
		t.Fatalf("%s process exited before reporting %s: %v", c.role, key, err)
	case <-time.After(90 * time.Second):
		t.Fatalf("%s process did not report %s within 90s", c.role, key)
	}
	return ""
}

func (c *levelCrashChild) wait(t *testing.T, wantSuccess bool) {
	t.Helper()
	select {
	case err := <-c.done:
		c.exited = true
		if wantSuccess && err != nil {
			t.Fatalf("%s process failed: %v", c.role, err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("%s process did not exit", c.role)
	}
}

// storedLevelVersion 读 Mongo 里关卡 DAO 的 _version（没有文档为 0）。
func storedLevelVersion(t *testing.T, ctx context.Context, client fmongo.IMongo, database string) uint64 {
	t.Helper()
	var doc struct {
		Version uint64 `bson:"_version"`
	}
	err := client.Database(database).Collection(CollectionName).FindOne(ctx, bson.M{"_id": levelCrashEntityID(t)}, &doc)
	if err != nil && !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatal(err)
	}
	return doc.Version
}

func errorsIsNotFound(err error) bool {
	return err == fmongo.ErrNotFound || strings.Contains(err.Error(), "no documents")
}

// ---------------- 子进程 ----------------

type levelCrashEntity struct {
	*entity.EntityBase
	entity.DaoManager
	component *LevelComponent
}

func (e *levelCrashEntity) Base() *entity.EntityBase { return e.EntityBase }

func levelCrashEntityID(t *testing.T) int64 {
	t.Helper()
	id, err := entity.BuildEntityID(9901, levelCrashKind)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func registerLevelCrashKind(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("testdata/waves.json")
	must(t, err)
	def, err := DecodeDefinition(raw)
	must(t, err)
	program := compiled(t, def, exampleRegistry())
	levelCrashOnce.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: levelCrashKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return NewLevelDAO(0, "game") }},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				dao, ok := param.Dao[CollectionName].(*LevelDAO)
				if !ok {
					return nil, fmt.Errorf("level crash entity %d has no level DAO", param.Id)
				}
				component, err := NewLevelComponent(dao, program)
				if err != nil {
					return nil, err
				}
				e := &levelCrashEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind), DaoManager: entity.NewDaoManager(), component: component}
				e.DaoManager.Set(CollectionName, dao)
				return e, nil
			},
		})
	})
}

// holdableStore 是投影用的 MongoStore：held 置位之后投影阻塞到 ctx 结束，记录只留在 WAL 里。
type holdableStore struct {
	*engine.MongoStore
	held    atomic.Bool
	blocked chan struct{}
	once    sync.Once
}

func (s *holdableStore) hold(ctx context.Context) error {
	if !s.held.Load() {
		return nil
	}
	s.once.Do(func() { close(s.blocked) })
	<-ctx.Done()
	return ctx.Err()
}
func (s *holdableStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	if err := s.hold(ctx); err != nil {
		return err
	}
	return s.MongoStore.Project(ctx, record)
}
func (s *holdableStore) ProjectBatch(ctx context.Context, records []coredata.CommitRecord) error {
	if err := s.hold(ctx); err != nil {
		return err
	}
	return s.MongoStore.ProjectBatch(ctx, records)
}

type levelCrashProcess struct {
	ctx       context.Context
	store     *holdableStore
	runtime   *engine.Runtime
	access    *entity.ManagerAccess
	scheduler *nest.NestMgr
	id        int64
}

// openLevelCrashProcess 装配一个进程的正式链路：Mongo 投影（经 holdableStore）、文件 WAL、Runtime 与 Nest。
func openLevelCrashProcess(t *testing.T) *levelCrashProcess {
	t.Helper()
	registerLevelCrashKind(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	database := os.Getenv(levelCrashDatabaseEnv)
	if !strings.HasPrefix(database, "roost_level_crash_") {
		t.Fatal("isolated database required")
	}
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	// LevelDAO 的逻辑库是 "game"：映射到本次唯一的物理库，不碰共享的 game 库。
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{
		Declared: []coredata.LogicalDatabase{{Name: "game"}}, Configured: map[string]string{"game": database},
	})
	if err != nil {
		t.Fatal(err)
	}
	mongoStore, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database, Databases: databases})
	if err != nil {
		t.Fatal(err)
	}
	if err := mongoStore.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	p := &levelCrashProcess{ctx: ctx, store: &holdableStore{MongoStore: mongoStore, blocked: make(chan struct{})}, id: levelCrashEntityID(t)}
	wal, err := nestwal.Open(nestwal.DefaultOptions(os.Getenv(levelCrashWALEnv)))
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, p.store, engine.ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	outboxStore, err := engine.NewMongoOutboxStore(mongoStore)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, noLevelEffects{}, engine.OutboxWorkerOptions{Owner: "level-crash"})
	if err != nil {
		t.Fatal(err)
	}
	p.access = entity.NewManagerAccess(entity.NewEntityManager())
	p.runtime, err = engine.NewRuntime(mongoStore, wal, projector, outbox, p.access, nil, func(err error) { t.Errorf("runtime fatal: %v", err) }, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.runtime.Shutdown(context.Background()) })
	p.scheduler = nest.NewEngine(append(p.runtime.NestOptions(), nest.NestOptionWithGetter(p.access),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 16}))...)
	p.scheduler.MustRegisterHandlerWithMeta(levelCrashCommand, func(es []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
		c := es[0].(*levelCrashEntity).component
		if err := args[0].(func(*LevelComponent) error)(c); err != nil {
			return nil, err
		}
		state, err := json.Marshal(c.Snapshot())
		return string(state), err
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	if err := p.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.scheduler.Shutdown(context.Background()) })
	return p
}

type noLevelEffects struct{}

func (noLevelEffects) Publish(context.Context, engine.OutboxItem) error {
	return fmt.Errorf("level crash test publishes no effects")
}

// run 在 Nest 里执行 fn 并等提交结论，返回提交后的状态 JSON。
func (p *levelCrashProcess) run(t *testing.T, fn func(*LevelComponent) error) string {
	t.Helper()
	result, err := p.scheduler.RequestSettled(p.ctx, levelCrashCommand, p.id, nest.NewParams(fn))
	if err != nil {
		t.Fatal(err)
	}
	return result.(string)
}

// enemyDead 是投给第 generation 代 wave 阶段的击杀事实。
func enemyDead(seq, generation uint64) Event {
	event := hit(seq)
	event.Kind, event.Stage, event.Generation = "enemy.dead", "wave", generation
	return event
}

// completePending 为当前全部待办动作回执成功（宿主已执行了它们），返回最后的状态 JSON。
func (p *levelCrashProcess) completePending(t *testing.T, state string, now int64) string {
	t.Helper()
	var pending []Effect
	p.run(t, func(c *LevelComponent) error { pending = c.PendingActions(); return nil })
	for _, e := range pending {
		receipt := Receipt{Instance: e.Instance, Epoch: e.Epoch, ID: e.ID, Success: true}
		state = p.run(t, func(c *LevelComponent) error { return c.Complete(receipt, now) })
	}
	return state
}

func reportLevel(key, value string) { fmt.Printf("%s%s %s\n", levelCrashReply, key, value) }

func runLevelCrashWriter(t *testing.T) {
	p := openLevelCrashProcess(t)
	if _, err := p.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: levelCrashKind, Id: p.id}); err != nil {
		t.Fatal(err)
	}
	state := p.run(t, initStart)            // 第一波：spawn_wave 待办 + 超时计时器
	state = p.completePending(t, state, 1)  // 宿主刷了第一波
	for seq := uint64(1); seq <= 2; seq++ { // 两次击杀完成第一波，重入 wave（generation 2、新的 spawn_wave 待办）
		state = p.run(t, func(c *LevelComponent) error { return c.Handle(enemyDead(seq, 1), int64(1+seq)) })
	}
	state = p.completePending(t, state, 4)
	if err := p.runtime.Flush(p.ctx); err != nil { // 以上全部投影进 Mongo
		t.Fatal(err)
	}
	p.store.held.Store(true)
	state = p.run(t, func(c *LevelComponent) error { return c.Handle(enemyDead(3, 2), 5) }) // 第二波第一次击杀：只进 WAL
	select {
	case <-p.store.blocked:
	case <-time.After(30 * time.Second):
		t.Fatal("the last record never reached the held projection")
	}
	reportLevel("STATE", state)
	reportLevel("HELD", "")
	<-p.ctx.Done() // 等 SIGKILL
}

func runLevelCrashRecovery(t *testing.T) {
	p := openLevelCrashProcess(t) // Runtime.Start 回放 WAL
	if err := p.runtime.Flush(p.ctx); err != nil {
		t.Fatal(err)
	}
	state := p.run(t, func(*LevelComponent) error { return nil }) // 冷加载
	reportLevel("STATE", state)
	reportLevel("REPLAYED", strconv.FormatUint(currentLevelVersion(t, p), 10))
	// 第二波第二次击杀：victory，settle 待办、进入 Finishing；回执结算后关闭，CloseDelay 3 之后 Closed。
	state = p.run(t, func(c *LevelComponent) error { return c.Handle(enemyDead(4, 2), 6) })
	state = p.completePending(t, state, 7)
	p.run(t, func(c *LevelComponent) error { return c.Close(7) })
	state = p.run(t, func(c *LevelComponent) error { return c.AdvanceTick(10) })
	var s State
	if err := json.Unmarshal([]byte(state), &s); err != nil {
		t.Fatal(err)
	}
	if s.Phase != Closed || s.Result != "victory" {
		t.Fatalf("the recovered level did not finish with victory and close: %s", state)
	}
	if err := p.runtime.Flush(p.ctx); err != nil {
		t.Fatal(err)
	}
	reportLevel("FINISHED", strconv.FormatUint(currentLevelVersion(t, p), 10))
}

// currentLevelVersion 读本进程内存里关卡 DAO 的版本（持实体锁）。
func currentLevelVersion(t *testing.T, p *levelCrashProcess) uint64 {
	t.Helper()
	var version uint64
	p.run(t, func(c *LevelComponent) error { version = c.dao.tracker.Version(); return nil })
	return version
}
