package roost

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

func renderProject(m Manifest) (map[string]plannedFile, error) {
	files := map[string]plannedFile{}
	add := func(path, body string, owned bool) { files[path] = plannedFile{Body: []byte(body), Owned: owned} }
	addGo := func(path, body string, owned bool) error {
		formatted, err := format.Source([]byte(body))
		if err != nil {
			return fmt.Errorf("format %s: %w\n%s", path, err, body)
		}
		files[path] = plannedFile{Body: formatted, Owned: owned}
		return nil
	}

	add("go.mod", renderGoMod(m), false)
	// Keep the file present so Docker's deterministic `COPY go.mod go.sum ./`
	// works before the first local `go mod tidy`. Sync never overwrites it.
	add("go.sum", "", false)
	add(".gitattributes", renderGitattributes(), true)
	// /data/wal/ 与 skippedProjectDirectory 一致：DataEngine 默认 WAL（data/wal/dataengine/<sid>，
	// second-game.sh 对手写 dir 改出的 data/wal/dataengine-<sid>）是运行期输出。只忽略 .dev/ 的话，
	// dev-run 之后 git add -A 会把 WAL 段提交进仓库。只忽略 WAL 目录，不忽略
	// data/（configs/data 是要提交的生成物，根目录 data/ 下也可能有使用者自己的文件）。
	// .gitignore 归使用者所有，sync 不覆盖，已有工程需手工补这一行。
	add(".gitignore", "bin/\ndist/\nlog/\n.dev/\n/data/wal/\n.roost-deploy/\n.env\ndeploy/docker/.env.*\n*.local.yaml\ndeploy/k8s/base/secret.*.local.yaml\n.idea/\n.vscode/\n", false)
	add("Makefile", renderMakefile(m), true)
	add("README.md", renderProjectReadme(m), false)
	add("docs/QUICKSTART.zh-CN.md", renderBeginnerQuickstart(m), true)
	add("docs/BEGINNER_WORKBOOK.zh-CN.md", renderBeginnerWorkbook(m), true)
	add("docs/ROOST_YAML.zh-CN.md", renderManifestGuide(), true)
	add("docs/ENTITY_COMPONENT.zh-CN.md", renderEntityComponentGuide(m), true)
	add("docs/FIRST_BUSINESS.zh-CN.md", renderFirstBusinessGuide(m), true)
	add("docs/ENTITY_LIFECYCLE.zh-CN.md", renderEntityLifecycleGuide(m), true)
	add("docs/PROTOCOL_TO_NEST.zh-CN.md", renderProtocolNestGuide(), true)
	playerTCPGuide := renderPlayerTCPGuide()
	if selfSyncEnabled(m) {
		playerTCPGuide += renderSelfSyncGuide()
	}
	add("docs/PLAYER_ACCESS_TCP.zh-CN.md", playerTCPGuide, true)
	add("docs/SKILL.zh-CN.md", renderSkillGuide(), true)
	add("docs/TROUBLESHOOTING.zh-CN.md", renderTroubleshootingGuide(), true)
	add("docs/USAGE.zh-CN.md", renderUsage(m), true)
	if m.usesFrameworkServices() {
		add("docs/SERVICES.zh-CN.md", renderFrameworkServicesGuide(m), true)
	}
	add(".github/workflows/ci.yml", renderCI(m), true)
	add(".github/workflows/dependency-update.yml", renderDependencyUpdateCI(m), true)
	add(".github/workflows/release.yml", renderReleaseCI(m), true)
	add(".github/workflows/deploy-shell.yml", renderShellDeployCI(m), true)
	add(".github/workflows/deploy-docker.yml", renderDockerDeployCI(m), true)
	add(".github/workflows/deploy-k8s.yml", renderKubernetesDeployCI(m), true)
	add(".github/workflows/security.yml", renderGeneratedSecurityCI(), true)
	add(".github/dependabot.yml", renderDependabot(), true)
	add(".github/actionlint.yaml", renderActionlintConfig(), true)
	add("deploy/dev/docker-compose.yaml", renderCompose(m), true)
	add("deploy/dev/run.sh", renderDevRun(m), true)
	if game := firstBusinessService(m); game != "" {
		add("deploy/dev/second-game.sh", renderSecondGame(m, game), true)
	}
	add("Dockerfile", renderDockerfile(m), true)
	for path, body := range renderProductionDeployment(m) {
		owned := !strings.HasPrefix(path, "deploy/k8s/base/secret.")
		add(path, body, owned)
	}
	// 生成工程自带的 compose 结构检查，CI 与 make compose-check 跑它。
	if err := addGo("deploy/docker/compose_check_test.go", renderComposeCheckTest(m), true); err != nil {
		return nil, err
	}
	add("docs/IMPLEMENTATION.zh-CN.md", renderImplementationGuide(m), true)
	add("docs/DEPLOYMENT.zh-CN.md", renderDeploymentGuide(m), true)
	if err := addGo("main.go", renderMain(m), false); err != nil {
		return nil, err
	}
	if err := addGo("cmd/healthprobe/main.go", renderHealthProbe(), true); err != nil {
		return nil, err
	}
	if err := addGo("internal/bootstrap/generated.go", renderBootstrap(m), true); err != nil {
		return nil, err
	}
	if err := addGo("internal/frameworkdeps/generated.go", renderFrameworkDeps(m), true); err != nil {
		return nil, err
	}
	if _, enabled := m.Access["player"]; enabled {
		if err := addGo("game/playeraccess/runtime_gen.go", renderPlayerAccessRuntime(), true); err != nil {
			return nil, err
		}
		if err := addGo("internal/access/player/mod_gen.go", renderPlayerAccessMod(m), true); err != nil {
			return nil, err
		}
		if selfSyncEnabled(m) {
			// 推送编号归项目：只在缺失时创建，之后由项目维护（冲突时改 id）。
			if err := addGo(selfSyncProtocolPath, renderSelfSyncProtocolDef(m), false); err != nil {
				return nil, err
			}
		}
		if contains(m.Access["player"].Transports, "tcp") {
			if err := addGo("internal/access/player/tcp/server_gen.go", renderPlayerTCPServer(m), true); err != nil {
				return nil, err
			}
			if err := addGo("internal/access/player/tcp/server_gen_test.go", renderPlayerTCPServerTests(m), true); err != nil {
				return nil, err
			}
			if err := addGo("internal/access/player/tcp/auth.go", renderPlayerTCPAuthenticator(), false); err != nil {
				return nil, err
			}
			if err := addGo("cmd/playerprobe/main.go", renderPlayerTCPProbe(), true); err != nil {
				return nil, err
			}
			add("configs/examples/access.player.tcp.yaml", renderPlayerTCPConfig(), true)
		}
	}

	// roost add timer：每个 timers 条目的组件 / 节点类型 / Tick handler（重写）与业务回调文件（只建一次）。
	if err := renderTimerFiles(m, addGo); err != nil {
		return nil, err
	}
	// roost add heartbeat / behavior / timedomain：心跳驱动、心跳宿主、行为组件与时间域（生成部分重写，业务文件只建一次）。
	if err := renderHeartbeatFiles(m, addGo); err != nil {
		return nil, err
	}

	services := sortedServiceNames(m)
	for _, name := range services {
		if m.isFrameworkService(name) {
			// A hosted framework service has no business Service; its
			// process is the roost-service Server plus the collaborators the
			// owner Mod needs, which are the project's to write.
			if err := addGo("internal/service/"+name+"/collaborators.go", renderFrameworkCollaborators(m, name), false); err != nil {
				return nil, err
			}
			add("configs/service/config."+name+".yaml", renderServiceConfig(m, name, false), false)
			add("configs/service/config."+name+".prod.example.yaml", renderServiceConfig(m, name, true), false)
			continue
		}
		if err := addGo("internal/service/"+name+"/service.go", renderService(m.servicePackage(name), name), false); err != nil {
			return nil, err
		}
		if len(m.Services[name].Uses) > 0 {
			if err := addGo("internal/service/"+name+"/framework_clients_gen.go", renderFrameworkClients(m, name), true); err != nil {
				return nil, err
			}
		}
		serviceMods, _ := resolveMods(effectiveServiceMods(m, name))
		if contains(serviceMods, "manager") {
			if err := addGo("internal/service/"+name+"/managers.go", renderServiceManagers(m.servicePackage(name)), false); err != nil {
				return nil, err
			}
		}
		add("configs/service/config."+name+".yaml", renderServiceConfig(m, name, false), false)
		add("configs/service/config."+name+".prod.example.yaml", renderServiceConfig(m, name, true), false)
	}
	if hostsActivityCoordinator(m) {
		// 活动组文件只创建一次，之后归项目（列出真实部署的 game sid）。
		add(activityGroupsFile, renderActivityGroups(m), false)
	}
	if hasFeature(m, "config") {
		if err := addGo("configs/schema/doc.go", "package schema\n", false); err != nil {
			return nil, err
		}
		if err := addGo("configs/generated/doc.go", "package generated\n", false); err != nil {
			return nil, err
		}
		add("configs/data/_manifest.json", "{\n  \"version\": 2,\n  \"generated_at\": \"\",\n  \"tables\": {}\n}\n", false)
	}
	featurePackages := map[string]string{
		"protocol": "protocol/def/doc.go", "entity": "game/entities/doc.go", "nest": "game/handler/doc.go",
		"dao": "db/def/doc.go", "attribute": "game/gameplay/attribute/doc.go",
		"webroute": "service/web/doc.go",
	}
	for feature, path := range featurePackages {
		if !hasFeature(m, feature) {
			continue
		}
		pkg := strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".go")
		_ = pkg
		dir := path[:strings.LastIndex(path, "/")]
		packageName := dir[strings.LastIndex(dir, "/")+1:]
		if packageName == "def" {
			packageName = feature + "def"
		}
		if feature == "dao" {
			packageName = "dbdef"
		}
		body := "package " + packageName + "\n"
		if feature == "webroute" {
			body += "\n// Service is what the generated routes receive as svc (a business file:\n" +
				"// generated once). Put the route handlers' dependencies here and build it in\n" +
				"// the service Init: kitweb.RegisterRoutes(registry, &web.Service{...}).\n" +
				"type Service struct{}\n"
		}
		if err := addGo(path, body, false); err != nil {
			return nil, err
		}
		if feature == "attribute" {
			// The attribute generator's output leans on a framework contract
			// (AttrID, AttributeProfile, Snapshot, Container, …). Shipping
			// only an empty package would make `roost add attribute` produce code
			// that referenced types nobody defined;
			// this file re-exports roost-core/attribute under those names and
			// is codegen-owned, so an upgrade keeps it in step.
			if err := addGo(dir+"/runtime.go", renderAttributeRuntime(packageName), true); err != nil {
				return nil, err
			}
		}
	}
	if hasFeature(m, "dao") {
		if err := addGo("db/doc.go", "package db\n", false); err != nil {
			return nil, err
		}
	}
	if hasFeature(m, "nettransport-quic") || hasFeature(m, "nettransport-kcp") || hasFeature(m, "nettransport-udp") {
		if err := addGo("internal/transport/generated.go", renderSyncTransport(m), true); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func renderSyncTransport(m Manifest) string {
	var b strings.Builder
	b.WriteString(generatedHeader + "\npackage transport\n\nimport (\n")
	if hasFeature(m, "nettransport-udp") {
		b.WriteString("\t\"net\"\n")
	}
	b.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync\"\n")
	b.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport\"\n)\n\n")
	b.WriteString("func AsyncConfig() nettransport.AsyncTransportConfig { return nettransport.DefaultAsyncTransportConfig() }\n\n")
	b.WriteString("// NewSyncTransport is the entity sync manager's lane over the async transport's reliable channel:\n")
	b.WriteString("// one frame per session per tick, backpressure closes the session (entitysync.ManagerConfig.Transport).\n")
	b.WriteString("func NewSyncTransport(async *nettransport.AsyncTransport) (*entitysync.AsyncTransport, error) {\n\treturn entitysync.NewAsyncTransport(async)\n}\n\n")
	if hasFeature(m, "nettransport-quic") {
		b.WriteString("func NewQUIC() (*nettransport.AsyncTransport, *nettransport.QUICTransport, error) {\n\tprotocol := nettransport.NewQUICTransport(nettransport.QUICTransportConfig{})\n\tasync, err := nettransport.NewAsyncTransport(protocol, AsyncConfig())\n\treturn async, protocol, err\n}\n\n")
	}
	if hasFeature(m, "nettransport-kcp") {
		b.WriteString("func NewKCP() (*nettransport.AsyncTransport, *nettransport.KCPTransport, error) {\n\tprotocol, err := nettransport.NewKCPTransport(nettransport.DefaultKCPTransportConfig())\n\tif err != nil { return nil, nil, err }\n\tasync, err := nettransport.NewAsyncTransport(protocol, AsyncConfig())\n\treturn async, protocol, err\n}\n\n")
	}
	if hasFeature(m, "nettransport-udp") {
		b.WriteString("func NewUDP(conn net.PacketConn, reliable nettransport.ReliableSender) (*nettransport.AsyncTransport, *nettransport.UDPTransport, error) {\n\tprotocol, err := nettransport.NewUDPTransport(nettransport.UDPTransportConfig{PacketConn: conn})\n\tif err != nil { return nil, nil, err }\n\tcomposite := nettransport.CompositeTransport{Datagrams: protocol, Reliable: reliable}\n\tasync, err := nettransport.NewAsyncTransport(composite, AsyncConfig())\n\treturn async, protocol, err\n}\n")
	}
	return b.String()
}

// generatedGoVersion is the Go release line every generated artifact agrees
// on: the go.mod directive, the Dockerfile's builder image and the docs. It
// is the framework's own requirement — roost-core is go 1.27.0
// — so a consumer never ends up with a go.mod one toolchain ahead of the
// image that builds it ("go.mod requires go >= 1.27.0 (running go 1.25.14)").
// TestGeneratedGoVersionMatchesTheGeneratorsOwn pins it to this module's go.mod.
const generatedGoVersion = "1.27"

// generatedToolchain 是生成工程 go.mod 的 toolchain 行，与本模块 go.mod 的
// toolchain 行一致（TestGeneratedGoVersionMatchesTheGeneratorsOwn 钉住）。
// go 指令仍是 1.27.0，只表示最低语言版本；toolchain 让 GOTOOLCHAIN=auto 时
// 构建、测试与生成工程 CI 里的 govulncheck 用带安全修复的补丁版标准库
// （go1.27.0 有 GO-2026-6599…6617，修复于 go1.27.2）。
const generatedToolchain = "go1.27.2"

func renderGoMod(m Manifest) string {
	return fmt.Sprintf(`module %s

go `+generatedGoVersion+`.0

toolchain `+generatedToolchain+`

// roost.yaml owns the framework update policy. When it is "latest", this
// bootstrap file starts at the supported minimum and roost project deps
// resolves it to the latest tag.
//
// One require: the framework is one module. Wiring and the generator live
// inside roost-core (wiring/, codegen/) as package paths, not as modules of
// their own.
require github.com/tjbdwanghaibo/roost-core %s
`, m.Project.Module,
		resolvedModuleVersion(m.Versions.Core, minimumVersions.Core))
}

// renderFrameworkDeps keeps optional framework-domain modules in go.mod even
// before business code imports them. This makes a freshly generated project
// go-mod-tidy clean while preserving the version baseline from roost.yaml.
func renderFrameworkDeps(m Manifest) string {
	if !m.usesFrameworkServices() {
		return generatedHeader + `
package frameworkdeps

import "github.com/tjbdwanghaibo/roost-core/gameplay/skill"

// SkillProgram retains the skill runtime selected by roost.yaml without adding
// runtime initialization or requiring business code to import it immediately.
type SkillProgram = skill.Program
`
	}
	return generatedHeader + `
package frameworkdeps

import (
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// SkillProgram retains the skill runtime selected by roost.yaml without adding
// runtime initialization or requiring business code to import it immediately.
type SkillProgram = skill.Program

// ServiceMetrics keeps the reporter (infra/observe/servicemetrics) that the
// framework services this project hosts or calls report through.
type ServiceMetrics = servicemetrics.Reporter
`
}

// renderMain 是工程的 main.go（新建时写一次，之后归应用）。它导入 time/tzdata（约 450 KB）：业务时区
// time.business_timezone 在启动检查里加载，生成的运行镜像（distroless static）与很多最小镜像没有 zoneinfo，
// 不内嵌时 Asia/Shanghai 这类名字加载失败、进程拒绝启动。
func renderMain(m Manifest) string {
	return fmt.Sprintf(`package main

import (
	"log/slog"
	"os"
	// The business time zone (time.business_timezone) is loaded at start; the runtime image
	// has no zoneinfo, so the time zone database is embedded in the binary.
	_ "time/tzdata"

	"%s/internal/bootstrap"
)

func main() {
	a, err := bootstrap.New()
	if err == nil { err = a.Execute() }
	if err != nil {
		slog.Error("server exit", "err", err)
		os.Exit(1)
	}
}
`, m.Project.Module)
}

func renderBootstrap(m Manifest) string {
	imports := map[string]string{
		"github.com/tjbdwanghaibo/roost-core/framework/app":           "",
		"github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo": "",
	}
	allMods := allProjectMods(m)
	// Persistence and Remote Entity constructors require instance-scoped
	// EntityAccess even when enabled transitively without generated handlers.
	// when a project enables them transitively (for example through Saga)
	// without generating Nest handlers.
	instanceRuntime := contains(allMods, "dataengine") || contains(allMods, "nest") || contains(allMods, "remote_entity")
	if instanceRuntime {
		imports["github.com/tjbdwanghaibo/roost-core/framework/entity"] = ""
	}
	for _, name := range allMods {
		spec := modCatalog[name]
		imports[spec.ImportPath] = spec.Alias
	}
	installSingleton := projectInstallsSingleton(m)
	if installSingleton {
		imports[modCatalog["redis"].ImportPath] = modCatalog["redis"].Alias
	}
	services := sortedServiceNames(m)
	for _, name := range services {
		imports[m.Project.Module+"/internal/service/"+name] = "service" + safeIdent(name)
		if spec, ok := frameworkCatalog[m.Services[name].Framework]; ok {
			imports[frameworkServiceModule+"/"+spec.ImportPath()] = "svc" + spec.Package
			imports[frameworkDomainModule+"/"+spec.ImportPath()] = "domain" + spec.Package
		}
		for _, used := range m.Services[name].Uses {
			if spec, ok := frameworkCatalog[m.Services[used].Framework]; ok {
				imports[frameworkServiceModule+"/"+spec.ImportPath()] = "svc" + spec.Package
			}
		}
	}
	if _, enabled := m.Access["player"]; enabled {
		imports[m.Project.Module+"/internal/access/player"] = "accessplayer"
		if contains(m.Access["player"].Transports, "tcp") {
			imports[m.Project.Module+"/internal/access/player/tcp"] = "accessplayertcp"
		}
	}
	imports[m.Project.Module+"/internal/registry"] = "registry"
	if selfSyncEnabled(m) {
		imports[selfSyncImport] = "selfsync"
		imports[m.Project.Module+"/game/entities/"+selfSyncEntity] = selfSyncEntity
		imports[m.Project.Module+"/protocol/msgid"] = ""
	}
	for _, name := range services {
		for _, rpc := range m.Services[name].Rpcs {
			imports[m.Project.Module+"/"+rpcDir(rpc)] = rpcAlias(rpc)
		}
		for _, rpc := range m.Services[name].UsesRpcs {
			imports[m.Project.Module+"/"+rpcDir(rpc)] = rpcAlias(rpc)
		}
	}
	for _, name := range m.Sagas {
		imports[m.Project.Module+"/saga/"+name] = "saga" + safeIdent(name)
	}
	if contains(allMods, "heartbeat") {
		imports[m.Project.Module+"/"+heartbeatDriverDir] = heartbeatDriverAlias
	}
	if len(m.Timers) > 0 {
		imports[entityTimerWiring] = "kitentitytimer"
		for _, name := range sortedTimerEntities(m) {
			// 与 self_sync 同一个别名：两者都导入 Player 包时是同一条 import。
			imports[m.Project.Module+"/game/entities/"+name] = name
		}
	}
	var b strings.Builder
	b.WriteString(generatedHeader + "\npackage bootstrap\n\nimport (\n")
	paths := make([]string, 0, len(imports))
	for path := range imports {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if alias := imports[path]; alias != "" {
			fmt.Fprintf(&b, "\t%s %q\n", alias, path)
		} else {
			fmt.Fprintf(&b, "\t%q\n", path)
		}
	}
	b.WriteString(")\n")
	if instanceRuntime {
		b.WriteString("\nvar EntityManager = entity.NewEntityManager()\n")
		b.WriteString("var EntityAccess = entity.NewManagerAccess(EntityManager)\n")
	}
	if selfSyncEnabled(m) {
		// access.player.self_sync：Nest Mod 装它的 Manager，player TCP Mod 接上连接生命周期。
		b.WriteString("\n// " + selfSyncVar + " 是 access.player.self_sync 的接线（wiring/selfsync）：每个在线玩家只收到自己的\n")
		b.WriteString("// Player（视图 \"self\"），经 PushSyncPlayer 以 msgid.MsgSelfSync 推送。Nest Mod 用它装 Entity Sync，\n")
		b.WriteString("// player TCP Mod 用它接上连接的激活与关闭。关掉开关后 roost project sync 删除这段接线。\n")
		fmt.Fprintf(&b, "var %s = selfsync.MustNew(selfsync.Config{Entities: EntityAccess, Kind: %s.EntityKind%s, MessageID: msgid.MsgSelfSync})\n", selfSyncVar, selfSyncEntity, toPascal(selfSyncEntity))
	}
	b.WriteString("\nfunc New() (*app.App, error) {\n")
	// Static registration first, before app.New: an entity builder registered
	// later than a Mod's Init is already too late for a Mod that resolves
	// entities in Provide.
	b.WriteString("\tif err := registry.RegisterAll(); err != nil { return nil, err }\n")
	fmt.Fprintf(&b, "\ta := app.New(%q, buildinfo.VersionString())\n", m.Project.Name)
	if installSingleton {
		// 单实例锁的后端（app.Singleton）。是否启用由每个服务配置的 singleton.enabled 决定，
		// enabled=false 时 opener 不会被调用，所以只要有配置能打开它就安装（projectInstallsSingleton）。
		b.WriteString("\ta.Singleton(kitredis.SingletonStore)\n")
	}
	shared, _ := resolveMods(m.SharedMods)
	if len(shared) > 0 {
		b.WriteString("\ta.Mods(\n")
		for _, name := range shared {
			fmt.Fprintf(&b, "\t\t%s,\n", renderModConstructor(m, name, allMods, ""))
		}
		b.WriteString("\t)\n")
	}
	for _, name := range services {
		if spec, hosted := frameworkCatalog[m.Services[name].Framework]; hosted {
			// The roost-service Server is the process's Service; the owner Mod
			// is built from the project's collaborators file.
			fmt.Fprintf(&b, "\ta.RegisterServer(app.ServiceName(domain%s.ServiceType), svc%s.NewServer()", spec.Package, spec.Package)
		} else {
			fmt.Fprintf(&b, "\ta.RegisterServer(app.ServiceName(%q), service%s.New()", name, safeIdent(name))
		}
		for _, constructor := range serviceModConstructors(m, name, allMods) {
			fmt.Fprintf(&b, ",\n\t\t%s", constructor)
		}
		b.WriteString(")\n")
	}
	b.WriteString("\treturn a, nil\n}\n")
	return b.String()
}

// serviceModConstructors is every Mod the bootstrap registers for one service
// after its Service, in registration order: the service's Kit Mods, then for a
// hosted framework service its owner Mod, for a business service the framework
// ClientMods it uses, its project rpcs' owner / client Mods and the player
// access Mods it hosts. serviceShutdownPlan counts this same list, so the
// generated shutdown.total_timeout follows what the process actually stops.
func serviceModConstructors(m Manifest, name string, allMods []string) []string {
	var out []string
	mods, _ := resolveMods(effectiveServiceMods(m, name))
	for _, mod := range mods {
		out = append(out, renderModConstructor(m, mod, allMods, name))
	}
	if spec, hosted := frameworkCatalog[m.Services[name].Framework]; hosted {
		args := make([]string, 0, len(spec.ModArgs))
		for _, arg := range spec.ModArgs {
			args = append(args, "service"+safeIdent(name)+"."+arg)
		}
		var chain strings.Builder
		for _, call := range spec.ModChain {
			fmt.Fprintf(&chain, ".%s", strings.Replace(call, "(", "(service"+safeIdent(name)+".", 1))
		}
		return append(out, fmt.Sprintf("svc%s.NewMod(%s)%s", spec.Package, strings.Join(args, ", "), chain.String()))
	}
	used := append([]string(nil), m.Services[name].Uses...)
	sort.Strings(used)
	usesAccount := false
	for _, target := range used {
		out = append(out, fmt.Sprintf("svc%s.NewClientMod()", frameworkCatalog[m.Services[target].Framework].Package))
		usesAccount = usesAccount || m.Services[target].Framework == "account"
	}
	// 承载玩家（access.player 指向它）且接了 account 的服务：启动自检本服没有被合服并入别处
	// （wiring/account.ServingGuardMod，DataEngine Mod 排在它之后）。
	if access, enabled := m.Access["player"]; enabled && access.Service == name && usesAccount {
		out = append(out, "svcaccount.NewServingGuardMod()")
	}
	// The project's own rpcs: the owner Mod wraps the implementation
	// (New() takes whatever the service needs; edit the call here), a
	// consumer gets the generated ClientMod.
	for _, rpc := range uniqueSorted(m.Services[name].Rpcs) {
		out = append(out, fmt.Sprintf("%s.NewMod(%s.New())", rpcAlias(rpc), rpcAlias(rpc)))
	}
	for _, rpc := range uniqueSorted(m.Services[name].UsesRpcs) {
		out = append(out, fmt.Sprintf("%s.NewClientMod()", rpcAlias(rpc)))
	}
	// roost add timer：本服务加载的实体的定时器驱动（wiring/entitytimer，依赖 Nest，先于它停止）。
	if timers := timerServices(m, name); len(timers) > 0 {
		args := []string{"EntityAccess"}
		for _, names := range timers {
			args = append(args, names.Entity+"."+names.CompType+"Driver")
		}
		out = append(out, "kitentitytimer.NewMod("+strings.Join(args, ", ")+")")
	}
	if access, enabled := m.Access["player"]; enabled && access.Service == name {
		out = append(out, "accessplayer.NewMod()")
		if contains(access.Transports, "tcp") {
			if access.SelfSync {
				out = append(out, "accessplayertcp.NewMod().WithSelfSync("+selfSyncVar+")")
			} else {
				out = append(out, "accessplayertcp.NewMod()")
			}
		}
	}
	return out
}

func renderModConstructor(m Manifest, name string, allMods []string, service string) string {
	switch name {
	case "manager":
		// Managers are per service: the same in-memory singleton may appear
		// in several services' sets and only the running service starts it.
		// The list itself is business code, so it comes from a hook the
		// generator creates once and never overwrites.
		if service == "" {
			// Unreachable via Validate, which rejects manager in shared_mods.
			return "kitmanager.NewManagerMod()"
		}
		return "kitmanager.NewManagerMod(service" + safeIdent(service) + ".Managers()...)"
	case "dataengine":
		options := []string{"kitdataengine.WithEntityAccess(EntityAccess)"}
		if contains(allMods, "remote_entity") {
			options = append(options, "kitdataengine.WithRemoteProjection(true)")
		}
		return "kitdataengine.NewMod(" + strings.Join(options, ", ") + ")"
	case "remote_entity":
		return "kitremoteentity.NewRemoteEntityMod(0, kitremoteentity.WithMongoStorage(EntityAccess))"
	case "nest":
		if selfSyncNest(m, service) {
			return "kitnest.NewModWithEntitySync(EntityAccess, " + selfSyncVar + ".EntitySyncSetup())"
		}
		return "kitnest.NewMod(EntityAccess)"
	case "heartbeat":
		// 工程唯一的心跳驱动（game/heartbeat/driver_gen.go），实体组件经它 NewHost。
		return "kitheartbeat.NewMod(EntityAccess, " + heartbeatDriverAlias + ".Driver)"
	case "saga":
		if len(m.Sagas) == 0 {
			return "kitsaga.NewMod()"
		}
		definitions := make([]string, 0, len(m.Sagas))
		for _, sagaName := range m.Sagas {
			definitions = append(definitions, "saga"+safeIdent(sagaName)+".Definitions()")
		}
		return "kitsaga.NewMod(kitsaga.CombineDefinitions(" + strings.Join(definitions, ", ") + ")...)"
	default:
		return modCatalog[name].Constructor
	}
}

func renderService(pkg, name string) string {
	return fmt.Sprintf(`package %s

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

type Service struct{}

func New() *Service { return &Service{} }
func (*Service) Name() app.ServiceName { return app.ServiceName(%q) }
func (*Service) Init(*app.Registry) error { return nil }
func (*Service) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (*Service) Shutdown(context.Context) error { return nil }

var _ app.Service = (*Service)(nil)
`, pkg, name)
}

// renderServiceManagers emits the business-owned manager list. It is created
// once and never overwritten, like service.go: which in-memory singletons a
// service starts is a business decision, and ManagerMod only owns the
// lifecycle. Ordering comes from each manager's DependsOn, so this list is
// registration order, not start order.
func renderServiceManagers(pkg string) string {
	return fmt.Sprintf(`package %s

import "github.com/tjbdwanghaibo/roost-core/framework/app"

// Managers returns the in-memory singleton managers this service starts.
//
// A manager is process-wide singleton logic with a Start/Stop lifecycle and no
// persistent state of its own — a scene registry, a routing table, a cache.
// Add yours here; ManagerMod starts them in dependency order (declare
// app.ManagerDependencyProvider to order one after another) and stops them in
// reverse. Registering after startup is refused, so this is the one place.
func Managers() []app.IManager {
	return []app.IManager{
		// mymgr.Mgr,
	}
}
`, pkg)
}

// App 单实例锁（app.Singleton，见 framework/app/singleton.go）的生成规则。
//
// 生成值与 app/singleton.go 的默认值相同（15s / 3s / 5s / 2×ttl），满足 App 启动检查（singleton.* 的声明）钉住的三条关系：
// renew_interval ≤ guard、2×renew_interval ≤ ttl − guard、startup_wait ≥ ttl + 2×renew_interval。
const (
	generatedSingletonTTL           = 15 * time.Second
	generatedSingletonRenewInterval = 3 * time.Second
	generatedSingletonGuard         = 5 * time.Second
	// generatedSingletonStartupWait 是 singleton.startup_wait 的生成值（2×ttl）；部署侧的启动等待
	// （k8s startupProbe、shell HEALTH_ATTEMPTS、compose start_period）按它覆盖。
	generatedSingletonStartupWait = 2 * generatedSingletonTTL
)

// serviceSingletonEnabled 报告生成器是否给这个服务的配置写 singleton.enabled: true：resolved mods 含
// dataengine。带 DataEngine 的服务按 sid 划分 WAL 目录，设计上就是每个 sid 一个写者，
// 打开不改变部署形态；其他服务是否按 sid 多副本部署生成器不知道，写 enabled: false，由应用自己打开。
func serviceSingletonEnabled(m Manifest, service string) bool {
	return contains(serviceConfigMods(m, service), "dataengine")
}

// serviceConfigMods 是一个服务的进程实际注册的 Mod（共享 Mod 加 effectiveServiceMods，按依赖解析）：
// 生成服务配置（renderServiceConfig）、给后加的 Mod 追加配置段（appendModConfigSections）与 doctor 的
// config-sections 检查都按这一份算。
func serviceConfigMods(m Manifest, service string) []string {
	mods, _ := resolveMods(append(append([]string{}, m.SharedMods...), effectiveServiceMods(m, service)...))
	return mods
}

// serviceConfigSections 是生成器为一个服务的当前 Mod 写进服务配置的配置段，按写入顺序：每个 Mod 的起始段
// （modConfigSection）、托管框架服务的段、带 DataEngine 的服务的单实例锁连接用 redis: 段与打开的 singleton: 段。
// 同一个顶层键只出现一次。renderServiceConfig 按它写新工程的配置，doctor 的 config-sections 检查按它找
// 已有配置里缺的段（只读，见 checkConfigSections）。不带 DataEngine 的服务的 singleton: 段（enabled: false）
// 不在其中：缺这一段与写 false 效果相同。
func serviceConfigSections(m Manifest, service string) []string {
	var sections []string
	seen := map[string]bool{}
	add := func(section string) {
		var kept strings.Builder
		for _, block := range topLevelConfigBlocks(section) {
			if block.key != "" && seen[block.key] {
				continue
			}
			seen[block.key] = block.key != ""
			kept.WriteString(block.text)
		}
		if kept.Len() > 0 {
			sections = append(sections, kept.String())
		}
	}
	for _, name := range serviceConfigMods(m, service) {
		add(modConfigSection(name, m.Project.Name))
	}
	if _, hosted := frameworkCatalog[m.Services[service].Framework]; hosted {
		add(frameworkConfigSection(m.Services[service].Framework, m.Project.Name))
	}
	if serviceSingletonEnabled(m, service) {
		// 只补 redis: 配置段给单实例锁的连接用，不把 Redis Mod 强加给服务。
		add(modConfigSection("redis", m.Project.Name))
		add(renderSingletonConfig(m.Project.Name, true))
	}
	return sections
}

// projectInstallsSingleton 报告 bootstrap 是否安装 kitredis.SingletonStore：项目里有 redis Mod（任何服务
// 都可以在配置里手工打开 singleton，bootstrap 是生成文件、用户改不了），或有服务默认打开
// （dataengine 服务没有 redis Mod 时同样要装，否则启动时 fail-closed）。
func projectInstallsSingleton(m Manifest) bool {
	all := allProjectMods(m)
	return contains(all, "redis") || contains(all, "dataengine")
}

// renderSingletonConfig 是服务配置里的 singleton: 段。打开的段要求同一文件有 redis: 段
// （kitredis.SingletonStore 从它建自己的连接），renderServiceConfig 与 appendModConfigSections 负责补上。
func renderSingletonConfig(project string, enabled bool) string {
	var b strings.Builder
	b.WriteString("singleton:\n")
	b.WriteString("  # One process per server type + sid: before any Mod starts, the App takes the Redis key\n")
	b.WriteString("  # <key_prefix>:<server_type>:<sid> (connecting with this file's redis.* settings), renews it,\n")
	b.WriteString("  # stops the process when it is lost, and releases it after every Mod has stopped.\n")
	if enabled {
		b.WriteString("  # On by default for a service with the Data Engine (one WAL writer per sid). The App refuses\n")
		b.WriteString("  # to start unless renew_interval <= guard, 2 x renew_interval <= ttl - guard and\n")
		b.WriteString("  # startup_wait >= ttl + 2 x renew_interval. Raising startup_wait: raise the deployment's\n")
		b.WriteString("  # start-up allowance with it (k8s startupProbe, compose start_period, HEALTH_ATTEMPTS).\n")
	} else {
		b.WriteString("  # Off: the generator cannot tell whether this service runs more than one process per sid.\n")
		b.WriteString("  # Turning it on needs redis.addr or redis.cluster_addrs in this file.\n")
	}
	fmt.Fprintf(&b, "  enabled: %t\n", enabled)
	fmt.Fprintf(&b, "  key_prefix: roost:%s:singleton\n", project)
	fmt.Fprintf(&b, "  ttl: %s\n  renew_interval: %s\n  guard: %s\n  startup_wait: %s\n",
		seconds(generatedSingletonTTL), seconds(generatedSingletonRenewInterval), seconds(generatedSingletonGuard),
		seconds(generatedSingletonStartupWait))
	return b.String()
}

// renderBusinessTimeConfig 是服务配置里的 time: 段：App 声明里标了 example 的业务时区与每日重置时刻
// （time.business_timezone / time.business_day_start），注释取声明的 help。开发配置与生产示例
// 都写明，缺省 UTC 00:00；同一工程各服务必须相同（shared，config check --all 检查）。
func renderBusinessTimeConfig() string {
	var keys []configschema.Key
	for _, key := range kitConfigSchemas["app"].Keys {
		if strings.HasPrefix(key.Name, "time.") && key.Starter {
			keys = append(keys, key)
		}
	}
	return "# Business days and daily resets follow this zone and wall-clock time, not the machine's TZ\n" +
		"# (production: the zone the game's day follows, e.g. Asia/Shanghai); every service of the\n" +
		"# project writes the same two values.\n" +
		configschema.Schema{Keys: keys}.StarterYAML(nil)
}

func renderServiceConfig(m Manifest, service string, production bool) string {
	var b strings.Builder
	b.WriteString("# Generated starter configuration; application-owned after project creation.\n")
	b.WriteString("sid: 1000\n")
	b.WriteString("log:\n  level: info\n  json: true\n  stdout: true\n  file: true\n  dir: log\n")
	b.WriteString(renderBusinessTimeConfig())
	b.WriteString(renderShutdownConfig(serviceShutdownPlan(m, service)))
	for _, section := range serviceConfigSections(m, service) {
		b.WriteString(section)
	}
	if !serviceSingletonEnabled(m, service) {
		b.WriteString(renderSingletonConfig(m.Project.Name, false))
	}
	if !production {
		return localizeDevConfig(m, service, b.String())
	}
	return productionizeConfig(b.String())
}

// localizeDevConfig 给开发配置里按服务区分的地址换上本服务的端口：开发配置集的服务在同一台机器上一起运行
// （deploy/dev/run.sh），ops 端口是 opsPort，etcd.advertise_addr 的端口是 advertisePort（同一套服务序号）。
// 新建工程（renderServiceConfig）与之后 add mod 追加的段（appendModConfigSections）都经这里，规则只有一处。
// 生产示例每个服务一个容器，保持 9100 / CHANGE_ME:9000。
func localizeDevConfig(m Manifest, service, text string) string {
	text = opsAddrLine.ReplaceAllString(text, fmt.Sprintf("${1}127.0.0.1:%d", opsPort(m, service)))
	return advertiseAddrLine.ReplaceAllString(text, fmt.Sprintf("${1}127.0.0.1:%d", advertisePort(m, service)))
}

// advertiseAddrLine 匹配生成的 etcd.advertise_addr 行（声明的 example 127.0.0.1:9000）。
var advertiseAddrLine = regexp.MustCompile(`(?m)^(  advertise_addr: )127\.0\.0\.1:9000$`)

// productionizeConfig turns a starter (loopback, single replica, local WAL)
// configuration text into the production example: every address becomes a
// CHANGE_ME the operator must fill, ops listens on all interfaces, replicated
// stores get three replicas, logs go to stdout only. It is applied to the
// whole rendered file at project creation and to each section a Mod added
// later appends (appendModConfigSections), so the two never diverge.
// opsAddrLine matches the generated ops.addr line (the only top-level section
// key "addr" whose value is the 9100 ops port), whatever help comments the
// declaration puts above it.
var opsAddrLine = regexp.MustCompile(`(?m)^(  addr: )(?:127\.0\.0\.1|CHANGE_ME):9100$`)

// streamReplicasLine matches a line whose key is exactly replicas with value 1.
var streamReplicasLine = regexp.MustCompile(`(?m)^([ \t]*)replicas: 1$`)

func productionizeConfig(value string) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "127.0.0.1", "CHANGE_ME"), "localhost", "CHANGE_ME")
	value = opsAddrLine.ReplaceAllString(value, "${1}0.0.0.0:9100")
	// 只改独占一行的 replicas: 1（syncbus / effects / saga 的流副本数）。
	// remote_entity.snapshot_l2_tombstone_wait_replicas 是 L2 墓碑 WAIT 的副本数，不是流副本，不能跟着变成 3。
	value = streamReplicasLine.ReplaceAllString(value, "${1}replicas: 3")
	value = strings.ReplaceAll(value, "file: true", "file: false")
	return productionWALDir(value)
}

// dataEngineWALSection 匹配 dataengine 段到其中 wal: 子段这一行为止：中间每行都属于 dataengine 段
// （缩进行、注释行或空行），碰到下一个顶层键就不再匹配。
var dataEngineWALSection = regexp.MustCompile(`(?m)^dataengine:\n(?:(?:  .*|#.*)?\n)*?  wal:\n`)

// productionWALDir 给生产示例的 dataengine.wal 写上 dir: /var/lib/roost/wal。开发配置不写 dir，运行期取
// data/wal/dataengine/<sid>，同一工作目录下不同 sid 的进程各用各的目录；生产每个实例一个容器 / Pod / systemd 单元，
// WAL 必须落在挂给这个实例独占的持久卷上（生成的 compose、k8s、docker run 都挂 /var/lib/roost/wal），所以显式写。
// wal: 下已经有 dir 的（重复调用、使用者手改）不动。
func productionWALDir(value string) string {
	loc := dataEngineWALSection.FindStringIndex(value)
	if loc == nil {
		return value
	}
	rest := value[loc[1]:]
	for strings.HasPrefix(rest, "    #") {
		next := strings.IndexByte(rest, '\n')
		if next < 0 {
			break
		}
		rest = rest[next+1:]
	}
	if strings.HasPrefix(rest, "    dir:") {
		return value
	}
	return value[:loc[1]] + "    # 生产：本实例独占的持久卷（生成的 compose / k8s / docker run 都挂在这里），一个卷只给一个 sid\n" +
		"    dir: /var/lib/roost/wal\n" + value[loc[1]:]
}

// appendModConfigSections writes the configuration sections of the Mods a
// service gained between two manifests into that service's two config files.
// The files are rendered once at project creation and application-owned
// afterwards (renderServiceConfig), so a Mod added later — `add mod`, or
// `add saga` putting the saga Mod on a service — would otherwise run on its
// code defaults with nothing in the file to change. The game-demo's saga did
// exactly that: its default 8 GiB stream did not fit a developer JetStream
// and the process died at start with no saga: block to edit.
//
// Only top-level keys the file does not have are appended (a Mod's section
// may carry several, dataengine's has three), so hand edits stay and a
// repeat is a no-op. The production example gets the same transforms the
// whole file got, and the k8s Secret example that embeds the production
// example gets the same production text (keeping only what was rendered at
// creation would leave, e.g., the game-demo's Secret without its saga: block). A Secret example it cannot recognise is a WARN on warn.
// It returns the files it changed.
func appendModConfigSections(root string, before, after Manifest, service string, warn io.Writer) ([]string, error) {
	previous, current := serviceConfigMods(before, service), serviceConfigMods(after, service)
	var sections []string
	for _, name := range current {
		if section := modConfigSection(name, after.Project.Name); !contains(previous, name) && section != "" {
			sections = append(sections, section)
		}
	}
	// 后加进 dataengine 的服务：生成时写的 enabled: false 段翻成 true（turnGeneratedSingletonOn），
	// 没有 singleton: 段的文件追加打开的段，并补上单实例锁连接用的 redis: 段（已有则跳过）。
	// 否则这个服务会带着 enabled: false 静默不启用。
	turnOn := !serviceSingletonEnabled(before, service) && serviceSingletonEnabled(after, service)
	if turnOn {
		sections = append(sections, modConfigSection("redis", after.Project.Name), renderSingletonConfig(after.Project.Name, true))
	}
	if len(sections) == 0 {
		return nil, nil
	}
	// edit applies both steps to one config text; kept reports a singleton: block
	// that is not the generated one and was left as it is.
	edit := func(rel, body string, production bool) (string, bool) {
		appendedAny := false
		if turnOn {
			var kept bool
			body, appendedAny, kept = turnGeneratedSingletonOn(body, after.Project.Name, production)
			if kept {
				fmt.Fprintf(warn, "WARN: %s: the singleton: block was edited, left as it is; service %s now runs the Data Engine, set singleton.enabled: true there by hand\n", rel, service)
			}
		}
		added := sections
		if !production {
			added = make([]string, len(sections))
			for i, section := range sections {
				added[i] = localizeDevConfig(after, service, section)
			}
		}
		body, appended := appendMissingConfigBlocks(body, added, production)
		return body, appendedAny || appended
	}
	var changed []string
	for _, target := range []struct {
		rel        string
		production bool
	}{
		{"configs/service/config." + service + ".yaml", false},
		{"configs/service/config." + service + ".prod.example.yaml", true},
	} {
		path := filepath.Join(root, filepath.FromSlash(target.rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return changed, err
		}
		// 按文件原有的行尾追加。CRLF 检出的配置之前一律按 LF 追加新段，
		// 一份文件里两种行尾混用；Secret 示例（下面 editKubernetesSecretExampleConfig）
		// 早已按原行尾写回，这两份文件走同一对 lfText / restoreLineEndings。
		// 空文件与 LF 文件保持 LF。
		text, crlf := lfText(raw)
		body, appended := edit(target.rel, text, target.production)
		if !appended {
			continue
		}
		if err := writeAtomic(path, restoreLineEndings(body, crlf), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, target.rel)
	}
	secret, err := editKubernetesSecretExampleConfig(root, service, warn, func(config string) (string, error) {
		if body, appended := edit(kubernetesSecretExampleRel(service), config, true); appended {
			return body, nil
		}
		return config, nil
	})
	if err != nil {
		return changed, err
	}
	if secret != "" {
		changed = append(changed, secret)
	}
	return changed, nil
}

// turnGeneratedSingletonOn replaces the generated enabled: false singleton:
// block of one config text with the enabled one. A singleton: block that is
// not exactly the generated one is a hand edit and stays (kept); a text with
// no singleton: block is left to appendMissingConfigBlocks.
func turnGeneratedSingletonOn(body, project string, production bool) (out string, changed, kept bool) {
	off, on := renderSingletonConfig(project, false), renderSingletonConfig(project, true)
	if production {
		off, on = productionizeConfig(off), productionizeConfig(on)
	}
	if strings.Count(body, off) == 1 {
		return strings.Replace(body, off, on, 1), true, false
	}
	for _, block := range topLevelConfigBlocks(body) {
		if block.key == "singleton" {
			return body, false, !strings.Contains(block.text, "\n  enabled: true\n")
		}
	}
	return body, false, false
}

// appendMissingConfigBlocks appends to one config text the top-level blocks of
// sections it does not have yet, productionized for a production config. It
// reports whether it appended anything.
func appendMissingConfigBlocks(body string, sections []string, production bool) (string, bool) {
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	_, text := missingConfigBlocks(body, sections, production)
	return body + text, text != ""
}

// missingConfigBlocks 是 sections 里 body 还没有的顶层段：键（按出现顺序）与追加到 body 末尾的文本
// （生产配置按 productionizeConfig 改写）。“有没有”只看行首的 `<key>:`，段里的内容不比较——业务改过的段
// 照样算有。appendMissingConfigBlocks 按它追加，doctor 的 config-sections 检查按它只读报告。
func missingConfigBlocks(body string, sections []string, production bool) (keys []string, text string) {
	var appended strings.Builder
	has := func(key string) bool {
		all := body + appended.String()
		return strings.HasPrefix(all, key+":") || strings.Contains(all, "\n"+key+":")
	}
	for _, section := range sections {
		for _, block := range topLevelConfigBlocks(section) {
			if has(block.key) {
				continue
			}
			value := block.text
			if production {
				value = productionizeConfig(value)
			}
			appended.WriteString(value)
			keys = append(keys, block.key)
		}
	}
	return keys, appended.String()
}

type configBlock struct {
	key  string
	text string
}

// topLevelConfigBlocks splits a catalog config section into its top-level
// keys: a line that starts in column one begins a block, indented lines
// belong to the block above.
func topLevelConfigBlocks(section string) []configBlock {
	var blocks []configBlock
	for _, line := range strings.SplitAfter(section, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "#") && strings.Contains(line, ":") {
			blocks = append(blocks, configBlock{key: strings.TrimSpace(line[:strings.Index(line, ":")]), text: line})
			continue
		}
		if len(blocks) == 0 {
			blocks = append(blocks, configBlock{})
		}
		blocks[len(blocks)-1].text += line
	}
	return blocks
}

// manifestWithService is m with one service's spec replaced, without touching
// m's own map: the "before" view appendModConfigSections compares against.
func manifestWithService(m Manifest, service string, spec ServiceSpec) Manifest {
	services := make(map[string]ServiceSpec, len(m.Services))
	for name, value := range m.Services {
		services[name] = value
	}
	services[service] = spec
	m.Services = services
	return m
}

func sortedServiceNames(m Manifest) []string {
	names := make([]string, 0, len(m.Services))
	for name := range m.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func safeIdent(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' })
	for i := range parts {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func hasFeature(m Manifest, feature string) bool { return contains(m.Features, feature) }
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func renderMakefile(m Manifest) string {
	service := sortedServiceNames(m)[0]
	return fmt.Sprintf(`# Code generated by roost-codegen. DO NOT EDIT.
APP_NAME := %s
CODEGEN_MODULE := github.com/tjbdwanghaibo/roost-core/codegen
CORE_VERSION ?= %s
ROOST := go run $(CODEGEN_MODULE)/cmd/roost@$(CORE_VERSION)
SERVICE ?= %s
ENTITY ?=
COMPONENT ?=
HANDLER ?=
PROTOCOL ?=
NEST_HANDLER ?=
NEXT_WORKFLOW ?=
SID ?= 1000
CONFIG ?= configs/service/config.$(SERVICE).yaml
VERSION ?= dev
ENV ?= staging
IMAGE ?=
ENV_FILE ?= deploy/docker/.env.production
WORKLOAD ?=
COMMIT := $(shell git rev-parse --short HEAD)
BUILD_TIME := $(shell git show -s --format=%%cI HEAD)
DIRTY := $(shell git status --porcelain)
LDFLAGS := -X github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo.Version=$(VERSION) -X github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo.Commit=$(COMMIT) -X github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo.BuildTime=$(BUILD_TIME) -X github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo.Dirty=$(DIRTY)

LOADTEST_ENDPOINT ?= 127.0.0.1:7000
LOADTEST_COUNT ?= 10
LOADTEST_METRICS_ADDR ?= 127.0.0.1:9300
LOADTEST_ACCOUNT_NATS ?= nats://127.0.0.1:4222

.PHONY: help sync project-upgrade deps-update roost-up codegen-up next doctor fmt fmt-check vet glsvet test test-race build run loadtest generate generate-changed check-generated config-check config-check-all player-tcp-enable player-tcp-disable id-check ci cicd-check release-check image-build compose-check k8s-render k8s-check deploy-shell rollback-shell deploy-docker rollback-docker deploy-k8s rollback-k8s dev-up dev-down dev-logs dev-run dev-stop dev-status dev-smoke clean
.PHONY: new-service add-mod new-access new-transport new-module new-protocol new-entity new-component new-handler new-lifecycle new-endpoint new-skill new-table new-dao new-timer new-webroute new-errcode new-saga new-effect new-rpc

help:
	$(ROOST) help-make
sync:
	$(ROOST) project sync
project-upgrade:
	go run $(CODEGEN_MODULE)/cmd/roost@latest project upgrade --root . -core latest
deps-update:
	$(ROOST) project deps
roost-up:
	GOWORK=off go get -u ./...
	GOWORK=off go mod tidy
codegen-up:
	go install $(CODEGEN_MODULE)/cmd/roost@latest
next:
	$(ROOST) project next $(if $(NEXT_WORKFLOW),--workflow $(NEXT_WORKFLOW),)
doctor:
	$(ROOST) project doctor
fmt:
	go fmt ./...
fmt-check:
	$(ROOST) format check
vet:
	go vet ./...
glsvet:
	go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...
test:
	go test ./...
test-race:
	go test -race ./...
build:
	go build -ldflags "$(LDFLAGS)" ./...
run:
	go run . $(SERVICE) --sid $(SID) --config $(CONFIG)
loadtest:
	@test -d cmd/loadtest || { echo "cmd/loadtest is not part of this project: generate with 'roost project new ... -template game-demo', or add your own robots under cmd/loadtest"; exit 1; }
	go run ./cmd/loadtest -endpoint $(LOADTEST_ENDPOINT) -count $(LOADTEST_COUNT) -metrics-addr $(LOADTEST_METRICS_ADDR) -account-nats $(LOADTEST_ACCOUNT_NATS) $(LOADTEST_ARGS)
generate:
	$(ROOST) generate
generate-changed:
	$(ROOST) generate --changed
check-generated:
	$(ROOST) generate --check
config-check:
	$(ROOST) config check --service $(SERVICE)
config-check-all:
	$(ROOST) config check --all
player-tcp-enable:
	$(ROOST) config enable player-tcp --service $(SERVICE)
player-tcp-disable:
	$(ROOST) config disable player-tcp --service $(SERVICE)
id-check:
	$(ROOST) id check
new-service:
	$(ROOST) add service $(NAME) -mods $(MODS)
add-mod:
	$(ROOST) add mod $(NAME) -service $(SERVICE)
new-access:
	$(ROOST) add access $(NAME) -service $(SERVICE)
new-transport:
	$(ROOST) add transport $(NAME) -service $(SERVICE)
new-module:
	$(ROOST) add module $(NAME)
new-protocol:
	$(ROOST) add protocol $(NAME) -group $(GROUP) $(if $(HANDLER),--handler $(HANDLER),)
new-entity:
	$(ROOST) add entity $(NAME)
new-component:
	$(ROOST) add component $(NAME) $(if $(ENTITY),--entity $(ENTITY),)
new-handler:
	$(ROOST) add handler $(NAME) --entity $(ENTITY) --component $(COMPONENT)
new-lifecycle:
	$(ROOST) add lifecycle $(NAME) $(if $(ENTITY),--entity $(ENTITY),) -service $(SERVICE)
new-endpoint:
	$(ROOST) add endpoint $(NAME) --handler $(HANDLER) $(if $(PROTOCOL),--protocol $(PROTOCOL),) $(if $(NEST_HANDLER),--nest-handler $(NEST_HANDLER),) $(if $(LIFECYCLE),--lifecycle $(LIFECYCLE),)
new-skill:
	$(ROOST) add skill $(NAME)
new-table:
	$(ROOST) add table $(NAME)
new-dao:
	$(ROOST) add dao $(NAME) $(if $(ENTITY),--entity $(ENTITY),)
new-timer:
	$(ROOST) add timer --entity $(ENTITY) $(if $(COMPONENT),--component $(COMPONENT),) $(if $(SERVICE),--service $(SERVICE),)
new-webroute:
	$(ROOST) add webroute $(NAME)
new-errcode:
	$(ROOST) add errcode $(NAME)
new-saga:
	$(ROOST) add saga $(NAME) -service $(SERVICE) -steps $(STEPS) $(if $(NATIVE),-native $(NATIVE) $(if $(ENTITY),-entity $(ENTITY),) -component $(COMPONENT),)
new-effect:
	$(ROOST) add effect $(NAME) $(if $(CONSUMER),-consumer $(CONSUMER),)
new-rpc:
	$(ROOST) add rpc $(NAME) -service $(SERVICE)
ci: fmt-check vet glsvet test test-race check-generated config-check-all id-check
cicd-check: ci compose-check k8s-check
release-check: cicd-check
	@test -z "$(DIRTY)" || (echo "release requires a clean worktree"; exit 1)
	@echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || (echo "VERSION must be vMAJOR.MINOR.PATCH"; exit 1)
image-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_TIME=$(BUILD_TIME) -t $(APP_NAME):$(VERSION) .
compose-check:
	ROOST_IMAGE=ghcr.io/example/$(APP_NAME)@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ROOST_CONFIG_ROOT=$(CURDIR)/configs/service docker compose -f deploy/docker/docker-compose.prod.yaml config --quiet
	`+composeCheckEnv+`=1 go test -count=1 ./deploy/docker/
k8s-render:
	kubectl kustomize deploy/k8s/overlays/$(ENV)
k8s-check:
	kubectl kustomize deploy/k8s/overlays/staging >/dev/null
	kubectl kustomize deploy/k8s/overlays/production >/dev/null
deploy-shell:
	sudo sh deploy/shell/install.sh $(SERVICE) $(SID) $(VERSION) $(CONFIG)
rollback-shell:
	sudo sh deploy/shell/rollback.sh $(SERVICE) $(SID) $(VERSION)
deploy-docker:
	ROOST_IMAGE=$(IMAGE) ROOST_ENV_FILE=$(ENV_FILE) sh deploy/docker/deploy.sh
rollback-docker:
	ROOST_ENV_FILE=$(ENV_FILE) sh deploy/docker/rollback.sh
deploy-k8s:
	ENVIRONMENT=$(ENV) ROOST_IMAGE=$(IMAGE) sh deploy/k8s/deploy.sh
rollback-k8s:
	kubectl -n roost rollout undo $(WORKLOAD)
dev-up:
	docker compose -f deploy/dev/docker-compose.yaml up -d
dev-down:
	docker compose -f deploy/dev/docker-compose.yaml down
dev-logs:
	docker compose -f deploy/dev/docker-compose.yaml logs -f
dev-run:
	sh deploy/dev/run.sh start
dev-stop:
	sh deploy/dev/run.sh stop
dev-status:
	sh deploy/dev/run.sh status
dev-smoke:
	@test -d cmd/loadtest || { echo "cmd/loadtest is not part of this project (game-demo template); run your own client against the services started by make dev-run"; exit 1; }
	go run ./cmd/loadtest -endpoint $(LOADTEST_ENDPOINT) -count 2 -account-nats $(LOADTEST_ACCOUNT_NATS) $(LOADTEST_ARGS)
clean:
	go clean
`, m.Project.Name, generatorVersion(m), service)
}

// generatorVersion resolves the Makefile's CORE_VERSION, the release the
// generator runs at. The generator is roost-core/codegen/cmd/roost, so it is
// the project's versions.core: a project pinned to core vX regenerates with
// vX's generator, one tracking latest tracks it for both.
// `make generate CORE_VERSION=vX.Y.Z` still overrides one invocation.
func generatorVersion(m Manifest) string {
	if v := strings.TrimSpace(m.Versions.Core); v != "" {
		return v
	}
	return "latest"
}

func renderCI(m Manifest) string {
	return replaceDeployTokens(`# Code generated by roost-codegen. DO NOT EDIT.
name: ci
on:
  push:
    branches: [main, master]
  pull_request:
  workflow_dispatch:

concurrency:
  group: ci-${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true

permissions:
  contents: read

jobs:
  quality:
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, windows-latest]
    runs-on: ${{ matrix.os }}
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4
      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5
        with:
          go-version-file: go.mod
          cache: true
      - name: release module has no replace
        shell: bash
        run: |
          if grep -Eq '^[[:space:]]*replace([[:space:]]|\()' go.mod; then
            echo "project go.mod must use published modules without replace"
            exit 1
          fi
      - run: go mod download
      - run: go mod verify
      - name: validate GitHub Actions workflows
        run: go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
      - name: module files are tidy
        shell: bash
        run: |
          cp go.mod "${RUNNER_TEMP}/go.mod.before"
          cp go.sum "${RUNNER_TEMP}/go.sum.before"
          GOWORK=off go mod tidy
          cmp go.mod "${RUNNER_TEMP}/go.mod.before"
          cmp go.sum "${RUNNER_TEMP}/go.sum.before"
      - run: go test ./...
      - if: runner.os == 'Linux'
        run: go vet ./...
      - if: runner.os == 'Linux'
        run: go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...
      - if: runner.os == 'Linux'
        run: go test -race ./...

  generated-and-deployment:
    runs-on: ubuntu-latest
    timeout-minutes: 25
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4
      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go mod download
      - run: go mod verify
      - name: generated files and project configuration are current
        run: |
          make check-generated
          make config-check-all
          make id-check
      - name: deployment shell syntax
        run: |
          for script in deploy/shell/*.sh; do
            sh -n "$script"
          done
          shellcheck deploy/shell/*.sh deploy/docker/*.sh deploy/k8s/*.sh
      - name: production compose renders
        env:
          ROOST_IMAGE: ghcr.io/example/{{APP}}@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          ROOST_CONFIG_ROOT: ${{ github.workspace }}/configs/service
        run: docker compose -f deploy/docker/docker-compose.prod.yaml config --quiet
      - name: production compose resolves to the shape the deployment relies on
        env:
          `+composeCheckEnv+`: "1"
        run: go test -count=1 ./deploy/docker/
      - name: kubernetes manifests render
        run: |
          kubectl kustomize deploy/k8s/overlays/staging > "${RUNNER_TEMP}/staging.yaml"
          kubectl kustomize deploy/k8s/overlays/production > "${RUNNER_TEMP}/production.yaml"
          docker run --rm -i ghcr.io/yannh/kubeconform:v0.7.0-alpine -strict -summary < "${RUNNER_TEMP}/staging.yaml"
          docker run --rm -i ghcr.io/yannh/kubeconform:v0.7.0-alpine -strict -summary < "${RUNNER_TEMP}/production.yaml"
      - name: production container builds
        run: docker build --build-arg VERSION=ci -t {{APP}}:ci .
`, m)
}

func renderCompose(m Manifest) string {
	needed := map[string]bool{}
	for _, name := range allProjectMods(m) {
		if svc := modCatalog[name].DevService; svc != "" {
			needed[svc] = true
		}
	}
	// A service whose generated config turns the singleton on connects to Redis
	// for the lock even without the Redis Mod.
	for _, service := range sortedServiceNames(m) {
		if serviceSingletonEnabled(m, service) {
			needed["redis"] = true
		}
	}
	var b strings.Builder
	b.WriteString("# Code generated by roost-codegen. DO NOT EDIT.\nservices:\n")
	if needed["redis"] {
		b.WriteString("  redis:\n    image: redis:7.4-alpine\n    command: [\"redis-server\", \"--appendonly\", \"yes\", \"--appendfsync\", \"everysec\"]\n    ports: [\"6379:6379\"]\n    volumes: [\"redis-data:/data\"]\n")
	}
	if needed["mongo"] {
		b.WriteString("  mongo:\n    image: mongo:8.0\n    command: [\"mongod\", \"--replSet\", \"rs0\", \"--bind_ip_all\"]\n    ports: [\"27017:27017\"]\n    volumes: [\"mongo-data:/data/db\"]\n    healthcheck:\n      test: [\"CMD\", \"mongosh\", \"--quiet\", \"--eval\", \"db.adminCommand('ping').ok\"]\n      interval: 2s\n      timeout: 2s\n      retries: 30\n  mongo-init:\n    image: mongo:8.0\n    restart: \"no\"\n    depends_on:\n      mongo:\n        condition: service_healthy\n    entrypoint: [\"mongosh\", \"--host\", \"mongo:27017\", \"--quiet\", \"--eval\", \"try { rs.status() } catch (e) { rs.initiate({_id:'rs0',members:[{_id:0,host:'127.0.0.1:27017'}]}) }\"]\n")
	}
	if needed["nats"] {
		b.WriteString("  nats:\n    image: nats:2.11-alpine\n    command: [\"-js\", \"-sd\", \"/data\", \"-m\", \"8222\"]\n    ports: [\"4222:4222\", \"8222:8222\"]\n    volumes: [\"nats-data:/data\"]\n")
	}
	if needed["etcd"] {
		b.WriteString("  etcd:\n    image: quay.io/coreos/etcd:v3.5.21\n    command: [\"etcd\", \"--advertise-client-urls=http://0.0.0.0:2379\", \"--listen-client-urls=http://0.0.0.0:2379\"]\n    ports: [\"2379:2379\"]\n    volumes: [\"etcd-data:/etcd-data\"]\n")
	}
	b.WriteString("volumes:\n")
	for _, name := range []string{"redis", "mongo", "nats", "etcd"} {
		if needed[name] {
			fmt.Fprintf(&b, "  %s-data: {}\n", name)
		}
	}
	return b.String()
}

func renderDockerfile(m Manifest) string {
	allMods := allProjectMods(m)
	wal := contains(allMods, "dataengine")
	var buildDirs []string
	walRuntime := ""
	exposedPorts := "EXPOSE 9100"
	if playerTCPEnabled(m) {
		exposedPorts += " 7000"
	}
	if wal {
		buildDirs = append(buildDirs, "/out/wal")
		walRuntime = "COPY --chown=65532:65532 --from=build /out/wal /var/lib/roost/wal\nVOLUME [\"/var/lib/roost/wal\"]\n"
	}
	// stats_log.dir 的生成值是相对的 log，容器里解析为 /app/log；compose 的
	// read_only 与 k8s 的 readOnlyRootFilesystem 让它不可写，之前 statslog 又吞掉写错误，
	// 统计文件从不落盘。compose 与 k8s 在 /app/log 挂可写卷（renderProductionCompose /
	// renderKubernetesWorkload）；镜像里先放一个属于运行用户
	// （65532）的空目录，docker 第一次把命名卷挂上去时按镜像内容初始化卷的属主，否则新卷
	// 属于 root，非 root 进程仍然写不进去。
	statsLog := ""
	if contains(allMods, "statslog") {
		buildDirs = append(buildDirs, "/out/"+defaultStatsLogDir)
		statsLog = "COPY --chown=65532:65532 --from=build /out/" + defaultStatsLogDir + " /app/" + defaultStatsLogDir + "\n"
	}
	// configdata Mod 在启动时读 config_data.dir（生成值是相对路径
	// configs/data，按进程工作目录解析；运行阶段 WORKDIR /app，所以是 /app/configs/data）。
	// 之前运行阶段只拷二进制与 healthprobe，compose / k8s 也只挂 config.yaml，容器启动即
	// "configdata: stat dir configs/data: no such file or directory"，按重启策略反复重启。
	// 配置数据是 make generate 从 configs/table 产出、随代码版本走的发布内容（表结构由二进制里
	// 的生成代码注册），和环境相关的 config.yaml / 密钥不同，所以跟二进制一起进镜像：
	// staging / production 晋级的是同一份制品。根用户所有、只读；要按环境覆盖时把卷挂到
	// /app/configs/data（整目录遮盖镜像内容），热更新走 configdata Reload，读的仍是这个目录。
	configData := ""
	if contains(allMods, "configdata") {
		configData = "COPY --from=build /src/" + defaultConfigDataDir + " /app/" + defaultConfigDataDir + "\n"
	}
	// 活动组文件与 configs/data 一样是随代码版本发布的内容，协调器与 game 都按相对路径读它。
	if hostsActivityCoordinator(m) {
		configData += "COPY --from=build /src/" + activityGroupsFile + " /app/" + activityGroupsFile + "\n"
	}
	mkdirs := ""
	if len(buildDirs) > 0 {
		mkdirs = "RUN mkdir -p " + strings.Join(buildDirs, " ") + "\n"
	}
	return fmt.Sprintf(`# Code generated by roost-codegen. DO NOT EDIT.
ARG GO_VERSION=`+generatedGoVersion+`
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo.Version=${VERSION} -X github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo.Commit=${COMMIT} -X github.com/tjbdwanghaibo/roost-core/framework/app/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /out/%s . && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/healthprobe ./cmd/healthprobe
%s

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --chown=65532:65532 --from=build /out/%s /app/%s
COPY --chown=65532:65532 --from=build /out/healthprobe /app/healthprobe
%s%s%s%s
USER nonroot:nonroot
ENTRYPOINT ["/app/%s"]
`, m.Project.Name, mkdirs, m.Project.Name, m.Project.Name, configData, statsLog, walRuntime, exposedPorts, m.Project.Name)
}

func renderHealthProbe() string {
	return `// Code generated by roost-codegen. DO NOT EDIT.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	url := "http://127.0.0.1:9100/readyz"
	if len(os.Args) == 2 {
		url = os.Args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintln(os.Stderr, resp.Status)
		os.Exit(1)
	}
}
`
}

func renderProjectReadme(m Manifest) string {
	return fmt.Sprintf(`# %s

本项目由 roost-codegen 生成，项目声明位于 roost.yaml。

安装 roost 后先运行 roost env doctor；用 roost help 查看能力目录，用 roost version 确认当前版本。

## 第一级：完全新手，五分钟运行

第一次只阅读 [小白逐步操作手册](docs/BEGINNER_WORKBOOK.zh-CN.md)，然后反复执行 make next。工具会根据
真实文件只给出当前一个动作，不需要先在多份文档间选择。

    make deps-update
    make next
    # 执行 next 输出的命令或文件修改，然后再次 make next
    make test
    make run SERVICE=%s

更新 codegen 管理的工程模板使用 make project-upgrade；更新框架依赖使用 make deps-update；更新全部项目依赖使用 make roost-up；更新本机安装的 codegen 使用 make codegen-up。

确认 http://127.0.0.1:9100/readyz 返回成功后开始编写 Entity、DAO 和 Nest handler。

## 第二级：有经验开发者

阅读 [完整使用说明](docs/USAGE.zh-CN.md)，了解 Service/Mod 装配、生成命令、配置、测试与常用业务路径；修改项目声明前查阅 [roost.yaml 字段参考](docs/ROOST_YAML.zh-CN.md)。

## 第三级：框架实现与生产运维

- [实现说明](docs/IMPLEMENTATION.zh-CN.md)：Entity/Nest、Data Engine/WAL、Remote Entity、Saga、同步与技能边界。
- [部署说明](docs/DEPLOYMENT.zh-CN.md)：Shell/systemd、Docker、Kubernetes、升级回滚与故障演练。

提交和发布前执行：

    make ci
`, m.Project.Name, sortedServiceNames(m)[0])
}

func renderUsage(m Manifest) string {
	var services bytes.Buffer
	for _, name := range sortedServiceNames(m) {
		fmt.Fprintf(&services, "- %s: mods %s\n", name, strings.Join(m.Services[name].Mods, ","))
	}
	return fmt.Sprintf(`<!-- Code generated by roost-codegen. DO NOT EDIT. -->
# %s 使用说明

本文件由 roost-codegen 根据 roost.yaml 生成。

## 常用流程

1. 修改 roost.yaml 声明 Service、Kit Mod 和 feature。
2. 执行 make sync 更新受控装配文件。
3. 执行 make generate-changed 更新受影响的代码。
4. 提交前执行 make ci。

## 服务

%s
## 命令

- roost help / roost help <capability>：查看能力目录或指定能力的配置与示例。
- make run SERVICE=<name> SID=<id>：启动服务。
- make project-upgrade：使用最新 codegen 升级受控工程模板，并将 versions.core 策略设为 latest。
- make roost-up：执行 go get -u ./... 和 go mod tidy，更新整个项目依赖图。
- make codegen-up：安装最新 roost-codegen CLI。
- make deps-update：只按 roost.yaml 更新框架模块 roost-core。
- make new-saga NAME=<name> SERVICE=<service> STEPS=a,b,c [NATIVE=a,b ENTITY=<Entity> COMPONENT=<Component>]：Saga 定义与步骤运行时（saga/<name>/steps.go 的 StartSteps），NATIVE 里的步骤是 Nest 事务（原生步骤，各生成正向与补偿两个 Nest handler）；服务 Init 里 StartSteps、Shutdown 里 Drain（roost help saga，含单进程容量）。
- make new-effect NAME=<topic> CONSUMER=<service>：事务性 effect 的载荷、Emit 辅助与消费服务里的 SubscribeEffects 骨架；服务 Init 里启动、Shutdown 里 Drain（roost help effect）。
- make new-rpc NAME=<name> SERVICE=<owner>：脚手架一个本工程自己的跨进程服务（internal/rpc/<name>：//roost:rpc 接口、实现、owner Mod、生成的传输与装配两半），调用方在 roost.yaml 的 services.<caller>.uses_rpcs 列出它。
- make dev-up/dev-down：启动或停止所需基础设施。
- make dev-run/dev-stop/dev-status：在本机一次起 / 停 / 查看工程声明的全部服务进程（每个服务自己的 ops 端口，日志在 .dev/）；game-demo 工程随后可用 make dev-smoke 跑两个机器人走完整条链。
- make next：根据项目真实状态只显示一个当前动作；不需要背完整流程。
- make generate：在临时项目按依赖顺序生成并 tidy 新增 import，全部成功后原子提交；不会执行依赖升级。
- make doctor：检查清单、依赖、配置和生成状态；阶段验收使用 roost project doctor --workflow first-business。
- make config-check SERVICE=<name>：检查服务配置。
- make id-check：检查协议、实体、组件和错误码 ID。
- make check-generated：在临时目录验证生成结果，不污染工作区。

## 框架版本策略

versions.core 默认是 latest。框架只有 roost-core 一个模块（wiring、skill、生成器都是其中的包，versions.kit 等旧字段写了会被拒绝），roost project new/sync/deps 只把它作为直接依赖解析，随后执行 go mod tidy；Makefile 按 versions.core 运行生成器。go.mod 不支持 latest 查询值，所以它保存本次实际解析出的具体版本，roost.yaml 保存持续跟随 latest 的策略，同一次测试和发布使用确定的依赖图。

当前兼容下限为 core %s。明确版本低于下限会在生成前失败；latest 始终允许。解析与 tidy 在临时项目完成，只提交最终 go.mod/go.sum；失败不会修改原依赖文件，并发变化会拒绝覆盖。

## 安全约定

生产配置使用 config.<service>.prod.example.yaml 为模板。必须替换 CHANGE_ME，配置真实认证、TLS、超时和 advertise address；不要提交真实密钥。

- MongoDB 必须使用副本集；启动会拒绝 standalone，并固定 majority write concern 与 snapshot transaction read concern。
- /var/lib/roost/wal 必须挂载单写持久卷，同一个 SID 不得由两个实例同时挂载。
- JetStream effect 必须先由 Data Engine 在业务 Mongo transaction 中持久化到 outbox；发布和消费不能只依赖 MsgID 去重窗口。
- 部署前必须执行 make ci 并完成容器重启恢复演练。

## 继续阅读

- 当前一步：BEGINNER_WORKBOOK.zh-CN.md
- 零基础流程：QUICKSTART.zh-CN.md
- 第一条完整业务：FIRST_BUSINESS.zh-CN.md
- Entity/Component/DAO 实战：ENTITY_COMPONENT.zh-CN.md
- Entity 生命周期：ENTITY_LIFECYCLE.zh-CN.md
- Protocol 到 Nest：PROTOCOL_TO_NEST.zh-CN.md
- 技能系统（roost-core/skill）：SKILL.zh-CN.md
- 常见错误：TROUBLESHOOTING.zh-CN.md
- roost.yaml 所有字段：ROOST_YAML.zh-CN.md
- 实现原理：IMPLEMENTATION.zh-CN.md
- 生产部署：DEPLOYMENT.zh-CN.md
`, m.Project.Name, services.String(), minimumVersions.Core)
}

// renderGitattributes pins LF for every text file the generators write. A
// Windows checkout with core.autocrlf=true otherwise stores generated files
// as CRLF, and a byte-exact comparison (generate --check, servicerpc -check)
// reports them stale although nothing changed; the checks normalise newlines
// too, but the repository should not depend on that.
func renderGitattributes() string {
	return "# Code generated by roost-codegen. DO NOT EDIT.\n* text=auto\n*.go text eol=lf\n*.yaml text eol=lf\n*.yml text eol=lf\n*.json text eol=lf\n*.proto text eol=lf\n*.csv text eol=lf\n*.sh text eol=lf\n*.md text eol=lf\nMakefile text eol=lf\nDockerfile text eol=lf\n"
}
