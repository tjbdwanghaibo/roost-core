package awaitflow_test

import (
	"context"
	flow "github.com/tjbdwanghaibo/roost-core/codegen/internal/entity/testdata/awaitflow"
	sender "github.com/tjbdwanghaibo/roost-core/codegen/internal/entity/testdata/awaitflow/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"testing"
	"time"
)

func TestGeneratedPlayerAwaitsAndResumesOnLongPool(t *testing.T) {
	flow.RegisterEntity()
	flow.RegisterPlayerNestHandlers()
	if entity.EntityBusinessPoolOfKind(flow.PlayerKind) != entity.BusinessPoolLong {
		t.Fatal("generated registration lost pool")
	}
	id, err := entity.BuildEntityID(50001, flow.PlayerKind)
	if err != nil {
		t.Fatal(err)
	}
	player, err := flow.NewPlayer(&entity.EntityCreateParam{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	manager := entity.NewEntityManager()
	if err := manager.TryAdd(player); err != nil {
		t.Fatal(err)
	}
	scheduler := nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(manager)), nest.NestOptionWithBusinessPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := sender.NewPlayerSender(scheduler)
	if err := client.Sync_ClaimReward(ctx, id, 7); err != nil {
		t.Fatal(err)
	}
	// 第一次同步回复必须晚于恢复写入；第二次业务防重入应看到已经领取。
	if err := client.Sync_ClaimReward(ctx, id, 7); err == nil || err.Error() != "already claimed" {
		t.Fatalf("resume/reply ordering lost: %v", err)
	}
}
