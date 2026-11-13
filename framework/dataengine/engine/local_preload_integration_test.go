//go:build integration

package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 真实副本集上的本地预加载与搬档：两个 sid 共用一个 Mongo（逻辑库 game 映射到一次性的物理名，库是 <物理名>_<sid>）。
// 覆盖替身不实现的两处：列 ID 的投影 / 排序 / 分页，跨库的搬档事务。资源：ROOST_DATAENGINE_IT_MONGO_URI 指向的副本集，
// 只用 <一次性物理名>_3 与 _4 两个库，用后删除。
func TestRealMongoLocalPreloadAndCopyAcrossServers(t *testing.T) {
	ctx, client, physical := isolatedRealMongo(t)
	t.Cleanup(func() {
		for _, sid := range []int32{3, 4} {
			_ = client.DatabaseForSid(physical, sid).Drop(context.Background())
		}
	})
	ensurePreloadKinds()
	server := func(sid int32) *preloadServer {
		databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{
			Declared: []coredata.LogicalDatabase{{Name: testDatabase, ServerScoped: true}}, Configured: map[string]string{testDatabase: physical}, ServerID: sid,
		})
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewMongoStore(client, MongoStoreConfig{Databases: databases, DefaultDatabase: physical, ServerID: sid, TransactionReceiptTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		s := &preloadServer{sid: sid, store: store, manager: entity.NewEntityManager(), loaded: &loadOrder{slow: map[entity.EntityKind]time.Duration{}}}
		s.access = entity.NewManagerAccess(s.manager)
		repository, err := NewEntityRepository(s.manager, store, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.access.ConfigureLoader(repository); err != nil {
			t.Fatal(err)
		}
		s.runtime = &Runtime{Store: store, Repository: repository, access: s.access}
		s.runtime.ready.Store(true)
		return s
	}
	seed := func(sid int32, base int64, count int) []int64 {
		coll := client.DatabaseForSid(physical, sid).Collection("preload_first")
		var ids []int64
		for i := 0; i < count; i++ {
			id, err := entity.BuildEntityID(base+int64(i), preloadFirstKind)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := coll.InsertOne(ctx, bson.M{"_id": id, "_version": uint64(3), "_schema": uint32(1), "_last_tx": fmt.Sprintf("tx-%d", id)}); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	three, four := server(3), server(4)
	ofThree, ofFour := seed(3, 1000, 2500), seed(4, 9000, 3)

	// 列 ID：分页（2500 跨三页）、只读本服库。
	report, err := three.runtime.PreloadLocalEntities(ctx, []entity.EntityKind{preloadFirstKind})
	if err != nil || report.Listed != 2500 || report.Loaded != 2500 {
		t.Fatalf("sid 3 preload = %+v, %v; want its 2500 entities", report, err)
	}
	if four.manager.Get(ofThree[0]) != nil || three.manager.Get(ofFour[0]) != nil {
		t.Fatal("an entity of one server was loaded by the other")
	}
	if report, err := four.runtime.PreloadLocalEntities(ctx, []entity.EntityKind{preloadFirstKind}); err != nil || report.Loaded != 3 {
		t.Fatalf("sid 4 preload = %+v, %v; want its 3 entities", report, err)
	}

	// 搬档：sid 3 交出一个实体，复制到 sid 4 的库，删本服的；之后 3 不再列出它，4 能加载它。
	copier := server(3)
	copier.runtime.Projector, _ = manualProjector(t, copier.store)
	moved := ofThree[7]
	copier.runtime.Projector.holdEntityHandOff(moved)
	copied, err := copier.runtime.CopyHandedOffEntity(ctx, moved, 4, true)
	if err != nil || len(copied.Documents) != 1 || copied.Documents[0].Outcome != EntityCopyCopied {
		t.Fatalf("copy = %+v, %v", copied, err)
	}
	if again, err := copier.runtime.CopyHandedOffEntity(ctx, moved, 4, false); err != nil || again.Documents[0].Outcome != EntityCopyAlreadyPresent {
		t.Fatalf("copy again = %+v, %v; want already_present", again, err)
	}
	if dropped, err := copier.runtime.DropEntityCopy(ctx, copied, 3); err != nil || dropped != 1 {
		t.Fatalf("drop the source = %d, %v", dropped, err)
	}
	var doc bson.M
	if err := client.DatabaseForSid(physical, 3).Collection("preload_first").FindOne(ctx, bson.M{"_id": moved}, &doc); err != fmongo.ErrNotFound {
		t.Fatalf("the source document after the move: %v, %v", doc, err)
	}
	restarted4 := server(4)
	if value, err := restarted4.access.Get(ctx, moved, entity.EntityCategoryNone); err != nil || value == nil {
		t.Fatalf("sid 4 loading the moved entity = %v, %v", value, err)
	}
	if report, err := server(3).runtime.PreloadLocalEntities(ctx, []entity.EntityKind{preloadFirstKind}); err != nil || report.Listed != 2499 {
		t.Fatalf("sid 3 preload after the move = %+v, %v; want 2499", report, err)
	}
}
