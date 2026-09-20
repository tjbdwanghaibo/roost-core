package roost

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	add(".gitignore", "bin/\ndist/\nlog/\n.dev/\n.roost-deploy/\n.env\ndeploy/docker/.env.*\n*.local.yaml\ndeploy/k8s/base/secret.*.local.yaml\n.idea/\n.vscode/\n", false)
	add("Makefile", renderMakefile(m), true)
	add("README.md", renderProjectReadme(m), false)
	add("docs/QUICKSTART.zh-CN.md", renderBeginnerQuickstart(m), true)
	add("docs/BEGINNER_WORKBOOK.zh-CN.md", renderBeginnerWorkbook(m), true)
	add("docs/ROOST_YAML.zh-CN.md", renderManifestGuide(), true)
	add("docs/ENTITY_COMPONENT.zh-CN.md", renderEntityComponentGuide(m), true)
	add("docs/FIRST_BUSINESS.zh-CN.md", renderFirstBusinessGuide(m), true)
	add("docs/ENTITY_LIFECYCLE.zh-CN.md", renderEntityLifecycleGuide(m), true)
	add("docs/PROTOCOL_TO_NEST.zh-CN.md", renderProtocolNestGuide(m), true)
	add("docs/PLAYER_ACCESS_TCP.zh-CN.md", renderPlayerTCPGuide(m), true)
	add("docs/SKILL.zh-CN.md", renderSkillGuide(m), true)
	add("docs/TROUBLESHOOTING.zh-CN.md", renderTroubleshootingGuide(m), true)
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
		if err := addGo("game/player_agent/runtime_gen.go", renderPlayerAccessRuntime(), true); err != nil {
			return nil, err
		}
		if err := addGo("internal/access/player/mod_gen.go", renderPlayerAccessMod(m), true); err != nil {
			return nil, err
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
		if err := addGo("internal/service/"+name+"/service.go", renderService(name), false); err != nil {
			return nil, err
		}
		if len(m.Services[name].Uses) > 0 {
			if err := addGo("internal/service/"+name+"/framework_clients_gen.go", renderFrameworkClients(m, name), true); err != nil {
				return nil, err
			}
		}
		serviceMods, _ := resolveMods(effectiveServiceMods(m, name))
		if contains(serviceMods, "manager") {
			if err := addGo("internal/service/"+name+"/managers.go", renderServiceManagers(name), false); err != nil {
				return nil, err
			}
		}
		add("configs/service/config."+name+".yaml", renderServiceConfig(m, name, false), false)
		add("configs/service/config."+name+".prod.example.yaml", renderServiceConfig(m, name, true), false)
	}
	if hasFeature(m, "config") {
		if err := addGo("configs/schema/doc.go", "package schema\n", false); err != nil {
			return nil, err
		}
		if err := addGo("configs/generated/doc.go", "package generated\n", false); err != nil {
			return nil, err
		}
		add("configs/data/_manifest.json", "{\n  \"version\": 1,\n  \"tables\": {}\n}\n", false)
	}
	featurePackages := map[string]string{
		"protocol": "protocol/def/doc.go", "entity": "game/entities/doc.go", "nest": "game/handler/doc.go",
		"event": "event/def/doc.go", "dao": "db/def/doc.go", "attribute": "game/gameplay/attribute/doc.go",
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
			body += "\ntype Service struct{}\n"
		}
		if err := addGo(path, body, false); err != nil {
			return nil, err
		}
		if feature == "attribute" {
			// The attribute generator's output leans on a framework contract
			// (AttrID, AttributeProfile, Snapshot, Container, …). Shipping
			// only an empty package meant `roost add attribute` produced code
			// that referenced types nobody defined (RR-20260918 / RR-20260917-06);
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
	if hasFeature(m, "event") {
		if err := addGo("event/doc.go", "package event\n", false); err != nil {
			return nil, err
		}
	}
	if hasFeature(m, "nettransport-quic") || hasFeature(m, "nettransport-kcp") || hasFeature(m, "nettransport-udp") {
		if err := addGo("internal/transport/generated.go", renderReplication(m), true); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func renderReplication(m Manifest) string {
	var b strings.Builder
	b.WriteString(generatedHeader + "\npackage transport\n\nimport (\n")
	b.WriteString("\t\"context\"\n")
	if hasFeature(m, "nettransport-udp") {
		b.WriteString("\t\"net\"\n")
	}
	b.WriteString("\tcoreentitysync \"github.com/tjbdwanghaibo/roost-core/entitysync\"\n")
	b.WriteString("\tcorerep \"github.com/tjbdwanghaibo/roost-core/statesync\"\n")
	b.WriteString("\tkitrep \"github.com/tjbdwanghaibo/roost-core/nettransport\"\n")
	b.WriteString("\tkitsync \"github.com/tjbdwanghaibo/roost-core/room\"\n)\n\n")
	b.WriteString("func AsyncConfig() kitnet.AsyncTransportConfig { return kitnet.DefaultAsyncTransportConfig() }\n\n")
	b.WriteString("type SessionResolver func(coreentitysync.SubscriberRef) (corestate.SessionID, error)\n\n")
	b.WriteString("func NewRoomSink(async *kitnet.AsyncTransport, resolve SessionResolver) (*kitroom.RoomTransportSink, error) {\n")
	b.WriteString("\tif resolve == nil { return nil, kitroom.ErrRoomSessionResolver }\n")
	b.WriteString("\treturn kitroom.NewRoomTransportSink(kitroom.RoomTransportSinkConfig{Transport: async, Sessions: kitroom.RoomSessionResolverFunc(func(_ context.Context, subscriber coreentitysync.SubscriberRef) (corestate.SessionID, error) { return resolve(subscriber) })})\n}\n\n")
	if hasFeature(m, "nettransport-quic") {
		b.WriteString("func NewQUIC() (*kitnet.AsyncTransport, *kitnet.QUICTransport, error) {\n\tprotocol := kitnet.NewQUICTransport(kitnet.QUICTransportConfig{})\n\tasync, err := kitnet.NewAsyncTransport(protocol, AsyncConfig())\n\treturn async, protocol, err\n}\n\n")
	}
	if hasFeature(m, "nettransport-kcp") {
		b.WriteString("func NewKCP() (*kitnet.AsyncTransport, *kitnet.KCPTransport, error) {\n\tprotocol, err := kitnet.NewKCPTransport(kitnet.DefaultKCPTransportConfig())\n\tif err != nil { return nil, nil, err }\n\tasync, err := kitnet.NewAsyncTransport(protocol, AsyncConfig())\n\treturn async, protocol, err\n}\n\n")
	}
	if hasFeature(m, "nettransport-udp") {
		b.WriteString("func NewUDP(conn net.PacketConn, reliable kitnet.ReliableSender) (*kitnet.AsyncTransport, *kitnet.UDPTransport, error) {\n\tprotocol, err := kitnet.NewUDPTransport(kitnet.UDPTransportConfig{PacketConn: conn})\n\tif err != nil { return nil, nil, err }\n\tcomposite := kitnet.CompositeTransport{Datagrams: protocol, Reliable: reliable}\n\tasync, err := kitnet.NewAsyncTransport(composite, AsyncConfig())\n\treturn async, protocol, err\n}\n")
	}
	return b.String()
}

// generatedGoVersion is the Go release line every generated artifact agrees
// on: the go.mod directive, the Dockerfile's builder image and the docs. It
// is the framework's own requirement — roost-core / roost-kit are go 1.27.0
// — so a consumer never ends up with a go.mod one toolchain ahead of the
// image that builds it ("go.mod requires go >= 1.27.0 (running go 1.25.14)").
// TestGeneratedGoVersionMatchesTheGeneratorsOwn pins it to this module's go.mod.
const generatedGoVersion = "1.27"

func renderGoMod(m Manifest) string {
	return fmt.Sprintf(`module %s

go `+generatedGoVersion+`.0

// roost.yaml owns the framework update policy. When it is "latest", this
// bootstrap file starts at the supported minimum and roost project deps
// resolves the direct framework modules together to their latest tags.
require (
	github.com/tjbdwanghaibo/roost-core %s
	github.com/tjbdwanghaibo/roost-kit %s
)
`, m.Project.Module,
		resolvedModuleVersion(m.Versions.Core, minimumVersions.Core),
		resolvedModuleVersion(m.Versions.Kit, minimumVersions.Kit))
}

// renderFrameworkDeps keeps optional framework-domain modules in go.mod even
// before business code imports them. This makes a freshly generated project
// go-mod-tidy clean while preserving the version baseline from roost.yaml.
func renderFrameworkDeps(m Manifest) string {
	if !m.usesFrameworkServices() {
		return generatedHeader + `
package frameworkdeps

import "github.com/tjbdwanghaibo/roost-core/skill"

// SkillProgram retains the skill runtime selected by roost.yaml without adding
// runtime initialization or requiring business code to import it immediately.
type SkillProgram = skill.Program
`
	}
	return generatedHeader + `
package frameworkdeps

import (
	"github.com/tjbdwanghaibo/roost-core/skill"
	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// SkillProgram retains the skill runtime selected by roost.yaml without adding
// runtime initialization or requiring business code to import it immediately.
type SkillProgram = skill.Program

// ServiceMetrics retains roost-kit/service, which the framework services this
// project hosts or calls are built from.
type ServiceMetrics = servicemetrics.Reporter
`
}

func renderMain(m Manifest) string {
	return fmt.Sprintf(`package main

import (
	"log/slog"
	"os"

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
		"github.com/tjbdwanghaibo/roost-core/app":           "",
		"github.com/tjbdwanghaibo/roost-core/app/buildinfo": "",
	}
	allMods := allProjectMods(m)
	// Persistence and Remote Entity constructors require instance-scoped
	// EntityAccess even when enabled transitively without generated handlers.
	// when a project enables them transitively (for example through Saga)
	// without generating Nest handlers.
	instanceRuntime := contains(allMods, "dataengine") || contains(allMods, "nest") || contains(allMods, "remote_entity")
	if instanceRuntime {
		imports["github.com/tjbdwanghaibo/roost-core/entity"] = ""
	}
	for _, name := range allMods {
		spec := modCatalog[name]
		imports[spec.ImportPath] = spec.Alias
	}
	services := sortedServiceNames(m)
	for _, name := range services {
		imports[m.Project.Module+"/internal/service/"+name] = "service" + safeIdent(name)
		if spec, ok := frameworkCatalog[m.Services[name].Framework]; ok {
			imports[frameworkServiceModule+"/"+spec.ImportPath()] = "svc" + spec.Package
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
	b.WriteString("\nfunc New() (*app.App, error) {\n")
	// Static registration first, before app.New: an entity builder registered
	// later than a Mod's Init is already too late for a Mod that resolves
	// entities in Provide.
	b.WriteString("\tif err := registry.RegisterAll(); err != nil { return nil, err }\n")
	fmt.Fprintf(&b, "\ta := app.New(%q, buildinfo.VersionString())\n", m.Project.Name)
	shared, _ := resolveMods(m.SharedMods)
	if len(shared) > 0 {
		b.WriteString("\ta.Mods(\n")
		for _, name := range shared {
			fmt.Fprintf(&b, "\t\t%s,\n", renderModConstructor(m, name, allMods, ""))
		}
		b.WriteString("\t)\n")
	}
	for _, name := range services {
		mods, _ := resolveMods(effectiveServiceMods(m, name))
		if spec, hosted := frameworkCatalog[m.Services[name].Framework]; hosted {
			// The roost-service Server is the process's Service; the owner Mod
			// is built from the project's collaborators file.
			args := make([]string, 0, len(spec.ModArgs))
			for _, arg := range spec.ModArgs {
				args = append(args, "service"+safeIdent(name)+"."+arg)
			}
			fmt.Fprintf(&b, "\ta.RegisterServer(app.ServiceName(svc%s.ServiceType), svc%s.NewServer()", spec.Package, spec.Package)
			for _, mod := range mods {
				fmt.Fprintf(&b, ",\n\t\t%s", renderModConstructor(m, mod, allMods, name))
			}
			var chain strings.Builder
			for _, call := range spec.ModChain {
				fmt.Fprintf(&chain, ".%s", strings.Replace(call, "(", "(service"+safeIdent(name)+".", 1))
			}
			fmt.Fprintf(&b, ",\n\t\tsvc%s.NewMod(%s)%s)\n", spec.Package, strings.Join(args, ", "), chain.String())
			continue
		}
		fmt.Fprintf(&b, "\ta.RegisterServer(app.ServiceName(%q), service%s.New()", name, safeIdent(name))
		for _, mod := range mods {
			fmt.Fprintf(&b, ",\n\t\t%s", renderModConstructor(m, mod, allMods, name))
		}
		used := append([]string(nil), m.Services[name].Uses...)
		sort.Strings(used)
		for _, target := range used {
			spec := frameworkCatalog[m.Services[target].Framework]
			fmt.Fprintf(&b, ",\n\t\tsvc%s.NewClientMod()", spec.Package)
		}
		// The project's own rpcs: the owner Mod wraps the implementation
		// (New() takes whatever the service needs; edit the call here), a
		// consumer gets the generated ClientMod.
		for _, rpc := range uniqueSorted(m.Services[name].Rpcs) {
			fmt.Fprintf(&b, ",\n\t\t%s.NewMod(%s.New())", rpcAlias(rpc), rpcAlias(rpc))
		}
		for _, rpc := range uniqueSorted(m.Services[name].UsesRpcs) {
			fmt.Fprintf(&b, ",\n\t\t%s.NewClientMod()", rpcAlias(rpc))
		}
		if access, enabled := m.Access["player"]; enabled && access.Service == name {
			b.WriteString(",\n\t\taccessplayer.NewMod()")
			if contains(access.Transports, "tcp") {
				b.WriteString(",\n\t\taccessplayertcp.NewMod()")
			}
		}
		b.WriteString(")\n")
	}
	b.WriteString("\treturn a, nil\n}\n")
	return b.String()
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
		return "kitnest.NewMod(EntityAccess)"
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

func renderService(name string) string {
	return fmt.Sprintf(`package %s

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/app"
)

type Service struct{}

func New() *Service { return &Service{} }
func (*Service) Name() app.ServiceName { return app.ServiceName(%q) }
func (*Service) Init(*app.Registry) error { return nil }
func (*Service) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (*Service) Shutdown(context.Context) error { return nil }

var _ app.Service = (*Service)(nil)
`, safeIdent(name), name)
}

// renderServiceManagers emits the business-owned manager list. It is created
// once and never overwritten, like service.go: which in-memory singletons a
// service starts is a business decision, and ManagerMod only owns the
// lifecycle. Ordering comes from each manager's DependsOn, so this list is
// registration order, not start order.
func renderServiceManagers(name string) string {
	return fmt.Sprintf(`package %s

import "github.com/tjbdwanghaibo/roost-core/app"

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
`, safeIdent(name))
}

func renderServiceConfig(m Manifest, service string, production bool) string {
	mods, _ := resolveMods(append(append([]string{}, m.SharedMods...), effectiveServiceMods(m, service)...))
	var b strings.Builder
	b.WriteString("# Generated starter configuration; application-owned after project creation.\n")
	b.WriteString("sid: 1000\n")
	b.WriteString("log:\n  level: info\n  json: true\n  stdout: true\n  file: true\n  dir: log\n")
	b.WriteString("shutdown:\n  total_timeout: 30s\n  serve_wait_timeout: 5s\n")
	seen := map[string]bool{}
	for _, name := range mods {
		if !seen[name] && modCatalog[name].Config != "" {
			b.WriteString(modCatalog[name].Config)
			seen[name] = true
		}
	}
	if spec, hosted := frameworkCatalog[m.Services[service].Framework]; hosted {
		b.WriteString(spec.ConfigFunc(m.Project.Name))
	}
	if !production {
		// One machine, every service at once: each gets its own ops port
		// (see opsPort). Production keeps 9100 — one process per container.
		return strings.Replace(b.String(), "ops:\n  enabled: true\n  addr: 127.0.0.1:9100",
			fmt.Sprintf("ops:\n  enabled: true\n  addr: 127.0.0.1:%d", opsPort(m, service)), 1)
	}
	return productionizeConfig(b.String())
}

// productionizeConfig turns a starter (loopback, single replica, local WAL)
// configuration text into the production example: every address becomes a
// CHANGE_ME the operator must fill, ops listens on all interfaces, replicated
// stores get three replicas, logs go to stdout only. It is applied to the
// whole rendered file at project creation and to each section a Mod added
// later appends (appendModConfigSections), so the two never diverge.
func productionizeConfig(value string) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "127.0.0.1", "CHANGE_ME"), "localhost", "CHANGE_ME")
	value = strings.Replace(value,
		"ops:\n  enabled: true\n  addr: CHANGE_ME:9100",
		"ops:\n  enabled: true\n  addr: 0.0.0.0:9100", 1)
	value = strings.ReplaceAll(value, "replicas: 1", "replicas: 3")
	value = strings.ReplaceAll(value, "file: true", "file: false")
	return strings.ReplaceAll(value, "dir: data/wal/dataengine", "dir: /var/lib/roost/wal")
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
// whole file got. It returns the files it changed.
func appendModConfigSections(root string, before, after Manifest, service string) ([]string, error) {
	previous, _ := resolveMods(append(append([]string{}, before.SharedMods...), effectiveServiceMods(before, service)...))
	current, _ := resolveMods(append(append([]string{}, after.SharedMods...), effectiveServiceMods(after, service)...))
	var sections []string
	for _, name := range current {
		if !contains(previous, name) && modCatalog[name].Config != "" {
			sections = append(sections, modCatalog[name].Config)
		}
	}
	if len(sections) == 0 {
		return nil, nil
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
		body := string(raw)
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		appended := false
		for _, section := range sections {
			for _, block := range topLevelConfigBlocks(section) {
				if strings.HasPrefix(body, block.key+":") || strings.Contains(body, "\n"+block.key+":") {
					continue
				}
				text := block.text
				if target.production {
					text = productionizeConfig(text)
				}
				body += text
				appended = true
			}
		}
		if !appended {
			continue
		}
		if err := writeAtomic(path, []byte(body), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, target.rel)
	}
	return changed, nil
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
CODEGEN_MODULE := github.com/tjbdwanghaibo/roost-codegen
CODEGEN_VERSION ?= %s
ROOST := go run $(CODEGEN_MODULE)/cmd/roost@$(CODEGEN_VERSION)
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
LDFLAGS := -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.Version=$(VERSION) -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.Commit=$(COMMIT) -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.BuildTime=$(BUILD_TIME) -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.Dirty=$(DIRTY)

LOADTEST_ENDPOINT ?= 127.0.0.1:7000
LOADTEST_COUNT ?= 10
LOADTEST_METRICS_ADDR ?= 127.0.0.1:9300
LOADTEST_ACCOUNT_NATS ?= nats://127.0.0.1:4222

.PHONY: help sync project-upgrade deps-update roost-up codegen-up next doctor fmt fmt-check vet glsvet test test-race build run loadtest generate generate-changed check-generated config-check config-check-all player-tcp-enable player-tcp-disable id-check ci cicd-check release-check image-build compose-check k8s-render k8s-check deploy-shell rollback-shell deploy-docker rollback-docker deploy-k8s rollback-k8s dev-up dev-down dev-logs dev-run dev-stop dev-status dev-smoke clean
.PHONY: new-service add-mod new-access new-transport new-module new-protocol new-entity new-component new-handler new-lifecycle new-endpoint new-skill new-event new-table new-dao new-webroute new-errcode new-saga new-rpc

help:
	$(ROOST) help-make
sync:
	$(ROOST) project sync
project-upgrade:
	go run $(CODEGEN_MODULE)/cmd/roost@latest project upgrade --root . -core latest -kit latest -codegen latest
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
	$(ROOST) add endpoint $(NAME) --handler $(HANDLER) $(if $(PROTOCOL),--protocol $(PROTOCOL),) $(if $(NEST_HANDLER),--nest-handler $(NEST_HANDLER),)
new-skill:
	$(ROOST) add skill $(NAME)
new-event:
	$(ROOST) add event $(NAME)
new-table:
	$(ROOST) add table $(NAME)
new-dao:
	$(ROOST) add dao $(NAME) $(if $(ENTITY),--entity $(ENTITY),)
new-webroute:
	$(ROOST) add webroute $(NAME)
new-errcode:
	$(ROOST) add errcode $(NAME)
new-saga:
	$(ROOST) add saga $(NAME) -service $(SERVICE) -steps $(STEPS)
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
`, m.Project.Name, codegenVersion(m), service)
}

// codegenVersion resolves the Makefile's CODEGEN_VERSION: an empty
// versions.codegen means "track latest" — projects pin only when they set an
// explicit version in roost.yaml (or override per-invocation via
// `make generate CODEGEN_VERSION=vX.Y.Z`).
func codegenVersion(m Manifest) string {
	if v := strings.TrimSpace(m.Versions.Codegen); v != "" {
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
	walBuild, walRuntime := "", ""
	exposedPorts := "EXPOSE 9100"
	if playerTCPEnabled(m) {
		exposedPorts += " 7000"
	}
	if wal {
		walBuild = "RUN mkdir -p /out/wal\n"
		walRuntime = "COPY --chown=65532:65532 --from=build /out/wal /var/lib/roost/wal\nVOLUME [\"/var/lib/roost/wal\"]\n"
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
    -ldflags "-s -w -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.Version=${VERSION} -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.Commit=${COMMIT} -X github.com/tjbdwanghaibo/roost-core/app/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /out/%s . && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/healthprobe ./cmd/healthprobe
%s

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --chown=65532:65532 --from=build /out/%s /app/%s
COPY --chown=65532:65532 --from=build /out/healthprobe /app/healthprobe
%s%s
USER nonroot:nonroot
ENTRYPOINT ["/app/%s"]
`, m.Project.Name, walBuild, m.Project.Name, m.Project.Name, walRuntime, exposedPorts, m.Project.Name)
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
- make project-upgrade：使用最新 codegen 升级受控工程模板，并将 core、kit、skill、codegen 策略设为 latest。
- make roost-up：执行 go get -u ./... 和 go mod tidy，更新整个项目依赖图。
- make codegen-up：安装最新 roost-codegen CLI。
- make deps-update：只按 roost.yaml 更新 core、kit、skill 三个框架模块。
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

core、kit、skill、codegen 默认都是 latest。roost project new/sync/deps 会把前三者作为直接依赖在一条 go get 命令中联合解析，随后执行 go mod tidy。go.mod 不支持 latest 查询值，所以它保存本次实际解析出的具体版本，roost.yaml 保存持续跟随 latest 的策略。这样既避免下游依赖把 core/kit 降级，也保证同一次测试和发布使用确定的依赖图。

当前兼容下限为 core %s、kit %s、codegen %s。明确版本低于下限会在生成前失败；latest 始终允许。解析与 tidy 在临时项目完成，只提交最终 go.mod/go.sum；失败不会修改原依赖文件，并发变化会拒绝覆盖。

## 安全约定

生产配置使用 config.<service>.prod.example.yaml 为模板。必须替换 CHANGE_ME，配置真实认证、TLS、超时和 advertise address；不要提交真实密钥。

- MongoDB 必须使用副本集；启动会拒绝 standalone，并固定 majority write concern 与 snapshot transaction read concern。
- /var/lib/roost/wal 必须挂载单写持久卷，同一个 SID 不得由两个实例同时挂载。
- JetStream effect 必须先由 Data Engine 在业务 Mongo transaction 中持久化到 outbox；发布和消费不能只依赖 MsgID 去重窗口。
- 发布顺序为 roost-codegen、roost-core、roost-kit；部署前必须执行 make ci 并完成容器重启恢复演练。

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
`, m.Project.Name, services.String(), minimumVersions.Core, minimumVersions.Kit, minimumVersions.Codegen)
}

// renderGitattributes pins LF for every text file the generators write. A
// Windows checkout with core.autocrlf=true otherwise stores generated files
// as CRLF, and a byte-exact comparison (generate --check, servicerpc -check)
// reports them stale although nothing changed; the checks normalise newlines
// too, but the repository should not depend on that.
func renderGitattributes() string {
	return "# Code generated by roost-codegen. DO NOT EDIT.\n* text=auto\n*.go text eol=lf\n*.yaml text eol=lf\n*.yml text eol=lf\n*.json text eol=lf\n*.proto text eol=lf\n*.csv text eol=lf\n*.sh text eol=lf\n*.md text eol=lf\nMakefile text eol=lf\nDockerfile text eol=lf\n"
}
