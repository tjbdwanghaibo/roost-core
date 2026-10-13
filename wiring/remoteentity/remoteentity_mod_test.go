package remoteentity

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	coreremote "github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 本机兴趣表只是被过期条目占满时，健康检查不能一直报 capacity exhausted：过期条目只在需要新建兴趣或定期续租时清理，
// 健康检查不能直接按 map 长度判断，否则读满一阵之后、活跃兴趣早已为 0 时仍永远是 Fail。
//
// kind 253：本包测试只注册这一个 kind。
const interestHealthKind entity.EntityKind = 253

func TestExpiredLocalInterestsDoNotKeepHealthFailing(t *testing.T) {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: interestHealthKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := coreremote.DefaultConfig()
	cfg.SnapshotInterestKeys = 4
	cfg.SnapshotInterestTTL = 40 * time.Millisecond
	manager := coreremote.NewManager(nil, cfg, 1000)
	mod := &RemoteEntityMod{asm: &coreremote.Assembly{Manager: manager}, cfg: cfg}
	ctx := context.Background()
	for i := int64(0); i < int64(cfg.SnapshotInterestKeys); i++ {
		id, err := entity.BuildEntityID(9500+i, interestHealthKind)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.RenewRemoteSnapshotInterest(ctx, entity.RemoteSnapshotKey{EntityID: id, Kind: interestHealthKind, Scope: 1}); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	// 对照：表真满且全部有效时报 Fail，这是既有的容量告警。
	if result := mod.checkHealth(ctx); result.Status != health.StatusFail {
		t.Fatalf("a full table of live interests = %s (%s), want fail", result.Status, result.Message)
	}
	// 等全部兴趣的租期过去（时间条件本身就是被测对象，不是为了排序并发）。
	deadline := time.Now().Add(cfg.SnapshotInterestTTL)
	for time.Now().Before(deadline.Add(5 * time.Millisecond)) {
		time.Sleep(5 * time.Millisecond)
	}
	if live := manager.Stats().LocalInterests; live != 0 {
		t.Errorf("Stats().LocalInterests = %d after every interest expired, want 0", live)
	}
	if result := mod.checkHealth(ctx); result.Status != health.StatusOK {
		t.Fatalf("health after every interest expired = %s (%s), want ok", result.Status, result.Message)
	}
}

// O-M6-6：RemoteEntityMod 在 App 单实例锁启用时把锁的身份（app.ModSingletonIncarnation）交给 Remote，取锁 token
// 带上 "~<sid>~" 开头的进程代际，同 sid 重启后第一次取锁即可接管上一代进程留下的锁；未启用单实例锁（没有这个能力）
// 或 Remote 的 sid 不是单实例锁的 sid 时不传，token 是旧格式，照旧按 lock_ttl 等待。

// tokenRecordingRedis 记下取锁脚本收到的 token，并回答“被别人持有”（不触及 Mongo 许可）。
type tokenRecordingRedis struct {
	fredis.IRedis
	mu     sync.Mutex
	tokens []string
}

func (r *tokenRecordingRedis) Eval(_ context.Context, _ string, _ []string, args ...any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) > 0 {
		token, _ := args[0].(string)
		r.tokens = append(r.tokens, token)
	}
	return []any{int64(0), int64(0), int64(0), int64(0)}, nil
}

type unusedMongo struct{ fmongo.IMongo }

func lockTokenAfterProvide(t *testing.T, modSid int32, incarnation *app.SingletonIncarnation) string {
	t.Helper()
	cfg := viper.New()
	cfg.Set("sid", 1101)
	redis := &tokenRecordingRedis{}
	r := app.NewRegistry(cfg)
	if err := r.Register(mods.ModRedis, fredis.IRedis(redis)); err != nil {
		t.Fatal(err)
	}
	if incarnation != nil {
		if err := r.Register(mods.ModSingletonIncarnation, *incarnation); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := coreremote.NewBackend(entity.NewManagerAccess(entity.NewEntityManager()), coreremote.NewMongoCommitter(unusedMongo{}, "remote_entity", 1101, 0))
	if err != nil {
		t.Fatal(err)
	}
	mod := NewRemoteEntityMod(modSid, WithBackend(backend))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(r); err != nil {
		t.Fatal(err)
	}
	factory := app.MustLookup[fredis.IVersionedLockFactory](r, mods.ModRedisVLock)
	lock := factory.NewVersionedLock(42, fredis.VersionedLockOptions{Key: "e", TTL: 3e9})
	_ = lock.TryLock(context.Background())
	redis.mu.Lock()
	defer redis.mu.Unlock()
	if len(redis.tokens) != 1 {
		t.Fatalf("lock scripts = %d, want one TryLock", len(redis.tokens))
	}
	return redis.tokens[0]
}

func TestRemoteEntityModPassesTheSingletonIncarnationToTheLocks(t *testing.T) {
	held := &app.SingletonIncarnation{Key: "roost:demo:singleton:game:1101", Sid: 1101, Token: "0123456789abcdef"}
	if token := lockTokenAfterProvide(t, 0, held); !strings.HasPrefix(token, "~1101~") || !strings.Contains(token, "~0123456789abcdef~") {
		t.Fatalf("lock token %q with the singleton held, want the incarnation prefix ~1101~<holder>~0123456789abcdef~", token)
	}
	if token := lockTokenAfterProvide(t, 0, nil); strings.HasPrefix(token, "~") {
		t.Fatalf("lock token %q with the singleton disabled, want the old format (no takeover)", token)
	}
	if token := lockTokenAfterProvide(t, 1102, held); strings.HasPrefix(token, "~") {
		t.Fatalf("lock token %q for a Remote sid other than the singleton's, want the old format (no takeover)", token)
	}
}

// remote_entity Mod 停止失败（ctx 到期、finalizer / 复制 handler 没排空）时不能记 “stopped”，否则与 App 随后的 “mod stop failed” 互相矛盾。
func TestRemoteEntityModDoesNotLogStoppedWhenStopFails(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	mod := &RemoteEntityMod{asm: &coreremote.Assembly{}, cfg: coreremote.DefaultConfig()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 停机预算已经用完
	if err := mod.StopWithContext(ctx); err == nil {
		t.Fatal("Stop with an expired ctx = nil")
	}
	if strings.Contains(logs.String(), `msg="remote_entity mod: stopped"`) {
		t.Fatalf("a failed stop logged success:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "remote_entity mod: stop incomplete") {
		t.Fatalf("a failed stop left no record:\n%s", logs.String())
	}

	logs.Reset()
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !strings.Contains(logs.String(), `msg="remote_entity mod: stopped"`) {
		t.Fatalf("a completed stop did not log it:\n%s", logs.String())
	}
}
