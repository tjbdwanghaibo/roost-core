package kit_test

// 维护者决定 A4 ①（docs/feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md）：每个 kit Mod 声明自己读的配置键
// （ConfigSchema），Init 用 app.LoadConfig 按这份声明读；App 启动前用同一份声明检查（app.CheckConfig）。
//
// 旧行为（A4 ②，3e3350d5）：kit Mod 在 Init 里逐键严格读取类型，范围与枚举散在“≤0 取默认”里：
// `dataengine.outbox.workers: -1`、`dataengine.effects.replicas: -3`、`redis.pool_size: -1`、
// `saga.coordinator_workers: -4` 静默取默认，`syncbus.storage: flie` 静默按驱动缺省，Init 与 ValidateServiceConfig
// 一律返回 nil（基线 66d72a33 上的同形用例，见方案文档“实施状态”）。

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	kitconfigdata "github.com/tjbdwanghaibo/roost-core/kit/configdata"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/kit/dataengine"
	kitetcd "github.com/tjbdwanghaibo/roost-core/kit/etcd"
	kitmongo "github.com/tjbdwanghaibo/roost-core/kit/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/kit/nats"
	kitnest "github.com/tjbdwanghaibo/roost-core/kit/nest"
	kitops "github.com/tjbdwanghaibo/roost-core/kit/ops"
	kitredis "github.com/tjbdwanghaibo/roost-core/kit/redis"
	kitremoteentity "github.com/tjbdwanghaibo/roost-core/kit/remoteentity"
	kitsaga "github.com/tjbdwanghaibo/roost-core/kit/saga"
	"github.com/tjbdwanghaibo/roost-core/kit/service/account"
	"github.com/tjbdwanghaibo/roost-core/kit/service/chat"
	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/kit/service/global"
	"github.com/tjbdwanghaibo/roost-core/kit/service/global/activity"
	"github.com/tjbdwanghaibo/roost-core/kit/service/mail"
	"github.com/tjbdwanghaibo/roost-core/kit/service/match"
	"github.com/tjbdwanghaibo/roost-core/kit/service/platform"
	"github.com/tjbdwanghaibo/roost-core/kit/service/rank"
	"github.com/tjbdwanghaibo/roost-core/kit/service/session"
	kitstatslog "github.com/tjbdwanghaibo/roost-core/kit/statslog"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/kit/syncbus"
)

type declaredMod interface {
	app.Mod
	app.ModConfigSchema
}

// everyKitMod 是 kit 里全部声明了配置的 Mod（服务 Mod 用零值：Init 先读配置、再查协作者）。
func everyKitMod() []declaredMod {
	return []declaredMod{
		kitops.NewOpsMod(), kitstatslog.NewStatsLogMod(), kitconfigdata.NewConfigDataMod(), kitetcd.NewEtcdMod(),
		kitredis.NewRedisMod(), kitmongo.NewMongoMod(), kitnats.NewNatsMod(nil), kitsyncbus.NewSyncBusMod(1000),
		kitremoteentity.NewRemoteEntityMod(1000), kitremoteentity.NewRemoteMirrorMod(1000),
		kitnest.NewMod(entity.NewManagerAccess(entity.NewEntityManager())), kitdataengine.NewMod(), kitsaga.NewMod(),
		new(account.Mod), new(chat.Mod), new(directory.Mod), new(global.Mod), new(activity.Mod), new(mail.Mod),
		new(match.Mod), new(platform.Mod), new(rank.Mod), new(session.Mod),
		account.NewClientMod(), chat.NewClientMod(), global.NewClientMod(), activity.NewClientMod(), mail.NewClientMod(),
		match.NewClientMod(), platform.NewClientMod(), rank.NewClientMod(), session.NewClientMod(),
	}
}

// wrongValue 是每种类型都读不出来的值。
func wrongValue(kind configschema.Kind) (any, bool) {
	switch kind {
	case configschema.KindBool:
		return "maybe", true
	case configschema.KindInt:
		return "8k", true
	case configschema.KindFloat:
		return "lots", true
	case configschema.KindDuration:
		return 15, true // 不带单位
	case configschema.KindString:
		return []any{"a", "b"}, true
	case configschema.KindStrings:
		return map[string]any{"a": 1}, true
	}
	return nil, false
}

// 声明与读取一致：把一个 Mod 声明的每个键依次写成读不出来的值，Init 都要点名它——Init 读的就是 ConfigSchema 返回的
// 那份声明，声明了的键没有一个被跳过。同时全部 kit Mod 与 App 的声明合并不冲突：同一个键在两处声明得不一样，
// App 启动时报错。
func TestEveryKitModLoadsWhatItDeclares(t *testing.T) {
	schemas := []app.ConfigSchema{app.AppConfigSchema()}
	for _, mod := range everyKitMod() {
		schema := mod.ConfigSchema()
		schemas = append(schemas, schema)
		checked := 0
		for _, key := range schema.Keys {
			value, ok := wrongValue(key.Kind)
			if !ok || strings.Contains(key.Name, "*") {
				continue
			}
			cfg := viper.New()
			cfg.Set(key.Name, value)
			if err := mod.Init(cfg); err == nil || !strings.Contains(err.Error(), key.Name) {
				t.Errorf("%s: %s = %v: Init = %v; want a refusal naming the key", mod.Name(), key.Name, value, err)
			}
			checked++
		}
		if checked == 0 {
			t.Errorf("%s declares no key", mod.Name())
		}
	}
	if _, err := configschema.Merge(schemas...); err != nil {
		t.Fatal(err)
	}
}

// 范围、枚举在 Mod Init（直接装配 Mod 的调用方）与 App 启动前（app.CheckConfig）都拒绝。服务配置没有运行期 reload：
// 这两处就是配置被加载的全部入口。
func TestKitModsRefuseOutOfRangeValuesAtLoadAndAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		mod              func() declaredMod
	}{
		{"outbox_workers_negative", "dataengine:\n  outbox:\n    workers: -1\n", "dataengine.outbox.workers must be positive", func() declaredMod { return kitdataengine.NewMod() }},
		{"effects_replicas_negative", "dataengine:\n  effects:\n    replicas: -3\n", "dataengine.effects.replicas must be positive", func() declaredMod { return kitdataengine.NewMod() }},
		{"wal_writer_version", "dataengine:\n  wal:\n    writer_version: 3\n", "dataengine.wal.writer_version must be at most 2", func() declaredMod { return kitdataengine.NewMod() }},
		{"redis_pool_negative", "redis:\n  pool_size: -1\n", "redis.pool_size must not be negative", func() declaredMod { return kitredis.NewRedisMod() }},
		{"saga_workers_negative", "saga:\n  coordinator_workers: -4\n", "saga.coordinator_workers must be positive", func() declaredMod { return kitsaga.NewMod() }},
		{"syncbus_storage_typo", "syncbus:\n  storage: flie\n", "syncbus.storage must be one of file, memory", func() declaredMod { return kitsyncbus.NewSyncBusMod(1000) }},
		{"nats_transport_typo", "nats:\n  rpc:\n    transport: jetsream\n", "nats.rpc.transport must be one of", func() declaredMod { return kitnats.NewNatsMod(nil) }},
		{"tombstone_wait_too_long", "remote_entity:\n  snapshot_l2_tombstone_wait_timeout: 2s\n", "remote_entity.snapshot_l2_tombstone_wait_timeout must be at most 1s", func() declaredMod { return kitremoteentity.NewRemoteEntityMod(1000) }},
		{"mail_send_ttl_missing", "mail:\n  key_prefix: roost:mail\n", "mail.send_ttl is required", func() declaredMod { return new(mail.Mod) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			cfg.SetConfigType("yaml")
			if err := cfg.ReadConfig(strings.NewReader("server_type: game\nsid: 1000\n" + tc.body)); err != nil {
				t.Fatal(err)
			}
			if err := tc.mod().Init(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Init = %v; want %q", err, tc.want)
			}
			if err := app.CheckConfig(cfg, tc.mod()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("app.CheckConfig = %v; want %q", err, tc.want)
			}
		})
	}
}

// 以下几条原来在 app 的 ValidateServiceConfig 里按服务类型写死；A4 ① 起跟着键的主人走，在 App 启动检查
// （app.CheckConfig 合并注册的 Mod）里得到同样的拒绝。

func serviceConfig(t *testing.T, serverType, body string) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader(fmt.Sprintf("server_type: %s\nsid: 2001\n%s", serverType, body))); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestStartupCheckRefusesJetStreamRPCWithoutPositiveTimeouts(t *testing.T) {
	cfg := serviceConfig(t, "game", "nats:\n  rpc:\n    transport: jetstream\n    call_timeout: 0s\n")
	if err := app.CheckConfig(cfg, kitnats.NewNatsMod(nil)); err == nil || !strings.Contains(err.Error(), "nats.rpc.call_timeout must be positive") {
		t.Fatalf("CheckConfig = %v", err)
	}
}

func TestStartupCheckRefusesOpsAdminWithoutAToken(t *testing.T) {
	cfg := serviceConfig(t, "game", "ops:\n  admin_enabled: true\n")
	if err := app.CheckConfig(cfg, kitops.NewOpsMod()); err == nil || !strings.Contains(err.Error(), "ops.admin_token") {
		t.Fatalf("CheckConfig = %v", err)
	}
}

func TestProductionRefusesDevSecrets(t *testing.T) {
	account := serviceConfig(t, "account", "env: production\naccount:\n  key_prefix: p\n  session_secret: dev-session-secret\n")
	if err := app.CheckConfig(account, new(accountMod)); err == nil || !strings.Contains(err.Error(), "production requires non-dev account.session_secret") {
		t.Fatalf("account: CheckConfig = %v", err)
	}
	platformCfg := serviceConfig(t, "platform", "env: production\nplatform:\n  key_prefix: p\n  session_secret: dev-session-secret\n  payment_secret: \"\"\n")
	err := app.CheckConfig(platformCfg, new(platform.Mod))
	for _, key := range []string{"platform.session_secret", "platform.payment_secret"} {
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("platform: CheckConfig = %v, want %s", err, key)
		}
	}
	// 非生产环境 dev- 密钥照常可用。
	dev := serviceConfig(t, "account", "account:\n  key_prefix: p\n  session_secret: dev-session-secret\n")
	if err := app.CheckConfig(dev, new(accountMod)); err != nil {
		t.Fatalf("dev account: CheckConfig = %v", err)
	}
}

type accountMod = account.Mod

func TestProductionRefusesAPublicOpsAddrUnlessDeclared(t *testing.T) {
	check := func(body string) error {
		return app.CheckConfig(serviceConfig(t, "game", "env: production\nops:\n  enabled: true\n"+body), kitops.NewOpsMod())
	}
	for _, addr := range []string{"0.0.0.0:9100", ":9100", "10.0.0.5:9100", "\"[::]:9100\""} {
		if err := check("  addr: " + addr + "\n"); err == nil || !strings.Contains(err.Error(), "ops.addr") {
			t.Errorf("public %s accepted: %v", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:9100", "\"[::1]:9100\"", "localhost:9100"} {
		if err := check("  addr: " + addr + "\n"); err != nil {
			t.Errorf("loopback %s refused: %v", addr, err)
		}
	}
	if err := check("  addr: 0.0.0.0:9100\n  allow_public_addr: true\n"); err != nil {
		t.Errorf("declared opt-out refused: %v", err)
	}
	if err := app.CheckConfig(serviceConfig(t, "game", "ops:\n  enabled: true\n  addr: 0.0.0.0:9100\n"), kitops.NewOpsMod()); err != nil {
		t.Errorf("non-production public bind refused: %v", err)
	}
}

// RR-20261005-NC-192（维护者决定 C1 方案 1）：生产检查只要求有读取方的设置；没人读的开关写不写都不影响。
func TestProductionDoesNotRequireSwitchesNothingReads(t *testing.T) {
	base := "env: production\nredis:\n  addr: 10.0.0.1:6379\n"
	for _, serverType := range []string{"game", "instance", "account", "global", "match_group"} {
		for _, extra := range []string{"", "player:\n  login_auth_required: false\nsave_load:\n  wal:\n    mode: async\n"} {
			if err := app.CheckConfig(serviceConfig(t, serverType, base+extra), kitredis.NewRedisMod()); err != nil {
				t.Errorf("production %s: %v", serverType, err)
			}
		}
	}
	if err := app.CheckConfig(serviceConfig(t, "game", "env: production\n"), kitredis.NewRedisMod()); err == nil ||
		!strings.Contains(err.Error(), "production requires redis.addr or redis.cluster_addrs") {
		t.Errorf("production redis without an address: %v", err)
	}
}

func TestCheckConfigErrorsAreJoined(t *testing.T) {
	err := app.CheckConfig(serviceConfig(t, "game", "redis:\n  db: x\nops:\n  enabled: maybe\n"), kitredis.NewRedisMod(), kitops.NewOpsMod())
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) < 2 {
		t.Fatalf("CheckConfig = %v; want every mod's error at once", err)
	}
}
