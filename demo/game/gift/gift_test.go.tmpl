package gift

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// 使用真实认证器的会话格式：长玩家 ID 会把时间戳挤出旧的 16 字符前缀。
func TestSagaIDDistinguishesDemoLogins(t *testing.T) {
	const playerID int64 = 123456789
	first := fmt.Sprintf("demo-%d-%d", playerID, int64(1_800_000_000_000_000_000))
	second := fmt.Sprintf("demo-%d-%d", playerID, int64(1_800_000_001_000_000_000))
	if a, b := SagaID(playerID, first, 2), SagaID(playerID, second, 2); a == b {
		t.Fatalf("distinct logins share a gift identity: %q", a)
	}
}

// 身份必须包含整个会话，不能因截断或把不同字符替换成同一字符而合并请求。
func TestSagaIDUsesTheCompleteSession(t *testing.T) {
	for _, pair := range [][2]string{
		{"session-00000000-A", "session-00000000-B"},
		{"session.a", "session*a"},
		{"会话一", "会话二"},
	} {
		if a, b := SagaID(42, pair[0], 1), SagaID(42, pair[1], 1); a == b {
			t.Fatalf("sessions %q and %q collide: %s", pair[0], pair[1], a)
		}
	}
}

func TestSagaIDIsStableScopedAndSubjectSafe(t *testing.T) {
	const session = "session.with * unsafe > bytes 和中文"
	id := SagaID(math.MaxInt64, session, math.MaxUint32)
	if id != SagaID(math.MaxInt64, session, math.MaxUint32) {
		t.Fatal("same request did not preserve its identity")
	}
	if id == SagaID(math.MaxInt64-1, session, math.MaxUint32) || id == SagaID(math.MaxInt64, session, math.MaxUint32-1) {
		t.Fatal("different player or sequence shared an identity")
	}
	if len(id) > 128 || strings.ContainsAny(id, ".*> \t\r\n") {
		t.Fatalf("identity is not a bounded subject token: %q", id)
	}
}
