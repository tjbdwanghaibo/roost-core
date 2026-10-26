package physicsroom

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// hostConfig 是 EntityHostMod 读的 physicsroom.* 键。容量与时间的取值范围只在 domain.New 里检查一次
// （Provide 时报出，带键名前缀），这里不再抄一份边界。
type hostConfig struct {
	Room struct {
		EntityKind     int           `config:"entity_kind" required:"true" min:"1" max:"255" example:"240" help:"房间 Entity 的 kind，部署方分配；未登记时按 entity_category 登记，已登记成别的分类则启动失败"`
		EntityCategory int           `config:"entity_category" required:"true" min:"1" max:"255" example:"1" help:"房间 Entity 的 category"`
		CommitWorkers  int           `config:"commit_workers" default:"2" min:"1" max:"64" help:"房间 Nest 引擎 short 池的 worker 数（只跑帧提交与快照读取，物理步进在房间执行器）"`
		CommitQueue    int           `config:"commit_queue" default:"128" min:"1" max:"65536" help:"房间 Nest 引擎 short 池的队列容量"`
		MaxRooms       int           `config:"max_rooms" default:"64" help:"同时存活的房间上限（1..1024）"`
		MaxPlayers     int           `config:"max_players" default:"20" help:"每房成员上限（1..1024）"`
		MaxBodies      int           `config:"max_bodies" default:"256" help:"每房刚体上限，含静态体（1..4096）"`
		MaxFrameBytes  int           `config:"max_frame_bytes" default:"65536" help:"每成员单帧字节上限"`
		InputTTL       time.Duration `config:"input_ttl" default:"250ms" help:"输入未更新多久后停止施力"`
		MemberTTL      time.Duration `config:"member_ttl" default:"5s" help:"成员不 Poll / Submit 多久后回收；不小于 input_ttl"`
		MaxLifetime    time.Duration `config:"max_lifetime" default:"1h" help:"房间寿命上限，也是销毁后房间 ID 拒绝重建的保留期"`
		AOIRadius      float64       `config:"aoi_radius" default:"12" help:"XYZ 球形 AOI 进入半径（米）"`
		AOIMargin      float64       `config:"aoi_margin" default:"2" help:"AOI 退出滞回（米）；半径加滞回不超过 100"`
	} `config:"physicsroom"`
}

// EntityHostMod 是物理房间 owner 进程的标准 Mod：按 physicsroom.* 配置建 EntityHost（瞬态房间 Entity + 专用 Nest
// 引擎）与房间 Service，Commit 写入 Host 的 Nest Guard，房间结束后 Host 释放 Entity。它与 NewMod 提供同一能力名，
// 一个进程只装其中一个：业务已有自己的房间 Entity 与 Nest handler 时用 NewMod + NestCommit。
//
// 进程入口：app.RegisterServer(app.ServiceName(domain.ServiceType), NewServer(), <NATS Mod>, NewEntityHostMod())。
// 停止顺序：Server.Shutdown 先排空房间；本 Mod 停止时再排空一次（幂等），成功后才关闭 Host；超时保留 Host 与引擎。
type EntityHostMod struct {
	cfg     hostConfig
	host    *EntityHost
	service *domain.Service
}

func NewEntityHostMod() *EntityHostMod { return &EntityHostMod{} }

func (*EntityHostMod) Name() app.ModName              { return domain.CapabilityName }
func (*EntityHostMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(hostConfig{}) }
func (m *EntityHostMod) Init(cfg *viper.Viper) error  { return app.LoadConfig(cfg, &m.cfg) }
func (*EntityHostMod) Start() error                   { return nil }
func (m *EntityHostMod) Stop()                        { _ = m.StopWithContext(context.Background()) }
func (m *EntityHostMod) Host() *EntityHost            { return m.host }
func (m *EntityHostMod) domainConfig(c domain.Commit) domain.Config {
	r := m.cfg.Room
	cfg := domain.DefaultConfig(c)
	cfg.MaxRooms, cfg.MaxPlayers, cfg.MaxBodies, cfg.MaxFrameBytes = r.MaxRooms, r.MaxPlayers, r.MaxBodies, r.MaxFrameBytes
	cfg.InputTTL, cfg.MemberTTL, cfg.MaxLifetime = r.InputTTL, r.MemberTTL, r.MaxLifetime
	cfg.Radius, cfg.Margin = r.AOIRadius, r.AOIMargin
	return cfg
}

func (m *EntityHostMod) Provide(r *app.Registry) error {
	kind, category := entity.EntityKind(m.cfg.Room.EntityKind), entity.EntityCategory(m.cfg.Room.EntityCategory)
	if kind == 0 || category == 0 {
		return errors.New("physicsroom: entity_kind and entity_category are required")
	}
	// 同一定义重复登记无变化；kind 已登记成别的分类时拒绝，不能静默共用业务的 Kind。
	if err := entity.RegisterEntityKindCategory(kind, category); err != nil {
		return fmt.Errorf("physicsroom.entity_kind: %w", err)
	}
	pool := nest.WorkerPoolConfig{Workers: m.cfg.Room.CommitWorkers, QueueCap: m.cfg.Room.CommitQueue}
	idle := nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}
	host, err := NewEntityHost(kind, category, m.cfg.Room.MaxRooms, nest.NestOptionWithWorkerPools(pool, idle, idle))
	if err != nil {
		return fmt.Errorf("physicsroom config: %w", err)
	}
	cfg := m.domainConfig(host.Commit)
	cfg.Closed = host.Forget
	// 容量、时间与 AOI 的范围在 domain.New 里检查；未启用 PhysX 后端时同样在这里失败。
	service, err := domain.New(cfg)
	if err != nil {
		_ = host.Close(context.Background())
		return fmt.Errorf("physicsroom config: %w", err)
	}
	if err = mods.RegisterAll(r, OwnerCapabilities(service)...); err != nil {
		_ = service.Shutdown(context.Background())
		_ = host.Close(context.Background())
		return err
	}
	m.host, m.service = host, service
	return nil
}

func (m *EntityHostMod) StopWithContext(ctx context.Context) error {
	if m.service == nil {
		return nil
	}
	if err := m.service.Shutdown(ctx); err != nil {
		return err
	}
	return m.host.Close(ctx)
}

var _ app.Mod = (*EntityHostMod)(nil)
var _ app.ModStopperWithContext = (*EntityHostMod)(nil)
