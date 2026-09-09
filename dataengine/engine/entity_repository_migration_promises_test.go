package engine

import (
	"context"
	"errors"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0109 · C2（空洞测试）· `entity_repository` 迁移三次仍冲突（U-0101 留下的一条）。
//
// 装载时发现存储 schema 落后就迁移一次再重读；如果迁移"成功"了但重读仍然落
// 后（别的进程并发改写、投影被回滚、迁移写到了别的文档），不能无限循环也
// 不能把落后的文档当作已迁移发布——第三次必须以 ErrMigrationConflict 退出。
// 需要一个 SystemCommitter 替身：提交立即"投影完成"，但什么都不写。

type doneTicket struct{ done chan struct{} }

func (t doneTicket) Done() <-chan struct{} { return t.done }
func (doneTicket) Err() error              { return nil }

type phantomSystemCommitter struct{ commits int }

func (c *phantomSystemCommitter) CommitSystem(context.Context, coredata.CommitRecord) (coredata.ProjectionTicket, error) {
	c.commits++
	done := make(chan struct{})
	close(done)
	return doneTicket{done: done}, nil
}

func TestEntityRepositoryGivesUpAfterThreeNonConvergingMigrations(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	ctx := context.Background()
	id, err := entity.BuildEntityID(1401, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	stale := repositoryRaw(t, "repository_profile", id, 1)
	stale.Schema = 0 // behind the DAO's schema 1: needs a migration
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{
		"repository_profile":   {stale},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
	}}
	committer := &phantomSystemCommitter{}
	migration, err := NewMigrationRunner(committer)
	if err != nil {
		t.Fatal(err)
	}
	manager := entity.NewEntityManager()
	repository, err := newEntityRepository(manager, store, migration, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
	if !errors.Is(err, coredata.ErrMigrationConflict) {
		t.Fatalf("LoadEntity = %v, want ErrMigrationConflict", err)
	}
	// 两次迁移尝试各提交一次；第三次重读仍落后时不再提交，直接放弃。
	if committer.commits != 2 {
		t.Fatalf("migration commits = %d, want 2", committer.commits)
	}
	if manager.Get(id) != nil {
		t.Fatal("a non-converging aggregate was published")
	}
}
