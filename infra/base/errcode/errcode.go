package errcode

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// 码段归属（2026-10-16 登记，D7）：
//
//	0              CodeOK
//	1 – 999        框架段：本包定义的、跨模块通用的框架码（CodeInternal、CodeEntityNotFound、CodeEntityDeleted …）
//	100000 – 199999 生成工程业务码的默认段（roost.yaml ids.errcode，可改，但不得与框架段重叠）
//	5x0101 – 5x0199 内置 service 包各自的段（directory 53、rank 54、match 55、account 56、
//	               global 57、chat 58、mail 59、platform 60、session 61、activity 62），
//	               每个包的 errcode_test.go 用 segmentFirst / segmentAllocated 钉住
//
// 框架段只在本包里分配：新增框架码时在下面的常量与 frameworkDefinitions 测试里同时登记。
const (
	CodeOK       int32 = 0
	CodeInternal int32 = 1
	// CodeEntityNotFound：请求的目标实体在权威存储里从未创建过。
	// 这是可预期的客户端状态而不是服务端故障：典型场景是首登前就发业务请求，
	// 客户端应先走登录 / 创角（生成工程的 EnterGame → lifecycle.GetOrCreate）再重试。
	CodeEntityNotFound int32 = 2
	// CodeEntityDeleted：请求的实体删过档（权威里只剩墓碑），这个 ID 不可再用（维护者 2026-10-17 决定 D10 方案 B）。
	// 与 CodeEntityNotFound 分开：重新登录不会让它恢复（登录边界的 GetOrCreate 同样返回本码），
	// 客户端不应重试或自动重新登录，而应提示角色已删除，由产品流程换用新的 ID（新建角色）。
	CodeEntityDeleted int32 = 3

	// FrameworkCodeLast 是框架段的上界（含）；业务码段的下界必须大于它。生成器的 manifest 校验
	// 持有同值副本（codegen/internal/roost/manifest.go 的 frameworkErrcodeLast），改动须同步。
	FrameworkCodeLast int32 = 999
)

// ErrEntityNotFound 是 CodeEntityNotFound 的定义。框架在确认“权威里没有这个实体”时用
// Wrap(ErrEntityNotFound, cause) 包装原错误：errcode.ClientError 给客户端稳定码，
// errors.Is 仍能判别原哨兵（entity.ErrAuthorityEntityNotFound、DataEngine 的
// ErrEntityAggregateNotFound）。本包不依赖 framework，判定与包装由调用方（Nest）完成。
var ErrEntityNotFound = Define(CodeEntityNotFound, "entity_not_found", "entity not found")

// ErrEntityDeleted 是 CodeEntityDeleted 的定义。框架在确认“这个 ID 删过档”时用 Wrap(ErrEntityDeleted, cause)
// 包装原错误（entity.ErrAuthorityEntityDeleted、DataEngine 的 ErrEntityAggregateDeleted 仍可 errors.Is 判别）：
// Nest 在声明目标的加载结果处包装，生成 lifecycle 在 Get / GetOrCreate（登录边界）处包装。
var ErrEntityDeleted = Define(CodeEntityDeleted, "entity_deleted", "entity deleted")

const internalReason = "server error"

// Definition is the stable client-facing description of one business error.
type Definition struct {
	Code    int32
	Name    string
	Message string
}

// IntError is an error with a stable client code. Business packages should
// define their own instances and wrap them with contextual causes when needed.
type IntError struct {
	def    Definition
	cause  error
	fields []any
}

var (
	registryMu sync.RWMutex
	registry   = make(map[int32]Definition)
)

func Define(code int32, name string, message string) *IntError {
	def := Definition{Code: code, Name: name, Message: message}
	if code > 0 {
		registryMu.Lock()
		registry[code] = def
		registryMu.Unlock()
	}
	return &IntError{def: def}
}

func Wrap(def *IntError, cause error, fields ...any) error {
	if def == nil {
		if cause == nil {
			return nil
		}
		return cause
	}
	return &IntError{
		def:    def.def,
		cause:  cause,
		fields: append([]any(nil), fields...),
	}
}

func Remote(code int32, reason string, fallback string) error {
	if code == CodeOK {
		return nil
	}
	if code <= 0 {
		code = CodeInternal
	}
	if reason == "" {
		reason = fallback
	}
	if reason == "" {
		reason = internalReason
	}
	return &IntError{
		def: Definition{
			Code:    code,
			Name:    fmt.Sprintf("remote.%d", code),
			Message: reason,
		},
	}
}

func CodeOf(err error) int32 {
	var coded *IntError
	if errors.As(err, &coded) && coded != nil && coded.def.Code > 0 {
		return coded.def.Code
	}
	return CodeInternal
}

func ReasonOf(err error) string {
	var coded *IntError
	if errors.As(err, &coded) && coded != nil && coded.def.Message != "" {
		return coded.def.Message
	}
	return internalReason
}

func ClientError(err error) (int32, string) {
	if err == nil {
		return CodeOK, ""
	}
	var coded *IntError
	if errors.As(err, &coded) && coded != nil && coded.def.Code > 0 {
		reason := coded.def.Message
		if reason == "" {
			reason = coded.def.Name
		}
		if reason == "" {
			reason = internalReason
		}
		return coded.def.Code, reason
	}
	return CodeInternal, internalReason
}

func Definitions() []Definition {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Definition, 0, len(registry))
	for _, def := range registry {
		out = append(out, def)
	}
	return out
}

func (e *IntError) Code() int32 {
	if e == nil {
		return CodeInternal
	}
	return e.def.Code
}

func (e *IntError) Name() string {
	if e == nil {
		return ""
	}
	return e.def.Name
}

func (e *IntError) Message() string {
	if e == nil {
		return ""
	}
	return e.def.Message
}

func (e *IntError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	if e.def.Name != "" {
		b.WriteString(e.def.Name)
	} else {
		b.WriteString(fmt.Sprintf("errcode:%d", e.def.Code))
	}
	if e.def.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.def.Message)
	}
	if len(e.fields) > 0 {
		b.WriteString(" [")
		for i := 0; i < len(e.fields); i += 2 {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(fmt.Sprint(e.fields[i]))
			if i+1 < len(e.fields) {
				b.WriteByte('=')
				b.WriteString(fmt.Sprint(e.fields[i+1]))
			}
		}
		b.WriteByte(']')
	}
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

func (e *IntError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *IntError) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	t, ok := target.(*IntError)
	if !ok || t == nil {
		return false
	}
	if e.def.Code > 0 && t.def.Code > 0 {
		return e.def.Code == t.def.Code
	}
	return e.def.Name != "" && e.def.Name == t.def.Name
}
