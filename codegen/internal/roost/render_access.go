package roost

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
)

func renderPlayerAccessMod(manifest Manifest) string {
	return fmt.Sprintf(`%s
package player

import (
	"fmt"
	"sync/atomic"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	%q
)

const (
	Name app.ModName = "access.player"
	// WriteGateCapability is an OPTIONAL capability a service may publish to
	// have every player request checked before it reaches a handler.
	//
	// It exists for deployments with more than one process, where "this
	// player is served here" is not true of every connection: a process must
	// not act on requests for a player it is not serving right now (in the
	// game-demo, one bound to another sid or whose copy is being unloaded),
	// and the boundary is the only place that covers every endpoint at once —
	// including endpoints added later. A project that publishes nothing under
	// this name is unaffected.
	WriteGateCapability app.ModName = "access.player.write_gate"
)

// WriteGate decides whether this process may act on a request at all. It is
// asked the message id as well as the player, because the gate's owner is the
// only one that knows which messages must work BEFORE the process serves the
// player — a login has to be able to run, since it is what takes the player
// into service.
//
// 返回 error 会拒绝派发并关闭当前 TCP 连接。需要稳定业务错误码时，
// 在 endpoint 构造响应，不用 WriteGate 的内部错误冒充协议响应。
type WriteGate interface {
	AdmitMessage(playerID int64, messageID uint32) error
}

type Runtime struct {
	Protocols *playeraccess.ProtocolRegistry
	Nest      corenest.Client
}

// ProtocolRegistrar在PB bootstrap之后、Seal之前接入原始Notify；错误中止启动。
type ProtocolRegistrar func(*playeraccess.ProtocolRegistry,*app.Registry) error

type Mod struct {
 registrars []ProtocolRegistrar
	runtime *Runtime
	requireWriteGate bool
}

%s

func NewMod(registrars ...ProtocolRegistrar) *Mod { return &Mod{registrars:append([]ProtocolRegistrar(nil),registrars...)} }
func (*Mod) Name() app.ModName { return Name }
func (*Mod) DependsOn() []app.ModName { return []app.ModName{"nest"} }
func (*Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(accessConfig{}) }
func (mod *Mod) Init(cfg *viper.Viper) error {
	var settings accessConfig
	if err := app.LoadConfig(cfg, &settings); err != nil { return err }
	mod.requireWriteGate = settings.RequireWriteGate
	return nil
}

func (mod *Mod) Provide(registry *app.Registry) error {
	if registry == nil {
		return fmt.Errorf("player access: app registry is nil")
	}
	nestClient, ok := app.Lookup[corenest.Client](registry, app.ModName("nest"))
	if !ok || nestClient == nil {
		return fmt.Errorf("player access: nest client is unavailable")
	}
	protocols := playeraccess.NewProtocolRegistry()
	// The gate goes on before the protocols, so it wraps every one of them.
	// It is resolved lazily: this Mod provides before the services that
	// publish a gate have started, and a gate that appears later must still
	// take effect.
	if err := protocols.Use(writeGateMiddleware(registry, mod.requireWriteGate)); err != nil {
		return err
	}
	// RegisterPlayerProtocols 在同包的 protocols_gen.go（协议生成器产出）。
	if err := RegisterPlayerProtocols(protocols, registry); err != nil {
		return err
	}
 for _,register:=range mod.registrars {if register!=nil {if err:=register(protocols,registry);err!=nil{return err}}}
	if err := protocols.Seal(); err != nil {
		return err
	}
	mod.runtime = &Runtime{Protocols: protocols, Nest: nestClient}
	return registry.Register(Name, mod.runtime)
}

func (*Mod) Start() error { return nil }
func (mod *Mod) Stop() { mod.runtime = nil }

// writeGateMiddleware asks the published gate, if there is one, before every
// handler. The resolved gate is cached once found; until then each request
// costs one capability lookup, which lasts only until the owning service has
// started.
func writeGateMiddleware(registry *app.Registry, required bool) playeraccess.Middleware {
	var cached atomic.Pointer[WriteGate]
	return func(next playeraccess.HandlerFunc) playeraccess.HandlerFunc {
		return func(ctx *playeraccess.Context, request any) (any, error) {
			gate := cached.Load()
			if gate == nil {
				if found, ok := app.Lookup[WriteGate](registry, WriteGateCapability); ok && found != nil {
					gate = &found
					cached.Store(gate)
				}
			}
			if gate != nil {
				if err := (*gate).AdmitMessage(ctx.PlayerID, ctx.MsgID); err != nil {
					return nil, err
				}
			} else if required {
				return nil, fmt.Errorf("player access: required write gate is not ready")
			}
			return next(ctx, request)
		}
	}
}

var _ app.Mod = (*Mod)(nil)
`, generatedHeader, protocol.PlayerAccessImport, renderPlayerAccessDeclaration())
}
