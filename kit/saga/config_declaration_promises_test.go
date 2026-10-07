package saga

// A4 ①：saga 的引擎参数以前写 0 或不写都取 coresaga.DefaultOptions；现在缺省值写在声明里（生成器与 doctor 看得到），
// 必须与 core 的缺省一致，否则文档化的缺省与运行时分叉。

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	kitnest "github.com/tjbdwanghaibo/roost-core/kit/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

func TestSagaDeclaredDefaultsMatchCoreDefaults(t *testing.T) {
	mod := NewMod()
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	got, want := mod.config.Engine, coresaga.DefaultOptions()
	if got.CoordinatorWorkers != want.CoordinatorWorkers || got.PublisherWorkers != want.PublisherWorkers ||
		got.CoordinatorBatch != want.CoordinatorBatch || got.PublisherBatch != want.PublisherBatch ||
		got.LeaseDuration != want.LeaseDuration || got.StoreTimeout != want.StoreTimeout || got.PollInterval != want.PollInterval ||
		got.PublishTimeout != want.PublishTimeout || got.PublishBackoffMin != want.PublishBackoffMin ||
		got.PublishBackoffMax != want.PublishBackoffMax || got.MaxPayloadBytes != want.MaxPayloadBytes {
		t.Fatalf("declared defaults %+v differ from coresaga.DefaultOptions %+v", got, want)
	}
	for name, timeout := range map[string]any{
		"result":        mod.config.Completions.ProcessTimeout,
		"start_effect":  mod.config.Starts.ProcessTimeout,
		"result_effect": mod.config.NestResults.ProcessTimeout,
	} {
		if timeout != want.StoreTimeout {
			t.Errorf("%s process_timeout default = %v, want core StoreTimeout %v", name, timeout, want.StoreTimeout)
		}
	}
}

// RR-20261006-66：saga.max_payload_bytes 由协调器（本 Mod）与发起方（kit/nest）共用一份声明。同一服务里两个 Mod 都在时
// 声明必须完全相同（A4 ① Merge），超过线上硬上限 4 MiB 在配置检查时就拒绝。
func TestSagaPayloadLimitIsOneDeclarationSharedWithNest(t *testing.T) {
	cfg := viper.New()
	cfg.Set("sid", 1)
	cfg.Set("server_type", "game")
	if err := app.CheckConfig(cfg, NewMod(), kitnest.NewMod(nil)); err != nil {
		t.Fatalf("saga and nest Mods in one service: %v", err)
	}
	for _, schema := range []app.ConfigSchema{NewMod().ConfigSchema(), kitnest.NewMod(nil).ConfigSchema()} {
		if _, ok := schema.Lookup("saga.max_payload_bytes"); !ok {
			t.Fatalf("saga.max_payload_bytes missing from a declaration: %+v", schema.Keys)
		}
	}
	cfg.Set("saga.max_payload_bytes", 4<<20+1)
	if err := app.CheckConfig(cfg, NewMod()); err == nil {
		t.Fatal("saga.max_payload_bytes over the 4 MiB wire cap passed the config check")
	}
}
