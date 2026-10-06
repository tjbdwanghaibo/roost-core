package remoteflow

// Mirror 第 5 步：公会摘要样例（roost-core docs/feature/MIRROR-STEP-5-2026-10-06.md）。
//
// 两个进程：
//   - owner = 本测试进程：Managed Guild 经正式链路（Nest → WAL → Remote 提交 → 投影）提交，提交后发布快照；
//   - 只读服务 = 子进程（同一测试二进制以 ROOST_MIRROR_CHILD=1 再执行 TestGeneratedRemoteMirrorReaderProcess）：
//     只装 kit SyncBusMod + RemoteMirrorMod，经生成的 NewGuildSummaryReader 读公会摘要；不调用 RegisterEntity、
//     不装写 Manager。
//
// JetStream 模式：只读方首读之后续租兴趣，owner 再提交，只读方在陈旧上限（60s）内经推送读到新版本。
// 普通 NATS 模式：推送关闭，新版本只在陈旧上限（3s）之后经 L2 / 权威按需读到。
//
// 单独运行：ROOST_REMOTE_RUN='^TestGeneratedRemoteMirror' scripts/test-remote-generated.sh

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
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	kitremote "github.com/tjbdwanghaibo/roost-core/kit/remoteentity"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/kit/syncbus"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	syncdriver "github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"
)

const (
	mirrorOwnerSid  = 1000
	mirrorReaderSid = 3000
)

// mirrorInterestBus 把 owner 收到的兴趣消息报给测试（owner 收到只读方的兴趣之后才提交第二版）。
type mirrorInterestBus struct {
	fsyncbus.ISyncBus
	interests chan struct{}
}

func (b mirrorInterestBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.ISyncBus.Subscribe(topic, func(ctx context.Context, msg *fsyncbus.SyncMsg) error {
		err := h(ctx, msg)
		if err == nil && topic == remoteentity.SyncTopicInterest {
			select {
			case b.interests <- struct{}{}:
			default:
			}
		}
		return err
	})
}

// mirrorInterestLiveBus 同上，并保留下层的可确认订阅（JetStream）。
type mirrorInterestLiveBus struct{ mirrorInterestBus }

func (b mirrorInterestLiveBus) SubscribeLive(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.ISyncBus.(fsyncbus.ILiveSubscriber).SubscribeLive(topic, h)
}

// guildOwner 是 owner 进程的正式链路：Remote Assembly（Mongo 权威 + Redis 锁 / L2 + 同步总线）、Runtime、Nest。
type guildOwner struct {
	scheduler *nest.NestMgr
	rename    nest.HandlerName
	interests chan struct{}
	id        int64
}

func newGuildOwner(t *testing.T, ctx context.Context, mongo fmongo.IMongo, redis fredis.IRedis, database, l2Prefix string, bus fsyncbus.ISyncBus, rawID int64) *guildOwner {
	t.Helper()
	owner := &guildOwner{rename: nest.NewHandlerName("remote_mirror_guild_rename"), interests: make(chan struct{}, 16)}
	// 只读方的 L2 前缀是 l2Prefix（remote_entity.snapshot_l2_key_prefix），键为 <前缀>:<快照键>；owner 经
	// scopedRedis 加同样的 "<前缀>:"，两边落在同一组 Redis 键上。
	scoped := &scopedRedis{IRedis: redis, prefix: l2Prefix + ":"}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		scoped.keys.Range(func(key, _ any) bool {
			if _, err := redis.Del(cleanup, key.(string)); err != nil {
				t.Errorf("Redis cleanup: %v", err)
			}
			return true
		})
	})
	access := entity.NewManagerAccess(entity.NewEntityManager())
	backend, err := remoteentity.NewBackend(access, remoteentity.NewMongoCommitter(mongo, database, mirrorOwnerSid, 0))
	if err != nil {
		t.Fatal(err)
	}
	cfg := remoteentity.DefaultConfig()
	cfg.LockTTL = 3 * time.Second
	assembly, err := remoteentity.Assemble(remoteentity.AssemblyDeps{Redis: scoped, Backend: backend}, cfg, mirrorOwnerSid, remoteentity.MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	observed := mirrorInterestBus{ISyncBus: bus, interests: owner.interests}
	var ownerBus fsyncbus.ISyncBus = observed
	if _, live := bus.(fsyncbus.ILiveSubscriber); live {
		ownerBus = mirrorInterestLiveBus{observed}
	}
	if err := assembly.Start(ctx, ownerBus); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.Stop(context.Background()); err != nil {
			t.Errorf("Remote stop: %v", err)
		}
	})
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRemoteProjection(backend, assembly.Manager); err != nil {
		t.Fatal(err)
	}
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(opts)
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
	outbox, err := engine.NewOutboxWorker(outboxStore, noEffects{}, engine.OutboxWorkerOptions{Owner: "remote-mirror"})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := engine.NewRuntime(store, wal, projector, outbox, access, assembly.Manager, nil, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Errorf("runtime shutdown: %v", err)
		}
	})
	options := append(runtime.NestOptions(), nest.NestOptionWithGetter(access), nest.NestOptionWithRemoteEntityManager(assembly.Manager),
		nest.NestOptionWithWorkerNumAndMsgCap(2, 1, 64),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 64}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 64}))
	owner.scheduler = nest.NewEngine(options...)
	owner.scheduler.MustRegisterHandlerWithMeta(owner.rename, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		guild := es[0].(*Guild)
		guild.dao.SetName(params[0].(string))
		guild.dao.SetMembers(guild.dao.GetMembers() + 1)
		guild.dao.SetMotto("not part of the summary")
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict})
	if err := owner.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.scheduler.Shutdown(context.Background()) })

	owner.id, err = entity.BuildEntityID(rawID, EntityKindGuild)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindGuild, Id: owner.id}); err != nil {
		t.Fatal(err)
	}
	prepareRemoteOwnership(t, ctx, assembly.Manager, []int64{owner.id})
	return owner
}

func (o *guildOwner) commit(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	if _, err := o.scheduler.Request(ctx, o.rename, o.id, nest.Params{name}); err != nil {
		t.Fatalf("owner commit %q: %v", name, err)
	}
}

// mirrorChild 是只读服务子进程的一问一答通道：每行 "MIRROR <事件> ..."。
type mirrorChild struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	log   *strings.Builder
}

func startMirrorChild(t *testing.T, ctx context.Context, env []string) *mirrorChild {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGeneratedRemoteMirrorReaderProcess$", "-test.v", "-test.count=1")
	cmd.Env = append(append(os.Environ(), "ROOST_MIRROR_CHILD=1"), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	child := &mirrorChild{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 64), log: &strings.Builder{}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			child.log.WriteString(line + "\n")
			if rest, ok := strings.CutPrefix(line, "MIRROR "); ok {
				child.lines <- rest
			}
		}
		close(child.lines)
	}()
	return child
}

// expect 等子进程报告 event，返回其余字段；子进程退出或超时即失败（带子进程输出）。
func (c *mirrorChild) expect(event string) []string {
	c.t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				_ = c.cmd.Wait()
				c.t.Fatalf("read-only process exited before %q:\n%s", event, c.log.String())
			}
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == event {
				return fields[1:]
			}
		case <-timeout:
			c.t.Fatalf("read-only process never reported %q:\n%s", event, c.log.String())
		}
	}
}

func (c *mirrorChild) send(command string) {
	c.t.Helper()
	if _, err := fmt.Fprintln(c.stdin, command); err != nil {
		c.t.Fatalf("send %q: %v\n%s", command, err, c.log.String())
	}
}

func (c *mirrorChild) wait() {
	c.t.Helper()
	_ = c.stdin.Close()
	for range c.lines {
	}
	if err := c.cmd.Wait(); err != nil {
		c.t.Fatalf("read-only process failed: %v\n%s", err, c.log.String())
	}
	c.t.Logf("read-only process output:\n%s", c.log.String())
}

func TestGeneratedRemoteMirrorGuildSummary(t *testing.T) {
	if os.Getenv("ROOST_MIRROR_CHILD") == "1" {
		t.Skip("parent-only test")
	}
	ctx, mongo, redis, database := rejectTestEnv(t)
	natsURL := remoteNatsURL()
	for index, mode := range []string{"jetstream", "nats"} {
		t.Run(mode, func(t *testing.T) {
			prefix := database + ".mirror." + mode
			l2Prefix := database + ":mirror:" + mode
			client, err := natsdriver.NewClient(fnats.DefaultConfig(natsURL), natsdriver.ClientOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			var bus fsyncbus.ISyncBus
			staleness := 3 * time.Second
			if mode == "jetstream" {
				staleness = time.Minute
				js, err := natsdriver.NewJetStreamClient(client)
				if err != nil {
					t.Fatal(err)
				}
				jsBus, err := syncdriver.NewJetStreamSyncBus(ctx, js, syncdriver.JetStreamSyncConfig{
					LocalSid: mirrorOwnerSid, Prefix: prefix, Storage: fnats.JetStreamStorageMemory, StreamMaxAge: 10 * time.Minute, AckWait: 2 * time.Second,
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = jsBus.StopWithContext(stopCtx)
					deleteMirrorStream(t, natsURL, syncdriver.JetStreamSyncStream(prefix))
				})
				bus = jsBus
			} else {
				bus = syncdriver.NewNatsSyncBus(client, mirrorOwnerSid, prefix)
			}
			// 每种模式一个公会，所有权互不干扰；v1 在只读方启动前提交。
			owner := newGuildOwner(t, ctx, mongo, redis, database, l2Prefix, bus, int64(9900+index))
			owner.commit(t, ctx, "alpha")

			child := startMirrorChild(t, ctx, []string{
				"ROOST_MIRROR_MODE=" + mode, "ROOST_MIRROR_PREFIX=" + prefix, "ROOST_MIRROR_L2_PREFIX=" + l2Prefix,
				"ROOST_MIRROR_DATABASE=" + database, "ROOST_MIRROR_GUILD=" + strconv.FormatInt(owner.id, 10),
				"ROOST_MIRROR_STALENESS=" + staleness.String(),
			})
			push := child.expect("READY")
			if want := fmt.Sprint(mode == "jetstream"); len(push) != 1 || push[0] != "push="+want {
				t.Fatalf("read-only process push mode %v, want push=%s", push, want)
			}
			if got := child.expect("FIRST"); len(got) != 3 || got[0] != "1" || got[1] != "alpha" || got[2] != "1" {
				t.Fatalf("first read %v, want version 1 name alpha members 1", got)
			}
			// owner 收到只读方的兴趣之后才提交第二版（兴趣决定 owner 向谁发布）。
			select {
			case <-owner.interests:
			case <-ctx.Done():
				t.Fatal("the read-only process's interest never reached the owner", ctx.Err())
			}
			owner.commit(t, ctx, "beta") // v2
			child.send("EXPECT 2 beta")
			got := child.expect("SECOND")
			if len(got) != 4 || got[0] != "2" || got[1] != "beta" || got[2] != "2" {
				t.Fatalf("second read %v, want version 2 name beta members 2", got)
			}
			after, err := time.ParseDuration(got[3])
			if err != nil {
				t.Fatal(err)
			}
			if mode == "jetstream" && after >= staleness {
				t.Fatalf("v2 reached the reader after %v, not before the staleness bound %v: it did not come by push", after, staleness)
			}
			if mode == "nats" && after < staleness {
				t.Fatalf("v2 reached the reader %v after its first read, before the staleness bound %v: plain NATS must not push", after, staleness)
			}
			child.send("STOP")
			child.expect("STOPPED")
			child.wait()
		})
	}
}

func deleteMirrorStream(t *testing.T, url, stream string) {
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Logf("cleanup connect: %v", err)
		return
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := js.DeleteStream(cleanupCtx, stream); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
		t.Logf("delete stream %s: %v", stream, err)
	}
}

// ---- 只读服务（子进程） ----

func mirrorReport(format string, args ...any) { fmt.Printf("MIRROR "+format+"\n", args...) }

// TestGeneratedRemoteMirrorReaderProcess 是只读服务：kit SyncBusMod + RemoteMirrorMod，没有写 Manager、没有
// RegisterEntity。只在父用例以 ROOST_MIRROR_CHILD=1 再执行时运行。
func TestGeneratedRemoteMirrorReaderProcess(t *testing.T) {
	if os.Getenv("ROOST_MIRROR_CHILD") != "1" {
		t.Skip("started by TestGeneratedRemoteMirrorGuildSummary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	mode := os.Getenv("ROOST_MIRROR_MODE")
	guildID, err := strconv.ParseInt(os.Getenv("ROOST_MIRROR_GUILD"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	staleness, err := time.ParseDuration(os.Getenv("ROOST_MIRROR_STALENESS"))
	if err != nil {
		t.Fatal(err)
	}

	// 部署配置：只有 sid、总线与快照段。
	cfg := viper.New()
	cfg.Set("server_type", "guildview")
	cfg.Set("sid", mirrorReaderSid)
	cfg.Set("syncbus.transport", mode)
	cfg.Set("syncbus.prefix", os.Getenv("ROOST_MIRROR_PREFIX"))
	cfg.Set("syncbus.storage", "memory")
	cfg.Set("syncbus.stream_max_age", "10m") // 与 owner 建流的参数一致
	cfg.Set("syncbus.ack_wait", "2s")
	cfg.Set("remote_entity.mongo.database", os.Getenv("ROOST_MIRROR_DATABASE"))
	cfg.Set("remote_entity.snapshot_l2_key_prefix", os.Getenv("ROOST_MIRROR_L2_PREFIX"))
	cfg.Set("remote_entity.snapshot_cache_ttl", "10m")
	cfg.Set("remote_entity.cached_max_staleness", staleness.String())
	if err := app.ValidateServiceConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// 基础设施能力（生产里由 redis / mongo / nats Mod 提供）。
	registry := app.NewRegistry(cfg)
	redis, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	if err != nil {
		t.Fatal(err)
	}
	defer redis.Close()
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	natsClient, err := natsdriver.NewClient(fnats.DefaultConfig(remoteNatsURL()), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer natsClient.Close()
	js, err := natsdriver.NewJetStreamClient(natsClient)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[app.ModName]any{
		mods.ModRedis: fredis.IRedis(redis), mods.ModMongo: fmongo.IMongo(mongo),
		mods.ModNats: fnats.IClient(natsClient), mods.ModNatsJetStream: fnats.IJetStream(js),
	} {
		if err := registry.Register(name, value); err != nil {
			t.Fatal(err)
		}
	}

	// 只读服务的装配就是这两个 Mod（总线 + 只读快照）。
	busMod := kitsyncbus.NewSyncBusMod(0)
	mirrorMod := kitremote.NewRemoteMirrorMod(0)
	for _, mod := range []app.Mod{busMod, mirrorMod} {
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		if err := mod.Provide(registry); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range []app.Mod{busMod, mirrorMod} {
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = mirrorMod.StopWithContext(context.Background())
		}
		_ = busMod.StopWithContext(context.Background())
	}()

	// 业务读取：一行拿到 reader。
	source, err := kitremote.MirrorSource(registry)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewGuildSummaryReader(source)
	if err != nil {
		t.Fatal(err)
	}

	// 读写分离：只读进程没有写能力。
	assertMirrorProcessReadOnly(t, registry, source)

	status := mirrorHealth(t, registry)
	mirrorReport("READY push=%v", strings.Contains(status, "snapshot_push=true"))

	// 首读：owner 在本进程启动前提交的 v1，经共享 L2 / Mongo 权威读到；同时续租兴趣。
	firstReadAt := time.Now()
	first := waitGuildSummary(t, ctx, reader, guildID, 1)
	mirrorReport("FIRST %d %s %d", first.Observation.StateVersion, first.Value.Name, first.Value.Members)

	// 跨租户 / profile：同一个公会 ID 在别的租户或视图下没有数据，绝不交出 owner 的快照。
	assertMirrorTenantIsolation(t, ctx, source, guildID)

	commands := bufio.NewScanner(os.Stdin)
	for commands.Scan() {
		fields := strings.Fields(commands.Text())
		switch fields[0] {
		case "EXPECT":
			version, _ := strconv.ParseUint(fields[1], 10, 64)
			got := waitGuildSummary(t, ctx, reader, guildID, version)
			if got.Value.Name != fields[2] {
				t.Fatalf("version %d name %q, want %q", version, got.Value.Name, fields[2])
			}
			mirrorReport("SECOND %d %s %d %s", got.Observation.StateVersion, got.Value.Name, got.Value.Members, time.Since(firstReadAt).Round(time.Millisecond))
		case "STOP":
			if err := mirrorMod.StopWithContext(ctx); err != nil {
				t.Fatalf("stop: %v", err)
			}
			stopped = true
			if err := mirrorMod.StopWithContext(ctx); err != nil {
				t.Fatalf("second stop: %v", err)
			}
			if _, _, err := reader.Read(ctx, guildID, entity.RemoteReadCached, entity.RemoteObservation{}); !errors.Is(err, remoteentity.ErrSnapshotClientStopped) {
				t.Fatalf("read after stop = %v, want ErrSnapshotClientStopped", err)
			}
			mirrorReport("STOPPED")
			return
		}
	}
	t.Fatal("the owner closed the command channel before STOP")
}

// waitGuildSummary 用 Cached 读等到 version（Cached 读在陈旧上限内不回源：JetStream 下新版本只能来自推送）。
func waitGuildSummary(t *testing.T, ctx context.Context, reader *entity.RemoteMirrorReader[GuildSummary], id int64, version uint64) entity.RemoteMirrorValue[GuildSummary] {
	t.Helper()
	for {
		got, found, err := reader.Read(ctx, id, entity.RemoteReadCached, entity.RemoteObservation{})
		if err != nil {
			t.Fatalf("read guild %d: %v", id, err)
		}
		if found && got.Observation.StateVersion >= version {
			if got.Observation.StateVersion != version {
				t.Fatalf("read version %d, want %d", got.Observation.StateVersion, version)
			}
			return got
		}
		select {
		case <-ctx.Done():
			t.Fatalf("version %d never arrived (last found=%v version=%d)", version, found, got.Observation.StateVersion)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func assertMirrorProcessReadOnly(t *testing.T, registry *app.Registry, source entity.RemoteSnapshotReadOnly) {
	t.Helper()
	for _, name := range []app.ModName{mods.ModRemoteEntity, mods.ModRemoteEntityAtomicStore, mods.ModRedisVLock} {
		if value, ok := registry.Get(name); ok {
			t.Fatalf("the read-only process holds write capability %q (%T)", name, value)
		}
	}
	if _, ok := source.(entity.IRemoteSnapshotPublisher); ok {
		t.Fatalf("the read-only source %T can publish snapshots", source)
	}
	if _, ok := source.(entity.IRemoteEntityManager); ok {
		t.Fatalf("the read-only source %T is a remote entity manager", source)
	}
	if param := entity.GetEntityBuilderParam(EntityKindGuild); param != nil {
		t.Fatalf("the read-only process registered the Guild entity builder: it could load and write guilds")
	}
}

func assertMirrorTenantIsolation(t *testing.T, ctx context.Context, source entity.RemoteSnapshotReadOnly, id int64) {
	t.Helper()
	for _, view := range []struct {
		name   string
		mutate func(*entity.RemoteMirrorSpec)
	}{
		{"tenant", func(s *entity.RemoteMirrorSpec) { s.Tenant = 7 }},
		{"profile", func(s *entity.RemoteMirrorSpec) { s.Policy = 3 }},
	} {
		spec := GuildSummaryMirrorSpec
		view.mutate(&spec)
		other, err := entity.NewRemoteMirrorReader(source, spec, DecodeGuildSummary)
		if err != nil {
			t.Fatal(err)
		}
		if got, found, err := other.Read(ctx, id, entity.RemoteReadMonotonic, entity.RemoteObservation{}); err != nil || found {
			t.Fatalf("another %s read guild %d: found=%v value=%+v err=%v; want not found (the owner's data belongs to tenant 0 / profile 0)", view.name, id, found, got.Value, err)
		}
		// 源交回另一个视图的快照（错配的 loader / 共享前缀）：reader 拒绝，不解码。
		forged, err := entity.NewRemoteMirrorReader(foreignViewSource{source}, spec, DecodeGuildSummary)
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := forged.Read(ctx, id, entity.RemoteReadCached, entity.RemoteObservation{}); err == nil || found {
			t.Fatalf("a %s read answered with tenant 0's snapshot: found=%v err=%v; want a rejection", view.name, found, err)
		}
	}
	mirrorReport("ISOLATED")
}

// foreignViewSource 把任何请求都换成 tenant 0 / profile 0 的 key 去读（模拟错配的源）。
type foreignViewSource struct{ next entity.RemoteSnapshotReadOnly }

func (s foreignViewSource) ReadSnapshot(ctx context.Context, req entity.RemoteSnapshotRead) (entity.RemoteSnapshotEnvelope, bool, error) {
	req.Key.Tenant, req.Key.Policy = 0, 0
	return s.next.ReadSnapshot(ctx, req)
}

// mirrorHealth 返回 remote_mirror 健康项的文本（带 snapshot_push 与 interest_refused）。
func mirrorHealth(t *testing.T, registry *app.Registry) string {
	t.Helper()
	reg, ok := app.Lookup[*health.Registry](registry, mods.ModHealth)
	if !ok {
		t.Fatal("no health registry")
	}
	for _, result := range reg.Snapshot(context.Background()).Results {
		if result.Name == "remote_mirror" {
			if result.Status != health.StatusOK || !strings.Contains(result.Message, "interest_refused=0") {
				t.Fatalf("remote_mirror health = %s (%s)", result.Status, result.Message)
			}
			return result.Message
		}
	}
	t.Fatal("RemoteMirrorMod registered no health check")
	return ""
}
