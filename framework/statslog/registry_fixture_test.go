package statslog
import("github.com/spf13/viper";"github.com/tjbdwanghaibo/roost-core/framework/app";"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics")
// Runtime 回归只注入普通配置/依赖；正式配置解析另由 wiring/statslog 验证。
func configureTestLogger(m *Logger,c *viper.Viper)error{return m.Configure(Config{Enabled:c.GetBool("stats_log.enabled"),Service:c.GetString("server_type"),SID:c.GetInt32("sid"),Dir:c.GetString("stats_log.dir"),Filename:c.GetString("stats_log.filename"),Interval:c.GetDuration("stats_log.interval")})}
func connectTestLogger(m *Logger,r *app.Registry)error{metric,_:=app.Lookup[*metrics.Registry](r,app.ModMetrics);return m.Connect(Dependencies{Metrics:metric,Nest:func()NestSource{n,_:=app.Lookup[NestSource](r,app.ModName("nest"));return n},Entities:func()EntitySource{e,_:=app.Lookup[EntitySource](r,app.ModName("entity.runtime"));return e}})}
