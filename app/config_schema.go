package app

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// 配置声明（维护者决定 A4 ①，docs/feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md）。
//
// 每个 Mod 把自己读的键写成一个带 tag 的结构体，实现 ConfigSchema 返回它的声明，在 Init 里用 LoadConfig 读：
//
//	type shopConfig struct {
//		MaxItems int           `config:"shop.max_items" default:"100" min:"1" max:"10000" help:"每个玩家商店最多上架的物品数"`
//		Refresh  time.Duration `config:"shop.refresh_interval" default:"1h" min:"1m"`
//		Region   string        `config:"shop.region" enum:"cn|us|eu" required:"true" example:"cn"`
//	}
//
//	func (*ShopMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(shopConfig{}) }
//	func (m *ShopMod) Init(cfg *viper.Viper) error  { return app.LoadConfig(cfg, &m.cfg) }
//
// tag 的含义见 internal/configschema。App 在任何 Mod Init 之前按本服务全部 Mod 的声明检查一次配置（CheckConfig），
// 错误一次报全；生成器按同一份声明写配置段，doctor 按它检查工程里的配置文件。框架代码（app、kit）读配置只经
// LoadConfig（TestFrameworkModsReadConfigOnlyThroughDeclarations）。

// ConfigSchema 是一组配置键的声明，Mod 的 ConfigSchema 方法返回它。
type ConfigSchema = configschema.Schema

// ModConfigSchema 由声明了配置的 Mod 实现。App 启动时合并本服务全部 Mod 的声明检查配置，`--print-config` 按它打印配置段。
type ModConfigSchema interface {
	ConfigSchema() ConfigSchema
}

// SchemaOf 返回配置结构体（值或指针）的声明。声明写错（tag 解析不了、缺省值超出范围）是编程错误，panic。
func SchemaOf(config any) ConfigSchema { return configschema.MustOf(config) }

// LoadConfig 按 dst（指向配置结构体的指针）的声明从 cfg 读出全部键：没写的取 default，写了的检查类型、范围、枚举、
// 必填与生产密钥，再调用结构体的 ValidateConfig；全部错误一次报出，每条点名键。cfg 为 nil 时按空配置读。
func LoadConfig(cfg *viper.Viper, dst any) error {
	if cfg == nil {
		cfg = viper.New()
	}
	return configschema.Decode(viperSource{cfg}, dst, isProductionServiceConfig(cfg))
}

// CheckConfig 按 App 自己的声明与 mods 的声明检查 cfg，返回全部错误（App.run 在任何 Mod Init 之前调用，
// `--check-config` 与生成工程的测试也用它）。同一个键被两个 Mod 声明得不一样时报错。
func CheckConfig(cfg *viper.Viper, mods ...Mod) error {
	if cfg == nil {
		return errors.New("config: viper is nil")
	}
	_, err := checkConfig(cfg, mods)
	return err
}

func checkConfig(cfg *viper.Viper, mods []Mod) (appConfig, error) {
	var settings appConfig
	errs := unjoinErrors(LoadConfig(cfg, &settings))
	schemas := []ConfigSchema{AppConfigSchema()}
	source := viperSource{cfg}
	production := isProductionServiceConfig(cfg)
	for _, mod := range mods {
		declared, ok := mod.(ModConfigSchema)
		if !ok {
			continue
		}
		schema := declared.ConfigSchema()
		schemas = append(schemas, schema)
		for _, err := range schema.Check(source, production) {
			errs = append(errs, fmt.Errorf("mod %s: %w", mod.Name(), err))
		}
	}
	// 没有任何 Mod 声明的键（拼错的键名）不在这里报：生成的配置会带别的进程才注册的 Mod 的键（例如只读 Mirror 的
	// 停机预算），业务代码也会读框架段里的键（game-demo 读 activity / platform 的键前缀），进程看不出哪些是笔误。
	// 这件事由 doctor 的 config-schema 检查按生成器认识的全部声明判断。
	if _, err := configschema.Merge(schemas...); err != nil {
		errs = append(errs, err)
	}
	return settings, errors.Join(uniqueErrors(errs)...)
}

// AppConfigSchema 返回 App 自己读的键的声明（日志、业务时钟偏移、停机预算、指标上限、单实例锁、部署环境、
// server_type / sid）。生成器的快照（kit/internal/configschemagen）与 doctor 用它。
func AppConfigSchema() ConfigSchema { return SchemaOf(appConfig{}) }

// CheckServiceConfig 用本 App 给 serverType 注册的全部 Mod（共享 + 服务专属）检查 cfg，与 run 启动前做的一样。
func (a *App) CheckServiceConfig(serverType ServiceName, cfg *viper.Viper) error {
	_, err := checkConfig(cfg, a.serviceMods(serverType))
	return err
}

// ServiceConfigSchema 返回 serverType 的全部声明（App 自己的加上全部 Mod 的），`--print-config` 用它。
func (a *App) ServiceConfigSchema(serverType ServiceName) (ConfigSchema, error) {
	schemas := []ConfigSchema{AppConfigSchema()}
	for _, mod := range a.serviceMods(serverType) {
		if declared, ok := mod.(ModConfigSchema); ok {
			schemas = append(schemas, declared.ConfigSchema())
		}
	}
	return configschema.Merge(schemas...)
}

func (a *App) serviceMods(serverType ServiceName) []Mod {
	mods := append([]Mod(nil), a.mods...)
	if entry, ok := a.services[serverType]; ok {
		mods = append(mods, entry.mods...)
	}
	return mods
}

func unjoinErrors(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return append([]error(nil), joined.Unwrap()...)
	}
	return []error{err}
}

// viperSource 是 configschema.Source 的 viper 实现：框架里唯一直接读 viper 的地方。
type viperSource struct{ cfg *viper.Viper }

func (s viperSource) Get(key string) (any, bool) {
	if !s.cfg.IsSet(key) {
		return nil, false
	}
	return s.cfg.Get(key), true
}

func (s viperSource) Keys() []string { return s.cfg.AllKeys() }

// environmentConfig 是部署环境的三种写法；任一为 prod / production 即生产环境。
type environmentConfig struct {
	Env         string `config:"env" help:"prod / production 时打开生产检查（与 app.env、environment 任一相同）"`
	AppEnv      string `config:"app.env"`
	Environment string `config:"environment"`
}

func (c environmentConfig) production() bool {
	for _, value := range []string{c.Env, c.AppEnv, c.Environment} {
		switch strings.ToLower(value) {
		case "prod", "production":
			return true
		}
	}
	return false
}

// isProductionServiceConfig：env / app.env / environment 为 prod / production。生产规则（secret 键、ops 端点、
// Redis 地址、业务时间偏移）按它打开；它要在其他声明之前读出，所以单独解码（读不出的值由启动检查报出）。
func isProductionServiceConfig(cfg *viper.Viper) bool {
	if cfg == nil {
		return false
	}
	var env environmentConfig
	_ = configschema.Decode(viperSource{cfg}, &env, false)
	return env.production()
}

// ServiceIdentity 是每个服务进程都有的两个键：App 按启动的子命令写 server_type，按 --sid 或配置写 sid。
// Mod 需要时匿名嵌入它，与 App 共用同一份声明。
type ServiceIdentity struct {
	ServerType string `config:"server_type" help:"服务类型，App 按启动的子命令写入"`
	Sid        int32  `config:"sid" help:"服务实例号（正整数），同一服务类型内唯一；也可以用 --sid 覆盖"`
}

// appConfig 是 App 自己读的键（日志、业务时钟偏移、停机预算、指标上限、单实例锁）。
type appConfig struct {
	ServiceIdentity
	environmentConfig
	Log struct {
		Level            string        `config:"level" default:"info" example:"info"`
		JSON             bool          `config:"json" example:"true"`
		Stdout           bool          `config:"stdout" default:"true" example:"true"`
		File             bool          `config:"file" default:"true" example:"true"`
		Dir              string        `config:"dir" default:"log" example:"log"`
		Caller           bool          `config:"caller"`
		RotateInterval   time.Duration `config:"rotate_interval" default:"24h"`
		RotateTimeFormat string        `config:"rotate_time_format"`
	} `config:"log"`
	Time     timeConfig    `config:"time"`
	Metrics  metricsConfig `config:"metrics"`
	Shutdown struct {
		TotalTimeout time.Duration `config:"total_timeout" default:"30s" min:"1ns" help:"停机总预算：全部 Mod 停完的时长上限"`
	} `config:"shutdown"`
	Singleton singletonConfig `config:"singleton"`
}

type timeConfig struct {
	LogicOffset time.Duration `config:"logic_offset" help:"业务时钟偏移（只给测试环境前拨业务时间用，生产必须为 0）"`
}

type metricsConfig struct {
	MaxSeriesPerMetric int `config:"max_series_per_metric" min:"0" help:"每个指标的序列数上限，0 取框架默认"`
}

// ValidateConfig 是 App 自己的跨键规则：server_type 必填、sid 是 int32 范围内的正整数、生产环境业务时钟偏移为 0（D-L3）。
// 键的类型与单实例锁的时间关系在声明与 singletonConfig.ValidateConfig 里。
func (c *appConfig) ValidateConfig(production bool) error {
	var errs []error
	if strings.TrimSpace(c.ServerType) == "" {
		errs = append(errs, errors.New("config: server_type is required"))
	}
	if c.Sid <= 0 || c.Sid > math.MaxInt32 {
		errs = append(errs, errors.New("config: sid must be positive"))
	}
	if production && c.Time.LogicOffset != 0 {
		errs = append(errs, fmt.Errorf("config: production requires %s = 0, got %s; the offset is for moving business time in test environments", logicOffsetKey, c.Time.LogicOffset))
	}
	return errors.Join(errs...)
}

// readTimeConfig / readMetricsConfig 给 NewRegistry 用：类型错误由启动检查报出，这里读不出就取零值。
func readTimeConfig(cfg *viper.Viper) timeConfig {
	var value struct {
		Time timeConfig `config:"time"`
	}
	_ = LoadConfig(cfg, &value)
	return value.Time
}

func readMetricsConfig(cfg *viper.Viper) metricsConfig {
	var value struct {
		Metrics metricsConfig `config:"metrics"`
	}
	_ = LoadConfig(cfg, &value)
	return value.Metrics
}
