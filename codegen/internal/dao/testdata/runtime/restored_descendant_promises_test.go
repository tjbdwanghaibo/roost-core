//go:build daoruntime

package testdata

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20261005-NC-32：正式 DAO 恢复后，深层子对象更新必须进入 Nest 提交记录。
// 旧 wire 转换在临时父对象上绑定子对象，再按值返回父对象；值相同但通知仍指向副本。
// 独立 nested.UnmarshalBSON 的原址恢复不会经过此复制，因此不能作为 DAO 加载的对照替代。
func TestRestoredDescendantReachesCommit(t *testing.T) {
	for _, parent := range []string{"map", "slice", "pointer"} {
		for _, child := range []string{"map", "slice", "pointer"} {
			t.Run(parent+"/"+child, func(t *testing.T) {
				leaf := bson.M{"id": int32(1), "level": int32(5)}
				branch := bson.M{"gems": bson.M{"1": leaf}, "runes": bson.A{leaf}, "core": leaf}
				raw, err := bson.Marshal(bson.M{"_id": int64(42), "equips": bson.M{"1": branch}, "squad": bson.A{branch}, "mount": branch})
				if err != nil {
					t.Fatal(err)
				}
				d := NewHeroDao()
				if err := d.RestorePersisted(raw, HeroDaoSchemaVersion, 7); err != nil {
					t.Fatal(err)
				}
				p := d.GetMount()
				if parent == "map" {
					p, _ = d.GetEquips(1)
				}
				if parent == "slice" {
					p, _ = d.GetSquad(0)
				}
				l := p.GetCore()
				if child == "map" {
					l, _ = p.GetGems(1)
				}
				if child == "slice" {
					l, _ = p.GetRunes(0)
				}
				committer := &ownershipCommitter{}
				if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "restored_descendant", func() (any, error) {
					l.SetLevel(99)
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				if len(committer.records) != 1 {
					t.Fatalf("loaded child level=%d but commit records=%d, want 1", l.GetLevel(), len(committer.records))
				}
			})
		}
	}
}
