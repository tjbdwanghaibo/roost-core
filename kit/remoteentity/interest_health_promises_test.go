package remoteentity

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"
)

// RR-20261005-NC-131：本机兴趣表只是被过期条目占满时，健康检查不能一直报 capacity exhausted。
//
// checkHealth 用 Manager.Stats().LocalInterests 与 snapshot_interest_keys 比较。本机兴趣表里过期的
// 条目只在下一次需要新建兴趣（表满）或每 1024 次续租时才被清理，Stats 直接数 map 长度。于是一阵
// 读取把表读满之后，即使所有兴趣早已过期、之后不再有读取，健康检查也永远是 Fail——编排器据此
// 摘流量或重启进程，而真实的活跃兴趣是 0。
//
// kind 253：本包测试只注册这一个 kind。
const interestHealthKind entity.EntityKind = 253

func TestExpiredLocalInterestsDoNotKeepHealthFailing(t *testing.T) {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: interestHealthKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := coreremote.DefaultConfig()
	cfg.SnapshotInterestKeys = 4
	cfg.SnapshotInterestTTL = 40 * time.Millisecond
	manager := coreremote.NewManager(nil, cfg, 1000)
	mod := &RemoteEntityMod{asm: &coreremote.Assembly{Manager: manager}, cfg: cfg}
	ctx := context.Background()
	for i := int64(0); i < int64(cfg.SnapshotInterestKeys); i++ {
		id, err := entity.BuildEntityID(9500+i, interestHealthKind)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.RenewRemoteSnapshotInterest(ctx, entity.RemoteSnapshotKey{EntityID: id, Kind: interestHealthKind, Scope: 1}); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	// 对照：表真满且全部有效时报 Fail，这是既有的容量告警。
	if result := mod.checkHealth(ctx); result.Status != health.StatusFail {
		t.Fatalf("a full table of live interests = %s (%s), want fail", result.Status, result.Message)
	}
	// 等全部兴趣的租期过去（时间条件本身就是被测对象，不是为了排序并发）。
	deadline := time.Now().Add(cfg.SnapshotInterestTTL)
	for time.Now().Before(deadline.Add(5 * time.Millisecond)) {
		time.Sleep(5 * time.Millisecond)
	}
	if live := manager.Stats().LocalInterests; live != 0 {
		t.Errorf("Stats().LocalInterests = %d after every interest expired, want 0", live)
	}
	if result := mod.checkHealth(ctx); result.Status != health.StatusOK {
		t.Fatalf("health after every interest expired = %s (%s), want ok", result.Status, result.Message)
	}
}
