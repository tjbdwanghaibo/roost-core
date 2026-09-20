package roost

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-codegen/internal/protocol"
)

func renderPlayerAccessRuntime() string {
	return generatedHeader + `
package player_agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/gateway"
)

var (
	ErrProtocolNotFound = errors.New("player access: protocol not found")
	ErrEncoderNotFound  = errors.New("player access: encoder not found")
	ErrRegistrySealed   = errors.New("player access: protocol registry is sealed")
)

// Context is the authenticated, transport-neutral boundary passed to generated
// protocol controllers. Entity access must continue through an injected Nest
// sender; controllers must not acquire EntityManager locks directly.
type Context struct {
	BaseCtx  context.Context
	Session  gateway.Session
	PlayerID int64
	MsgID    uint32
	Seq      uint32
}

func (ctx *Context) Context() context.Context {
	if ctx != nil && ctx.BaseCtx != nil {
		return ctx.BaseCtx
	}
	return context.Background()
}

// Response is a serialized protocol response. A TCP/WebSocket/QUIC adapter
// owns framing and writes this value to its authenticated session.
type Response struct {
	MessageID uint32
	Sequence  uint32
	Payload   []byte
}

type Decoder func([]byte) (any, error)
type Encoder func(any) ([]byte, error)
type HandlerFunc func(*Context, any) (any, error)
type Middleware func(HandlerFunc) HandlerFunc

type ProtocolDef struct {
	ReqID      uint32
	RespID     uint32
	DecodeReq  Decoder
	EncodeResp Encoder
	Handler    HandlerFunc
}

type registrySnapshot struct {
	defs     map[uint32]ProtocolDef
	encoders map[uint32]Encoder
}

// ProtocolRegistry is safe for concurrent dispatch and registration. Register
// protocols during app startup; Dispatch snapshots middleware before invoking
// business code and never holds the registry lock across a handler call.
type ProtocolRegistry struct {
	mu          sync.RWMutex
	defs        map[uint32]ProtocolDef
	encoders    map[uint32]Encoder
	middlewares []Middleware
	sealed      bool
	snapshot    atomic.Pointer[registrySnapshot]
}

func NewProtocolRegistry() *ProtocolRegistry {
	return &ProtocolRegistry{
		defs:        make(map[uint32]ProtocolDef),
		encoders:    make(map[uint32]Encoder),
		middlewares: []Middleware{RecoverMiddleware},
	}
}

func RecoverMiddleware(next HandlerFunc) HandlerFunc {
	return func(ctx *Context, request any) (response any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("player access: handler panic: %v", recovered)
			}
		}()
		return next(ctx, request)
	}
}

func (registry *ProtocolRegistry) Use(middlewares ...Middleware) error {
	if registry == nil {
		return errors.New("player access: protocol registry is nil")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.sealed {
		return ErrRegistrySealed
	}
	for _, middleware := range middlewares {
		if middleware != nil {
			registry.middlewares = append(registry.middlewares, middleware)
		}
	}
	return nil
}

// Seal pre-composes middleware once after startup registration. The production
// dispatch path then performs one atomic snapshot/map lookup with no registry
// lock or per-request slice allocation. Registration after sealing fails
// instead of racing live traffic.
func (registry *ProtocolRegistry) Seal() error {
	if registry == nil {
		return errors.New("player access: protocol registry is nil")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.sealed {
		return nil
	}
	for messageID, definition := range registry.defs {
		definition.Handler = chainHandler(definition.Handler, registry.middlewares)
		registry.defs[messageID] = definition
	}
	definitions := make(map[uint32]ProtocolDef, len(registry.defs))
	for messageID, definition := range registry.defs {
		definitions[messageID] = definition
	}
	encoders := make(map[uint32]Encoder, len(registry.encoders))
	for messageID, encoder := range registry.encoders {
		encoders[messageID] = encoder
	}
	registry.middlewares = nil
	registry.sealed = true
	registry.snapshot.Store(&registrySnapshot{defs: definitions, encoders: encoders})
	return nil
}

func chainHandler(handler HandlerFunc, middlewares []Middleware) HandlerFunc {
	for index := len(middlewares) - 1; index >= 0; index-- {
		handler = middlewares[index](handler)
	}
	return handler
}

func (registry *ProtocolRegistry) Register(definition ProtocolDef) error {
	if registry == nil {
		return errors.New("player access: protocol registry is nil")
	}
	if definition.ReqID == 0 || definition.DecodeReq == nil || definition.Handler == nil {
		return fmt.Errorf("player access: protocol %d requires request id, decoder and handler", definition.ReqID)
	}
	if definition.RespID != 0 && definition.EncodeResp == nil {
		return fmt.Errorf("player access: response %d requires an encoder", definition.RespID)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.sealed {
		return ErrRegistrySealed
	}
	if _, exists := registry.defs[definition.ReqID]; exists {
		return fmt.Errorf("player access: duplicate request id %d", definition.ReqID)
	}
	if definition.RespID != 0 {
		if _, exists := registry.encoders[definition.RespID]; exists {
			return fmt.Errorf("player access: duplicate response id %d", definition.RespID)
		}
		registry.encoders[definition.RespID] = definition.EncodeResp
	}
	registry.defs[definition.ReqID] = definition
	return nil
}

func (registry *ProtocolRegistry) RegisterEncoder(messageID uint32, encoder Encoder) error {
	if registry == nil || messageID == 0 || encoder == nil {
		return fmt.Errorf("player access: message id and encoder are required")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.sealed {
		return ErrRegistrySealed
	}
	if _, exists := registry.encoders[messageID]; exists {
		return fmt.Errorf("player access: duplicate encoder id %d", messageID)
	}
	registry.encoders[messageID] = encoder
	return nil
}

func (registry *ProtocolRegistry) Encode(messageID uint32, value any) ([]byte, error) {
	if registry == nil {
		return nil, ErrEncoderNotFound
	}
	var encoder Encoder
	var exists bool
	if snapshot := registry.snapshot.Load(); snapshot != nil {
		encoder, exists = snapshot.encoders[messageID]
	} else {
		registry.mu.RLock()
		encoder, exists = registry.encoders[messageID]
		registry.mu.RUnlock()
	}
	if !exists {
		return nil, fmt.Errorf("%w: %d", ErrEncoderNotFound, messageID)
	}
	return encoder(value)
}

// Dispatch runs one authenticated request. It returns encoded bytes instead
// of writing a socket so transports remain application-owned and testable.
func (registry *ProtocolRegistry) Dispatch(ctx context.Context, session gateway.Session, messageID, sequence uint32, payload []byte) (*Response, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: registry is nil", ErrProtocolNotFound)
	}
	if session == nil {
		return nil, gateway.ErrUnauthenticated
	}
	principal := session.Principal()
	if !principal.Authenticated() {
		return nil, gateway.ErrUnauthenticated
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var definition ProtocolDef
	var exists bool
	var middlewares []Middleware
	if snapshot := registry.snapshot.Load(); snapshot != nil {
		definition, exists = snapshot.defs[messageID]
	} else {
		registry.mu.RLock()
		definition, exists = registry.defs[messageID]
		middlewares = append([]Middleware(nil), registry.middlewares...)
		registry.mu.RUnlock()
	}
	if !exists {
		return nil, fmt.Errorf("%w: %d", ErrProtocolNotFound, messageID)
	}
	request, err := definition.DecodeReq(payload)
	if err != nil {
		return nil, fmt.Errorf("player access: decode request %d: %w", messageID, err)
	}
	handler := definition.Handler
	if len(middlewares) > 0 {
		handler = chainHandler(handler, middlewares)
	}
	response, err := handler(&Context{BaseCtx: ctx, Session: session, PlayerID: principal.PlayerID, MsgID: messageID, Seq: sequence}, request)
	if err != nil {
		return nil, err
	}
	if definition.RespID == 0 || response == nil {
		return nil, nil
	}
	encoded, err := definition.EncodeResp(response)
	if err != nil {
		return nil, fmt.Errorf("player access: encode response %d: %w", definition.RespID, err)
	}
	return &Response{MessageID: definition.RespID, Sequence: sequence, Payload: encoded}, nil
}

func RegisterProtocol[Request any, ResponseValue any](
	registry *ProtocolRegistry,
	requestID uint32,
	responseID uint32,
	decode func([]byte) (Request, error),
	encode func(ResponseValue) ([]byte, error),
	handler func(*Context, Request) (ResponseValue, error),
) error {
	if decode == nil || handler == nil || responseID != 0 && encode == nil {
		return fmt.Errorf("player access: incomplete typed protocol %d", requestID)
	}
	var responseEncoder Encoder
	if responseID != 0 {
		responseEncoder = func(value any) ([]byte, error) {
			typed, ok := value.(ResponseValue)
			if !ok {
				return nil, fmt.Errorf("player access: response %d has type %T", responseID, value)
			}
			return encode(typed)
		}
	}
	return registry.Register(ProtocolDef{
		ReqID: requestID, RespID: responseID, EncodeResp: responseEncoder,
		DecodeReq: func(payload []byte) (any, error) { return decode(payload) },
		Handler: func(ctx *Context, value any) (any, error) {
			typed, ok := value.(Request)
			if !ok {
				return nil, fmt.Errorf("player access: request %d has type %T", requestID, value)
			}
			return handler(ctx, typed)
		},
	})
}

func RegisterNotify[Request any](registry *ProtocolRegistry, requestID uint32, decode func([]byte) (Request, error), handler func(*Context, Request) error) error {
	if decode == nil || handler == nil {
		return fmt.Errorf("player access: incomplete typed notify %d", requestID)
	}
	return registry.Register(ProtocolDef{
		ReqID: requestID,
		DecodeReq: func(payload []byte) (any, error) { return decode(payload) },
		Handler: func(ctx *Context, value any) (any, error) {
			typed, ok := value.(Request)
			if !ok {
				return nil, fmt.Errorf("player access: notify %d has type %T", requestID, value)
			}
			return nil, handler(ctx, typed)
		},
	})
}

func RegisterTypedEncoder[Value any](registry *ProtocolRegistry, messageID uint32, encode func(Value) ([]byte, error)) error {
	if encode == nil {
		return fmt.Errorf("player access: encoder %d is nil", messageID)
	}
	return registry.RegisterEncoder(messageID, func(value any) ([]byte, error) {
		typed, ok := value.(Value)
		if !ok {
			return nil, fmt.Errorf("player access: message %d has type %T", messageID, value)
		}
		return encode(typed)
	})
}
`
}

func renderPlayerAccessMod(manifest Manifest) string {
	return fmt.Sprintf(`%s
package player

import (
	"fmt"
	"sync/atomic"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	%q
	protocolbootstrap %q
)

const (
	Name app.ModName = "access.player"
	// WriteGateCapability is an OPTIONAL capability a service may publish to
	// have every player request checked before it reaches a handler.
	//
	// It exists for deployments with more than one process, where "this
	// player is served here" is a fact with an expiry date: a process whose
	// ownership lease has lapsed must stop acting on that player's requests,
	// and the boundary is the only place that covers every endpoint at once —
	// including endpoints added later. A project that publishes nothing under
	// this name is unaffected.
	WriteGateCapability app.ModName = "access.player.write_gate"
)

// WriteGate decides whether this process may act on a request at all. It is
// asked the message id as well as the player, because the gate's owner is the
// only one that knows which messages must work BEFORE the process holds
// anything — a login has to be able to run, since it is what takes ownership.
//
// Returning an error refuses the request; the error reaches the client
// through the usual coded-response path.
type WriteGate interface {
	AdmitMessage(playerID int64, messageID uint32) error
}

type Runtime struct {
	Protocols *player_agent.ProtocolRegistry
	Nest      corenest.Client
}

type Mod struct {
	runtime *Runtime
}

func NewMod() *Mod { return &Mod{} }
func (*Mod) Name() app.ModName { return Name }
func (*Mod) DependsOn() []app.ModName { return []app.ModName{"nest"} }
func (*Mod) Init(*viper.Viper) error { return nil }

func (mod *Mod) Provide(registry *app.Registry) error {
	if registry == nil {
		return fmt.Errorf("player access: app registry is nil")
	}
	nestClient, ok := app.Lookup[corenest.Client](registry, app.ModName("nest"))
	if !ok || nestClient == nil {
		return fmt.Errorf("player access: nest client is unavailable")
	}
	protocols := player_agent.NewProtocolRegistry()
	// The gate goes on before the protocols, so it wraps every one of them.
	// It is resolved lazily: this Mod provides before the services that
	// publish a gate have started, and a gate that appears later must still
	// take effect.
	if err := protocols.Use(writeGateMiddleware(registry)); err != nil {
		return err
	}
	if err := protocolbootstrap.RegisterPlayerProtocols(protocols, registry); err != nil {
		return err
	}
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
func writeGateMiddleware(registry *app.Registry) player_agent.Middleware {
	var cached atomic.Pointer[WriteGate]
	return func(next player_agent.HandlerFunc) player_agent.HandlerFunc {
		return func(ctx *player_agent.Context, request any) (any, error) {
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
			}
			return next(ctx, request)
		}
	}
}

var _ app.Mod = (*Mod)(nil)
`, generatedHeader, manifest.Project.Module+protocol.PlayerAgentImportSuffix, manifest.Project.Module+"/game/protocol_bootstrap")
}
