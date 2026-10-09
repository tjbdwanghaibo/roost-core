package statslog

import (
	"context"
	"encoding/json"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	runtime "github.com/tjbdwanghaibo/roost-core/framework/statslog"
	"os"
	"path/filepath"
	"testing"
)

// 正式 Mod 链路验证配置、provider 不丢失，以及文件运行确实委托给 Logger。
func TestWiringPreservesProvidersAndWritesConfiguredRecord(t *testing.T) {
	m := NewStatsLogMod()
	m.RegisterProvider("game", func() (any, error) { return "ready", nil })
	c := viper.New()
	c.Set("server_type", "game")
	c.Set("sid", 7)
	c.Set("stats_log.enabled", true)
	c.Set("stats_log.dir", t.TempDir())
	c.Set("stats_log.filename", "record.jsonl")
	c.Set("stats_log.interval", "1h")
	if err := m.Init(c); err != nil {
		t.Fatal(err)
	}
	if err := m.Provide(app.NewRegistry(c)); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	if err := m.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(c.GetString("stats_log.dir"), "record.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record runtime.StatsRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Service != "game" || record.Sid != 7 || record.Providers["game"] != "ready" {
		t.Fatalf("record=%+v", record)
	}
}
func TestWiringRejectsInvalidInterval(t *testing.T) {
	m := NewStatsLogMod()
	c := viper.New()
	c.Set("stats_log.interval", "-1s")
	if err := m.Init(c); err == nil {
		t.Fatal("negative interval accepted")
	}
}
