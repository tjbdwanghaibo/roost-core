package awaitflow_test

import (
	"context"
	flow "github.com/tjbdwanghaibo/roost-core/codegen/internal/entity/testdata/awaitflow"
	sender "github.com/tjbdwanghaibo/roost-core/codegen/internal/entity/testdata/awaitflow/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"sync"
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
	scheduler := nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(manager)), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
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

// 业务时间参数（nowUnixMilli）是 handler 执行时这条消息冻结的业务时间，不是调用方发送前读的时间。
// 进程业务时钟的来源每读一次走 1ms：调用方读的时间与消息冻结的时间一定不同。
func TestGeneratedDispatchFillsTheBusinessTimeAtExecution(t *testing.T) {
	flow.RegisterEntity()
	flow.RegisterPlayerNestHandlers()
	var mu sync.Mutex
	tick := time.UnixMilli(1_800_000_000_000)
	clock.SetSource(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		tick = tick.Add(time.Millisecond)
		return tick
	})
	t.Cleanup(func() { clock.SetSource(nil) })
	id, err := entity.BuildEntityID(50002, flow.PlayerKind)
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
	scheduler := nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(manager)), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}))
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	requestTime := clock.UnixMilli() // 调用方（endpoint）在发送前读的业务时间
	got, err := sender.NewPlayerSender(scheduler).Sync_StampVisit(ctx, id, "hello")
	if err != nil {
		t.Fatalf("stamp: %v (request time %d)", err, requestTime)
	}
	if got <= requestTime {
		t.Fatalf("handler business time %d is not after the caller's %d", got, requestTime)
	}
}
