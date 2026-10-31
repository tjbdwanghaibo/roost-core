package match

import (
	"reflect"
	"testing"
	"time"

	"github.com/spf13/viper"
	domain "github.com/tjbdwanghaibo/roost-core/service/match"
)

// match.sweep_queues entries are validated like any queue, at Init.
func TestSweepQueuesConfigurationFailsClosed(t *testing.T) {
	for _, bad := range []string{"ranked", "ranked:x", "ranked:1", "ranked:2:asia:extra", ":2"} {
		if _, err := parseSweepQueues([]string{bad}); err == nil {
			t.Errorf("entry %q was accepted", bad)
		}
	}
	queues, err := parseSweepQueues([]string{"ranked:2", " casual:4:eu "})
	if err != nil || len(queues) != 2 || queues[1] != (domain.Queue{Mode: "casual", GroupSize: 4, Partition: "eu"}) {
		t.Fatalf("parsed %+v err=%v", queues, err)
	}
	if _, err := domain.NewMemoryStore(domain.Config{SweepQueues: []domain.Queue{{Mode: "ranked", GroupSize: 1}}}); err == nil {
		t.Fatal("NewMemoryStore accepted an invalid sweep queue")
	}
	cfg := viper.New()
	cfg.Set("match.key_prefix", "roost:match")
	cfg.Set("match.sweep_queues", []string{"ranked:oops"})
	if err := NewMod(nil).Init(cfg); err == nil {
		t.Fatal("Init accepted a malformed match.sweep_queues entry")
	}
}

// 匹配 store 里没有任何路径读取 Grouping（成组由调用方 Candidates → Grouping → Commit 驱动），所以 Mod 与 Store Config 不接受一个不会被
// 执行的分组策略；Grouping 接口及 FirstCome / ScoreWindow 保留为调用方（应用 matchmaker）的工具。用反射断言签名。
func TestGroupingIsNotAnInjectionPointOfTheStore(t *testing.T) {
	if _, has := reflect.TypeOf(domain.Config{}).FieldByName("Grouping"); has {
		t.Fatal("Config.Grouping still exists: the store never executes it, so accepting it is a promise the store cannot keep")
	}
	if in := reflect.TypeOf(NewMod).NumIn(); in != 1 {
		t.Fatalf("NewMod takes %d parameters; the Mod has no executor for a Grouping and must not accept one", in)
	}
	// The policy types stay: they are the matchmaker's tools (see the
	// codegen game-demo matchmaker), just not the store's configuration.
	var _ domain.Grouping = domain.FirstComeGrouping{}
	var _ domain.Grouping = domain.ScoreWindowGrouping{}
}

// match.retention：缺省值与领域常量 DefaultRetention 是同一个数（标签缺省与常量不能各说各的），
// 配置值原样交给 Store，小于 1s 在 Init 拒绝。
func TestRetentionConfigurationReachesTheStore(t *testing.T) {
	load := func(retention any) (*Mod, error) {
		cfg := viper.New()
		cfg.Set("match.key_prefix", "roost:match")
		if retention != nil {
			cfg.Set("match.retention", retention)
		}
		mod := NewMod(nil)
		return mod, mod.Init(cfg)
	}
	mod, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if mod.retention != domain.DefaultRetention {
		t.Fatalf("default match.retention = %v, domain.DefaultRetention = %v", mod.retention, domain.DefaultRetention)
	}
	if mod, err = load("30s"); err != nil || mod.retention != 30*time.Second {
		t.Fatalf("match.retention 30s loaded as %v (err %v)", mod.retention, err)
	}
	if _, err := load("500ms"); err == nil {
		t.Fatal("Init accepted a sub-second match.retention")
	}
}

// match.sweep_queues 的人数段与 Queue.LogicalKey 同一种写法：固定队列 "8"，弹性队列 "<MinSize>-<GroupSize>@<秒数>s"；
// 解析后与代码里声明的弹性队列相等（同一个键），格式错误或违反弹性规则在 Init 拒绝。
func TestSweepQueuesAcceptFlexibleQueues(t *testing.T) {
	queues, err := parseSweepQueues([]string{"party:2-8@20s:eu", "party:3-4@0s"})
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Queue{Mode: "party", GroupSize: 8, MinSize: 2, FillAfterSeconds: 20, Partition: "eu"}
	if len(queues) != 2 || queues[0] != want || queues[0].Key() != want.Key() {
		t.Fatalf("parsed %+v, want %+v first", queues, want)
	}
	if queues[1] != (domain.Queue{Mode: "party", GroupSize: 4, MinSize: 3}) {
		t.Fatalf("parsed %+v", queues[1])
	}
	if got := want.LogicalKey() + ":" + want.Partition; got != "party:2-8@20s:eu" {
		t.Fatalf("the sweep entry and the logical key disagree: %q", got)
	}
	for _, bad := range []string{
		"party:2-8", "party:2-8@", "party:x-8@20s", "party:2-y@20s", "party:2-8@soon",
		"party:8-8@20s", "party:1-8@20s", "party:2-8@1500ms", "party:2-8@-1s", "party:-8@20s",
	} {
		if _, err := parseSweepQueues([]string{bad}); err == nil {
			t.Errorf("entry %q was accepted", bad)
		}
	}
}
