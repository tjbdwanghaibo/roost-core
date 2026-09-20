package roost

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjbdwanghaibo/roost-codegen/internal/servicerpc"
)

// rpcDir is where a project's own cross-process services live: one package
// per service under internal/rpc/, each holding the //roost:rpc interface,
// its implementation and owner Mod, and the two generated halves.
func rpcDir(name string) string { return "internal/rpc/" + toSnake(name) }

// rpcAlias is the import alias the bootstrap uses for an rpc package.
func rpcAlias(name string) string { return "rpc" + toPascal(name) }

// addRPC scaffolds a business RPC service owned by -service: the interface
// (with the ErrRequestInvalid the generated handlers need, from the
// manifest's errcode space), an implementation stub, the owner Mod that
// publishes the capability and registers the bus handlers, the generated
// transport and assembly halves, and the manifest entry that makes the
// bootstrap assemble the Mod into the owning process. Other services reach
// it by listing the name under services.<name>.uses_rpcs, which assembles
// the generated ClientMod there.
func addRPC(root string, m Manifest, options AddOptions) ([]string, error) {
	snake, pascal := toSnake(options.Name), toPascal(options.Name)
	if !validName(snake) {
		return nil, fmt.Errorf("invalid rpc name %q", options.Name)
	}
	if _, taken := frameworkCatalog[snake]; taken {
		return nil, fmt.Errorf("rpc name %q is a hosted framework service; pick another name", snake)
	}
	service := toSnake(options.Service)
	if service == "" {
		var business []string
		for _, name := range sortedServiceNames(m) {
			if !m.isFrameworkService(name) {
				business = append(business, name)
			}
		}
		if len(business) != 1 {
			return nil, fmt.Errorf("rpc requires -service when the project has multiple business services")
		}
		service = business[0]
	}
	spec, ok := m.Services[service]
	if !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	if m.isFrameworkService(service) {
		return nil, fmt.Errorf("service %q hosts a framework service and cannot own a business rpc", service)
	}
	for _, name := range sortedServiceNames(m) {
		if contains(m.Services[name].Rpcs, snake) {
			return nil, fmt.Errorf("rpc %q is already owned by service %q", snake, name)
		}
	}
	dir := rpcDir(snake)
	if _, err := os.Stat(filepath.Join(root, dir)); err == nil {
		return nil, fmt.Errorf("%s already exists", dir)
	}
	code, err := NextID(root, m, "errcode", "")
	if err != nil {
		return nil, fmt.Errorf("allocate ErrRequestInvalid code: %w", err)
	}

	files := map[string]string{
		dir + "/" + snake + ".go": renderRPCInterface(snake, pascal, code),
		dir + "/service.go":       renderRPCService(snake, pascal),
		dir + "/mod.go":           renderRPCMod(snake),
		dir + "/server_run.go":    renderRPCServerRun(snake),
	}
	rollbackPaths := make([]string, 0, len(files)+2)
	for rel := range files {
		rollbackPaths = append(rollbackPaths, rel)
	}
	rollbackPaths = append(rollbackPaths, dir+"/"+snake+"_rpc_gen.go", dir+"/"+snake+"_rpc_assembly_gen.go")
	backups, err := captureFiles(root, rollbackPaths)
	if err != nil {
		return nil, err
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	created := make([]string, 0, len(rollbackPaths)+2)
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := writeAtomic(path, []byte(body), 0o644); err != nil {
			return created, restoreFiles(root, backups, err)
		}
		created = append(created, rel)
	}
	// The transport and assembly halves, from the interface just written.
	if err := servicerpc.Run([]string{"-dir", filepath.Join(root, filepath.FromSlash(dir))}, io.Discard); err != nil {
		return created, restoreFiles(root, backups, fmt.Errorf("generate %s transport: %w", snake, err))
	}
	created = append(created, dir+"/"+snake+"_rpc_gen.go", dir+"/"+snake+"_rpc_assembly_gen.go")
	after, captureErr := captureFiles(root, rollbackPaths)
	if captureErr != nil {
		return created, restoreFiles(root, backups, captureErr)
	}
	spec.Rpcs = uniqueSorted(append(spec.Rpcs, snake))
	m.Services[service] = spec
	m.Features = uniqueSorted(append(m.Features, "rpc"))
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return created, restoreFilesIfCurrent(root, backups, after, err)
	}
	return append(created, "roost.yaml", "internal/bootstrap/generated.go"), nil
}

func renderRPCInterface(snake, pascal string, code int64) string {
	return fmt.Sprintf(`// Package %[1]s is a cross-process service this project owns. The %[2]s
// interface below is the contract; roost servicerpc generates the transport
// (%[1]s_rpc_gen.go: wire types, handler table, BusClient) and the assembly
// (%[1]s_rpc_assembly_gen.go: Server, OwnerCapabilities, ClientMod) from it,
// and service.go is the implementation. The owning process assembles Mod
// (roost.yaml: services.<owner>.rpcs); a process that calls it lists the name
// under services.<caller>.uses_rpcs, gets NewClientMod assembled, and reaches
// it with app.Lookup[%[2]s](registry, CapabilityName). Either way the caller
// looks up the interface, so moving the service into its own process changes
// no business code.
//
// This file is application code: generated once, yours afterwards. Edit the
// interface, run make generate (or go generate ./...), implement the method.
package %[1]s

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/errcode"
)

//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir .

// ErrRequestInvalid is what the generated handlers answer a request they
// cannot decode with. The code is allocated from the manifest's errcode space
// (roost id check knows about it); a caller sees it as a coded error, not as
// a server fault.
var ErrRequestInvalid = errcode.Define(%[3]d, "%[1]s_request_invalid", "%[1]s: request is invalid")

// Error maps an error to the code and reason a caller sees; the generated
// handlers fill the response status from it. errcode.ClientError knows every
// coded error (ErrRequestInvalid, the project's internal/errors) and collapses
// anything else to CodeInternal, so internals never cross the bus.
func Error(err error) (int32, string) { return errcode.ClientError(err) }

// %[2]s is what another process may ask of this service.
//
// Every method takes a context first and returns named results; parameters
// and results cross the bus as generated wire types, so keep them to exported
// fields of plain types (no time.Duration, no unexported fields, no channels).
// A method whose calls should all land on one instance carries
// //roost:rpc affinity=<param> — see roost help servicerpc.
//
//roost:rpc service_type=%[1]s capability=rpc.%[1]s
type %[2]s interface {
	// Ping is a placeholder round trip so the package compiles and can be
	// called on day one; replace it with the service's real methods.
	Ping(ctx context.Context, callerID int64, message string) (echo string, err error)
}
`, snake, pascal, code)
}

func renderRPCService(snake, pascal string) string {
	return fmt.Sprintf(`package %[1]s

import (
	"context"
	"fmt"
)

// Service implements %[2]s in the owning process. Application code: generated
// once, yours afterwards. It holds whatever the service needs (stores,
// clients to other services) — give New the arguments and wire them in the
// bootstrap's NewMod(New(...)) call.
type Service struct{}

// New builds the service.
func New() *Service { return &Service{} }

// Ping is the placeholder; replace it together with the interface method.
func (s *Service) Ping(_ context.Context, callerID int64, message string) (string, error) {
	if message == "" {
		return "", fmt.Errorf("%%w: message is empty", ErrRequestInvalid)
	}
	return fmt.Sprintf("pong %%d: %%s", callerID, message), nil
}

var _ %[2]s = (*Service)(nil)
`, snake, pascal)
}

func renderRPCMod(snake string) string {
	return fmt.Sprintf(`package %[1]s

import (
	"fmt"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/bus"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// Mod is the owner side of this rpc inside a business process. It publishes
// the capability twice (the interface for consumers in this process, the
// implementation under the owner-only name) and registers the bus handlers,
// so a process that assembled NewClientMod reaches this implementation.
//
// The process's business Service stays its app.Service; the generated Server
// in %[1]s_rpc_assembly_gen.go is for a process dedicated to this rpc and is
// not used by this Mod. Application code: generated once, yours afterwards.
type Mod struct {
	service  *Service
	registry *app.Registry
}

// NewMod wraps the implementation the bootstrap builds with New(...).
func NewMod(service *Service) *Mod { return &Mod{service: service} }

// Name is the capability name, so the Mod's name and what it publishes are
// one fact.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn names the Mod that publishes the bus.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModNats} }

func (m *Mod) Init(*viper.Viper) error {
	if m.service == nil {
		return fmt.Errorf("%[1]s mod: service is nil")
	}
	return nil
}

func (m *Mod) Provide(r *app.Registry) error {
	if r == nil {
		return fmt.Errorf("%[1]s mod: registry is nil")
	}
	m.registry = r
	return mods.RegisterAll(r, OwnerCapabilities(m.service)...)
}

// Start registers the handlers on the bus. Start rather than Provide because
// the bus is published by another Mod and every Mod's Provide runs before
// any Start.
func (m *Mod) Start() error {
	busClient, ok := app.Lookup[bus.IBus](m.registry, mods.ModBus)
	if !ok || busClient == nil {
		return fmt.Errorf("%[1]s mod: capability %%q not found; the process needs the nats mod", mods.ModBus)
	}
	return RegisterHandlers(busClient, m.service)
}

func (m *Mod) Stop() {}

var (
	_ app.Mod                   = (*Mod)(nil)
	_ app.ModDependencyProvider = (*Mod)(nil)
)
`, snake)
}

func renderRPCServerRun(snake string) string {
	return fmt.Sprintf(`package %[1]s

import "context"

// run is the generated Server's serve loop, for a process dedicated to this
// rpc (a.RegisterServer(..., NewServer(), ...)). The default has no periodic
// work; the business processes that assemble Mod do not call it.
func (s *Server) run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
`, snake)
}

// projectRPCDirs lists the rpc packages the manifest declares that exist on
// disk, for the generate pipeline.
func projectRPCDirs(root string, m Manifest) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, name := range sortedServiceNames(m) {
		for _, rpc := range m.Services[name].Rpcs {
			dir := rpcDir(rpc)
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err == nil {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

// rpcOwnerOf returns the service that owns the named rpc, if any.
func rpcOwnerOf(m Manifest, rpc string) (string, bool) {
	for _, name := range sortedServiceNames(m) {
		if contains(m.Services[name].Rpcs, rpc) {
			return name, true
		}
	}
	return "", false
}

var _ = strings.TrimSpace
