// Package relay 把 relay 转发服（service/relay）接进 App：Mod 读 relay.* 配置、拿 NATS 与 etcd、建好 relay；
// Server（app.Service）打开监听、订阅 lobby 的命令、定时心跳与 etcd 广播，停机时按相反顺序收掉。
//
// 与 lobby 的契约（service/lobby 的 events.go、relays.go；两边写同一个 subject 与 registry 前缀）：
//
//	relay → lobby  NATS  <relay.subject>.events                 房间报告（service/relay.Report，JSON）
//	lobby → relay  NATS  <relay.subject>.relay.<instance>       命令 {"type":"leave","room":..,"account":..}
//	relay → lobby  etcd  <relay.registry_prefix><instance>       {"instance","tcp_addr","ws_addr","rooms"}，挂在本进程的租约上
//	relay → lobby  NATS  <relay.subject>.verify (request)        每条连接进房前验权 {"room","account","relay","create"} → {"ok","reason"}
package relay

import domain "github.com/tjbdwanghaibo/roost-core/service/relay"

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/wsconn"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// CapabilityName 是 relay Mod 的名字，也是它登记 *service/relay.Server 的能力名。
const CapabilityName app.ModName = "relay"

// runtimeName 是 Mod 交给 Server 的装配结果（监听地址、NATS、etcd、relay 本体）。
const runtimeName app.ModName = "relay.runtime"

// ServiceName 是 relay 进程的 app.Service 名。
const ServiceName app.ServiceName = "relay"

// config 是 relay.* 的声明。限额的缺省值与 service/relay 的 Default* 相同，取值理由见 docs/framework/RELAY.md §4。
type config struct {
	app.ServiceIdentity
	Relay struct {
		Instance       string        `config:"instance" example:"relay-1" help:"本实例名：令牌里的 relay 实例、etcd 广播键与命令主题的最后一段都是它；字母、数字、_、-，1～64 个字符。不写取 <server_type>-<sid>"`
		Listen         string        `config:"listen" listen:"always" required:"true" example:":7100" help:"TCP 监听地址"`
		AdvertiseAddr  string        `config:"advertise_addr" required:"true" example:"10.0.0.5:7100" help:"客户端连 TCP 用的 host:port，经 etcd 广播给 lobby；生产环境不要写回环地址"`
		TokenKeyEnv    string        `config:"token_key_env" required:"true" example:"ROOST_RELAY_TOKEN_KEY" help:"放 relay 令牌密钥的环境变量名，与 lobby 写同一个变量（至少 16 字节）"`
		Subject        string        `config:"subject" required:"true" example:"roost.{project}.lobby" help:"与 lobby.subject 相同：报告发到 <subject>.events，lobby 的命令从 <subject>.relay.<instance> 收"`
		RegistryPrefix string        `config:"registry_prefix" required:"true" example:"/{project}/relays/" help:"与 lobby.relay_registry_prefix 相同：etcd 广播键是 <registry_prefix><instance>"`
		RegistryTTL    time.Duration `config:"registry_ttl" default:"10s" min:"2s" example:"10s" help:"etcd 广播的租约：进程没了，键在这么久内消失，lobby 不再把新房间分到这里"`

		WSListen         string   `config:"ws_listen" listen:"always" example:":7101" help:"WebSocket 监听地址（浏览器客户端）；不写就不开。每条二进制消息装一帧，令牌、转发、限额与 TCP 相同"`
		WSPath           string   `config:"ws_path" default:"/relay" example:"/relay" help:"WebSocket 升级的路径（精确匹配）"`
		WSAdvertiseAddr  string   `config:"ws_advertise_addr" example:"wss://relay1.example.com/relay" help:"客户端连 WebSocket 用的完整 ws:// 或 wss:// URL（含路径），经 etcd 广播给 lobby；开了 ws_listen 就必填。wss 由前置负载均衡终结"`
		WSAllowedOrigins []string `config:"ws_allowed_origins" example:"https://play.example.com" help:"允许的游戏页面 Origin（不带 Origin 的原生客户端总是放行）；空表示拒绝所有浏览器页面，写法见 infra/network/wsconn"`

		MaxPayloadBytes     int           `config:"max_payload_bytes" default:"65536" min:"1" example:"65536" help:"单帧载荷上限，超过断开连接"`
		FramesPerSecond     int           `config:"frames_per_second" default:"200" min:"1" example:"200" help:"每条连接每秒帧数上限（所有通道合计，突发量同为一秒的额度），超过断开连接"`
		RoomFramesPerSecond int           `config:"room_frames_per_second" default:"30" min:"1" example:"30" help:"房间通道（聊天、表情、语音）每条连接每秒帧数上限，超过的帧丢弃、连接保留"`
		RoomBytesPerSecond  int           `config:"room_bytes_per_second" default:"65536" min:"1" example:"65536" help:"房间通道每条连接每秒字节数上限，不能小于 max_payload_bytes"`
		SendQueueFrames     int           `config:"send_queue_frames" default:"1024" min:"1" example:"1024" help:"每条连接发送队列的帧数上限，超过断开这条连接"`
		SendQueueBytes      int           `config:"send_queue_bytes" default:"4194304" min:"1" example:"4194304" help:"每条连接发送队列的字节上限，至少两个满帧"`
		IdleTimeout         time.Duration `config:"idle_timeout" default:"30s" min:"1ms" example:"30s" help:"连接上这么久没有任何帧（含心跳）就断开；客户端约每 1/3 发一次 Ping"`
		AuthTimeout         time.Duration `config:"auth_timeout" default:"5s" min:"1ms" example:"5s" help:"接入后送来令牌首帧的期限"`
		WriteTimeout        time.Duration `config:"write_timeout" default:"10s" min:"1ms" example:"10s" help:"一次写 socket 的期限，写不出去断开这条连接"`
		EmptyRoomTimeout    time.Duration `config:"empty_room_timeout" default:"10m" min:"1ms" example:"10m" help:"房间里只剩房主这么久就关房"`
		ClosedRoomRetention time.Duration `config:"closed_room_retention" default:"2m" min:"1ms" example:"2m" help:"关过的房间号记住这么久，挡住还没过期的 host 令牌重建；不要短于 lobby 签的令牌有效期"`
		HeartbeatInterval   time.Duration `config:"heartbeat_interval" default:"5s" min:"1ms" example:"5s" help:"每个房间向 lobby 发心跳的间隔，要明显短于 lobby.room_ttl（推荐 room_ttl/3）"`

		HostIdleTimeout     time.Duration `config:"host_idle_timeout" default:"2s" min:"1ms" example:"2s" help:"房主连接上这么久没有任何帧就当房主断线、进入宽限期；房主约每 1/3 发一次 Ping（一直在广播时不用另发）"`
		HostGrace           time.Duration `config:"host_grace" default:"5s" min:"1ms" example:"5s" help:"房主断线后等它用同一账号重连回来的时间，期满按候选顺序换 host"`
		HostReadyTimeout    time.Duration `config:"host_ready_timeout" default:"10s" min:"1ms" example:"10s" help:"新房主候选收到 you_are_host 后回 host_ready 的期限，超时或断开换下一个，都失败关房"`
		SnapshotMinInterval time.Duration `config:"snapshot_min_interval" default:"1s" min:"1ms" example:"1s" help:"同一房主两份快照之间的最小间隔，太密的拒收并告诉房主"`
		SnapshotMaxBytes    int           `config:"snapshot_max_bytes" default:"262144" min:"1" max:"16777216" example:"262144" help:"单份快照（压缩后）与转交交接数据的字节上限，超限拒收并告诉房主；relay 快照内存约为 房间数 × 它"`
		AdmitTimeout        time.Duration `config:"admit_timeout" default:"2s" min:"1ms" example:"2s" help:"每条连接向 lobby 验权（<subject>.verify）的期限；超时或 lobby 不可用都拒绝这条连接"`
	} `config:"relay"`
}

// Mod 装配 relay。它不起 goroutine、不开监听：那些在 Server.Serve 里，停机由 Server.Shutdown 收掉。
type Mod struct {
	settings config
	domain   domain.Config
}

// NewMod 返回 relay Mod。
func NewMod() *Mod { return &Mod{} }

// Name 实现 app.Mod。
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn：报告与命令走 NATS，实例广播走 etcd。
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModNats, mods.ModEtcd} }

// ConfigSchema 声明 relay.*。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init 读配置与令牌密钥。
func (m *Mod) Init(cfg *viper.Viper) error {
	if err := app.LoadConfig(cfg, &m.settings); err != nil {
		return fmt.Errorf("relay mod: %w", err)
	}
	c := m.settings.Relay
	instance := strings.TrimSpace(c.Instance)
	if instance == "" {
		serverType := m.settings.ServerType
		if serverType == "" {
			serverType = string(ServiceName)
		}
		instance = fmt.Sprintf("%s-%d", serverType, m.settings.Sid)
	}
	if strings.ContainsAny(c.Subject, " \t\r\n*>") || strings.HasSuffix(c.Subject, ".") {
		return fmt.Errorf("relay mod: relay.subject %q must be a plain NATS subject (no spaces or wildcards)", c.Subject)
	}
	if strings.ContainsAny(c.RegistryPrefix, " \t\r\n") {
		return fmt.Errorf("relay mod: relay.registry_prefix %q contains whitespace", c.RegistryPrefix)
	}
	if c.WSListen != "" {
		if !strings.HasPrefix(c.WSAdvertiseAddr, "ws://") && !strings.HasPrefix(c.WSAdvertiseAddr, "wss://") {
			return fmt.Errorf("relay mod: relay.ws_advertise_addr %q must be a ws:// or wss:// URL when relay.ws_listen is set", c.WSAdvertiseAddr)
		}
		if err := wsconn.ValidatePath(c.WSPath); err != nil {
			return fmt.Errorf("relay mod: relay.ws_path: %w", err)
		}
		if err := wsconn.ValidateOrigins(c.WSAllowedOrigins); err != nil {
			return fmt.Errorf("relay mod: relay.ws_allowed_origins: %w", err)
		}
	}
	key, err := token.KeyFromEnv(c.TokenKeyEnv)
	if err != nil {
		return fmt.Errorf("relay mod: relay.token_key_env: %w", err)
	}
	m.domain = domain.Config{Instance: instance, TokenKey: key,
		MaxPayloadBytes: c.MaxPayloadBytes, FramesPerSecond: c.FramesPerSecond,
		RoomFramesPerSecond: c.RoomFramesPerSecond, RoomBytesPerSecond: c.RoomBytesPerSecond,
		SendQueueFrames: c.SendQueueFrames, SendQueueBytes: c.SendQueueBytes,
		IdleTimeout: c.IdleTimeout, AuthTimeout: c.AuthTimeout, WriteTimeout: c.WriteTimeout,
		EmptyRoomTimeout: c.EmptyRoomTimeout, ClosedRoomRetention: c.ClosedRoomRetention, HeartbeatInterval: c.HeartbeatInterval,
		HostIdleTimeout: c.HostIdleTimeout, HostGrace: c.HostGrace, HostReadyTimeout: c.HostReadyTimeout,
		SnapshotMinInterval: c.SnapshotMinInterval, SnapshotMaxBytes: c.SnapshotMaxBytes, AdmitTimeout: c.AdmitTimeout}
	return m.domain.WithDefaults().Validate()
}

// Provide 建 relay（报告经 NATS 发出），登记 *service/relay.Server 与给 Server 的装配结果。
func (m *Mod) Provide(r *app.Registry) error {
	raw, ok := app.Lookup[fnats.RawClient](r, mods.ModNatsRaw)
	if !ok || raw == nil {
		return fmt.Errorf("relay mod: capability %q not found", mods.ModNatsRaw)
	}
	etcdClient, ok := app.Lookup[fetcd.IEtcd](r, mods.ModEtcd)
	if !ok || etcdClient == nil {
		return fmt.Errorf("relay mod: capability %q not found", mods.ModEtcd)
	}
	c := m.settings.Relay
	server, err := domain.NewServer(m.domain, natsReporter{client: raw, subject: EventSubject(c.Subject)},
		natsAdmission{client: raw, subject: VerifySubject(c.Subject)})
	if err != nil {
		return fmt.Errorf("relay mod: %w", err)
	}
	advert := Advertisement{Instance: server.Config().Instance, TCPAddr: c.AdvertiseAddr}
	rt := &runtime{server: server, nats: raw, listen: c.Listen, commandSubject: CommandSubject(c.Subject, server.Config().Instance)}
	if c.WSListen != "" {
		advert.WSAddr = c.WSAdvertiseAddr
		rt.wsListen = c.WSListen
		// 一条消息装一帧：消息上限取客户端可能发来的最大一帧（快照帧可以比普通帧大，Config.MaxFrameBytes）；
		// 升级请求的期限沿用令牌首帧的期限。
		rt.ws = wsconn.Config{Path: c.WSPath, AllowedOrigins: c.WSAllowedOrigins,
			MaxMessageBytes: int64(server.Config().MaxFrameBytes()), HandshakeTimeout: server.Config().AuthTimeout}
	}
	rt.advertiser = newAdvertiser(etcdClient, c.RegistryPrefix, c.RegistryTTL, advert, server)
	return mods.RegisterAll(r, mods.Capability{Name: CapabilityName, Value: server}, mods.Capability{Name: runtimeName, Value: rt})
}

// Start 实现 app.Mod：没有要启动的。
func (m *Mod) Start() error { return nil }

// Stop 实现 app.Mod：没有要停的（连接与房间由 Server.Shutdown 收掉）。
func (m *Mod) Stop() {}

// EventSubject 是 relay 报告房间事件的主题（与 service/lobby.EventSubject 相同）。
func EventSubject(subject string) string { return subject + ".events" }

// VerifySubject 是 relay 向 lobby 验权的请求主题（与 service/lobby.VerifySubject 相同）。
func VerifySubject(subject string) string { return subject + ".verify" }

// CommandSubject 是发给某个 relay 实例的命令主题（与 service/lobby.RelayCommandSubject 相同）。
func CommandSubject(subject, instance string) string { return subject + ".relay." + instance }

var _ app.Mod = (*Mod)(nil)
