package roost

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/internal/configfile"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"gopkg.in/yaml.v3"
)

// 独立 Gate（roost add gate，维护者 2026-11-16 定的命令形态）。
//
// roost.yaml 的表示：access.player.gate 写 Gate 服务的名字；那个服务本身照常声明在 services 下（mods: [etcd, nats]，
// uses: [account]），所以它的配置文件、端口块、部署模板、deploy/dev/run.sh 都走普通业务服务的同一套生成，
// internal/service/<gate>/service.go 是只等停机的业务服务。生成器按 access.player.gate 另外做三件事：
//
//   - Gate 服务：bootstrap 给它装不自动登记的 etcd（Gate 自己登记候选，一个 Discovery 只有一个登记拥有者）与生成的
//     accessplayertcp.NewGateMod()；配置写 player_access.gate（namespace / addr / advertise_addr），单实例锁打开
//     （wiring/gate 要求 App singleton）。
//   - 玩家服务：bootstrap 的 New() 改走 Game Ingress（gateIngressOptions：认证器取工程的 NewAuthenticator、登记发现），
//     生成的 IngressMod 从 player_access.gate 读 namespace 与 advertise_addr；嵌入 TCP（player_access.tcp.enabled）必须关。
//   - 部署模板：客户端端口 7000 跟到 Gate 服务上（k8s 的 player-tcp 端口、docker 示例）。
//
// Gate 的 TCP 资源上限与内部接入限额取 gatewiring.DefaultOptions()（与 scripts/testdata/demo-drill 的演练接法相同），
// 配置只开放地址与命名空间；要调这些上限需要手工接线（docs/framework/GATEWAY.md §2）。

// playerGateDeclaration 是生成的 Gate Mod（accessplayertcp.NewGateMod）读的键。
type playerGateDeclaration struct {
	Gate struct {
		Namespace     string `config:"namespace" example:"{project}.access" help:"独立 Gate 与 Game Ingress 之间内部主题的命名空间（roost add gate）：Gate 服务与玩家服务两端必须相同（roost project doctor 检查）；不写取 wiring/gate 缺省 roost.access"`
		Addr          string `config:"addr" listen:"always" default:"0.0.0.0:7000" example:"0.0.0.0:7000" help:"Gate 的客户端监听地址（玩家连的 TCP 端口；玩家服务自己不再监听）"`
		AdvertiseAddr string `config:"advertise_addr" example:"127.0.0.1:7000" help:"客户端连 Gate 的地址 host:port。Gate 把它登记进发现，玩家服务的 Game Ingress 把它写进自己的发现候选；登录入口给客户端的区服地址（login.presence.addr、portal.servers）应与它相同（roost project doctor 检查开发配置）"`
	} `config:"player_access.gate"`
}

// playerGateIngressDeclaration 是 access.player.gate 声明时生成的 IngressMod 额外读的键（与上面同名的键逐字相同）。
type playerGateIngressDeclaration struct {
	Gate struct {
		Namespace     string `config:"namespace" example:"{project}.access" help:"独立 Gate 与 Game Ingress 之间内部主题的命名空间（roost add gate）：Gate 服务与玩家服务两端必须相同（roost project doctor 检查）；不写取 wiring/gate 缺省 roost.access"`
		AdvertiseAddr string `config:"advertise_addr" example:"127.0.0.1:7000" help:"客户端连 Gate 的地址 host:port。Gate 把它登记进发现，玩家服务的 Game Ingress 把它写进自己的发现候选；登录入口给客户端的区服地址（login.presence.addr、portal.servers）应与它相同（roost project doctor 检查开发配置）"`
	} `config:"player_access.gate"`
}

func playerGateSchema() configschema.Schema { return configschema.MustOf(playerGateDeclaration{}) }
func playerGateIngressSchema() configschema.Schema {
	return configschema.MustOf(playerGateIngressDeclaration{})
}

// gatewiringDefaultNamespace 是 gateway.DefaultConfig().Namespace 的副本（生成器不 import 运行时）：键不写时两端都取它。
const gatewiringDefaultNamespace = "roost.access"

// playerGateService 是 access.player.gate 声明的 Gate 服务名；没有声明时是空串。
func playerGateService(m Manifest) string {
	if access, ok := m.Access["player"]; ok {
		return strings.TrimSpace(access.Gate)
	}
	return ""
}

func isPlayerGateService(m Manifest, service string) bool {
	gate := playerGateService(m)
	return gate != "" && gate == service
}

// servicePlayerListener 报告 service 的进程是否监听玩家 TCP（部署模板发布 7000 的那个）：声明了 Gate 时是 Gate 服务，
// 否则是带 tcp 传输的玩家服务。
func servicePlayerListener(m Manifest, service string) bool {
	if gate := playerGateService(m); gate != "" {
		return gate == service
	}
	return serviceOwnsPlayerTCP(m, service)
}

// accountService 是托管 account 的服务名（没有时空串）。
func accountService(m Manifest) string {
	for _, name := range sortedServiceNames(m) {
		if strings.TrimSpace(m.Services[name].Framework) == "account" {
			return name
		}
	}
	return ""
}

// validatePlayerGate 是 access.player.gate 的结构规则：Gate 服务与玩家服务各自要有的 Mod 与 account。
func (m Manifest) validatePlayerGate() error {
	var joined error
	for name, access := range m.Access {
		gate := strings.TrimSpace(access.Gate)
		if gate == "" {
			continue
		}
		if name != "player" {
			joined = errors.Join(joined, fmt.Errorf("access.%s.gate is only supported on access.player", name))
			continue
		}
		spec, exists := m.Services[gate]
		switch {
		case !exists:
			joined = errors.Join(joined, fmt.Errorf("access.player.gate references unknown service %q", gate))
			continue
		case gate == access.Service:
			joined = errors.Join(joined, fmt.Errorf("access.player.gate %q is the player service itself; the Gate is a separate process (roost add gate)", gate))
			continue
		case m.isFrameworkService(gate):
			joined = errors.Join(joined, fmt.Errorf("access.player.gate %q hosts the framework service %s; the Gate is its own business service", gate, spec.Framework))
			continue
		}
		if !contains(access.Transports, "tcp") {
			joined = errors.Join(joined, errors.New("access.player.gate needs the tcp transport (the Gate speaks the player TCP protocol); run: roost add transport tcp"))
		}
		account := accountService(m)
		if account == "" {
			joined = errors.Join(joined, errors.New("access.player.gate needs a hosted account service: the Gate validates handshake tickets through account (roost module add login hosts it)"))
		}
		gateMods, _ := resolveMods(effectiveServiceMods(m, gate))
		for _, required := range []string{"etcd", "nats"} {
			if !contains(gateMods, required) {
				joined = errors.Join(joined, fmt.Errorf("access.player.gate service %q needs the %s mod (services.%s.mods)", gate, required, gate))
			}
		}
		for _, forbidden := range []string{"nest", "dataengine"} {
			if contains(gateMods, forbidden) {
				joined = errors.Join(joined, fmt.Errorf("access.player.gate service %q must not run %s: the Gate forwards connections and holds no entity state", gate, forbidden))
			}
		}
		if account != "" && !contains(spec.Uses, account) {
			joined = errors.Join(joined, fmt.Errorf("access.player.gate service %q must list %s under uses (the Gate validates tickets through it)", gate, account))
		}
		playerMods, _ := resolveMods(effectiveServiceMods(m, access.Service))
		if !contains(playerMods, "etcd") {
			joined = errors.Join(joined, fmt.Errorf("access.player service %q needs the etcd mod behind a Gate (its Game Ingress registers the candidate the Gate resolves)", access.Service))
		}
		if account != "" && !contains(m.Services[access.Service].Uses, account) {
			joined = errors.Join(joined, fmt.Errorf("access.player service %q must list %s under uses behind a Gate (Game Ingress validates every binding through it)", access.Service, account))
		}
	}
	return joined
}

// playerGateConfigSection 是 Gate 服务配置里的 player_access.gate 段（开发值；生产示例由 productionizeConfig 统一改写）。
func playerGateConfigSection(m Manifest) string {
	return playerGateSchema().StarterYAML(map[string]string{"project": m.Project.Name})
}

// playerAuthenticatorFactoryDir 是认证器所在的包目录：其中声明了 func NewAuthenticator() 时 Game Ingress 用它。
const playerAuthenticatorFactoryDir = "internal/access/player/tcp"

// hasPlayerAuthenticatorFactory 报告 root 的玩家 TCP 包（非测试文件）里是否有包级 func NewAuthenticator()（无参数）：
// 登录模块写的 auth.go 有它，生成的“一律拒绝”的桩没有。
func hasPlayerAuthenticatorFactory(root string) bool {
	paths, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(playerAuthenticatorFactoryDir), "*.go"))
	if err != nil {
		return false
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == "NewAuthenticator" && fn.Type.Params.NumFields() == 0 && fn.Type.Results.NumFields() == 1 {
				return true
			}
		}
	}
	return false
}

// renderGateIngressNew 是声明了 Gate 时 bootstrap 里的 New()：玩家服务走 Game Ingress。
func renderGateIngressNew(m Manifest) string {
	gate := playerGateService(m)
	var b strings.Builder
	fmt.Fprintf(&b, "// New 按 roost.yaml 起进程。access.player.gate 声明了独立 Gate（服务 %s，roost add gate）：玩家连 Gate，\n", gate)
	b.WriteString("// 本服的玩家接入是 Game Ingress（gateIngressOptions），不开嵌入 TCP（player_access.tcp.enabled 必须是 false）。\n")
	b.WriteString("func New() (*app.App, error) { return NewWithOptions(Options{GameIngress: gateIngressOptions()}) }\n\n")
	b.WriteString("// gateIngressOptions 是 Game Ingress 的接线：命名空间与登记地址在 Init 时从 player_access.gate 读（生成的 IngressMod），\n")
	b.WriteString("// 发现候选由 Ingress 自己登记（etcd Mod 因此不自动登记，见 playerDiscovery）。Ready 恒真：App 的失锁 fail-stop\n")
	b.WriteString("// 由 wiring/gate 合进准入判断，本服没有另外的业务准入条件。\n")
	b.WriteString("func gateIngressOptions() *gatewiring.Options {\n options := gatewiring.DefaultOptions()\n")
	if m.playerAuthenticatorFactory {
		b.WriteString(" // 认证器取工程的 internal/access/player/tcp.NewAuthenticator（登录模块写的 auth.go），与嵌入 TCP 同一套规则。\n")
		b.WriteString(" options.Authenticator = accessplayertcp.NewAuthenticator()\n")
	} else {
		b.WriteString(" // 工程的 internal/access/player/tcp 没有 NewAuthenticator：不设认证器，Game Ingress 用 account 校验票据（wiring/gate.AccountAuthenticator）。\n")
	}
	b.WriteString(" options.RegisterDiscovery = true\n options.Ready = func() bool { return true }\n return &options\n}\n")
	return b.String()
}

// gateEtcdConstructor 是 Gate 服务的 etcd Mod：Gate 自己登记候选（GateMod 的 RegisterDiscovery）。
const gateEtcdConstructor = "kitetcd.NewEtcdMod(kitetcd.WithoutServiceRegistration())"

// gateModConstructor 是 Gate 服务注册的生成 Mod（server_gen.go 的 NewGateMod）。
const gateModConstructor = "accessplayertcp.NewGateMod()"

// playerTCPGateEdits 是声明了 Gate 时对 server_gen.go 的增量（在 self_sync 的增量之后套用；anchor 在两种文件里都恰好一次）：
// IngressMod 读 player_access.gate，文件末尾加上 Gate Mod。
var playerTCPGateEdits = []struct{ anchor, replace string }{
	{
		anchor: " if parsed.Enabled { return errors.New(\"game ingress: embedded TCP listener must be disabled\") }\n",
		replace: " if parsed.Enabled { return errors.New(\"game ingress: embedded TCP listener must be disabled\") }\n" +
			" // access.player.gate（roost add gate）：命名空间与登记地址取配置，两端的命名空间必须相同。\n" +
			" var gate ingressGateConfig\n" +
			" if err:=app.LoadConfig(cfg,&gate);err!=nil{return fmt.Errorf(\"game ingress: %w\",err)}\n" +
			" if gate.Gate.Namespace!=\"\" { mod.options.Config.Namespace=gate.Gate.Namespace }\n" +
			" if gate.Gate.AdvertiseAddr!=\"\" { mod.options.AdvertiseAddr=gate.Gate.AdvertiseAddr }\n",
	},
	{
		anchor: "func (*IngressMod) ConfigSchema()app.ConfigSchema{return app.SchemaOf(tcpConfig{})}\n",
		replace: "func (*IngressMod) ConfigSchema()app.ConfigSchema{\n" +
			" schema:=app.SchemaOf(tcpConfig{})\n" +
			" schema.Keys=append(schema.Keys,app.SchemaOf(ingressGateConfig{}).Keys...)\n" +
			" return schema\n" +
			"}\n",
	},
	{
		anchor:  "var _ app.Mod = (*IngressMod)(nil)\n",
		replace: "var _ app.Mod = (*IngressMod)(nil)\n\n@@GATE_MOD@@",
	},
}

// renderPlayerTCPGateMod 是 server_gen.go 里的 Gate Mod 与两份配置声明（由生成器的声明渲染）。
func renderPlayerTCPGateMod() string {
	var b strings.Builder
	b.WriteString("// ingressGateConfig 是 access.player.gate 声明时 IngressMod 额外读的键（生成器的声明渲染而来）。\n")
	b.WriteString("type ingressGateConfig ")
	writeGoStructType(&b, reflect.TypeOf(playerGateIngressDeclaration{}), "")
	b.WriteString("\n\n// gateConfig 是独立 Gate（NewGateMod）读的键（生成器的声明渲染而来）。\n")
	b.WriteString("type gateConfig ")
	writeGoStructType(&b, reflect.TypeOf(playerGateDeclaration{}), "")
	b.WriteString(`

// NewGateMod 是 roost.yaml 的 access.player.gate 声明的独立 Gate 进程（roost add gate）的玩家接入：在
// player_access.gate.addr 上接玩家 TCP，握手票据交给 account 校验（wiring/gate 缺省的 AccountAuthenticator：
// enter:<进服票据> 与 session:<id>:<token>），按 etcd 里 Game Ingress 登记的候选把连接转给角色的承载服。
// Gate 不处理业务、不持有实体。TCP 资源上限与内部接入限额是 gatewiring.DefaultOptions()；配置只开放地址与命名空间。
func NewGateMod() *GateMod { return &GateMod{inner: gatewiring.NewGateMod(gateOptions())} }

// GateMod 把配置读进 gatewiring.Options 后交给 wiring/gate 的 GateMod；生命周期全部委托给它。
type GateMod struct { inner *gatewiring.GateMod }

func gateOptions() gatewiring.Options {
 options := gatewiring.DefaultOptions()
 options.RegisterDiscovery = true
 options.TCP.Enabled = true
 // Gate 没有业务准入条件：App 的失锁 fail-stop 由 wiring/gate 合进准入判断。
 options.Ready = func() bool { return true }
 return options
}
func (*GateMod) Name() app.ModName { return gatewiring.GateName }
func (mod *GateMod) DependsOn() []app.ModName { return mod.inner.DependsOn() }
func (mod *GateMod) Init(cfg *viper.Viper) error {
 var settings gateConfig
 if err:=app.LoadConfig(cfg,&settings);err!=nil{return fmt.Errorf("player gate: %w",err)}
 if settings.Gate.AdvertiseAddr=="" { return errors.New("player gate: player_access.gate.advertise_addr is empty; write the host:port clients connect to") }
 options:=gateOptions()
 if settings.Gate.Namespace!="" { options.Config.Namespace=settings.Gate.Namespace }
 options.TCP.Addr=settings.Gate.Addr
 options.AdvertiseAddr=settings.Gate.AdvertiseAddr
 mod.inner=gatewiring.NewGateMod(options)
 return mod.inner.Init(cfg)
}
func (mod *GateMod) Provide(registry *app.Registry) error { return mod.inner.Provide(registry) }
func (mod *GateMod) Start() error { return mod.inner.Start() }
func (mod *GateMod) Stop() { mod.inner.Stop() }
func (mod *GateMod) StopBudget() time.Duration { return mod.inner.StopBudget() }
func (mod *GateMod) StopWithContext(ctx context.Context) error { return mod.inner.StopWithContext(ctx) }
// Addr 是实际监听的地址（addr 写 :0 时由系统分配）；未启动时为空。
func (mod *GateMod) Addr() string { return mod.inner.Addr() }
func (*GateMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(gateConfig{}) }
var _ app.Mod = (*GateMod)(nil)
var _ app.ModStopperWithContext = (*GateMod)(nil)
var _ app.ModStopBudgetProvider = (*GateMod)(nil)
`)
	return b.String()
}

func withPlayerTCPGate(source string) string {
	for _, edit := range playerTCPGateEdits {
		if strings.Count(source, edit.anchor) != 1 {
			panic(fmt.Sprintf("roost: gate edit anchor not found exactly once in server_gen.go: %q", edit.anchor))
		}
		source = strings.Replace(source, edit.anchor, edit.replace, 1)
	}
	return strings.Replace(source, "@@GATE_MOD@@", renderPlayerTCPGateMod(), 1)
}

// ---- roost add gate ----

// gateDevAdvertiseAddr / gateProdAdvertiseAddr 是 roost add gate 写进玩家服务配置的客户端地址（与 Gate 服务配置的
// player_access.gate.advertise_addr 相同：Gate 接过玩家服务原来的 7000）。
const (
	gateDevAdvertiseAddr  = "127.0.0.1:7000"
	gateProdAdvertiseAddr = "CHANGE_ME:7000"
)

// addGate 把嵌入 TCP 的工程改成独立 Gate：声明 Gate 服务与 access.player.gate、玩家服务补 etcd，改玩家服务的三份配置
// （嵌入 TCP 关、player_access.gate 段、装了登录模块时 login.presence.addr 指向 Gate），然后与别的 add 一样 sync。
// 已经是同名 Gate 时什么都不改（返回空列表）。
func addGate(root string, m Manifest, options AddOptions) ([]string, error) {
	name := toSnake(strings.TrimSpace(options.Name))
	if name == "" {
		name = "gate"
	}
	access, ok := m.Access["player"]
	if !ok || !contains(access.Transports, "tcp") {
		return nil, errors.New("roost add gate turns embedded player TCP into a standalone Gate, and this project has no player TCP yet: run roost add access player --service <service>, roost add transport tcp (and roost module add login for the authenticator) first")
	}
	if service := toSnake(options.Service); service != "" && service != access.Service {
		return nil, fmt.Errorf("player access belongs to service %q, not %q", access.Service, service)
	}
	if current := strings.TrimSpace(access.Gate); current != "" {
		if current == name {
			return nil, nil
		}
		return nil, fmt.Errorf("access.player.gate is already %q; a project has one Gate service (more Gate processes run as more sids of it)", current)
	}
	if _, exists := m.Services[name]; exists {
		return nil, fmt.Errorf("service %q already exists; name the Gate service another way: roost add gate <name>", name)
	}
	account := accountService(m)
	if account == "" {
		return nil, errors.New("the Gate validates handshake tickets through the account service, which this project does not host: run roost module add login (it hosts account and writes the player authenticator), then roost add gate")
	}
	player := m.Services[access.Service]
	if !contains(player.Uses, account) {
		return nil, fmt.Errorf("player service %q does not use %s (services.%s.uses); Game Ingress validates bindings through it: roost module add login wires it", access.Service, account, access.Service)
	}
	devConfig := "configs/service/config." + access.Service + ".yaml"
	prodConfig := "configs/service/config." + access.Service + ".prod.example.yaml"
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(devConfig))); err != nil {
		return nil, fmt.Errorf("roost add gate edits %s: %w", devConfig, err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	previous := manifestWithService(m, access.Service, ServiceSpec{Framework: player.Framework, PortBlock: player.PortBlock, Mods: append([]string(nil), player.Mods...), Uses: player.Uses, Rpcs: player.Rpcs, UsesRpcs: player.UsesRpcs})
	player.Mods = uniqueSorted(append(append([]string(nil), player.Mods...), "etcd"))
	m.Services[access.Service] = player
	m.Services[name] = ServiceSpec{Mods: []string{"etcd", "nats"}, Uses: []string{account}}
	access.Gate = name
	m.Access["player"] = access
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("roost add gate: %w", err)
	}
	presence := hasLoginModule(m)
	configPaths := []string{devConfig, prodConfig, kubernetesSecretExampleRel(access.Service)}
	backups, err := captureFiles(root, configPaths)
	if err != nil {
		return nil, err
	}
	project := m.Project.Name
	changed := []string{ManifestName}
	for _, target := range []struct {
		rel        string
		production bool
	}{{devConfig, false}, {prodConfig, true}} {
		path := filepath.Join(root, filepath.FromSlash(target.rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) && target.production {
			continue
		}
		if err != nil {
			return nil, restoreFiles(root, backups, err)
		}
		body, crlf := lfText(raw)
		edited, err := mergeGateIngressConfig(body, target.rel, project, target.production, presence)
		if err != nil {
			return nil, restoreFiles(root, backups, fmt.Errorf("roost add gate: %w", err))
		}
		if edited != body {
			if err := writeAtomic(path, restoreLineEndings(edited, crlf), 0o644); err != nil {
				return nil, restoreFiles(root, backups, err)
			}
			changed = append(changed, target.rel)
		}
	}
	secret, err := editKubernetesSecretExampleConfig(root, access.Service, options.Warnings, func(config string) (string, error) {
		return mergeGateIngressConfig(config, "stringData.config.yaml", project, true, presence)
	})
	if err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	if secret != "" {
		changed = append(changed, secret)
	}
	configured, err := captureFiles(root, configPaths)
	if err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return nil, restoreFilesIfCurrent(root, backups, configured, err)
	}
	// 玩家服务新加了 etcd 时给它的配置补上 etcd 段（与 add mod 相同）。
	sections, err := appendModConfigSections(root, previous, m, access.Service, options.Warnings)
	if err != nil {
		return changed, err
	}
	changed = append(changed, sections...)
	gateDir := m.serviceDir(name)
	changed = append(changed,
		"configs/service/config."+name+".yaml", "configs/service/config."+name+".prod.example.yaml",
		gateDir+"/service.go", "internal/bootstrap/generated.go", "internal/access/player/tcp/server_gen.go",
		"deploy/dev/run.sh", "deploy/k8s/base/"+name+".yaml", "deploy/k8s/base/"+access.Service+".yaml")
	return uniqueOrdered(changed), nil
}

// hasLoginModule 报告工程装了登录模块（它的 game/managers/login_presence.go 读 login.presence.addr）。
func hasLoginModule(m Manifest) bool {
	_, ok := m.Modules["login"]
	return ok
}

func uniqueOrdered(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0:0]
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// mergeGateIngressConfig 是 roost add gate 对玩家服务一份配置文本（LF）的改动：player_access.tcp.enabled 设为 false
// （缺的 tcp 键照 add transport 补上），没有 player_access.gate 段时加上（namespace = <工程>.access，advertise_addr =
// Gate 的客户端地址），presence 为真（装了登录模块）且没写 login.presence.addr 时写上同一个地址。已有的值不动，其余逐字保留。
func mergeGateIngressConfig(text, name, project string, production, presence bool) (string, error) {
	text, err := mergePlayerTCPConfig(text, name, false)
	if err != nil {
		return "", err
	}
	advertise := gateDevAdvertiseAddr
	if production {
		advertise = gateProdAdvertiseAddr
	}
	document, err := parseYAMLDocument([]byte(text))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	playerKey, playerNode := yamlMappingEntry(document, "player_access")
	if _, gateNode := yamlMappingEntry(playerNode, "gate"); gateNode == nil {
		block := playerGateIngressSchema().StarterYAML(map[string]string{"project": project})
		block = strings.ReplaceAll(block, gateDevAdvertiseAddr, advertise)
		lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
		if len(lines) < 2 || lines[0] != "player_access:" {
			return "", fmt.Errorf("unexpected player_access.gate starter block %q", block)
		}
		if text, err = insertYAMLLines(text, playerKey.Line, lines[1:]); err != nil {
			return "", err
		}
	} else if gateNode.Kind != yaml.MappingNode || gateNode.Style&yaml.FlowStyle != 0 {
		return "", errors.New("player_access.gate must use block-map YAML so codegen can safely check it")
	}
	if presence {
		if text, err = ensureLoginPresenceAddr(text, name, advertise); err != nil {
			return "", err
		}
	}
	if _, err := parseYAMLDocument([]byte(text)); err != nil {
		return "", fmt.Errorf("validate updated %s: %w", name, err)
	}
	return text, nil
}

// loginPresenceComment 是 roost add gate 写 login.presence.addr 时的说明行。
const loginPresenceComment = "# 登录模块的区服入口上报（game/managers/login_presence.go）：给客户端的是 Gate 的地址（roost add gate 写入）"

// ensureLoginPresenceAddr 在没有 login.presence.addr 时写上 addr（缺哪一层补哪一层），已有的不动。
func ensureLoginPresenceAddr(text, name, addr string) (string, error) {
	document, err := parseYAMLDocument([]byte(text))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	loginKey, loginNode := yamlMappingEntry(document, "login")
	if loginNode == nil {
		return strings.TrimRight(text, "\n") + "\n" + loginPresenceComment + "\nlogin:\n  presence:\n    addr: " + addr + "\n", nil
	}
	if loginNode.Kind != yaml.MappingNode || loginNode.Style&yaml.FlowStyle != 0 {
		return "", errors.New("login must use block-map YAML so codegen can safely add login.presence.addr")
	}
	presenceKey, presenceNode := yamlMappingEntry(loginNode, "presence")
	if presenceNode == nil {
		return insertYAMLLines(text, loginKey.Line, []string{"  presence:", "    addr: " + addr})
	}
	if presenceNode.Kind != yaml.MappingNode || presenceNode.Style&yaml.FlowStyle != 0 {
		return "", errors.New("login.presence must use block-map YAML so codegen can safely add addr")
	}
	if _, addrNode := yamlMappingEntry(presenceNode, "addr"); addrNode != nil {
		return text, nil
	}
	indent := strings.Repeat(" ", presenceKey.Column+1)
	if len(presenceNode.Content) > 0 {
		indent = strings.Repeat(" ", presenceNode.Content[0].Column-1)
	}
	return insertYAMLLines(text, presenceKey.Line, []string{indent + "addr: " + addr})
}

// ---- roost project doctor ----

// gateConfigView 是 doctor 从一份开发配置里读的、与独立 Gate 有关的值。
type gateConfigView struct {
	found                bool
	tcpEnabled           bool
	namespace, advertise string
	hasGateSection       bool
	singleton            bool
	etcd                 bool
	servicePrefix        string
	presence             string
	portalServers        map[string]string
}

func readGateConfigView(root, service string) (gateConfigView, error) {
	rel := "configs/service/config." + service + ".yaml"
	path := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return gateConfigView{}, nil
		}
		return gateConfigView{}, err
	}
	cfg, _, err := configfile.Load(path)
	if err != nil {
		return gateConfigView{}, fmt.Errorf("%s: %w", rel, err)
	}
	view := gateConfigView{
		found:          true,
		tcpEnabled:     cfg.GetBool("player_access.tcp.enabled"),
		hasGateSection: cfg.IsSet("player_access.gate"),
		namespace:      strings.TrimSpace(cfg.GetString("player_access.gate.namespace")),
		advertise:      strings.TrimSpace(cfg.GetString("player_access.gate.advertise_addr")),
		singleton:      cfg.GetBool("singleton.enabled"),
		etcd:           len(cfg.GetStringSlice("etcd.endpoints")) > 0 || strings.TrimSpace(cfg.GetString("etcd.endpoints")) != "",
		servicePrefix:  strings.TrimSpace(cfg.GetString("etcd.service_prefix")),
		presence:       strings.TrimSpace(cfg.GetString("login.presence.addr")),
		portalServers:  cfg.GetStringMapString("portal.servers"),
	}
	if view.namespace == "" {
		view.namespace = gatewiringDefaultNamespace
	}
	return view, nil
}

// checkPlayerGate 是 doctor 的 gate:* 检查（只读开发配置与生成的 bootstrap）：
//
//   - gate:ingress：声明了 Gate ⇔ 玩家服务走 Game Ingress（生成的 bootstrap 的 New() 用 gateIngressOptions）；没声明 Gate 时
//     任何服务配置里写了 player_access.gate 都是 FAIL（没有进程读它）。
//   - gate:player-tcp：玩家服务的 player_access.tcp.enabled 必须是 false（Game Ingress 起服时拒绝嵌入 TCP）。
//   - gate:namespace：两端 player_access.gate.namespace 相同（不写按 roost.access）。
//   - gate:etcd-singleton：两端都有 etcd.endpoints、singleton.enabled: true（wiring/gate 两样都要），etcd.service_prefix 相同
//     （Gate 在自己的前缀下找 Game Ingress 登记的候选）。
//   - gate:client-address：Gate 的 advertise_addr 非空；玩家服务的 player_access.gate.advertise_addr（写了时）、装了登录模块时
//     它的 login.presence.addr、account 配置的 portal.servers 的每个地址都等于它（给客户端的地址指向 Gate，不是玩家服务）。
func checkPlayerGate(root string, m Manifest) []CheckItem {
	gate := playerGateService(m)
	if gate == "" {
		var stray []string
		for _, service := range sortedServiceNames(m) {
			view, err := readGateConfigView(root, service)
			if err == nil && view.hasGateSection {
				stray = append(stray, "configs/service/config."+service+".yaml")
			}
		}
		if len(stray) == 0 {
			return nil
		}
		return []CheckItem{{Name: "gate:ingress", Status: StatusFail, Detail: strings.Join(stray, ", ") + " set player_access.gate, but roost.yaml declares no Gate (access.player.gate), so no process reads it: run roost add gate, or delete the section"}}
	}
	player := m.Access["player"].Service
	items := make([]CheckItem, 0, 5)
	fail := func(name, detail string) { items = append(items, CheckItem{Name: name, Status: StatusFail, Detail: detail}) }
	ok := func(name, detail string) { items = append(items, CheckItem{Name: name, Status: StatusOK, Detail: detail}) }

	bootstrap, err := os.ReadFile(filepath.Join(root, "internal", "bootstrap", "generated.go"))
	switch {
	case err != nil:
		fail("gate:ingress", "read internal/bootstrap/generated.go: "+err.Error())
	case !strings.Contains(string(bootstrap), "GameIngress: gateIngressOptions()") || !strings.Contains(string(bootstrap), gateModConstructor):
		fail("gate:ingress", fmt.Sprintf("roost.yaml declares Gate %s, but internal/bootstrap/generated.go does not run %s behind Game Ingress with the Gate Mod: run roost project sync", gate, player))
	default:
		ok("gate:ingress", fmt.Sprintf("players connect to %s; %s runs Game Ingress", gate, player))
	}

	gateView, gateErr := readGateConfigView(root, gate)
	playerView, playerErr := readGateConfigView(root, player)
	if err := errors.Join(gateErr, playerErr); err != nil {
		fail("gate:config", err.Error())
		return items
	}
	if !gateView.found || !playerView.found {
		fail("gate:config", fmt.Sprintf("configs/service/config.%s.yaml and config.%s.yaml must both exist (roost project sync creates the Gate's)", gate, player))
		return items
	}
	if playerView.tcpEnabled {
		fail("gate:player-tcp", fmt.Sprintf("configs/service/config.%s.yaml: player_access.tcp.enabled is true; behind Gate %s players connect to the Gate and Game Ingress refuses to start with the embedded listener on: set it to false", player, gate))
	} else {
		ok("gate:player-tcp", player+" does not listen for players")
	}
	if gateView.namespace != playerView.namespace {
		fail("gate:namespace", fmt.Sprintf("player_access.gate.namespace differs: %s has %q, %s has %q; the Gate and Game Ingress only find each other in one namespace", gate, gateView.namespace, player, playerView.namespace))
	} else {
		ok("gate:namespace", gateView.namespace)
	}
	var missing []string
	for _, side := range []struct {
		service string
		view    gateConfigView
	}{{gate, gateView}, {player, playerView}} {
		if !side.view.etcd {
			missing = append(missing, side.service+": etcd.endpoints")
		}
		if !side.view.singleton {
			missing = append(missing, side.service+": singleton.enabled: true")
		}
	}
	if gateView.servicePrefix != playerView.servicePrefix {
		missing = append(missing, fmt.Sprintf("the same etcd.service_prefix (%s has %q, %s has %q: the Gate looks up Game Ingress candidates under its own)", gate, gateView.servicePrefix, player, playerView.servicePrefix))
	}
	if len(missing) > 0 {
		fail("gate:etcd-singleton", "the Gate and Game Ingress find each other through etcd and check each other's singleton incarnation; missing "+strings.Join(missing, ", "))
	} else {
		ok("gate:etcd-singleton", "etcd and singleton on "+gate+" and "+player)
	}
	var wrong []string
	if gateView.advertise == "" {
		wrong = append(wrong, fmt.Sprintf("configs/service/config.%s.yaml: player_access.gate.advertise_addr is empty", gate))
	} else {
		if playerView.advertise != "" && playerView.advertise != gateView.advertise {
			wrong = append(wrong, fmt.Sprintf("configs/service/config.%s.yaml: player_access.gate.advertise_addr %s", player, playerView.advertise))
		}
		if hasLoginModule(m) && playerView.presence != "" && playerView.presence != gateView.advertise {
			wrong = append(wrong, fmt.Sprintf("configs/service/config.%s.yaml: login.presence.addr %s", player, playerView.presence))
		}
		if account := accountService(m); account != "" {
			accountView, err := readGateConfigView(root, account)
			if err != nil {
				wrong = append(wrong, err.Error())
			}
			for _, sid := range sortedKeys(accountView.portalServers) {
				if addr := strings.TrimSpace(accountView.portalServers[sid]); addr != gateView.advertise {
					wrong = append(wrong, fmt.Sprintf("configs/service/config.%s.yaml: portal.servers.%s %s", account, sid, addr))
				}
			}
		}
	}
	if len(wrong) > 0 {
		fail("gate:client-address", "clients must be given the Gate's address "+fmt.Sprintf("%q", gateView.advertise)+" (player_access.gate.advertise_addr of "+gate+"), not another one: "+strings.Join(wrong, "; "))
	} else {
		ok("gate:client-address", "clients connect to "+gateView.advertise)
	}
	return items
}


// printGateNext 是 roost add gate 之后的提示；unchanged 表示已经是这个 Gate、什么都没改。
func printGateNext(stdout io.Writer, root, name string, unchanged bool) {
	if unchanged {
		fmt.Fprintf(stdout, "access.player.gate is already %s; nothing changed (roost project doctor checks the Gate shape: gate:*)\n", name)
		return
	}
	m, err := LoadManifest(root)
	if err != nil {
		return
	}
	player := m.Access["player"].Service
	fmt.Fprintf(stdout, "Gate %s: players connect to it (player_access.gate.addr, 0.0.0.0:7000 in development); %s now runs Game Ingress with its embedded TCP off\n", name, player)
	fmt.Fprintf(stdout, "  run:   bin/app %s --sid <gate sid> --config configs/service/config.%s.yaml (make dev-run starts it with the other services)\n", name, name)
	fmt.Fprintf(stdout, "  check: roost project doctor (gate:ingress, gate:player-tcp, gate:namespace, gate:etcd-singleton, gate:client-address)\n")
	fmt.Fprintf(stdout, "  production: set player_access.gate.advertise_addr in config.%s.prod.example.yaml and config.%s.prod.example.yaml to the address clients reach the Gate at\n", name, player)
	fmt.Fprintf(stdout, "  guide: https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/framework/GATEWAY.md\n")
}
