package nest

import (
	"fmt"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"time"
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
