package errcode

import (
	"errors"
	"strings"
	"testing"
)

func TestIntErrorMapsClientCodeAndPreservesCause(t *testing.T) {
	cause := errors.New("shop state missing")
	def := Define(500101, "shop.item_not_found", "商店商品不存在")

	err := Wrap(def, cause, "shop_id", 10, "item_id", 20)

	if !errors.Is(err, cause) {
		t.Fatalf("wrapped error should preserve cause")
	}
	if got := CodeOf(err); got != 500101 {
		t.Fatalf("CodeOf = %d, want 500101", got)
	}
	if got := ReasonOf(err); got != "商店商品不存在" {
		t.Fatalf("ReasonOf = %q, want business message", got)
	}
	code, reason := ClientError(err)
	if code != 500101 || reason != "商店商品不存在" {
		t.Fatalf("ClientError = (%d,%q), want (500101, business message)", code, reason)
	}
	if got := err.Error(); !strings.Contains(got, "shop_id=10") || !strings.Contains(got, "item_id=20") {
		t.Fatalf("Error() missing context fields: %q", got)
	}
}

func TestClientErrorFallsBackForPlainError(t *testing.T) {
	code, reason := ClientError(errors.New("plain failure"))
	if code != CodeInternal || reason != "server error" {
		t.Fatalf("ClientError plain = (%d,%q), want internal fallback", code, reason)
	}
}

func TestRemoteErrorPreservesClientCode(t *testing.T) {
	err := Remote(500101, "商店参数无效", "fallback")
	code, reason := ClientError(err)
	if code != 500101 || reason != "商店参数无效" {
		t.Fatalf("ClientError remote = (%d,%q), want remote code/reason", code, reason)
	}
}

// 框架段（1 – FrameworkCodeLast）只在本包分配，且每个框架码都带自己的码与可读原因。
// 新增框架码时同时改这张表：段内码是公开协议，增删都是有意的动作。
func TestFrameworkCodesAreExactlyAsAllocated(t *testing.T) {
	frameworkDefinitions := map[int32]*IntError{
		CodeEntityNotFound: ErrEntityNotFound,
	}
	const frameworkAllocated = 1 // CodeInternal 不是 Define 出来的，只作回退码，不计入
	if len(frameworkDefinitions) != frameworkAllocated {
		t.Fatalf("%d framework codes paired, want %d", len(frameworkDefinitions), frameworkAllocated)
	}
	for code, def := range frameworkDefinitions {
		if code <= CodeInternal || code > FrameworkCodeLast {
			t.Fatalf("framework code %d is outside the framework segment 2-%d", code, FrameworkCodeLast)
		}
		got, reason := ClientError(Wrap(def, errors.New("cause")))
		if got != code || reason == internalReason {
			t.Fatalf("framework definition %v reports (%d, %q), want its own code %d and reason", def, got, reason, code)
		}
	}
	for _, def := range Definitions() {
		if def.Code > CodeInternal && def.Code <= FrameworkCodeLast && frameworkDefinitions[def.Code] == nil {
			t.Fatalf("code %d (%s) is in the framework segment but not allocated here", def.Code, def.Name)
		}
	}
}

// 包装后仍能按码判别、也能判别原错误：Nest 冷加载不存在时就是这样包装的。
func TestEntityNotFoundWrapKeepsCauseAndCode(t *testing.T) {
	cause := errors.New("dataengine repository: entity aggregate not found")
	err := Wrap(ErrEntityNotFound, cause)
	if !errors.Is(err, cause) || !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("wrapped entity-not-found lost errors.Is identity: %v", err)
	}
	if code, reason := ClientError(err); code != CodeEntityNotFound || reason != "entity not found" {
		t.Fatalf("ClientError = (%d, %q), want (%d, entity not found)", code, reason, CodeEntityNotFound)
	}
}
