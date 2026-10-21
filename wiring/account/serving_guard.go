package account

import domain "github.com/tjbdwanghaibo/roost-core/service/account"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// ServingGuardModName 是启动自检 Mod 的名字（mods.ModAccountServingGuard）。
const ServingGuardModName = mods.ModAccountServingGuard

// ServingGuardMod 是装配了 account 的 game 进程的启动自检（别名合服，维护者 2026-10-20 决定）：本服已经并入
// 另一服（account 的服务器记录 MergedInto != 0）就拒绝启动，并点名承载服。
//
// 为什么需要它：合服后 A 的角色由 B 承载，误启动的 game(A) 收不到玩家，却会照常跑 World(A) 等按服逻辑、认领
// 共享 outbox、写 A 的按 sid 分库（那里现在是只读归档）。所以检查要早于任何会写数据的 Mod：它在 Provide 里
// 取得 Accounts，在 Start 里查询；DataEngine Mod 把它列为可选依赖（kitdataengine OptionalDependsOn），有它时
// 它先 Start，DataEngine 的 WAL 与 outbox 在它通过之后才打开。
//
// 结论只有三种：
//   - 本服记录不存在（ErrServerInvalid：还没 upsert-server，开发环境的常态）或没有合服：放行；
//   - 已并入：返回错误，进程不启动；
//   - 查不到（account 不可达、超时）：在 startupBudget 内重试，仍然查不到就拒绝启动（fail-closed：无法证明
//     本服没被并入时不写数据）。代价是 account 不可用时 game 也起不来；只影响启动，运行中的进程不再查询。
//
// 生成器把它加进 access.player 指向、且 uses: account 的业务服务（承载玩家的 game 进程）；手写装配时放在
// account 的 ClientMod（或 owner Mod）之后。
type ServingGuardMod struct {
	sid      int32
	accounts domain.Accounts
	// startupBudget 与 retryInterval 是查询的总预算与重试间隔；测试可以缩短，生产取 newServingGuard 的值。
	startupBudget time.Duration
	retryInterval time.Duration
}

// NewServingGuardMod 返回启动自检 Mod。
func NewServingGuardMod() *ServingGuardMod {
	return &ServingGuardMod{startupBudget: 30 * time.Second, retryInterval: time.Second}
}

// Name implements app.Mod.
func (m *ServingGuardMod) Name() app.ModName { return ServingGuardModName }

// DependsOn implements app.ModDependencyProvider: the account ClientMod or the
// owner Mod, whichever this process has — both are named CapabilityName.
func (m *ServingGuardMod) DependsOn() []app.ModName { return []app.ModName{domain.CapabilityName} }

type servingGuardConfig struct {
	app.ServiceIdentity
}

// ConfigSchema implements app.ModConfigSchema: only the process sid.
func (m *ServingGuardMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(servingGuardConfig{}) }

// Init reads the process sid.
func (m *ServingGuardMod) Init(cfg *viper.Viper) error {
	var settings servingGuardConfig
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("account serving guard: %w", err)
	}
	if settings.Sid <= 0 {
		return fmt.Errorf("account serving guard: sid is %d; a game process serves one server and needs its id", settings.Sid)
	}
	m.sid = settings.Sid
	return nil
}

// Provide looks up the Accounts capability the account Mod published.
func (m *ServingGuardMod) Provide(r *app.Registry) error {
	accounts, ok := app.Lookup[domain.Accounts](r, domain.CapabilityName)
	if !ok || accounts == nil {
		return fmt.Errorf("account serving guard: capability %q not found; add account.NewClientMod() (or the owner Mod) to this process", domain.CapabilityName)
	}
	m.accounts = accounts
	return nil
}

// ErrServerMerged 表示本进程的服已经并入另一服，进程拒绝启动。
var ErrServerMerged = errors.New("account serving guard: this server is merged into another one")

// Start asks the account service whether this server was merged away.
func (m *ServingGuardMod) Start() error {
	deadline := time.Now().Add(m.startupBudget)
	for attempt := 1; ; attempt++ {
		server, err := m.accounts.GetServer(context.Background(), m.sid)
		switch {
		case err == nil && server.MergedInto != 0:
			return fmt.Errorf("%w: server %d was merged into %d (%s) and its roles are served there; do not start it. "+
				"To bring it back, follow the revert in docs/framework/PLAYER-MIGRATION.md (accountctl unmerge-server -from %d)",
				ErrServerMerged, m.sid, server.MergedInto, server.MergeNote, m.sid)
		case err == nil:
			slog.Info("account serving guard: this server is not merged", "sid", m.sid)
			return nil
		case errors.Is(err, domain.ErrServerInvalid):
			slog.Info("account serving guard: this server is not registered in account yet; nothing is merged", "sid", m.sid)
			return nil
		}
		if time.Now().Add(m.retryInterval).After(deadline) {
			return fmt.Errorf("account serving guard: could not confirm with the account service that server %d is not merged "+
				"(%d attempts in %s): %w; refusing to start", m.sid, attempt, m.startupBudget, err)
		}
		slog.Warn("account serving guard: account service did not answer; retrying", "sid", m.sid, "attempt", attempt, "err", err)
		time.Sleep(m.retryInterval)
	}
}

// Stop implements app.Mod. Nothing to stop.
func (m *ServingGuardMod) Stop() {}

var _ app.Mod = (*ServingGuardMod)(nil)
