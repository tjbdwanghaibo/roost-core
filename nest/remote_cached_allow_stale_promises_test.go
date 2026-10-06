package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
)

// Mirror 第 2 步（docs/feature/MIRROR-STEPS-1-3-2026-10-06.md）把 Cached + 最低版本收紧为：L1 低于最低版本时
// 读出口返回 ErrRemoteSnapshotStale。nest 的 RemoteAccess 另有 AllowStale（生成器标签 allow_stale）：业务明确
// 接受低于 MinVersion 的快照，由 RemoteSnapshot.Accepts 判定。Mirror 之前 Cached 读交出 L1 的低版本、Accepts 按
// AllowStale 放行；收紧之后错误在 Accepts 之前就返回，allow_stale 对 Cached 不再起作用，required 的访问整笔失败。
// 这里用真实的 SnapshotClient（Manager 的快照读委托给它）钉住：AllowStale 的 Cached 访问照旧拿到 L1 的值，
// 不带 AllowStale 的同一访问照旧被拒绝。

const cachedAllowStaleSchema uint32 = 0x6e6f7374 // 只在本用例使用的 schema 号

type snapshotClientManager struct {
	entity.IRemoteEntityManager
	client *remoteentity.SnapshotClient
}

func (m snapshotClientManager) ReadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	return m.client.ReadRemoteSnapshot(ctx, key, consistency, minVersion)
}

type cachedAllowStaleRequest struct{ access RemoteAccess }

func (r cachedAllowStaleRequest) RemoteAccess() []RemoteAccess { return []RemoteAccess{r.access} }

func TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot(t *testing.T) {
	ctx := context.Background()
	if err := entity.RegisterRemoteSnapshotDecoder(cachedAllowStaleSchema, func(data []byte) (any, error) { return string(data), nil }); err != nil &&
		!errors.Is(err, entity.ErrRemoteSnapshotDecoderDuplicate) {
		t.Fatal(err)
	}
	refID := mustBuildCastID(t, 7651, entity.EntityCategoryRemote, nestRemoteManagedKind)
	key := entity.RemoteSnapshotKey{EntityID: refID, Kind: nestRemoteManagedKind, Scope: 7}
	loads := 0
	client, err := remoteentity.NewSnapshotClient(nil, remoteentity.SnapshotClientDeps{
		ConsumerSID: 1,
		Loader: func(_ context.Context, requested entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			loads++
			data := []byte("guild-summary-v3")
			return entity.RemoteSnapshotEnvelope{
				Key: requested, StateVersion: 3, MarkerEpoch: 1, RouteEpoch: 1, Schema: cachedAllowStaleSchema, Codec: 1, Full: true,
				Checksum: entity.RemoteSnapshotChecksum(data), Payload: entity.CopyFrozenRemoteSnapshotPayload(data),
			}, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Stop(ctx) }()
	// L1 先持有版本 3（Monotonic 回源一次）。
	if got, found, err := client.ReadRemoteSnapshot(ctx, key, entity.RemoteReadMonotonic, 0); err != nil || !found || got.StateVersion != 3 {
		t.Fatalf("seed read: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
	manager := snapshotClientManager{client: client}
	ref := entity.RemoteViewRef{EntityID: refID, Kind: nestRemoteManagedKind, RouteEpoch: 1}
	access := RemoteAccess{Alias: "guild", Ref: ref, Mode: RemoteAcquireCache, Scope: 7, MinVersion: 5, Required: true}

	stale := access
	stale.AllowStale = true
	msg := &Msg{Params: []any{cachedAllowStaleRequest{access: stale}}}
	if err := prepareRemoteSnapshots(msg, nil, manager); err != nil {
		t.Fatalf("cached access with allow_stale and min_version=5 over a cached version 3: %v; want the older snapshot accepted (RemoteSnapshot.Accepts honours AllowStale)", err)
	}

	msg = &Msg{Params: []any{cachedAllowStaleRequest{access: access}}}
	err = prepareRemoteSnapshots(msg, nil, manager)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("cached access with min_version=5 and no allow_stale over a cached version 3: err=%v; want a stale refusal", err)
	}
	if loads != 1 {
		t.Fatalf("authority loads = %d, want 1 (a Cached access never loads)", loads)
	}
}
