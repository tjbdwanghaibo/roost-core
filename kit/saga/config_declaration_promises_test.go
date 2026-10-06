package saga

// A4 ①：saga 的引擎参数以前写 0 或不写都取 coresaga.DefaultOptions；现在缺省值写在声明里（生成器与 doctor 看得到），
// 必须与 core 的缺省一致，否则文档化的缺省与运行时分叉。

import (
	"testing"

	"github.com/spf13/viper"
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
