package frame

import (
	"errors"
	"fmt"
)

var (
	ErrReceiverBusy  = errors.New("frame: receiver callback is already running")
	ErrReceiverReset = errors.New("frame: receiver reset during application")
)

// Receiver 管理一个连接中一个 Sync 流的接收基线，不保存业务对象或历史帧。
// 调用者串行调用 Receive/Reset；应用 Full 时先清空旧流对象，再应用其中的 create。
// Full 可能只是恢复的第一包，其余对象通过后续连续 Delta/create 到达。
type Receiver struct {
	limits     Limits
	meta       SnapshotMeta
	needsFull  bool
	applying   bool
	generation uint64
}

func NewReceiver(limits Limits) *Receiver {
	return &Receiver{limits: normalizeLimits(limits), needsFull: true}
}

// Reset 用于切换连接或业务显式放弃旧状态。旧连接的包必须由调用者隔离。
// 允许应用回调切换连接；旧回调返回后不能把旧基线写到新连接。
func (r *Receiver) Reset() {
	r.generation++
	r.meta = SnapshotMeta{}
	r.needsFull = true
}

// Receive 解码、核对基线后调用 apply，只有 apply 成功才推进。
// applied=false、err=nil 表示旧代或重复包，未调用 apply。
// 缺口或应用失败后拒绝 Delta；接入方应重新建立会话以取得新 Full，
// 或通过已鉴权的业务控制入口 HoldSession/ReadySession，不能继续盲目应用增量。
func (r *Receiver) Receive(raw []byte, apply func(Frame) error) (applied bool, err error) {
	if r.applying {
		return false, ErrReceiverBusy
	}
	if apply == nil {
		return false, errors.New("frame: application callback is required")
	}
	f, err := Decode(raw, r.limits)
	if err != nil {
		r.needsFull = true
		return false, err
	}
	if r.meta.RoomID != 0 {
		if f.RoomID != r.meta.RoomID {
			r.needsFull = true
			return false, fmt.Errorf("%w: stream changed without reset", ErrBaselineMismatch)
		}
		if f.Epoch < r.meta.Epoch || f.Epoch == r.meta.Epoch && f.Tick <= r.meta.Tick {
			return false, nil
		}
	}
	if f.Kind == Delta && (r.needsFull || f.Epoch != r.meta.Epoch || f.BaseTick != r.meta.Tick || f.SchemaVersion != r.meta.SchemaVersion) {
		r.needsFull = true
		return false, ErrBaselineMismatch
	}
	generation := r.generation
	r.applying = true
	// 回调返回错误或panic都可能已部分修改业务状态，必须等下一份全量。
	r.needsFull = true
	defer func() { r.applying = false }()
	if err := apply(f); err != nil {
		return false, err
	}
	if generation != r.generation {
		return false, ErrReceiverReset
	}
	r.meta = f.SnapshotMeta
	r.needsFull = false
	return true, nil
}
