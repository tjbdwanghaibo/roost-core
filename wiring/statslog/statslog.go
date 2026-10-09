package statslog

import (
	"fmt"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	runtime "github.com/tjbdwanghaibo/roost-core/framework/statslog"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"time"
)

// StatsLogMod 保留便捷的 provider 接入；采样窗口、文件和周期任务由唯一 Logger 拥有。
type StatsLogMod struct{ *runtime.Logger }

func NewStatsLogMod() *StatsLogMod       { return &StatsLogMod{Logger: runtime.New()} }
func (m *StatsLogMod) Name() app.ModName { return mods.ModStatsLog }

// config 是 stats_log.* 的声明（维护者决定 A4 ①）。
type config struct {
	app.ServiceIdentity
	Enabled  bool          `config:"stats_log.enabled" example:"true" help:"按 interval 把指标快照写进 stats 日志"`
	Dir      string        `config:"stats_log.dir" default:"log" example:"log" help:"stats 日志目录（相对进程工作目录，部署要给它可写的挂载）"`
	Filename string        `config:"stats_log.filename" help:"文件名，不写取 <server_type>-<sid>.stats.log"`
	Interval time.Duration `config:"stats_log.interval" default:"1m" min:"1ns" example:"1m"`
}

func (m *StatsLogMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }
func (m *StatsLogMod) Init(cfg *viper.Viper) error {
	var c config
	if err := app.LoadConfig(cfg, &c); err != nil {
		return fmt.Errorf("stats_log: %w", err)
	}
	return m.Configure(runtime.Config{Enabled: c.Enabled, Service: c.ServerType, SID: c.Sid, Dir: c.Dir, Filename: c.Filename, Interval: c.Interval})
}
func (m *StatsLogMod) Provide(r *app.Registry) error {
	if r == nil {
		return nil
	}
	metric, _ := app.Lookup[*metrics.Registry](r, mods.ModMetrics)
	if err := m.Connect(runtime.Dependencies{Metrics: metric, Nest: func() runtime.NestSource { n, _ := app.Lookup[runtime.NestSource](r, mods.ModNest); return n }, Entities: func() runtime.EntitySource {
		e, _ := app.Lookup[runtime.EntitySource](r, mods.ModEntityRuntime)
		return e
	}}); err != nil {
		return err
	}
	return r.Register(mods.ModStatsLog, m.Logger)
}

var _ app.Mod = (*StatsLogMod)(nil)
