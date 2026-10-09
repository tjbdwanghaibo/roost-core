package statslog

import (
	"encoding/json"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type snapshotNest struct{ n uint64 }

func (s *snapshotNest) Stats() nest.DispatcherStats {
	return nest.DispatcherStats{Work: nest.DispatcherWorkStats{ProcessedMessages: s.n}}
}
func TestStatsEndpointDoesNotConsumeFileWindow(t *testing.T) {
	cfg := viper.New()
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", t.TempDir())
	cfg.Set("stats_log.filename", "b7.jsonl")
	m := New()
	if err := configureTestLogger(m, cfg); err != nil {
		t.Fatal(err)
	}
	r := app.NewRegistry(cfg)
	n := &snapshotNest{n: 10}
	if err := r.Register(app.ModName("nest"), n); err != nil {
		t.Fatal(err)
	}
	if err := connectTestLogger(m, r); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if err := m.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	n.n = 15
	m.CollectStats()
	n.n = 20
	if err := m.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.dir, m.filename))
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var record StatsRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		total += record.Nest.ProcessedMessages
	}
	if total != 20 {
		t.Fatalf("file windows contain %d messages, want 20; /statsz consumed part of the window", total)
	}
}
