package match

import (
	"reflect"
	"testing"

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
