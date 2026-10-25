// Package physicsroom 承载进程内的瞬态物理房间。房间不持久化，所属进程消失即结束。
package physicsroom

import (
	"context"
	"time"

	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

var (
	ErrRequestInvalid = errcode.Define(640101, "physicsroom: invalid request", "")
	ErrMissing        = errcode.Define(640102, "physicsroom: room missing or stale", "")
	ErrCapacity       = errcode.Define(640103, "physicsroom: capacity exceeded", "")
	ErrMember         = errcode.Define(640104, "physicsroom: membership stale", "")
	ErrSequence       = errcode.Define(640105, "physicsroom: sequence mismatch", "")
	ErrStopping       = errcode.Define(640106, "physicsroom: stopping", "")
)

type Binding = gateway.Binding

func Error(err error) (int32, string) { return errcode.ClientError(err) }

type Ref struct {
	ID    string
	Epoch uint64
}
type Spec struct {
	Instance        string // Describe 返回的 owner 进程代际；旧进程的 Create 重试必须失败。
	ID              string // 可信调用方生成的唯一开房键；同键重试必须使用相同配置。
	Bodies          []physx.Body
	LifetimeSeconds int64
}
type Member struct {
	Room     Ref
	PlayerID int64
	Token    string
}
type Input struct {
	Sequence     uint64
	Acceleration physx.Vec3
	Jump         bool // 单次跳跃意图，服务器校验落地，不接受持续向上的加速度。
}
type Delivery struct {
	Room     Ref
	Sequence uint64
	Payload  []byte
}

// Commit 必须在业务 Nest Guard 中接受物理事实，等待提交完成后才返回。
// 失败会停止所属房间；原生物理不可回滚，不会重试失败 Tick。
type Commit func(context.Context, Ref, gamephysics.Frame) error

type Config struct {
	MaxRooms, MaxPlayers, MaxBodies  int
	MaxFrameBytes                    int
	InputTTL, MemberTTL, MaxLifetime time.Duration
	Radius, Margin                   float64
	Commit                           Commit
	Closed                           func(Ref) // Run 与 native 资源已结束后调用，用于释放房间 Entity。
	Metrics                          servicemetrics.Reporter
}

//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir . -emit transport

// Rooms 仅暴露给可信服务总线。Join 的 Binding 必须由 Game 已认证入口获取，不能采信客户端字段。
// 所有调用必须固定到 owner SID，禁止实例自动切换。Epoch 防止重建房间混入旧请求。
//
//roost:rpc service_type=physicsroom capability=service.physicsroom
type Rooms interface {
	Describe(ctx context.Context) (instance string, err error)
	Create(ctx context.Context, spec Spec) (room Ref, err error)
	Join(ctx context.Context, room Ref, binding Binding, bodyID uint64) (member Member, err error)
	Submit(ctx context.Context, member Member, input Input) (err error)
	Poll(ctx context.Context, member Member, ack uint64) (delivery Delivery, err error)
	Leave(ctx context.Context, member Member) (err error)
	Destroy(ctx context.Context, room Ref) (err error)
}

// DefaultConfig 给出有界的初始配置；容量上限不是压测承诺。
func DefaultConfig(commit Commit) Config {
	return Config{MaxRooms: 64, MaxPlayers: 20, MaxBodies: 256, MaxFrameBytes: 64 << 10, InputTTL: 250 * time.Millisecond, MemberTTL: 5 * time.Second, MaxLifetime: time.Hour, Radius: 12, Margin: 2, Commit: commit}
}
