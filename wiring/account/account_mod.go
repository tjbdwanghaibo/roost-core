package account

import domain "github.com/tjbdwanghaibo/roost-core/service/account"

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// Mod wires an account Service into an app and registers it as a capability.
//
// Three collaborators are required constructor arguments with no defaults,
// each answering a confirmed defect rather than a style preference:
//
//   - Verifier checks an identity with its channel. The implementation this
//     replaces signed a session token for whatever {channel, open_id} arrived,
//     which is complete account takeover — and the missing check looked like a
//     default.
//   - Allocator mints player ids. Its old default was a per-process counter
//     starting at 1, which combined with an upsert meant a restart destroyed
//     existing players.
//   - NameRules validates a display name. There is no universal answer, and a
//     permissive default is an unchecked field.
type Mod struct {
	verifier  domain.IdentityVerifier
	allocator domain.PlayerIDAllocator
	nameRules domain.NameValidator
	metrics   servicemetrics.Reporter

	prefix        string
	sessionSecret string
	sessionTTL    time.Duration
	ticketTTL     time.Duration
	claimTTL      time.Duration
	games         []domain.GameSpec
	services      []domain.ServiceSpec
	service       *domain.Service
}

// NewMod returns an account Mod. All three collaborators are required.
func NewMod(
	verifier domain.IdentityVerifier,
	allocator domain.PlayerIDAllocator,
	nameRules domain.NameValidator,
	reporter servicemetrics.Reporter,
) *Mod {
	return &Mod{verifier: verifier, allocator: allocator, nameRules: nameRules, metrics: reporter}
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return domain.CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// keyPrefixConfig 是 account.key_prefix 的声明：Mod 与 KeyPrefix 共用。
type keyPrefixConfig struct {
	KeyPrefix string `config:"key_prefix" required:"true" example:"roost:{project}:account" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
}

// KeyPrefix 按 Mod 的声明读 account.key_prefix：同一个 Redis 前缀下还有业务自己的键（game-demo 的发货队列、玩家号计数器）的
// 代码用它，与 Mod 读到的是同一个值、同一套检查。
func KeyPrefix(cfg *viper.Viper) (string, error) {
	var settings struct {
		Account keyPrefixConfig `config:"account"`
	}
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", fmt.Errorf("account: %w", err)
	}
	if err := mods.CheckKeyPrefix("account", settings.Account.KeyPrefix); err != nil {
		return "", err
	}
	return settings.Account.KeyPrefix, nil
}

// config 是 account.* 的声明（维护者决定）。
type config struct {
	mods.ServiceMetricsConfig
	// redis.cluster_addrs：角色记录与在途换服的索引同一个脚本写（D8），Redis Cluster 上 key_prefix 要带 hash tag。
	kitredis.ClusterConfig
	Account struct {
		keyPrefixConfig
		SessionSecret string        `config:"session_secret" required:"true" secret:"true" example:"CHANGE_ME"`
		SessionTTL    time.Duration `config:"session_ttl" default:"30m" min:"1ns" example:"30m"`
		// 不带 example：缺省值够用，不写进生成的配置文件；要改时在 account: 段里加 ticket_ttl。
		TicketTTL time.Duration `config:"ticket_ttl" default:"10m" min:"10s" max:"24h" help:"账号票据的有效期（登录之后多久之内可以拿区服清单、要进服票据）；进服票据与角色会话令牌用 session_ttl"`
		ClaimTTL  time.Duration `config:"claim_ttl" default:"30s" min:"1ns" example:"5m"`
		// games 与 services 不带 example：没有它们的工程（只有 game 区服）行为不变，生成的配置文件里也不写。
		Games    map[string]gameConfig    `config:"games" help:"游戏账号 token 服务的游戏：games.<游戏名>.token_ttl（必填，至多 24h）。不在这里的游戏不签发 token"`
		Services map[string]serviceConfig `config:"services" help:"接受本账号体系的服务：services.<服务名>.kind（game | lobby | relay）。不写时按单一 game 服务处理"`
	} `config:"account"`
}

// gameConfig 是 account.games.<游戏名>。
type gameConfig struct {
	// 必填：一个游戏的条目里至少要有一个键，配置加载器才看得见它（空的 `party: {}` 读不出来）。
	TokenTTL time.Duration `config:"token_ttl" required:"true" min:"1s" max:"24h" help:"这个游戏的游戏账号 token 寿命（例如 2h，至多 24h）"`
}

// serviceConfig 是 account.services.<服务名>。
type serviceConfig struct {
	Kind string `config:"kind" required:"true" enum:"game|lobby|relay" help:"服务类型：game（传统区服）、lobby（大厅）、relay（转发）"`
}

// gameSpecs 与 serviceSpecs 把配置的 map 交给领域层；名字的写法、寿命的范围在 domain.New 里检查一次。
func gameSpecs(games map[string]gameConfig) []domain.GameSpec {
	specs := make([]domain.GameSpec, 0, len(games))
	for name, game := range games {
		specs = append(specs, domain.GameSpec{Name: name, TokenTTL: game.TokenTTL})
	}
	return specs
}

func serviceSpecs(services map[string]serviceConfig) []domain.ServiceSpec {
	specs := make([]domain.ServiceSpec, 0, len(services))
	for name, service := range services {
		specs = append(specs, domain.ServiceSpec{Name: name, Kind: domain.ServiceKind(service.Kind)})
	}
	return specs
}

// ConfigSchema 声明 account.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	account:
//	  key_prefix: roost:account   # required, no default
//	  session_secret: "..."       # required, non-empty
//	  session_ttl: 30m            # optional
//	  ticket_ttl: 10m             # optional：账号票据的有效期
//	  claim_ttl: 30s              # optional
//	  games:                      # optional：游戏账号 token 服务的游戏（game_token.go）
//	    party: {token_ttl: 2h}
//	  services:                   # optional：接受本账号体系的服务（services.go）
//	    lobby: {kind: lobby}
//	    relay: {kind: relay}
//	    slg:   {kind: game}
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Account
	if err := mods.CheckKeyPrefix("account", c.KeyPrefix); err != nil {
		return err
	}
	if err := mods.ValidateClusterKeyPrefix(settings.ClusterAddrs, "account", c.KeyPrefix); err != nil {
		return err
	}
	missing := []string{}
	if m.verifier == nil {
		missing = append(missing, "identity verifier")
	}
	if m.allocator == nil {
		missing = append(missing, "player id allocator")
	}
	if m.nameRules == nil {
		missing = append(missing, "name validator")
	}
	if len(missing) > 0 {
		return fmt.Errorf("account mod: %v are required and have no defaults; each of them "+
			"was absent in the implementation this replaces, and the absence looked like a default", missing)
	}
	m.prefix, m.sessionSecret, m.sessionTTL, m.ticketTTL, m.claimTTL = c.KeyPrefix, c.SessionSecret, c.SessionTTL, c.TicketTTL, c.ClaimTTL
	m.games, m.services = gameSpecs(c.Games), serviceSpecs(c.Services)
	if err := domain.CheckGamesAndServices(m.games, m.services); err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	// Collaborators bind first: the ones that implement RegistryBound are
	// exactly the ones that go on to look capabilities up, and their error
	// names the collaborator, which the Redis lookup below cannot.
	if err := bindCollaborators(r, m.verifier, m.allocator, m.nameRules); err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	stores, err := domain.NewRedisStores(client, m.prefix, m.claimTTL)
	if err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	service, err := domain.New(domain.Config{
		Accounts: stores.Accounts, Roles: stores.Roles, Servers: stores.Servers,
		Slots: stores.Slots, Names: stores.Names, Presence: stores.Presence,
		Verifier: m.verifier, Allocator: m.allocator, NameRules: m.nameRules,
		SessionSecret: m.sessionSecret, SessionTTL: m.sessionTTL, AccountTicketTTL: m.ticketTTL, ClaimTTL: m.claimTTL,
		Games: m.games, Services: m.services,
		Metrics: m.metrics,
		// Creation and login times are business time; session tokens and
		// operator stamps are system time.
		Now: app.BusinessClock(r).Now, SystemNow: time.Now,
	})
	if err != nil {
		return fmt.Errorf("account mod: %w", err)
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// bindCollaborators hands the registry to every collaborator that asked for
// one (RegistryBound). The names match the ones Init's "required" error uses,
// so an operator reading logs sees one vocabulary.
func bindCollaborators(r *app.Registry, verifier domain.IdentityVerifier, allocator domain.PlayerIDAllocator, rules domain.NameValidator) error {
	collaborators := []struct {
		name  string
		value any
	}{
		{"identity verifier", verifier},
		{"player id allocator", allocator},
		{"name validator", rules},
	}
	for _, collaborator := range collaborators {
		bound, ok := collaborator.value.(RegistryBound)
		if !ok {
			continue
		}
		if err := bound.BindRegistry(r); err != nil {
			return fmt.Errorf("bind %s: %w", collaborator.name, err)
		}
	}
	return nil
}

// Start implements app.Mod. Nothing to start.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
