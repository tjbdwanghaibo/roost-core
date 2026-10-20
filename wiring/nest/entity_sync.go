package nest

import (
	"context"
	"fmt"
	"math"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// EntitySyncSetup 是正式启动装配，业务只提供传输与内容/兴趣规则。
// Configure 在配置文件之后执行，可显式覆盖 Mode、Interval 等启动参数。
type EntitySyncSetup struct {
	Config    entitysync.ManagerConfig
	Configure func(*entitysync.ManagerConfig)
	// ConfigSchema 可选：LoadConfig 读的键的声明，并入 Nest Mod 的 ConfigSchema——App 在任何 Mod Init 之前按它检查
	// 一次（--check-config、doctor 同一份）。读别的 Mod 的键时声明须与那个 Mod 的逐字相同（configschema.Merge 比较）。
	ConfigSchema app.ConfigSchema
	// LoadConfig 可选：Nest Mod Init 读完自己的配置之后、Configure 与创建 Manager 之前调用一次，按 ConfigSchema
	// （app.LoadConfig）读部署配置——Manager 的帧上限等在创建时就定下，之后改不了。返回错误时 Init 失败。
	LoadConfig func(cfg *viper.Viper) error
	// Attach 可选：Manager 创建之后（Mod Init 内、Start 之前）调用一次，用来把政策装到这个 Manager 上
	// （例如 wiring/selfsync 的自身视图）。返回的 detach 在 Mod 停止、Manager 关闭之后调用，可为 nil；
	// 返回错误时 Init 失败，Manager 被关闭。
	Attach func(*entitysync.Manager) (detach func(), err error)
	// EntityLoaded 可选：DataEngine 发布一个从权威加载的实体之后调用，与 Mod 自己的 Rebind 同一回调、在它之后
	// （Nest 业务池上，不得阻塞、不得做 I/O）。
	EntityLoaded func(entity.IThreadSafeEntity)
}

func NewModWithEntitySync(getter entity.Getter, setup EntitySyncSetup, opts ...corenest.NestOption) *Mod {
	m := NewMod(getter, opts...)
	m.syncSetup = &setup
	return m
}

func (m *Mod) initEntitySync(cfg *viper.Viper, settings config, batchWindowSet bool) error {
	if m.syncSetup == nil {
		return nil
	}
	if m.syncSetup.LoadConfig != nil {
		if err := m.syncSetup.LoadConfig(cfg); err != nil {
			return fmt.Errorf("nest mod entity sync: %w", err)
		}
	}
	config := m.syncSetup.Config
	if settings.EntitySync.Mode != "" {
		mode, err := entitysync.ParseSyncMode(settings.EntitySync.Mode)
		if err != nil {
			return err
		}
		config.Mode = mode
	}
	if settings.EntitySync.Interval > 0 {
		config.Interval = settings.EntitySync.Interval
	}
	// 显式 0 也覆盖代码配置，以便部署时关闭合批；Configure 仍拥有最终覆盖权。
	if batchWindowSet {
		config.OnChangeBatchWindow = settings.EntitySync.OnChangeBatchWindow
	}
	if settings.EntitySync.MaxFrozenBytes > 0 {
		config.MaxFrozenBytes = settings.EntitySync.MaxFrozenBytes
	}
	if m.syncSetup.Configure != nil {
		m.syncSetup.Configure(&config)
	}
	if config.DurableWatermark == nil {
		// 业务没有显式指定水位时由 wiring 接线：Provide 确定 committer 后，pipelined committer 的
		// DurableLSN 成为外发水位。Manager 在 Init 创建而 committer 在 Provide 才确定，
		// 所以这里装一个转发函数。
		config.DurableWatermark = m.committerDurableWatermark
		m.autoWatermark = true
	}
	if config.Name == "" {
		config.Name = "nest" // 诊断里点名这个 Manager（ErrSubjectRegisteredElsewhere 等）
	}
	manager, err := entitysync.NewManager(config)
	if err != nil {
		return fmt.Errorf("nest mod entity sync: %w", err)
	}
	if m.syncSetup.Attach != nil {
		detach, err := m.syncSetup.Attach(manager)
		if err != nil {
			_ = manager.Close(context.Background())
			return fmt.Errorf("nest mod entity sync: attach: %w", err)
		}
		m.syncDetach = detach
	}
	m.entitySync = manager
	return nil
}

// EntitySync 在 Init 后可用于安装 Interest、注册实体和会话；由此 Mod 管理生命周期。
func (m *Mod) EntitySync() *entitysync.Manager {
	if m == nil {
		return nil
	}
	return m.entitySync
}

// durableWatermarkSource 是 Provide 解析出的水位来源；lsn 为 nil 表示 committer 不支持 pipelined。
type durableWatermarkSource struct{ lsn func() uint64 }

// bindDurableWatermark 在引擎构造后确定水位来源。选择自动接线而不是缺失时拒绝启动：wiring 的 Data Engine
// committer 总是实现 pipelined 接口，拒绝启动会让所有未手工接线的既有部署无法启动；而同步持久提交不产生
// CommitLSN，接上水位对它们没有效果。
func (m *Mod) bindDurableWatermark(engine *corenest.NestMgr) {
	if m == nil || !m.autoWatermark || engine == nil {
		return
	}
	m.watermark.Store(&durableWatermarkSource{lsn: engine.DurableWatermark()})
}

// committerDurableWatermark 是 wiring 自动装进 ManagerConfig.DurableWatermark 的来源。
// Provide 之前返回 0：带 CommitLSN 的内容一律暂缓（此时也不可能已有 pipelined 提交）；
// committer 不支持 pipelined 时返回最大值——不存在已准入而未持久的提交，内容不受门槛影响。
func (m *Mod) committerDurableWatermark() uint64 {
	source := m.watermark.Load()
	if source == nil {
		return 0
	}
	if source.lsn == nil {
		return math.MaxUint64
	}
	return source.lsn()
}
