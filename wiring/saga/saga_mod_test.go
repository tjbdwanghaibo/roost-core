package saga

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
)

// Nest 发起的 start 由 DataEngine 发布到 dataengine.effects 那条流上，启动中继必须订阅同一条流、同一个主题前缀。没写
// saga.start_effect_stream / start_effect_prefix 时直接取 dataengine.effects 的值（效果流只写一处）。
func TestSagaFollowsTheDataEngineEffectStreamByDefault(t *testing.T) {
	mod := NewMod()
	if err := mod.Init(yamlConfig(t, "dataengine:\n  effects:\n    stream: OTHER_EFFECTS\n    subject_prefix: other.effect.\n")); err != nil {
		t.Fatal(err)
	}
	if mod.config.Starts.Stream != "OTHER_EFFECTS" || mod.config.Starts.EffectPrefix != "other.effect" {
		t.Fatalf("start relay = %s / %s, want the dataengine.effects values", mod.config.Starts.Stream, mod.config.Starts.EffectPrefix)
	}
}

// 同一进程里有 DataEngine 时，显式写了且与 dataengine.effects 不同的 saga 效果流键在配置检查（启动前、--check-config）
// 就拒绝并点名两个键与改法；修前要到启动订阅时才报 "stream not found"，不说是哪两个键。
func TestConfigCheckRefusesSagaEffectKeysThatDisagreeWithDataEngine(t *testing.T) {
	base := "sid: 1\nserver_type: game\ndataengine:\n  effects:\n    stream: OTHER_EFFECTS\n    subject_prefix: other.effect\n  projection:\n    max_unacked_records: 20000\n"
	for name, tc := range map[string]struct{ saga, keys string }{
		"stream": {"saga:\n  start_effect_stream: ROOST_EFFECTS\n", "saga.start_effect_stream dataengine.effects.stream"},
		"prefix": {"saga:\n  start_effect_prefix: roost.effect\n", "saga.start_effect_prefix dataengine.effects.subject_prefix"},
	} {
		err := app.CheckConfig(yamlConfig(t, base+tc.saga), NewMod(), kitdataengine.NewMod())
		if err == nil {
			t.Errorf("%s: config check passed although saga and dataengine.effects name different effect streams", name)
			continue
		}
		for _, want := range append(strings.Fields(tc.keys), "delete") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not mention %s", name, err, want)
			}
		}
	}
	// 写成相同的值（含首尾点的差别）不报错；没有 DataEngine 的进程（只跑协调器）可以显式指向别的流。
	same := base + "saga:\n  start_effect_stream: OTHER_EFFECTS\n  start_effect_prefix: .other.effect\n"
	if err := app.CheckConfig(yamlConfig(t, same), NewMod(), kitdataengine.NewMod()); err != nil {
		t.Errorf("equal values refused: %v", err)
	}
	if err := app.CheckConfig(yamlConfig(t, base+"saga:\n  start_effect_stream: ROOST_EFFECTS\n"), NewMod()); err != nil {
		t.Errorf("a saga-only process pointing at another effect stream refused: %v", err)
	}
}

// 准入只拦新启动，在途推进照常产生投影积压，硬上限 dataengine.projection.max_unacked_records 是最后一道闸：同进程有 DataEngine
// 且准入开启时，它必须 > 0 且 ≥ 4 × saga.admission.high_records（设计 §6）。准入关闭或没有 DataEngine 时不比较。
func TestConfigCheckRequiresAProjectionCapAboveTheAdmissionWatermark(t *testing.T) {
	const identity = "sid: 1\nserver_type: game\n"
	for name, tc := range map[string]struct {
		text string
		ok   bool
	}{
		"no cap":                {"", false},
		"cap below 4 x high":    {"dataengine:\n  projection:\n    max_unacked_records: 19999\n", false},
		"cap at 4 x high":       {"dataengine:\n  projection:\n    max_unacked_records: 20000\n", true},
		"lower watermark":       {"dataengine:\n  projection:\n    max_unacked_records: 4000\nsaga:\n  admission:\n    high_records: 1000\n    low_records: 500\n", true},
		"admission off, no cap": {"saga:\n  admission:\n    enabled: false\n", true},
	} {
		err := app.CheckConfig(yamlConfig(t, identity+tc.text), NewMod(), kitdataengine.NewMod())
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok=%v", name, err, tc.ok)
		} else if err != nil && (!strings.Contains(err.Error(), "dataengine.projection.max_unacked_records") || !strings.Contains(err.Error(), "saga.admission.high_records")) {
			t.Errorf("%s: %v does not name both keys", name, err)
		}
	}
	if err := app.CheckConfig(yamlConfig(t, identity), NewMod()); err != nil {
		t.Errorf("a process without DataEngine was held to the projection cap: %v", err)
	}
}

// saga.* 的跨键规则（设计 §9.2 与第 1、2 批留给第 4 批的生产规则）：每条点名键。
func TestSagaConfigRulesNameTheirKeys(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"saga:\n  partitions: 400\n  stream_groups: 4\n", "partitions on one stream"},                // ceil(400/4) = 100 > 50
		{"saga:\n  partitions: 2\n  stream_groups: 4\n", "stream groups 4"},                           // G > P
		{"saga:\n  publish_unknown_budget: 20s\n", "saga.publish_unknown_budget"},                     // < 3 × 7s
		{"saga:\n  duplicate_window: 50s\n", "saga.duplicate_window"},                                 // < 2 × 30s
		{"saga:\n  publish_timeout: 9s\n  publish_unknown_budget: 21s\n  duplicate_window: 1m\n", ""}, // 合法
		{"saga:\n  terminal_retention: 200h\n", "saga.terminal_retention"},                            // > receipt_retention
		{"saga:\n  admission:\n    low_records: 6000\n", "saga.admission.low_records"},                // ≥ high
		{"saga:\n  admission:\n    stale_after: 500ms\n", "saga.admission.stale_after"},               // < 5 × 200ms
		{"saga:\n  start_effect_workers: 9\n  start_effect_max_ack_pending: 8\n", "saga.start_effect_workers"},
		{"saga:\n  coordinators: [\"1\", \"1\"]\n", "listed twice"},
		{"saga:\n  coordinators: [\"x\"]\n", "not a positive sid"},
	} {
		err := NewMod().Init(yamlConfig(t, tc.yaml))
		if tc.want == "" {
			if err != nil {
				t.Errorf("%q: %v, want accepted", tc.yaml, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: Init = %v, want a refusal naming %s", tc.yaml, err, tc.want)
		}
	}
	if err := NewMod().Init(viper.New()); err == nil || !strings.Contains(err.Error(), "saga.coordinators") {
		t.Fatalf("Init without saga.coordinators = %v, want it required", err)
	}
}

// 生产：流与桶副本数必须 3（前提：JetStream R3）；saga.coordinators 里的 sid 必须启用单实例锁（App 级“一个 sid 一个进程”）。
// 不在列表里的进程（只做发起方）不受单实例锁规则约束。
func TestSagaProductionRules(t *testing.T) {
	const production = "env: production\nsid: 1\nserver_type: game\n"
	for name, tc := range map[string]struct {
		text, want string
	}{
		"single replica":                {"saga:\n  replicas: 1\nsingleton:\n  enabled: true\n  key_prefix: p\n", "saga.replicas"},
		"coordinator without singleton": {"saga:\n  replicas: 3\n", "singleton.enabled=true"},
		"coordinator with singleton":    {"saga:\n  replicas: 3\nsingleton:\n  enabled: true\n  key_prefix: p\n", ""},
		"client-only without singleton": {"saga:\n  replicas: 3\n  coordinators: [\"2\"]\n", ""},
	} {
		err := app.CheckConfig(yamlConfig(t, production+tc.text), NewMod())
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want accepted", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %s", name, err, tc.want)
		}
	}
	if err := app.CheckConfig(yamlConfig(t, "sid: 1\nserver_type: game\nsaga:\n  replicas: 1\n"), NewMod()); err != nil {
		t.Errorf("development config (one replica, no singleton) refused: %v", err)
	}
}

// 分区归属（选项 P-A）：saga.coordinators 排序后本进程 sid 的下标 i 拥有 k mod len == i 的分区；不在列表里的进程没有分区。
func TestCoordinatorsAssignPartitionsStaticallyBySid(t *testing.T) {
	for _, tc := range []struct {
		coordinators []string
		sid          string
		want         []int
	}{
		{[]string{"1"}, "1", []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}},
		{[]string{"1002", "1001"}, "1001", []int{0, 2, 4, 6, 8, 10, 12, 14}},
		{[]string{"1002", "1001"}, "1002", []int{1, 3, 5, 7, 9, 11, 13, 15}},
		{[]string{"1002", "1001"}, "1003", nil},
	} {
		cfg := yamlConfig(t, "sid: "+tc.sid+"\n")
		cfg.Set("saga.coordinators", tc.coordinators)
		mod := NewMod()
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(mod.config.Engine.Owned, tc.want) {
			t.Errorf("coordinators %v sid %s: owned %v, want %v", tc.coordinators, tc.sid, mod.config.Engine.Owned, tc.want)
		}
	}
	// 全部协调器的分区合起来恰好是全部分区，没有重叠。
	seen := map[int]string{}
	for _, sid := range []string{"7", "3", "5"} {
		cfg := yamlConfig(t, "sid: "+sid+"\n")
		cfg.Set("saga.coordinators", []string{"7", "3", "5"})
		mod := NewMod()
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		for _, k := range mod.config.Engine.Owned {
			if owner, dup := seen[k]; dup {
				t.Fatalf("partition %d owned by %s and %s", k, owner, sid)
			}
			seen[k] = sid
		}
	}
	if len(seen) != 16 {
		t.Fatalf("%d of 16 partitions owned", len(seen))
	}
}

// 没有 Provide / Start 的 Mod 报告不健康（协调器与启动中继的健康在真实依赖用例里，见 framework/saga 的
// TestRealAssemblyStartsTheRelayAndStopsInOrder）。
func TestSagaModHealthBeforeStart(t *testing.T) {
	if got := NewMod().checkHealth(context.Background()); got.Status != health.StatusFail {
		t.Fatalf("health before Provide = %+v", got)
	}
}
