package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/viper"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

type countingCommitter struct{ effects int }

func (c *countingCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	c.effects += len(record.Effects)
	return nil
}

// RR-20261006-66（F06-S7 跨进程）：saga.max_payload_bytes 是发起方与协调器共用的配置键。发起方进程没有这个类型的协调器
// （协调器在别的进程）时，EmitStart 也要在 Nest 事务里按配置值拒绝超限的意图，事务不提交；之前只按线上硬上限 4 MiB 校验，
// 意图随业务事务提交，协调器拒绝后只能由启动消费者 Term 并告警。
func TestEmitStartRefusesOverTheSharedLimitWithoutALocalCoordinator(t *testing.T) {
	const limit = 1024
	cfg := viper.New()
	cfg.Set("saga.max_payload_bytes", limit)
	if err := NewMod(emptyGetter{}).Init(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coresaga.SetStartDataLimit(0) })
	start := func(key string, size int) (*countingCommitter, error) {
		committer := &countingCommitter{}
		_, err := corenest.RunIsolatedTransaction(context.Background(), committer, "start", func() (any, error) {
			return nil, coresaga.EmitStart(coresaga.StartRequest{Type: "coordinated-elsewhere", DefinitionVersion: 1, BusinessKey: key, Data: make([]byte, size)})
		})
		return committer, err
	}
	if committer, err := start("at-limit", limit); err != nil || committer.effects != 1 {
		t.Fatalf("start intent at the shared limit: err=%v effects=%d, want committed", err, committer.effects)
	}
	committer, err := start("over-limit", limit+1)
	if !errors.Is(err, coresaga.ErrInvalidRecord) || committer.effects != 0 {
		t.Fatalf("start intent over saga.max_payload_bytes with the coordinator in another process: err=%v effects=%d, want ErrInvalidRecord inside the Nest transaction and nothing committed", err, committer.effects)
	}
}
