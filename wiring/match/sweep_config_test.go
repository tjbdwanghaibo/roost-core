package match

import domain "github.com/tjbdwanghaibo/roost-core/service/match"

import (
	"github.com/spf13/viper"
	"testing"
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
