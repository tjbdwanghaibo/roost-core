// Package lobby 把大厅服（service/lobby）接进 App：Mod 读配置、装配 Redis 房间目录 / etcd relay 发现 / account
// 的游戏账号 token 校验 / NATS 命令通道，登记能力；Server（app.Service）负责监听 HTTP 与订阅 relay 的房间事件。
//
// 进程里的装配（framework: lobby 托管服务）：
//
//	Mods：redis、nats、etcd、account 的 ClientMod（account.NewClientMod()）、lobby.NewMod(nil)
//	Service：lobby.NewServer()
//
// Mod 不起 goroutine；监听与订阅都在 Server.Serve 里，停机走 Server.Shutdown（先退订、再停 HTTP 并等在途请求）。
package lobby

import domain "github.com/tjbdwanghaibo/roost-core/service/lobby"

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/service/account"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// CapabilityName 是 Mod 登记 *Lobby 的名字，Server 按它取。
const CapabilityName app.ModName = "service.lobby"

// Lobby 是 Mod 装配好的大厅：服务本体与 Server 要用的监听设置。
type Lobby struct {
	Service     *domain.Service
	Listen      string
	CORSOrigins []string
	Subject     string
}

// Mod 装配大厅服。
type Mod struct {
	tokens domain.GameTokenValidator

	cfg   config
	key   []byte
	lobby *Lobby
}

// NewMod 返回大厅 Mod。tokens 为 nil 时用本进程的 account.Accounts（account.NewClientMod() 登记的远端能力）
// 校验游戏账号 token；测试或自带账号体系的工程可以传自己的实现。
func NewMod(tokens domain.GameTokenValidator) *Mod { return &Mod{tokens: tokens} }

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName {
	deps := []app.ModName{mods.ModRedis, mods.ModNats, mods.ModEtcd}
	if m.tokens == nil {
		deps = append(deps, account.CapabilityName)
	}
	return deps
}

// config 是 lobby.* 的声明。
type config struct {
	Lobby struct {
		Listen              string                `config:"listen" listen:"always" default:"127.0.0.1:8090" example:"0.0.0.0:8090" help:"大厅 HTTP 的监听地址（对客户端开放；接口自带鉴权）"`
		KeyPrefix           string                `config:"key_prefix" required:"true" example:"roost:{project}:lobby" help:"Redis 键前缀：必填、没有缺省（共用一个 Redis 的两套部署会静默共享房间目录）；Redis Cluster 上要带 hash tag"`
		Games               map[string]gameConfig `config:"games" help:"本大厅服务的游戏：games.<游戏名>.max_members（房间人数上限，含房主）。不在这里的游戏一律拒绝，至少写一个"`
		RoomTTL             time.Duration         `config:"room_ttl" default:"30s" min:"1s" max:"10m" example:"30s" help:"房间记录的寿命，relay 的每条报告续一次；relay 的心跳间隔要明显短于它（推荐 room_ttl/3）"`
		TokenTTL            time.Duration         `config:"token_ttl" default:"30s" min:"1s" max:"10m" help:"relay 令牌的寿命：只用于建立连接，断线重连重新 join 拿新的"`
		TokenKeyEnv         string                `config:"token_key_env" required:"true" example:"ROOST_RELAY_TOKEN_KEY" help:"relay 令牌 HMAC 密钥所在的环境变量名，与 relay.token_key_env 指向同一个变量；值至少 16 字节"`
		RelayRegistryPrefix string                `config:"relay_registry_prefix" required:"true" example:"/{project}/relays/" help:"relay 实例在 etcd 里广播的键前缀（<前缀><实例名>），与 relay 侧同一个值"`
		Subject             string                `config:"subject" required:"true" example:"roost.{project}.lobby" help:"与 relay 之间的 NATS 主题前缀：<subject>.events 收房间事件，<subject>.relay.<实例> 发命令；与 relay 侧同一个值"`
		CORSOrigins         []string              `config:"cors_origins" example:"[]" help:"允许跨域调用大厅的游戏页面 Origin（例如 https://play.example.com）；\"*\" 表示任何 Origin"`
	} `config:"lobby"`
}

// gameConfig 是 lobby.games.<游戏名>。
type gameConfig struct {
	// 必填：一个游戏的条目里至少要有一个键，配置加载器才看得见它。
	MaxMembers int `config:"max_members" required:"true" min:"1" max:"1024" help:"这个游戏的房间人数上限（含房主）"`
}

// ConfigSchema 声明 lobby.*。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init 读 lobby.* 并读出 relay 令牌密钥（环境变量必须已设置，至少 16 字节）。
//
//	lobby:
//	  listen: 0.0.0.0:8090
//	  key_prefix: roost:{party}:lobby
//	  games: {party: {max_members: 8}}
//	  token_key_env: ROOST_RELAY_TOKEN_KEY
//	  relay_registry_prefix: /party/relays/
//	  subject: roost.party.lobby
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("lobby mod: %w", err)
	}
	c := settings.Lobby
	if err := mods.CheckKeyPrefix("lobby", c.KeyPrefix); err != nil {
		return err
	}
	if len(c.Games) == 0 {
		return errors.New("lobby mod: config: lobby.games lists no game; a lobby that serves no game refuses every request")
	}
	key, err := token.KeyFromEnv(c.TokenKeyEnv)
	if err != nil {
		return fmt.Errorf("lobby mod: lobby.token_key_env: %w", err)
	}
	m.cfg, m.key = settings, key
	return nil
}

// Provide 装配服务并登记 *Lobby。
func (m *Mod) Provide(r *app.Registry) error {
	c := m.cfg.Lobby
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	rooms, err := domain.NewRedisRooms(client, c.KeyPrefix, c.RoomTTL)
	if err != nil {
		return fmt.Errorf("lobby mod: %w", err)
	}
	etcdClient, ok := app.Lookup[fetcd.IEtcd](r, mods.ModEtcd)
	if !ok || etcdClient == nil {
		return fmt.Errorf("lobby mod: capability %q not found; the lobby discovers relay instances in etcd", mods.ModEtcd)
	}
	natsClient, ok := app.Lookup[fnats.IClient](r, mods.ModNats)
	if !ok || natsClient == nil {
		return fmt.Errorf("lobby mod: capability %q not found; the lobby talks to relays over NATS", mods.ModNats)
	}
	tokens := m.tokens
	if tokens == nil {
		accounts, ok := app.Lookup[account.Accounts](r, account.CapabilityName)
		if !ok || accounts == nil {
			return fmt.Errorf("lobby mod: capability %q not found; add account.NewClientMod() so game tokens can be validated", account.CapabilityName)
		}
		tokens = AccountTokens{Accounts: accounts}
	}
	games := make(map[string]domain.GameRule, len(c.Games))
	for name, game := range c.Games {
		games[name] = domain.GameRule{MaxMembers: game.MaxMembers}
	}
	service, err := domain.New(domain.Config{
		Games: games, Rooms: rooms, Tokens: tokens,
		Relays:   domain.EtcdRelays{Client: etcdClient, Prefix: c.RelayRegistryPrefix},
		Commands: natsClient, Subject: c.Subject,
		RelayTokenKey: m.key, RelayTokenTTL: c.TokenTTL,
		// 令牌与席位预留都是凭证寿命，按系统时间，不跟业务时间偏移走。
		Now: time.Now,
	})
	if err != nil {
		return fmt.Errorf("lobby mod: %w", err)
	}
	m.lobby = &Lobby{Service: service, Listen: c.Listen, CORSOrigins: c.CORSOrigins, Subject: c.Subject}
	return r.Register(CapabilityName, m.lobby)
}

// Start implements app.Mod. 监听与订阅在 Server.Serve。
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod.
func (m *Mod) Stop() {}

// AccountTokens 用 account 的 ValidateGameToken 校验游戏账号 token，再核对 token 的游戏就是请求的游戏。
// token 无效、游戏不符、游戏已不在 account.games、账号不在或被封，都答 lobby.ErrUnauthorized；其余错误（account 不可达）
// 原样返回，大厅按服务不可用答。
type AccountTokens struct {
	Accounts interface {
		ValidateGameToken(ctx context.Context, gameToken string) (account.GameAccount, error)
	}
}

// ValidateGameToken implements lobby.GameTokenValidator.
func (a AccountTokens) ValidateGameToken(ctx context.Context, gameToken, game string) (string, error) {
	got, err := a.Accounts.ValidateGameToken(ctx, gameToken)
	if err == nil {
		err = got.RequireGame(game)
	}
	if err != nil {
		switch account.Code(err) {
		case account.CodeSessionInvalid, account.CodeIdentityDenied, account.CodeAccountMissing, account.CodeGameInvalid:
			return "", fmt.Errorf("%w: %w", domain.ErrUnauthorized, err)
		}
		return "", err
	}
	return got.AccountID, nil
}

var (
	_ app.Mod                   = (*Mod)(nil)
	_ domain.GameTokenValidator = AccountTokens{}
)
