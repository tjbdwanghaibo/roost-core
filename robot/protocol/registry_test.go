package protocol_test

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"testing"
)

// 生成的 PB 表自带逐消息编解码器，不依赖 JSON 或一个重复的兜底 codec。
func TestExplicitCodecsNeedNoFallback(t *testing.T) {
	r := protocol.NewRegistry(nil)
	r.MustRegisterEncoder(7, func(any) ([]byte, error) { return []byte{42}, nil })
	r.MustRegisterDecoder(7, func([]byte) (any, error) { return 42, nil })
	if err := protocol.EnsureEncoder(r, 7); err != nil {
		t.Fatal(err)
	}
	if err := protocol.EnsureDecoder[int](r, 7); err != nil {
		t.Fatal(err)
	}
	if err := protocol.EnsureEncoder(r, 8); !errors.Is(err, protocol.ErrCodecRequired) {
		t.Fatal(err)
	}
	if err := protocol.EnsureDecoder[int](r, 8); !errors.Is(err, protocol.ErrCodecRequired) {
		t.Fatal(err)
	}
}
