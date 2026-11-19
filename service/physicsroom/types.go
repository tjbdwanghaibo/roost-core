// Package physicsroom 承载进程内的瞬态物理房间。房间不持久化，所属进程消失即结束。
package physicsroom

import (
	"context"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
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
	// ErrLevel 标记“关卡流程在提交里出错”（预算用尽、事实次序冲突等）。Commit 的实现用它包住流程的错误，
	// 房间照常停止，只是指标记成 level_failed 而不是 room_failed。
	ErrLevel = errcode.Define(640107, "physicsroom: level flow failed", "")
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
	Motions         []gamephysics.MotionTrack
	Joints          []physx.Joint
	LifetimeSeconds int64
	// 以下字段给关卡流程用，可以全部为空。Flow 是 gameplay/level.Definition 的 JSON（tick 时间域），
	// 建房时与物理房间自带的能力、Config.Actions 登记的业务能力一起编译，编译不过就拒绝建房。
	// Players 是玩家刚体：Watch 为空的区域关注它们，出生点位置按刚体在这里的下标选，检查点与重生只对它们有效。
	Players      []uint64
	Regions      []gamephysics.Region
	Spawns       []gamephysics.Spawn
	DefaultSpawn string
	Flow         []byte
}
type Member struct {
	InputGeneration uint64
	Room            Ref
	PlayerID        int64
	Token           string
}
type Input struct {
	GrabTarget   uint64 // 按住抓取的目标；0 松手，距离/类型由服务器校验。
	Sequence     uint64
	Acceleration physx.Vec3
	Jump         bool // 单次跳跃意图，服务器校验落地，不接受持续向上的加速度。
}
type Delivery struct {
	Room     Ref
	Sequence uint64
	Payload  []byte
	// Level 是编码后的 RoomLevelView（流程的公开状态）；只在这个成员还没确认过当前版本时随帧带上，
	// 和 Payload 一起冻结、重发、确认。没有流程的房间永远为空。
	Level []byte
}

// Tick 是一次提交的全部内容：物理帧，加上这一帧要交给关卡流程的事实与动作回执。
// 没有流程的房间只有 Frame。
type Tick struct {
	Frame gamephysics.Frame
	// Program 只在有流程的房间的第一帧（Frame.Tick==1）非空：Commit 的实现据此给房间建流程组件。
	Program *level.Program
	// Facts 是区域与成员事实，Source 固定为 FactSource，Sequence 在房间内单调（可以跳号），已按投递次序排好。
	Facts []level.Event
	// Receipts 是已经执行完的动作的回执（物理动作与业务动作）。动作已被流程取消时回执作废，不算错误。
	Receipts []level.Receipt
}

// Settled 是提交成功后流程交回的结果。流程状态没变（或房间没有流程）时 Actions 与 View 都为空。
type Settled struct {
	// Actions 是流程当前全部待执行的动作（含此前已经交出过的），按 Sequence 升序；房间按 ID 去重。
	Actions []level.Effect
	// View 是流程的公开状态。
	View *level.View
	// StaleReceipts 是这次提交里因为动作已被取消而作废的回执数，只用于指标。
	StaleReceipts int
}

// ActionHandler 执行流程请求的业务动作（结算、发奖等物理房间自己不认识的能力）。
type ActionHandler interface {
	// Registry 声明业务能力与业务事件的名字和参数类型，与物理房间自带的声明合并后用来编译流程；重名时建房失败。
	// 本轮没有投递业务事件的入口：声明了的业务事件能通过编译，但不会被触发。
	Registry() level.Registry
	// Execute 执行一个动作并给出它的结果。必须按 e.ID 幂等：超时或返回错误表示结果未知，房间会带着同一个
	// e.ID 再次调用，直到拿到明确结果或房间结束。在独立的 goroutine 上调用，可以阻塞，ctx 取消后必须尽快返回。
	Execute(ctx context.Context, room Ref, e level.Effect) (success bool, err error)
}

// Commit 必须在业务 Nest Guard 中接受物理事实，等待提交完成后才返回。
// 失败会停止所属房间；原生物理不可回滚，不会重试失败 Tick。
// 任何时候都不得修改 Tick.Frame.States：房间把这块切片直接用作本帧共享的只读视图索引；
// 需要保留的实现自行复制（StateDAO.Accept 即如此）。
//
// 有流程的房间：物理帧、Tick.Facts、Tick.Receipts 必须在同一笔事务里生效或一起撤回，返回的 Settled 只能
// 来自已经提交成功的状态。房间只在 Commit 返回 nil 之后才执行 Settled.Actions。wiring/physicsroom 的
// EntityHost.Commit 与 NestCommit + AcceptLevel 是现成的实现。
type Commit func(context.Context, Ref, Tick) (Settled, error)

type Config struct {
	MaxRooms, MaxPlayers, MaxBodies  int
	MaxFrameBytes                    int
	InputTTL, MemberTTL, MaxLifetime time.Duration
	Radius, Margin                   float64
	Replication                      gamephysics.ReplicationPolicy
	Commit                           Commit
	Closed                           func(Ref) // Run 与 native 资源已结束后调用，用于释放房间 Entity。
	Metrics                          servicemetrics.Reporter
	// Actions 执行流程里的业务动作；nil 时流程只能用物理房间自带的动作。
	Actions ActionHandler
	// ActionTimeout 是一次 Execute 的期限，ActionConcurrency 是每个房间同时在途的业务动作数；
	// 0 取缺省（5s、16）。
	ActionTimeout     time.Duration
	ActionConcurrency int
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
	return Config{MaxRooms: 64, MaxPlayers: 20, MaxBodies: 256, MaxFrameBytes: 64 << 10, InputTTL: 250 * time.Millisecond, MemberTTL: 5 * time.Second, MaxLifetime: time.Hour, Radius: 12, Margin: 2, Replication: gamephysics.DefaultReplicationPolicy(), Commit: commit}
}
