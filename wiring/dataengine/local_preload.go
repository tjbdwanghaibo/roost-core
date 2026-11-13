package dataengine

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// LocalPreloadReport 是一次本地预加载的结果（engine.LocalPreloadReport）。
type LocalPreloadReport = engine.LocalPreloadReport

// LocalPreloadMod 是本地实体的起服预加载（实体类型的显式策略 //roost:entity preload=startup [preload_order=<N>]）：
// Start 时对构造时给出的 kind 调 engine.Runtime.PreloadLocalEntities——按 LoadPriority（preload_order）分批，枚举本服库
// （dbscope=sid，<库>_<sid>）里这些 kind 主 DAO 集合的未删除文档，经实体运行时加载进内存。
//
// 顺序：依赖 dataengine 与 nest（DataEngine 恢复完 WAL、Nest 业务池起来之后才加载；加载出来的实体经 Nest 绑定的本地
// 执行器发布）。App 在全部 Mod 启动之后才调 Service.Init，所以服务初始化时这些实体已经在内存里。玩家接入等别的 Mod
// 不等它：预加载期间来的访问与预加载共用同一次冷加载。
//
// 失败：kind 不满足规则（不是本地持久实体、有 DAO 不是 dbscope=sid）或枚举失败让启动失败（进程管理拉起重试）；
// 单个实体加载失败只记错误日志与指标 dataengine.local_preload_total{result="error"}，留给按需加载。
// 生成工程的 bootstrap 给声明了本地 preload 的服务装它（roost project sync），生成前检查 bootstrap 与实体标记一致。
type LocalPreloadMod struct {
	kinds    []entity.EntityKind
	registry *app.Registry
	// Report 是最近一次预加载的结果（诊断与测试用）。
	Report LocalPreloadReport
}

// NewLocalPreloadMod 返回预加载 kinds 的 Mod（生成的 registry.LocalPreloadKinds(service)）。kinds 为空时 Start 什么也不做。
func NewLocalPreloadMod(kinds ...entity.EntityKind) *LocalPreloadMod {
	return &LocalPreloadMod{kinds: slices.Clone(kinds)}
}

func (m *LocalPreloadMod) Name() app.ModName { return mods.ModDataEnginePreload }

func (m *LocalPreloadMod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModDataEngine, mods.ModNest}
}

// ConfigSchema：本 Mod 不读配置键（预加载 kind 来自实体标记，经构造参数传入）。
func (m *LocalPreloadMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(struct{}{}) }

// Init 在配置阶段把规则强制一次：kind 已登记 builder（生成的实体包在 init 里登记）、本地、持久、每个 DAO 都是 dbscope=sid。
func (m *LocalPreloadMod) Init(*viper.Viper) error {
	if err := engine.ValidateLocalPreloadKinds(m.kinds); err != nil {
		return fmt.Errorf("dataengine preload mod: %w", err)
	}
	return nil
}

func (m *LocalPreloadMod) Provide(r *app.Registry) error {
	m.registry = r
	return nil
}

// Start 同步跑完预加载才返回。
func (m *LocalPreloadMod) Start() error {
	if m == nil || m.registry == nil {
		return errors.New("dataengine preload mod: not provided")
	}
	if len(m.kinds) == 0 {
		slog.Info("dataengine preload mod: no preload kinds declared")
		return nil
	}
	dataEngine, ok := app.Lookup[*Mod](m.registry, mods.ModDataEngine)
	if !ok || dataEngine == nil || dataEngine.Runtime() == nil {
		return fmt.Errorf("dataengine preload mod: required capability %q not found or not started", mods.ModDataEngine)
	}
	report, err := dataEngine.Runtime().PreloadLocalEntities(fctx.BaseContext(), m.kinds)
	m.Report = report
	for _, kind := range report.Kinds {
		slog.Info("dataengine preload mod: preloaded a kind", "kind", kind.Kind, "order", kind.Order, "listed", kind.Listed,
			"loaded", kind.Loaded, "deleted", kind.Deleted, "missing", kind.Missing, "failed", kind.Failed)
	}
	if err != nil {
		return fmt.Errorf("dataengine preload mod: %w", err)
	}
	slog.Info("dataengine preload mod: preloaded local entities", "kinds", m.kinds, "listed", report.Listed, "loaded", report.Loaded,
		"deleted", report.Deleted, "missing", report.Missing, "failed", report.Failed)
	return nil
}

func (m *LocalPreloadMod) Stop() {}

var _ app.Mod = (*LocalPreloadMod)(nil)
