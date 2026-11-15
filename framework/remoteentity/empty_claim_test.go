// 删空认领（D17）：空认领是“所有权记录在、这条记录下从没有提交落进权威”的实体。运维入口 Manager.DeleteEmptyClaim 只删
// 本 sid 独占、仍是判定时那一代、仍没有提交的记录（比较删除）；有提交的、别人的、判定之后变了的一律不删。累积计数
// remote_entity.empty_claim_total{event} 记留下（left）与删掉（deleted）的条数。

package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// emptyClaimKind 与 newWriterNodesOn 的 kind 相同。
const emptyClaimKind entity.EntityKind = 125

func useFreshMetrics(t *testing.T) {
	t.Helper()
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(metrics.NewRegistry())
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })
}

func emptyClaimCount(event string) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == emptyClaimMetric && metric.Labels["event"] == event {
			return metric.Value
		}
	}
	return 0
}

// rejectFirstWrite 像建盟被业务拒绝那样：写准入（首写认领）之后 handler 失败，批次 Abort、Close，没有任何提交。
func rejectFirstWrite(t *testing.T, ctx context.Context, n sharedWriterNode, id int64) {
	t.Helper()
	batch, err := n.mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		t.Fatalf("sid %d: admission of %d: %v", n.sid, id, err)
	}
	if err := batch.Abort(ctx, errors.New("business rejected the founding")); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func readMeta(t *testing.T, n sharedWriterNode, id int64) (mongoAuthority, bool) {
	t.Helper()
	var doc mongoAuthority
	err := n.loader.store.controlDB().Collection(remoteMetaCollection).FindOne(context.Background(), bson.M{"_id": id}, &doc)
	if errors.Is(err, fmongo.ErrNotFound) {
		return mongoAuthority{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return doc, true
}

// 被拒绝的首写留下空认领（计 left）；删空认领只删它：有提交的答 not_empty、别的 sid 的答 not_owned，都不动。删掉之后
// 所有权记录不在了，同 ID 的写重新首写认领并提交。
func TestDeleteEmptyClaimRemovesOnlyANeverCommittedClaim(t *testing.T) {
	useFreshMetrics(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes := newSharedWriterNodes(t, 2401, nil, 1000, 1004)
	a, b := nodes[0], nodes[1]
	empty := testRemoteFullIDWithKind(2401, 1, emptyClaimKind)
	committed := testRemoteFullIDWithKind(2402, 1, emptyClaimKind)
	foreign := testRemoteFullIDWithKind(2403, 1, emptyClaimKind)

	rejectFirstWrite(t, ctx, a, empty)
	if doc, ok := readMeta(t, a, empty); !ok || doc.Owner != 1000 || doc.Version != 0 {
		t.Fatalf("the rejected first write left %+v (found=%v); the case under test does not arise", doc, ok)
	}
	if got := emptyClaimCount("left"); got != 1 {
		t.Fatalf("empty_claim_total{left} = %d after one rejected first write; want 1", got)
	}
	a.loader.rawID = 2402
	if _, err := a.write(t, ctx, committed, "A1", "", 0x41); err != nil {
		t.Fatal(err)
	}
	b.loader.rawID = 2403
	rejectFirstWrite(t, ctx, b, foreign)
	if got := emptyClaimCount("left"); got != 2 {
		t.Fatalf("empty_claim_total{left} = %d; want 2 (a committed first write is not counted)", got)
	}

	candidates, next, err := a.mgr.ListEmptyClaims(ctx, []entity.EntityKind{emptyClaimKind}, 0, 10)
	if err != nil || next != 0 || !slices.Equal(candidates, []int64{empty}) {
		t.Fatalf("candidates = %v next=%d err=%v; want only this sid's never-committed claim %d", candidates, next, err, empty)
	}
	for id, want := range map[int64]EmptyClaimOutcome{committed: EmptyClaimNotEmpty, foreign: EmptyClaimNotOwned} {
		if got, err := a.mgr.DeleteEmptyClaim(ctx, id); err != nil || got != want {
			t.Fatalf("DeleteEmptyClaim(%d) = %v, %v; want %v", id, got, err, want)
		}
		if _, ok := readMeta(t, a, id); !ok {
			t.Fatalf("entity %d lost its ownership record although it is not this sid's empty claim", id)
		}
	}
	if got, err := a.mgr.DeleteEmptyClaim(ctx, empty); err != nil || got != EmptyClaimDeleted {
		t.Fatalf("DeleteEmptyClaim(empty claim) = %v, %v; want deleted", got, err)
	}
	if _, ok := readMeta(t, a, empty); ok {
		t.Fatal("the empty claim's ownership record is still there")
	}
	if _, found, err := a.mgr.GetRemoteOwnership(ctx, empty); err != nil || found {
		t.Fatalf("ownership after the delete found=%v err=%v; want no record", found, err)
	}
	if got := emptyClaimCount("deleted"); got != 1 {
		t.Fatalf("empty_claim_total{deleted} = %d; want 1", got)
	}
	if candidates, _, err := a.mgr.ListEmptyClaims(ctx, []entity.EntityKind{emptyClaimKind}, 0, 10); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates after the delete = %v, %v; want none", candidates, err)
	}

	// 同 ID 再建：内存里的空实例还在，写准入重新首写认领（第一代记录），提交落库。
	a.loader.rawID = 2401
	if base, err := a.write(t, ctx, empty, "again", "", 0x42); err != nil || base != 0 {
		t.Fatalf("re-creating write after the delete: base=%d err=%v", base, err)
	}
	if doc, ok := readMeta(t, a, empty); !ok || doc.Owner != 1000 || doc.Version != 1 {
		t.Fatalf("after the re-creating write the record is %+v (found=%v); want owner 1000 version 1", doc, ok)
	}
	if got := emptyClaimCount("left"); got != 2 {
		t.Fatalf("empty_claim_total{left} = %d after a committed re-creation; want still 2", got)
	}
}

// 同 sid 重启后内存里没有实例：删空认领经 loader 冷加载确认“权威里没有聚合”再删。读请求在权威里不存在的实体上的
// 首写认领（冷加载不到、准入失败）同样留下空认领，计 left。
func TestDeleteEmptyClaimAfterRestartConfirmsTheAggregateIsAbsent(t *testing.T) {
	useFreshMetrics(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := newRemoteMongoFake()
	before := newWriterNodesOn(t, db, 2411, nil, 1000)[0]
	before.loader.absentIsNotFound = true
	id := testRemoteFullIDWithKind(2411, 1, emptyClaimKind)
	if _, err := before.mgr.PrepareRemoteWriteBatch(ctx, []int64{id}); !errors.Is(err, entity.ErrAuthorityEntityNotFound) {
		t.Fatalf("admission on an entity that does not exist: %v; want ErrAuthorityEntityNotFound", err)
	}
	if doc, ok := readMeta(t, before, id); !ok || doc.Version != 0 {
		t.Fatalf("the failed admission left %+v (found=%v); the case under test does not arise", doc, ok)
	}
	if got := emptyClaimCount("left"); got != 1 {
		t.Fatalf("empty_claim_total{left} = %d after a claim whose admission failed; want 1", got)
	}

	restarted := newWriterNodesOn(t, db, 2411, nil, 1000)[0]
	restarted.loader.absentIsNotFound = true

	// 权威记录还是 _ver 0、聚合却读得到（例如同 sid 重启后 DataEngine 看得到、元数据还没投影的那一笔）：冷加载确认
	// 不了“不存在”，不删。
	withData := testRemoteFullIDWithKind(2412, 1, emptyClaimKind)
	if _, err := restarted.loader.store.ClaimOwnership(ctx, withData, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.loader.store.controlDB().Collection(fullDocCollection).InsertOne(ctx, bson.M{"_id": withData, "data": []byte("a=seen;b=")}); err != nil {
		t.Fatal(err)
	}
	restarted.loader.rawID = 2412
	if got, err := restarted.mgr.DeleteEmptyClaim(ctx, withData); err != nil || got != EmptyClaimNotEmpty {
		t.Fatalf("DeleteEmptyClaim on a version-0 record whose aggregate loads = %v, %v; want not_empty", got, err)
	}
	if _, ok := readMeta(t, restarted, withData); !ok {
		t.Fatal("a claim whose aggregate exists was deleted")
	}
	restarted.loader.rawID = 2411

	if got, err := restarted.mgr.DeleteEmptyClaim(ctx, id); err != nil || got != EmptyClaimDeleted {
		t.Fatalf("DeleteEmptyClaim after restart = %v, %v; want deleted", got, err)
	}
	if restarted.loader.loads.Load() == 0 {
		t.Fatal("the delete did not confirm the empty claim through the loader")
	}
	if _, ok := readMeta(t, restarted, id); ok {
		t.Fatal("the empty claim's ownership record is still there")
	}
}

// 代际守护：判定是空认领之后、比较删除之前，权威记录变了就不删。每一格在这个窗口里改一次权威：
//   - commit_landed：这一代下有提交落进来（例如同 sid 重启后重放的投影），_ver 0 → 1；
//   - transferred：所有权转给了别的 sid（单实例前提被打破时重叠的进程）；
//   - newer_generation：同一个 owner、更新的代际（重叠进程进出了一次共享）；
//   - reclaimed_elsewhere：记录被删掉、别的 sid 重新认领，代际值与原来相同（1/1）。
//
// 删掉守护（只按 _id 删，或去掉其中一项条件）对应的格子变红。
func TestDeleteEmptyClaimIsGuardedByTheOwnershipGeneration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, store, other *MongoCommitter, id int64, lease entity.RemoteEntityMarkerLease)
		check  func(doc mongoAuthority) bool
	}{
		{"commit_landed", func(t *testing.T, store, _ *MongoCommitter, id int64, _ entity.RemoteEntityMarkerLease) {
			if _, err := store.controlDB().Collection(remoteMetaCollection).UpdateOne(context.Background(), bson.M{"_id": id}, bson.M{"$set": bson.M{"_ver": int64(1)}}); err != nil {
				t.Fatal(err)
			}
		}, func(doc mongoAuthority) bool { return doc.Owner == 1000 && doc.Version == 1 }},
		{"transferred", func(t *testing.T, store, _ *MongoCommitter, id int64, lease entity.RemoteEntityMarkerLease) {
			if _, err := store.TransferExpected(context.Background(), id, lease, 1004); err != nil {
				t.Fatal(err)
			}
		}, func(doc mongoAuthority) bool { return doc.Owner == 1004 }},
		{"newer_generation", func(t *testing.T, store, _ *MongoCommitter, id int64, lease entity.RemoteEntityMarkerLease) {
			shared, err := store.EnterSharedExpected(context.Background(), id, lease)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.LeaveSharedExpected(context.Background(), id, shared); err != nil {
				t.Fatal(err)
			}
		}, func(doc mongoAuthority) bool { return doc.Owner == 1000 && !doc.Shared && doc.Marker == 3 }},
		{"reclaimed_elsewhere", func(t *testing.T, store, other *MongoCommitter, id int64, _ entity.RemoteEntityMarkerLease) {
			if _, err := store.controlDB().Collection(remoteMetaCollection).DeleteOne(context.Background(), bson.M{"_id": id}); err != nil {
				t.Fatal(err)
			}
			if lease, err := other.ClaimOwnership(context.Background(), id, 1004); err != nil || lease.MarkerEpoch != 1 || lease.RouteEpoch != 1 {
				t.Fatalf("re-claim by 1004 = %+v, %v; want the same generation values 1/1", lease, err)
			}
		}, func(doc mongoAuthority) bool { return doc.Owner == 1004 && doc.Marker == 1 && doc.Version == 0 }},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useFreshMetrics(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			raw := int64(2421 + i)
			nodes := newSharedWriterNodes(t, raw, nil, 1000, 1004)
			a, b := nodes[0], nodes[1]
			id := testRemoteFullIDWithKind(raw, 1, emptyClaimKind)
			rejectFirstWrite(t, ctx, a, id)
			lease, found, err := a.loader.store.GetOwnership(ctx, id)
			if err != nil || !found {
				t.Fatalf("empty claim ownership = %+v, %v, %v", lease, found, err)
			}
			hooked := false
			emptyClaimCheckedHook = func(hookID int64) {
				if hookID == id && !hooked {
					hooked = true
					tc.mutate(t, a.loader.store, b.loader.store, id, lease)
				}
			}
			t.Cleanup(func() { emptyClaimCheckedHook = nil })
			got, err := a.mgr.DeleteEmptyClaim(ctx, id)
			if !hooked {
				t.Fatal("the hook between the check and the delete did not run")
			}
			doc, ok := readMeta(t, a, id)
			if !ok {
				t.Fatalf("the record changed after the check (%s) was deleted anyway (outcome %v, err %v)", tc.name, got, err)
			}
			if err != nil || got != EmptyClaimChanged || !tc.check(doc) {
				t.Fatalf("DeleteEmptyClaim = %v, %v, record %+v; want changed and the changed record kept", got, err, doc)
			}
			if n := emptyClaimCount("deleted"); n != 0 {
				t.Fatalf("empty_claim_total{deleted} = %d; want 0", n)
			}
		})
	}
}

// 删除窗口里本进程到达的写（同 ID 的首写认领）排在写门后：删空认领持写门做完判定与比较删除，写随后重新认领并提交；
// 不会出现“写已认领 / 已提交、删除又把记录删掉”。
func TestDeleteEmptyClaimSerializesWithAConcurrentWriteToTheSameEntity(t *testing.T) {
	useFreshMetrics(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := newSharedWriterNodes(t, 2431, nil, 1000)[0]
	id := testRemoteFullIDWithKind(2431, 1, emptyClaimKind)
	rejectFirstWrite(t, ctx, a, id)

	written := make(chan error, 1)
	started := make(chan struct{})
	emptyClaimCheckedHook = func(hookID int64) {
		if hookID != id {
			return
		}
		go func() {
			close(started)
			_, err := a.write(t, ctx, id, "racing", "", 0x51)
			written <- err
		}()
		<-started
		select {
		case err := <-written:
			t.Errorf("a write to the entity finished (err=%v) while the delete held its write gate", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Cleanup(func() { emptyClaimCheckedHook = nil })
	if got, err := a.mgr.DeleteEmptyClaim(ctx, id); err != nil || got != EmptyClaimDeleted {
		t.Fatalf("DeleteEmptyClaim = %v, %v; want deleted (the racing write was queued behind it)", got, err)
	}
	if err := <-written; err != nil {
		t.Fatalf("the write queued behind the delete: %v", err)
	}
	doc, ok := readMeta(t, a, id)
	if !ok || doc.Owner != 1000 || doc.Version != 1 {
		t.Fatalf("after the queued write the record is %+v (found=%v); want a fresh claim with the write committed", doc, ok)
	}
	if got, err := a.mgr.DeleteEmptyClaim(ctx, id); err != nil || got != EmptyClaimNotEmpty {
		t.Fatalf("a second DeleteEmptyClaim on the committed entity = %v, %v; want not_empty", got, err)
	}
}

// 运维命令 remote_entity.empty_claims：不写 delete 只列候选，什么也不改；delete=true 删这一页的空认领并答各结论的个数。
func TestEmptyClaimsAdminCommandListsThenDeletes(t *testing.T) {
	useFreshMetrics(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := newSharedWriterNodes(t, 2441, nil, 1000)[0]
	var ids []int64
	for raw := int64(2441); raw < 2444; raw++ {
		a.loader.rawID = raw
		id := testRemoteFullIDWithKind(raw, 1, emptyClaimKind)
		rejectFirstWrite(t, ctx, a, id)
		ids = append(ids, id)
	}
	slices.Sort(ids)
	reg := admin.NewRegistry()
	if err := RegisterAdminCommands(reg, func() *Manager { return a.mgr }); err != nil {
		t.Fatal(err)
	}
	execute := func(payload string) admin.Result {
		t.Helper()
		result, err := reg.Execute(ctx, admin.Command{Name: AdminCommandEmptyClaims, Payload: json.RawMessage(payload)})
		if err != nil {
			t.Fatalf("%s: %v", payload, err)
		}
		return result
	}
	listed := execute(`{"kinds":[125],"limit":2}`)
	if got := listed.Data["candidates"].([]int64); !slices.Equal(got, ids[:2]) || listed.Data["next"].(int64) != ids[1] {
		t.Fatalf("first page = %v next=%v; want %v next=%d", got, listed.Data["next"], ids[:2], ids[1])
	}
	for _, id := range ids {
		if _, ok := readMeta(t, a, id); !ok {
			t.Fatal("listing without delete removed a claim")
		}
	}
	deleted := execute(`{"kinds":[125],"delete":true,"reason":"test"}`)
	if deleted.Data["deleted"].(int) != 3 || !slices.Equal(deleted.Data["deleted_ids"].([]int64), ids) {
		t.Fatalf("delete answer %+v; want all three deleted", deleted.Data)
	}
	for _, id := range ids {
		if _, ok := readMeta(t, a, id); ok {
			t.Fatalf("claim %d still there after delete=true", id)
		}
	}
	if _, err := reg.Execute(ctx, admin.Command{Name: AdminCommandEmptyClaims, Payload: json.RawMessage(`{"kinds":[7]}`)}); !errors.Is(err, admin.ErrCommandInvalid) {
		t.Fatalf("a kind that is not Remote-managed: %v; want ErrCommandInvalid", err)
	}
}
