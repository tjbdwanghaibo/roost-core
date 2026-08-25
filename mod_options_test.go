package nestwal

import (
	"testing"

	"github.com/spf13/viper"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

func TestModNestOptionsCarryPipelinedRolloutConfig(t *testing.T) {
	cfg := viper.New()
	cfg.Set("nest.pipelined.allowlist", []string{"player.consume", "player.reward"})
	cfg.Set("nest.pipelined.async", true)
	cfg.Set("nest.pipelined.async_workers", 8)
	cfg.Set("nest.pipelined.async_queue_capacity", 1024)

	mod := NewMod(false)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	committer, err := NewCommitter(w, nil, nil, DefaultCommitterOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close(t.Context())
	mod.runtime = &Runtime{WAL: w, Committer: committer}

	applied := &corenest.NestOpts{}
	for _, option := range mod.NestOptions() {
		option(applied)
	}
	if applied.Committer == nil {
		t.Fatal("committer option missing")
	}
	if len(applied.PipelinedAllowlist) != 2 || applied.PipelinedAllowlist[0] != "player.consume" {
		t.Fatalf("allowlist=%v", applied.PipelinedAllowlist)
	}
	if !applied.PipelinedAsync || applied.PipelinedAsyncWorkers != 8 || applied.PipelinedAsyncQueueCap != 1024 {
		t.Fatalf("async config=%+v", applied)
	}

	// Defaults: no allowlist, async off — only the committer option remains,
	// so an unconfigured deployment keeps exactly the old behavior.
	plain := NewMod(false)
	if err := plain.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	plain.runtime = mod.runtime
	if got := len(plain.NestOptions()); got != 1 {
		t.Fatalf("unconfigured mod contributes %d options, want 1", got)
	}
}
