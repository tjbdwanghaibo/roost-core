package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

const InternalVersion uint16 = 1

type ProcessIdentity struct {
	ServerID    int32
	Incarnation string
}

func (identity ProcessIdentity) Validate() error {
	if identity.ServerID <= 0 || len(identity.Incarnation) < 16 || len(identity.Incarnation) > 128 || !subjectToken(identity.Incarnation) {
		return fmt.Errorf("%w: process identity", ErrInvalidRequest)
	}
	return nil
}

// Binding 是连接的完整代次。SID 相同不等于同一进程，SessionID 相同不等于同一连接。
// Close、Unbind、发送失败和 Room Detach 都必须匹配整份身份。
type Binding struct {
	PlayerID        int64
	SessionID       string
	Gate            ProcessIdentity
	ConnectionNonce string
	Game            ProcessIdentity
	BindID          string
}

func (binding Binding) Validate() error {
	if len(binding.BindID) != 32 || !subjectToken(binding.BindID) {
		return fmt.Errorf("%w: bind id", ErrInvalidRequest)
	}
	return binding.validateConnection()
}

func (binding Binding) validateConnection() error {
	if binding.PlayerID == 0 || binding.SessionID == "" || len(binding.SessionID) > 128 || len(binding.ConnectionNonce) != 32 {
		return fmt.Errorf("%w: binding", ErrInvalidRequest)
	}
	if !subjectToken(binding.ConnectionNonce) {
		return ErrInvalidRequest
	}
	return errors.Join(binding.Gate.Validate(), binding.Game.Validate())
}

func newBindingToken() string {
	var bytes [16]byte
	_, _ = rand.Read(bytes[:]) // 当前 Go 的 crypto/rand.Read 失败会终止进程，不返回弱随机令牌。
	return hex.EncodeToString(bytes[:])
}

// Budget 在 Gate 准入时建立；出队或跨 NATS 不重新获得完整超时。
// Receiver 立即把跨机墙钟期限与剩余预算取保守最小值，之后只依赖本地 context。
type Budget struct {
	DeadlineUnixNano int64
	RemainingNanos   int64
}

func BudgetFromContext(ctx context.Context) (Budget, error) {
	if ctx == nil {
		return Budget{}, ErrInvalidRequest
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return Budget{}, fmt.Errorf("%w: deadline required", ErrInvalidRequest)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return Budget{}, context.DeadlineExceeded
	}
	return Budget{DeadlineUnixNano: deadline.UnixNano(), RemainingNanos: int64(remaining)}, nil
}

func (budget Budget) Context(parent context.Context, maxWait, clockSkew time.Duration) (context.Context, context.CancelFunc, error) {
	if parent == nil || maxWait <= 0 || clockSkew < 0 || budget.RemainingNanos <= 0 || budget.DeadlineUnixNano <= 0 {
		return nil, nil, ErrInvalidRequest
	}
	remaining := min(time.Duration(budget.RemainingNanos), maxWait, time.Until(time.Unix(0, budget.DeadlineUnixNano))-clockSkew)
	if remaining <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	ctx, cancel := context.WithTimeout(parent, remaining)
	return ctx, cancel, nil
}

type Outcome uint8

const (
	NotAdmitted Outcome = iota + 1
	Completed
	Unknown
)

// ExecutionResult 只描述服务内部执行/准入结果；不是客户端收到或 Mongo 已投影的回执。
type ExecutionResult struct {
	Version uint16
	Outcome Outcome
	Reason  string
}

type ControlOperation uint8

const (
	Bind ControlOperation = iota + 1
	Activate
	Renew
	Unbind
)

type ControlRequest struct {
	Version   uint16
	Operation ControlOperation
	Binding   Binding
	Budget    Budget
	// Ticket 只用于 Bind 的双端鉴权，不进入后续 Forward、日志和指标。
	Ticket string
}

type ControlResponse struct {
	ExecutionResult
	Binding            Binding
	LeaseUntilUnixNano int64
}

type ForwardRequest struct {
	Version   uint16
	Binding   Binding
	RequestID string
	Budget    Budget
	MessageID uint32
	Sequence  uint32
	Kind      wire.PayloadKind
	Payload   []byte
}

type OutboundPacket struct {
	Version   uint16
	Binding   Binding
	OutSeq    uint64
	Budget    Budget
	MessageID uint32
	Sequence  uint32
	Kind      wire.PayloadKind
	Push      bool
	Payload   []byte
}

type AudienceKind uint8

const (
	AllOnline AudienceKind = iota + 1
	GameOnline
	Players
)

type Audience struct {
	Kind      AudienceKind
	GameID    int32
	PlayerIDs []int64
}

type BroadcastRequest struct {
	Version     uint16
	Source      ProcessIdentity
	Target      ProcessIdentity
	BroadcastID string
	Budget      Budget
	Audience    Audience
	MessageID   uint32
	Payload     []byte
}

type BroadcastResponse struct {
	ExecutionResult
	Matched  int
	Accepted int
	Refused  int
	Stale    int
}

type BroadcastGateResult struct {
	Gate      ProcessIdentity
	Outcome   Outcome
	Admission BroadcastResponse
}

// BroadcastResult 只汇总有回信的 Gate；Unknown 可能已经投递，不能计作零或自动重发。
type BroadcastResult struct{ Gates []BroadcastGateResult }

func (request ControlRequest) Validate() error {
	if request.Version != InternalVersion || request.Operation < Bind || request.Operation > Unbind {
		return ErrInvalidRequest
	}
	binding := request.Binding
	if request.Operation == Bind {
		if request.Ticket == "" || len(request.Ticket) > 8192 || binding.BindID != "" {
			return ErrInvalidRequest
		}
		return binding.validateConnection()
	} else if request.Ticket != "" {
		return ErrInvalidRequest
	}
	return binding.Validate()
}

func (request ForwardRequest) Validate(maxPayload int) error {
	if request.Version != InternalVersion || request.MessageID == 0 || request.Sequence == 0 || len(request.RequestID) != 32 || !subjectToken(request.RequestID) || maxPayload <= 0 || len(request.Payload) > maxPayload || (request.Kind != wire.PayloadProtobuf && request.Kind != wire.PayloadLockstep) {
		return ErrInvalidRequest
	}
	return request.Binding.Validate()
}

func (packet OutboundPacket) Validate(maxPayload int) error {
	if packet.Version != InternalVersion || packet.OutSeq == 0 || packet.MessageID == 0 || maxPayload <= 0 || len(packet.Payload) > maxPayload || packet.Kind > wire.PayloadLockstep {
		return ErrInvalidRequest
	}
	if !packet.Push && (packet.Kind != wire.PayloadProtobuf || packet.Sequence == 0) {
		return ErrInvalidRequest
	}
	if packet.Push && packet.Sequence != 0 {
		return ErrInvalidRequest
	}
	return packet.Binding.Validate()
}

func (audience Audience) Validate(maxPlayers int) error {
	switch audience.Kind {
	case AllOnline:
		if audience.GameID != 0 || len(audience.PlayerIDs) != 0 {
			return ErrInvalidRequest
		}
	case GameOnline:
		if audience.GameID <= 0 || len(audience.PlayerIDs) != 0 {
			return ErrInvalidRequest
		}
	case Players:
		if audience.GameID != 0 || len(audience.PlayerIDs) == 0 || maxPlayers <= 0 || len(audience.PlayerIDs) > maxPlayers {
			return ErrInvalidRequest
		}
		for _, id := range audience.PlayerIDs {
			if id == 0 {
				return ErrInvalidRequest
			}
		}
	default:
		return ErrInvalidRequest
	}
	return nil
}

func subjectToken(token string) bool {
	if token == "" {
		return false
	}
	for _, c := range token {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func ValidateNamespace(namespace string) error {
	if len(namespace) == 0 || len(namespace) > 128 {
		return ErrInvalidRequest
	}
	for _, part := range strings.Split(namespace, ".") {
		if !subjectToken(part) {
			return ErrInvalidRequest
		}
	}
	return nil
}

// ChannelSubject 把发布来源固定进主题，供 NATS ACL 约束。payload 身份只能校验一致性，
// 不能代替服务账号和 publish 权限。目标具名且不使用 queue group。
func ChannelSubject(namespace, targetRole string, target ProcessIdentity, sourceRole string, source ProcessIdentity, channel string) (string, error) {
	if err := errors.Join(ValidateNamespace(namespace), target.Validate(), source.Validate()); err != nil {
		return "", err
	}
	if (targetRole != "game" && targetRole != "gate") || (sourceRole != "game" && sourceRole != "gate" && sourceRole != "system") || !subjectToken(channel) {
		return "", ErrInvalidRequest
	}
	return strings.Join([]string{namespace, targetRole, strconv.Itoa(int(target.ServerID)), target.Incarnation, "from", sourceRole, strconv.Itoa(int(source.ServerID)), source.Incarnation, channel}, "."), nil
}
