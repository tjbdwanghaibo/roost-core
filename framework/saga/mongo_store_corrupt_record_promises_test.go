package saga

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// RR-20261006-45（F06-S3）：一条校验不过的 saga 记录不拖累整批。
//
// 承诺：MongoStore.ClaimDue 与 List 遇到校验不过的记录（手工改坏、不兼容的写者整体 Replace 丢了字段）时只跳过这一条，
// 计 saga.store.corrupt_record_total{op} 并记点名记录的 ERROR；同批其他记录照常返回。坏记录不被改写（不自动修改生产数据），
// 运维修好文档前它在每个 lease_duration 被领取、告警一次。
//
// 旧行为：ClaimDue 逐条领取后才校验，遇到坏记录返回 (已领取的, err)，coordinatorLoop 在 err != nil 时整批丢弃——同批已领取的
// 正常记录要等 LeaseDuration（15s）租约过期才被重新领取；坏记录排在前面时每一轮都如此。List 遇到一条坏记录整次失败，运维面
// 读不出别的记录。
func TestMongoStoreSkipsACorruptRecordWithoutFailingTheBatch(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	store, err := NewMongoStore(client, MongoStoreOptions{Database: "corrupt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := func(id string, offset time.Duration) Record {
		return Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "key-" + id, Status: StatusPending, Phase: PhaseForward, Version: 1,
			NextRunAt: now.Add(offset), CreatedAt: now, UpdatedAt: now.Add(offset)}
	}
	for _, r := range []Record{record("good-a", -2*time.Second), record("good-c", -time.Second)} {
		if err := store.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// 坏记录排在最前（next_run_at 最早）：业务键被不兼容的写者丢掉。
	corrupt := toRecordDoc(record("corrupt-b", -3*time.Second))
	corrupt.BusinessKey = ""
	if err := client.Collection("corrupt", defaultSagaCollection).Seed(corrupt); err != nil {
		t.Fatal(err)
	}
	ids := func(records []Record) []string {
		out := make([]string, 0, len(records))
		for _, r := range records {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return out
	}

	before := counterValue("saga.store.corrupt_record_total")
	claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "coordinator", Now: now, LeaseDuration: 15 * time.Second, Limit: 10})
	if err != nil {
		t.Errorf("ClaimDue with one corrupt record among three = (%v, %v); want the two good records and no error — the coordinator discards a batch that comes back with an error, so the good records wait out their lease", ids(claimed), err)
	}
	if got := ids(claimed); len(got) != 2 || got[0] != "good-a" || got[1] != "good-c" {
		t.Errorf("ClaimDue returned %v, want [good-a good-c]", got)
	}
	if grown := counterValue("saga.store.corrupt_record_total") - before; grown != 1 {
		t.Errorf("ClaimDue: saga.store.corrupt_record_total grew by %d, want 1", grown)
	}

	before = counterValue("saga.store.corrupt_record_total")
	listed, err := store.List(ctx, Query{Limit: 100})
	if err != nil {
		t.Errorf("List with one corrupt record = (%v, %v); want the good records and no error", ids(listed), err)
	}
	if got := ids(listed); len(got) != 2 || got[0] != "good-a" || got[1] != "good-c" {
		t.Errorf("List returned %v, want [good-a good-c]", got)
	}
	if grown := counterValue("saga.store.corrupt_record_total") - before; grown != 1 {
		t.Errorf("List: saga.store.corrupt_record_total grew by %d, want 1", grown)
	}
	// 坏记录原样保留，不被改写。
	var raw recordDoc
	if err := client.Collection("corrupt", defaultSagaCollection).FindOne(ctx, map[string]any{"_id": "corrupt-b"}, &raw); err != nil || raw.BusinessKey != "" || raw.Status != StatusPending {
		t.Fatalf("the corrupt record was changed: %+v err=%v", raw, err)
	}
}
