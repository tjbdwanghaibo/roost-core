package nest

import (
	"fmt"
	"math"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// EntitySyncSetup 是正式启动装配，业务只提供传输与内容/兴趣规则。
// Configure 在配置文件之后执行，可显式覆盖 Mode、Interval 等启动参数。
type EntitySyncSetup struct {
	Config    entitysync.ManagerConfig
	Configure func(*entitysync.ManagerConfig)
}

func NewModWithEntitySync(getter entity.Getter, setup EntitySyncSetup, opts ...corenest.NestOption) *Mod {
	m := NewMod(getter, opts...)
	m.syncSetup = &setup
	return m
}

func (m *Mod) initEntitySync(cfg *viper.Viper) error {
	if m.syncSetup == nil {
		return nil
	}
	config := m.syncSetup.Config
	if cfg.IsSet("sync.entity.mode") {
		mode, err := entitysync.ParseSyncMode(cfg.GetString("sync.entity.mode"))
		if err != nil {
			return err
		}
		config.Mode = mode
	}
	if cfg.IsSet("sync.entity.interval") {
		interval, err := time.ParseDuration(cfg.GetString("sync.entity.interval"))
		if err != nil {
			return fmt.Errorf("sync.entity.interval: %w", err)
		}
		config.Interval = interval
	}
	if cfg.IsSet("sync.entity.max_frozen_bytes") {
		config.MaxFrozenBytes = cfg.GetInt64("sync.entity.max_frozen_bytes")
	}
	if m.syncSetup.Configure != nil {
		m.syncSetup.Configure(&config)
	}
	if config.DurableWatermark == nil {
		// 业务没有显式指定水位时由 kit 接线：Provide 确定 committer 后，pipelined committer 的
		// DurableLSN 成为外发水位（RR-20260926-35）。Manager 在 Init 创建而 committer 在 Provide 才确定，
		// 所以这里装一个转发函数。
		config.DurableWatermark = m.committerDurableWatermark
		m.autoWatermark = true
	}
	manager, err := entitysync.NewManager(config)
	if err != nil {
		return fmt.Errorf("nest mod entity sync: %w", err)
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

// bindDurableWatermark 在引擎构造后确定水位来源。选择自动接线而不是缺失时拒绝启动：kit 的 Data Engine
// committer 总是实现 pipelined 接口，拒绝启动会让所有未手工接线的既有部署无法启动；而同步持久提交不产生
// CommitLSN，接上水位对它们没有效果。
func (m *Mod) bindDurableWatermark(engine *corenest.NestMgr) {
	if m == nil || !m.autoWatermark || engine == nil {
		return
	}
	m.watermark.Store(&durableWatermarkSource{lsn: engine.DurableWatermark()})
}

// committerDurableWatermark 是 kit 自动装进 ManagerConfig.DurableWatermark 的来源。
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
