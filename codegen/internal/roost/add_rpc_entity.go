package roost

import "fmt"

// roost add rpc -entity-service 的骨架（联盟服务方案 §2.3 写 RPC 契约、§2.10 第 4 项）。拥有者是带 Nest 的实体服务：
// 写经 Nest 落到它独占的实体上，调用方经总线调用。骨架只写框架约定，不写业务规则：
//
//   - 每个写方法带 requestID（调用方生成、重试时不变）与调用者身份（callerID、callerSID 来自调用方已鉴权的会话），
//     服务按 requestID 幂等；Result(target, requestID) 回答这笔请求的结局。
//   - target 是完整 Entity ID（kind 在 ID 里）。对已有实体的方法标 //roost:rpc route（路由键缺省是 target，
//     route=<param> 指定别的 int64 参数），客户端按实体的路由登记与所有权记录发往 owner、改投一次；创建类方法不标，
//     走队列组随机路由，它的 requestID 记在按稳定 ID 路由的实体上（方案 §2.3）。骨架的 Apply、Result 都标了 route，
//     所以拥有者必须托管 Remote 实体 kind（addRPC 检查）。
//   - 服务端前置检查：route 方法的生成 handler 先调 Service.CheckRoute（kitremoteentity.RPCRouteCheck）——实体不存在、
//     不是 owner、所有权读不了时不进实现；owner Mod 在 Start 里把 Remote Manager 与本进程 sid 交给 Service
//     （SetOwnership），再注册 handler。owner Mod 依赖 nats、nest、remote_entity，并经 OptionalDependsOn 排在起服
//     预加载（mods.ModRemoteEntityPreload）之后：恢复与预加载完成前请求得到 no responders（可重试，未执行）。
//   - 接收端处理预算 HandleBudget：普通 Bus RPC 不把调用方截止时间带到接收端，整次处理（进 Nest、等提交确认）
//     由接收端自己限时。
//   - 答复四类用框架的约定（阶段 1）：写的错误经 wiring/remoteentity.RPCWriteAnswer 转成答复——未执行可重试是
//     errcode.CodeRetryable、不是 owner 是 CodeRedirect（带 owner sid），结果未知原样返回；调用方用
//     servicerpc.Classify 分成 OK / Rejected / Retryable / Unknown。项目不再自己分配这两类错误码。
//
// 请求回执（requestID → 结果）放在被改的实体里、随业务修改同一事务提交，属于服务骨架（方案阶段 3），
// 这里只在注释里写出 handler 里的顺序。

func renderEntityRPCInterface(snake, pascal string, code int64) string {
	return fmt.Sprintf(`// Package %[1]s is an entity service this project owns: the entities it writes
// belong to the owning service (//roost:entity service=<owner>), and other
// processes change them only through the %[2]s interface below. roost servicerpc
// generates the transport (%[1]s_rpc_gen.go) and the assembly
// (%[1]s_rpc_assembly_gen.go) from it; service.go is the implementation.
//
// This file is application code: generated once, yours afterwards. Edit the
// interface, run make generate (or go generate ./...), implement the method.
package %[1]s

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir .

// ErrRequestInvalid answers a request the handlers cannot decode or that lacks
// a request id. The code is allocated from the manifest's errcode space.
//
// The other answers are the framework's (servicerpc.Classify on the caller):
// a business rejection is a coded error of the project's own; "not executed,
// retry with the same request id" is errcode.CodeRetryable; "not the owner"
// is errcode.CodeRedirect; a timeout or CodeInternal means the result is
// unknown — ask Result with the same request id, or send it again.
var ErrRequestInvalid = errcode.Define(%[3]d, "%[1]s_request_invalid", "%[1]s: request is invalid")

// Error maps an error to the code and reason a caller sees; the generated
// handlers fill the response status from it. errcode.ClientError knows every
// coded error (CodeRetryable / CodeRedirect included) and collapses anything
// else to CodeInternal.
func Error(err error) (int32, string) { return errcode.ClientError(err) }

// %[2]s is what another process may ask of this service.
//
// Every write method takes requestID first: the caller makes one per user
// action and keeps it across retries, and the service answers the same
// requestID with the same result (the NATS client itself may deliver a
// request more than once). callerID and callerSID come from the caller's
// authenticated session, never from client-supplied fields. A write answers
// the entity's new state version; show the result from the reply, and read
// mirrors afterwards no older than that version.
//
// target is the full entity id (roost id entity) of the entity a method acts
// on. A method on an existing entity carries //roost:rpc route: the call goes
// to the instance that owns target, found through the entity's route
// registration and ownership record, and follows one redirect (route=<param>
// names another int64 parameter). A create method carries no route and any
// instance may take it; anchor its request id on an entity routed by a stable
// id, since the new entity does not exist yet when a retry arrives.
//
//roost:rpc service_type=%[1]s capability=rpc.%[1]s
type %[2]s interface {
	// Apply is a placeholder write on the entity target; replace it with the
	// service's real write methods, each with the same first parameters.
	//
	//roost:rpc route
	Apply(ctx context.Context, requestID string, callerID int64, callerSID int32, target int64) (version uint64, err error)
	// Result answers what the write requestID on target did: found=false
	// means it was never executed (sending it again is safe); otherwise
	// outcome is 0 for success or the business rejection's code, and version
	// is the state version the write produced. (Not named code: the response
	// already carries the envelope's Code, and servicerpc refuses the clash.)
	//
	//roost:rpc route
	Result(ctx context.Context, target int64, requestID string) (found bool, outcome int32, version uint64, err error)
}
`, snake, pascal, code)
}

func renderEntityRPCService(snake, pascal string) string {
	return fmt.Sprintf(`package %[1]s

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	kitremoteentity "github.com/tjbdwanghaibo/roost-core/wiring/remoteentity"
)

// HandleBudget is how long the receiving side spends on one request. A plain
// bus RPC does not carry the caller's deadline to the receiver, so the handler
// bounds the whole handling (entering Nest, waiting for the commit to be
// confirmed) itself. When it runs out the write may already be committed: the
// answer stays a timeout (result unknown), never a plain failure.
const HandleBudget = 5 * time.Second

// Service implements %[2]s in the owning process. Application code: generated
// once, yours afterwards. Give New the generated Nest senders of the entities
// this service owns and wire them in the bootstrap's NewMod(New(...)) call.
// The owner Mod hands it the Remote ownership manager and this process's sid
// (SetOwnership) before the handlers are registered; until then every route
// check answers "not executed, retry".
type Service struct {
	owners   entity.RemoteOwnershipManager
	localSID int32
}

// New builds the service.
func New() *Service { return &Service{} }

// SetOwnership gives the service the Remote ownership manager and this
// process's sid. The owner Mod calls it in Start, before RegisterHandlers.
func (s *Service) SetOwnership(owners entity.RemoteOwnershipManager, localSID int32) {
	s.owners, s.localSID = owners, localSID
}

// CheckRoute implements RouteChecker: the generated handler of every route
// method calls it before the method (wiring/remoteentity.RPCRouteCheck). nil
// means this instance owns target (an entity owned here but not loaded yet is
// loaded and its route registered); otherwise it answers "entity not found"
// (no ownership record: a non-create request must not claim it), a redirect to
// the owner, or "not executed, retry".
func (s *Service) CheckRoute(ctx context.Context, target int64) error {
	return kitremoteentity.RPCRouteCheck(ctx, s.owners, s.localSID, target)
}

// Apply is the placeholder write. A real write method:
//
//  1. checks requestID / the arguments (ErrRequestInvalid);
//  2. calls the owned entity's generated Sync_ sender with ctx bounded by
//     HandleBudget;
//  3. inside the Nest handler, under the entity lock: looks up the receipt for
//     requestID and replays its result if present; otherwise checks the
//     caller's permission against the authoritative state, then writes the
//     change and the receipt in the same transaction and returns the new
//     state version (or a business rejection);
//  4. returns s.answer(ctx, target, err), so the caller can tell "not
//     executed" and "not the owner" from "result unknown".
func (s *Service) Apply(ctx context.Context, requestID string, callerID int64, callerSID int32, target int64) (uint64, error) {
	if requestID == "" || target == 0 {
		return 0, fmt.Errorf("%%w: request id and target are required", ErrRequestInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, HandleBudget)
	defer cancel()
	_, _ = callerID, callerSID
	return 0, s.answer(ctx, target, errors.New("%[1]s: Apply is not implemented"))
}

// Result is the placeholder receipt lookup: read the receipt for requestID on
// target (a Nest read inside the entity lock, or the authoritative store).
func (s *Service) Result(ctx context.Context, target int64, requestID string) (bool, int32, uint64, error) {
	if requestID == "" || target == 0 {
		return false, 0, 0, fmt.Errorf("%%w: request id and target are required", ErrRequestInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, HandleBudget)
	defer cancel()
	return false, 0, 0, s.answer(ctx, target, errors.New("%[1]s: Result is not implemented"))
}

// answer turns what a write on target returned into the RPC answer
// (wiring/remoteentity.RPCWriteAnswer): retryable when it was refused before
// anything committed, a redirect to the owner when this instance is not it,
// unchanged for success, business rejections and unknown results. It reads
// the ownership record, so it runs here in the RPC handler (I/O), never inside
// a Nest handler.
func (s *Service) answer(ctx context.Context, target int64, err error) error {
	return kitremoteentity.RPCWriteAnswer(ctx, s.owners, s.localSID, target, err)
}

var (
	_ %[2]s        = (*Service)(nil)
	_ RouteChecker = (*Service)(nil)
)
`, snake, pascal)
}

// renderEntityRPCMod 是 -entity-service 的 owner Mod：依赖 nats、nest、remote_entity；经 OptionalDependsOn 排在起服预加载
// 之后（没装预加载 Mod 时没有这条边）；Start 时把 Remote Manager 与本进程 sid 交给 Service，再在总线上注册 handler。
func renderEntityRPCMod(snake string) string {
	return fmt.Sprintf(`package %[1]s

import (
	"fmt"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Mod is the owner side of this rpc inside the entity service's process. It
// publishes the capability twice (the interface for consumers in this process,
// the implementation under the owner-only name) and registers the bus
// handlers, so a process that assembled NewClientMod reaches this
// implementation. Application code: generated once, yours afterwards.
type Mod struct {
	service  *Service
	registry *app.Registry
}

// NewMod wraps the implementation the bootstrap builds with New(...).
func NewMod(service *Service) *Mod { return &Mod{service: service} }

// Name is the capability name, so the Mod's name and what it publishes are
// one fact.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn names the bus, Nest and the Remote Mod: the handlers are
// registered in Start, after Nest (and the DataEngine recovery before it) has
// started, so a request that arrives while the process is still recovering
// finds no responder — not executed, and the caller retries with the same
// request id.
func (m *Mod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModNats, mods.ModNest, mods.ModRemoteEntity}
}

// OptionalDependsOn orders Start after the startup preload
// (//roost:entity preload=startup) when this process has one: no request is
// served before the owned entities are loaded and their routes registered.
func (m *Mod) OptionalDependsOn() []app.ModName {
	return []app.ModName{mods.ModRemoteEntityPreload}
}

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

// Start hands the service the Remote ownership manager and this process's sid
// (the route check needs both), then registers the handlers on the bus. Start
// rather than Provide because the bus is published by another Mod and every
// Mod's Provide runs before any Start.
func (m *Mod) Start() error {
	owners, ok := app.Lookup[entity.IRemoteEntityManager](m.registry, mods.ModRemoteEntity)
	if !ok || owners == nil {
		return fmt.Errorf("%[1]s mod: capability %%q not found; the owning service needs the remote_entity mod", mods.ModRemoteEntity)
	}
	var identity app.ServiceIdentity
	if err := app.LoadConfig(m.registry.Config(), &identity); err != nil {
		return fmt.Errorf("%[1]s mod: %%w", err)
	}
	m.service.SetOwnership(owners, identity.Sid)
	busClient, ok := app.Lookup[bus.IBus](m.registry, mods.ModBus)
	if !ok || busClient == nil {
		return fmt.Errorf("%[1]s mod: capability %%q not found; the process needs the nats mod", mods.ModBus)
	}
	return RegisterHandlers(busClient, m.service)
}

func (m *Mod) Stop() {}

var (
	_ app.Mod                           = (*Mod)(nil)
	_ app.ModDependencyProvider         = (*Mod)(nil)
	_ app.ModOptionalDependencyProvider = (*Mod)(nil)
)
`, snake)
}
