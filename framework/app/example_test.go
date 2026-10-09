package app_test

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coremanager "github.com/tjbdwanghaibo/roost-core/framework/manager"

	"github.com/spf13/viper"
)

// --- Example Mods ---

type MongoMod struct{}

func (m *MongoMod) Name() app.ModName           { return "mongo" }
func (m *MongoMod) Init(cfg *viper.Viper) error { return nil }
func (m *MongoMod) Provide(r *app.Registry) error {
	return r.Register("mongo", "mongo_client_placeholder")
}
func (m *MongoMod) Start() error { return nil }
func (m *MongoMod) Stop()        {}

type RedisMod struct{}

func (m *RedisMod) Name() app.ModName           { return "redis" }
func (m *RedisMod) Init(cfg *viper.Viper) error { return nil }
func (m *RedisMod) Provide(r *app.Registry) error {
	return r.Register("redis", "redis_client_placeholder")
}
func (m *RedisMod) Start() error { return nil }
func (m *RedisMod) Stop()        {}

// --- Example Services ---

type GameService struct{}

func (g *GameService) Name() app.ServiceName              { return "game" }
func (g *GameService) Init(r *app.Registry) error         { return nil }
func (g *GameService) Serve(ctx context.Context) error    { <-ctx.Done(); return nil }
func (g *GameService) Shutdown(ctx context.Context) error { return nil }

type GateService struct{}

func (g *GateService) Name() app.ServiceName              { return "gate" }
func (g *GateService) Init(r *app.Registry) error         { return nil }
func (g *GateService) Serve(ctx context.Context) error    { <-ctx.Done(); return nil }
func (g *GateService) Shutdown(ctx context.Context) error { return nil }

// --- Example Manager ---

type DataLoaderMgr struct{}

func (m *DataLoaderMgr) Name() string                              { return "data_loader" }
func (m *DataLoaderMgr) Start(r *app.Registry) error               { return nil }
func (m *DataLoaderMgr) Stop()                                     {}
func (m *DataLoaderMgr) StopWithContext(ctx context.Context) error { return nil }

// ManagerMod 演示与 kit/manager 相同的生命周期，实际应用可直接装配 kit Mod。
// Provide 只交接能力，启动与排空分别由 Start / StopWithContext 负责。
type ManagerMod struct{ engine *coremanager.Engine }

func (m *ManagerMod) Name() app.ModName                         { return "manager" }
func (m *ManagerMod) Init(*viper.Viper) error                   { return nil }
func (m *ManagerMod) Provide(r *app.Registry) error             { return m.engine.Provide(r) }
func (m *ManagerMod) Start() error                              { return m.engine.Start() }
func (m *ManagerMod) Stop()                                     { _ = m.StopWithContext(context.Background()) }
func (m *ManagerMod) StopWithContext(ctx context.Context) error { return m.engine.Stop(ctx) }

func Example() {
	// Game-specific managers
	// 正式 Mod 在 Provide 发布能力、Start 启动 manager，StopWithContext 受 App 预算约束。
	gameMgrs := &ManagerMod{engine: coremanager.NewEngine(&DataLoaderMgr{})}

	a := app.New("roost", "1.0.0").
		Mods(&MongoMod{}, &RedisMod{}).                   // shared mods
		RegisterServer("game", &GameService{}, gameMgrs). // game with its managers
		RegisterServer("gate", &GateService{})            // gate without managers

	// In real usage:
	//   a.Execute()
	// CLI:
	//   ./roost game -c configs/service/config.game.yaml --sid 2001
	//   ./roost gate -c configs/service/config.gate.yaml --sid 1001

	_ = a
	fmt.Println("app created with shared mods: mongo, redis; game mods: manager; services: game, gate")
	// Output: app created with shared mods: mongo, redis; game mods: manager; services: game, gate
}
