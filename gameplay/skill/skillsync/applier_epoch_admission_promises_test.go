package skillsync

// RR-20261005-NC-116：Applier 的 ErrApplyInProgress 承诺“可重试”。旧 admit 在开新 epoch 时先置 pendingEpoch，
// 再检查 full 包的 BaseSequence；这一步拒绝后直接返回，pendingEpoch 不再清零，之后每个包（包括合法的恢复
// full）都得到 ErrApplyInProgress，Applier 永久卡死，只能重建。被拒绝的包不能改变 Applier 状态。

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

func TestRejectedFullPacketDoesNotWedgeTheApplier(t *testing.T) {
	observer := syncstream.Observer{ID: 3}
	consumer := &recordingConsumer{}
	applier, err := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 1, State: consumer})
	if err != nil {
		t.Fatal(err)
	}
	projector, _ := NewProjector(1)
	for _, step := range []struct {
		name  string
		epoch uint64
	}{{"first epoch", 5}, {"epoch switch", 6}} {
		malformed, _ := projector.StateSnapshotPacket(observer, 1, skill.RuntimeStateSnapshot{})
		malformed.Epoch, malformed.Sequence, malformed.BaseSequence = step.epoch, 10, 9
		if _, err := applier.Apply(malformed); !errors.Is(err, ErrPacketShape) {
			t.Fatalf("%s: malformed full = %v, want ErrPacketShape", step.name, err)
		}
		valid, _ := projector.StateSnapshotPacket(observer, 1, skill.RuntimeStateSnapshot{})
		valid.Epoch, valid.Sequence = step.epoch, 11
		result, err := applier.Apply(valid)
		if err != nil || !result.Applied || applier.Epoch() != step.epoch {
			t.Fatalf("%s: valid full after a rejected one = %+v, %v (epoch %d); the rejected packet wedged the applier", step.name, result, err, applier.Epoch())
		}
	}
	if consumer.snapshots != 2 {
		t.Fatalf("snapshots applied = %d, want 2", consumer.snapshots)
	}
}
