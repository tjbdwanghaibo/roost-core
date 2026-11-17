package modulehost

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// 模块的配置（维护者 2026-11-14 决定 D19 B）：模块清单的 config: 指出模块包里一个带 config tag 的结构体，
// 写法与 Mod 的配置声明相同（键、缺省值、范围、说明写在 tag 上，跨键规则写在它的 ValidateConfig）。
// 安装器在 bootstrap 里把它交给本包的 Mod（托管服务上就是装模块入口的那个 Mod，业务服务上另加一个只带配置的），
// 于是：
//
//   - 声明进入 App 的配置声明：`--print-config`、`--print-config-schema`、`roost config check` 与 doctor 都看得见；
//   - App 启动前（任何 Mod Init 之前）与 `--check-config` 按声明检查键，再执行结构体的 ValidateConfig
//     （CheckConfigAcrossMods），写错的值在检查时就报，不等到请求读它；
//   - Mod 的 Init（本服务任何 Mod Start 之前）按声明装载一次，经 use 交给模块，模块代码从它的 Value 读。
//
// 规则只声明一次（结构体 + tag），装载时统一检查一次；模块里不再有手写的读取与校验。

// Config 是一个模块的配置声明与接收入口。用 ConfigOf 构造。
type Config struct {
	schema app.ConfigSchema
	// check 按声明解码一份新值（含 ValidateConfig），只检查不交出；load 解码后交给模块（use 为 nil 时不装载）。
	check func(*viper.Viper) error
	load  func(*viper.Viper) error
}

// ConfigOf 返回配置结构体 T 的声明。use 接收进程启动时装载好的值（通常是模块包里 Value[T] 的 Use）；
// 为 nil 时只声明：模块在自己的入口里用 app.LoadConfig 读（例如登录入口在 Start 里读 portal.*）。
// T 的声明写错（tag 解析不了、缺省值超出范围）是编程错误，在这里 panic，与 app.SchemaOf 相同。
func ConfigOf[T any](use func(T)) *Config {
	decode := func(cfg *viper.Viper) (T, error) {
		var value T
		err := app.LoadConfig(cfg, &value)
		return value, err
	}
	config := &Config{schema: app.SchemaOf(new(T))}
	config.check = func(cfg *viper.Viper) error {
		_, err := decode(cfg)
		return err
	}
	config.load = func(cfg *viper.Viper) error {
		if use == nil {
			return nil
		}
		value, err := decode(cfg)
		if err != nil {
			return err
		}
		use(value)
		return nil
	}
	return config
}

// Value 持有一个模块的配置，给模块代码读：零值可用，并发安全。进程启动时 Mod 的 Init 装载一次并 Use；
// 没有装载过（模块场景测试、工具）时 Get 返回声明的缺省值。测试换一份配置时用 Use，结束时换回 Get 读到的旧值。
type Value[T any] struct {
	current atomic.Pointer[T]

	defaultsOnce sync.Once
	defaults     T
}

// Use 换上一份配置；之后的 Get 读到它。
func (v *Value[T]) Use(value T) { v.current.Store(&value) }

// Get 返回当前的配置：装载过就是装载的值，否则是声明的缺省值。
func (v *Value[T]) Get() T {
	if current := v.current.Load(); current != nil {
		return *current
	}
	v.defaultsOnce.Do(func() {
		// 空配置按声明解码就是缺省值；连缺省值都通不过声明（必填键没有缺省值）是编程错误。
		if err := app.LoadConfig(nil, &v.defaults); err != nil {
			panic(fmt.Sprintf("modulehost: the declared defaults of %T do not pass the declaration: %v", v.defaults, err))
		}
	})
	return v.defaults
}

// ConfigSchema 实现 app.ModConfigSchema：本 Mod 上全部模块的键。两个模块声明了同一个键而写法不同时，
// 合并出错的那一份在 CheckConfigAcrossMods 与 Init 里报（这里返回不了错误）。
func (m *Mod) ConfigSchema() app.ConfigSchema {
	schema, _ := m.mergedConfigSchema()
	return schema
}

func (m *Mod) mergedConfigSchema() (app.ConfigSchema, error) {
	var schemas []configschema.Schema
	for _, module := range m.modules {
		if module.Config != nil {
			schemas = append(schemas, module.Config.schema)
		}
	}
	merged, err := configschema.Merge(schemas...)
	if err != nil {
		return merged, fmt.Errorf("modulehost: the modules of service %s declare conflicting config keys: %w", m.service, err)
	}
	return merged, nil
}

// CheckConfigAcrossMods 实现 app.ModConfigCrossChecker：键的检查（类型、范围、枚举）由合并的声明在前面做完，
// 这里按每个模块自己的结构体再解码一次，执行它的跨键规则（ValidateConfig）。App 在启动前与 --check-config
// 时调用，与键检查同一处。
func (m *Mod) CheckConfigAcrossMods(cfg *viper.Viper, _ func(app.ModName) bool) error {
	if _, err := m.mergedConfigSchema(); err != nil {
		return err
	}
	var problems []error
	for _, module := range m.modules {
		if module.Config == nil {
			continue
		}
		if err := module.Config.check(cfg); err != nil {
			problems = append(problems, fmt.Errorf("module %s: %w", module.Name, err))
		}
	}
	return errors.Join(problems...)
}

// loadConfigs 在 Init 里按声明装载每个模块的配置并交给它。
func (m *Mod) loadConfigs(cfg *viper.Viper) error {
	if _, err := m.mergedConfigSchema(); err != nil {
		return err
	}
	for _, module := range m.modules {
		if module.Config == nil {
			continue
		}
		if err := module.Config.load(cfg); err != nil {
			return fmt.Errorf("modulehost: config of module %s on service %s: %w", module.Name, m.service, err)
		}
	}
	return nil
}

var (
	_ app.ModConfigSchema       = (*Mod)(nil)
	_ app.ModConfigCrossChecker = (*Mod)(nil)
)
