package split

import (
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
)

// U-0150 · C2 · gap map kit `service/examples/split` 2/2：mail 能力缺失时消费者不能构造。
// `consumer.go:78`（授予失败且取消预留也失败）需要一个 CancelClaim 出错的 mail 替身，留待。
func TestRewardFlowRefusesARegistryWithoutMail(t *testing.T) {
	if _, err := NewRewardFlow(&app.Registry{}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("NewRewardFlow without mail = %v", err)
	}
}
