package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 本地实体起服预加载（实体类型的显式策略 //roost:entity preload=startup [preload_order=<N>]，维护者 2026-11-13）。
//
// 哪些属于本进程：本地 Nest 实体的 DAO 是 dbscope=sid，物理库 <库>_<sid> 本身就按服区分，所以“本进程的实体”就是
// 本服库里这个 kind 主 DAO（builder 的第一个 DAO）集合中未删除的文档，不另加字段与索引。按 ID 冷加载同样只在本服库
// 里找，别的服的实体在这里加载不到。规则在运行期强制一次（ValidateLocalPreloadKinds）：预加载的 kind 必须是本地
// （没有 Remote 策略）、持久、每个 DAO 都是 dbscope=sid；生成器在生成期检查同一条规则（能读到 DAO 源码时）。
//
// 顺序：按 kind 的 EntityBuilderParam.LoadPriority（生成自 preload_order，缺省 0）从小到大分批；同一批的 kind 一起
// 枚举、并发加载（上限 localPreloadConcurrency），一批全部完成（成功或失败）才开始下一批。
//
// 失败语义与 Remote 预加载（remoteentity.Manager.PreloadOwnedEntities）一致：
//   - 规则不满足、枚举（列 ID 的查询）失败：返回错误，调用方让启动失败，由进程管理拉起重试；
//   - 单个实体加载失败：记错误日志与指标，跳过它，留给按需冷加载（到时同样报错并答给调用方）——不让一个坏文档挡住整个服；
//   - 删过档（全部 DAO 只剩墓碑）与枚举之后被删掉的：确定的状态，不是失败，分别计入 Deleted / Missing。
//
// 加载走正式的实体运行时（ManagerAccess.Get → 共享冷加载 → EntityRepository），与业务访问共用一次加载；发布经 Nest
// 绑定的本地执行器回业务池。所以要在 DataEngine 恢复与 Nest 启动之后、Service.Init 之前调用
// （wiring/dataengine.LocalPreloadMod）。预加载只把实体放进内存，不改变它的生命周期：框架没有按冷热自动卸载，
// 卸载只发生在显式入口（运维 entity.unload、交出实体、业务自己的闲置卸载），被卸载的实体回到按需加载。

const (
	// localPreloadPage 是一次列 ID 的页大小。
	localPreloadPage = 1000
	// localPreloadConcurrency 是一批里同时在途的加载数上限（与 Remote 预加载相同）。
	localPreloadConcurrency = 16
	// metricLocalPreload 计每个实体的预加载结果（标签 kind、result：ok / deleted / missing / error）。
	metricLocalPreload = "dataengine.local_preload_total"
)

// ErrLocalPreloadKind 是预加载的 kind 不满足规则（不是本地持久实体、DAO 不是 dbscope=sid、没有登记 builder）。
var ErrLocalPreloadKind = errors.New("dataengine preload: kind cannot be preloaded locally")

// LocalPreloadCounts 是一个 kind（或全部）的预加载结果。
type LocalPreloadCounts struct {
	// Listed 是枚举到的未删除主 DAO 文档数。
	Listed int
	// Loaded 是加载进内存（或本来就在内存里）的实体数。
	Loaded int
	// Deleted 是删过档的实体数（主 DAO 文档没有墓碑、别的 DAO 只剩墓碑时由加载判定）。
	Deleted int
	// Missing 是枚举之后、加载之前不在了的实体数。
	Missing int
	// Failed 是加载失败、留给按需加载的实体数。
	Failed int
}

func (counts *LocalPreloadCounts) add(other LocalPreloadCounts) {
	counts.Listed += other.Listed
	counts.Loaded += other.Loaded
	counts.Deleted += other.Deleted
	counts.Missing += other.Missing
	counts.Failed += other.Failed
}

// LocalPreloadKindReport 是一个 kind 的预加载结果。
type LocalPreloadKindReport struct {
	Kind entity.EntityKind
	// Order 是它的批次（LoadPriority）。
	Order int
	LocalPreloadCounts
}

// LocalPreloadReport 是一次本地预加载的结果：Kinds 按实际执行的顺序排列，嵌入的计数是总和。
type LocalPreloadReport struct {
	LocalPreloadCounts
	Kinds []LocalPreloadKindReport
}

// localPreloadTarget 是一个预加载 kind 的主 DAO 位置。
type localPreloadTarget struct {
	kind  entity.EntityKind
	order int
	key   coredata.DocumentKey // Database、Scope、Resource；ID 为 0
}

// ValidateLocalPreloadKinds 检查 kinds 都可以做本地预加载：登记了 builder、没有 Remote 策略、持久（不是 noPersist、
// 至少一个 DAO）、每个 DAO 都是 dbscope=sid。每个违规一条错误，用 errors.Join 汇总，都满足 ErrLocalPreloadKind。
// 进程链接了生成的实体包之后调用（builder 在 init 里登记）。
func ValidateLocalPreloadKinds(kinds []entity.EntityKind) error {
	_, err := resolveLocalPreloadTargets(kinds)
	return err
}

func resolveLocalPreloadTargets(kinds []entity.EntityKind) ([]localPreloadTarget, error) {
	var problems []error
	var targets []localPreloadTarget
	seen := map[entity.EntityKind]bool{}
	for _, kind := range kinds {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		builder := entity.GetEntityBuilderParam(kind)
		switch {
		case builder == nil:
			problems = append(problems, fmt.Errorf("%w: kind %d has no registered entity builder", ErrLocalPreloadKind, kind))
			continue
		case entity.GetEntityKindRemotePolicy(kind) != entity.RemotePolicyNone:
			problems = append(problems, fmt.Errorf("%w: kind %d is a Remote kind; a remote=managed kind is preloaded by its owning service "+
				"from the ownership records (remote_entity preload), not from this server's database", ErrLocalPreloadKind, kind))
			continue
		case builder.NoPersist || len(builder.DaoBuilders) == 0:
			problems = append(problems, fmt.Errorf("%w: kind %d is not persistent (noPersist or no DAO); there is nothing stored to preload", ErrLocalPreloadKind, kind))
			continue
		}
		var target localPreloadTarget
		ok := true
		for i, build := range builder.DaoBuilders {
			dao := build()
			if dao == nil {
				problems = append(problems, fmt.Errorf("%w: kind %d dao[%d] builder returned nil", ErrLocalPreloadKind, kind, i))
				ok = false
				break
			}
			scope := coredata.ResolveDatabaseScope(dao)
			if scope != coredata.DatabaseServer {
				problems = append(problems, fmt.Errorf("%w: kind %d dao[%d] %T (collection %q) is dbscope=global; a preloaded local entity is "+
					"\"the documents in this server's database\", so every DAO of it must be dbscope=sid (//roost:dao ... dbscope=sid)",
					ErrLocalPreloadKind, kind, i, dao, dao.CollName()))
				ok = false
				continue
			}
			if i == 0 {
				target = localPreloadTarget{kind: kind, order: builder.LoadPriority,
					key: coredata.DocumentKey{Database: dao.DbName(), Scope: scope, Resource: dao.CollName()}}
			}
		}
		if ok {
			targets = append(targets, target)
		}
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	// 稳定排序：同一批里保持调用方给出的顺序（生成的列表按 kind 排序）。
	slices.SortStableFunc(targets, func(a, b localPreloadTarget) int { return cmp.Compare(a.order, b.order) })
	return targets, nil
}

// ListEntityIDs 按 _id 升序列出 key（Database、Scope、Resource；ID 不用）所指集合里 _id 大于 after 的未删除文档，
// 至多 limit 个；next 是这一页最后一个 _id，页不满时为 0（没有下一页）。dbscope=sid 的集合在本服库（<库>_<sid>）里列。
// 只读 _id。要 I/O，只能在业务池之外调用。
func (store *MongoStore) ListEntityIDs(ctx context.Context, key coredata.DocumentKey, after int64, limit int) (ids []int64, next int64, err error) {
	if err := fctx.BlockingError("dataengine.MongoStore.ListEntityIDs"); err != nil {
		return nil, 0, err
	}
	if store == nil || store.client == nil {
		return nil, 0, coredata.ErrStoreRequired
	}
	if key.Resource == "" || limit <= 0 {
		return nil, 0, errors.New("dataengine mongo: invalid id listing request")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	collection, err := store.collection(key)
	if err != nil {
		return nil, 0, err
	}
	filter := bson.M{"$and": bson.A{bson.M{"_id": bson.M{"$gt": after}}, bson.M{"_deleted": bson.M{"$ne": true}}}}
	var rows []struct {
		ID int64 `bson:"_id"`
	}
	options := fmongo.FindOption{Sort: bson.D{{Key: "_id", Value: 1}}, Limit: int64(limit), Projection: bson.M{"_id": 1}}
	if err := collection.Find(ctx, filter, &rows, options); err != nil {
		return nil, 0, err
	}
	ids = make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if len(ids) == limit {
		next = ids[len(ids)-1]
	}
	return ids, next, nil
}

// PreloadLocalEntities 是本地实体的起服预加载（见本文件开头）：按批次把 kinds 在本服库里的全部未删除实体经实体运行时
// 加载进内存。kinds 不满足规则或枚举失败时返回错误（报告里是已完成的部分），单个实体失败只计入报告。
func (runtime *Runtime) PreloadLocalEntities(ctx context.Context, kinds []entity.EntityKind) (LocalPreloadReport, error) {
	var report LocalPreloadReport
	if len(kinds) == 0 {
		return report, nil
	}
	if err := fctx.BlockingError("dataengine.Runtime.PreloadLocalEntities"); err != nil {
		return report, err
	}
	if runtime == nil || runtime.Store == nil || runtime.access == nil {
		return report, errors.New("dataengine preload: runtime is not configured")
	}
	if !runtime.Ready() {
		return report, coredata.ErrRecoveryIncomplete
	}
	targets, err := resolveLocalPreloadTargets(kinds)
	if err != nil {
		return report, err
	}
	access := runtime.access
	load := func(ctx context.Context, id int64) error {
		value, err := access.Get(ctx, id, entity.EntityCategoryNone)
		if err == nil && value == nil {
			return ErrEntityAggregateNotFound
		}
		return err
	}
	return preloadLocalTargets(ctx, runtime.Store, load, targets)
}

// entityIDLister 是预加载需要的存储能力（MongoStore.ListEntityIDs），测试替换它。
type entityIDLister interface {
	ListEntityIDs(ctx context.Context, key coredata.DocumentKey, after int64, limit int) ([]int64, int64, error)
}

// preloadLocalTargets 按 order 分批执行：同一批的 kind 依次枚举、加载并发（共用一个上限），批末等全部加载结束。
func preloadLocalTargets(ctx context.Context, lister entityIDLister, load func(context.Context, int64) error, targets []localPreloadTarget) (LocalPreloadReport, error) {
	var report LocalPreloadReport
	if ctx == nil {
		ctx = context.Background()
	}
	for start := 0; start < len(targets); {
		end := start + 1
		for end < len(targets) && targets[end].order == targets[start].order {
			end++
		}
		batch := targets[start:end]
		counts, err := preloadLocalBatch(ctx, lister, load, batch)
		for i, target := range batch {
			report.Kinds = append(report.Kinds, LocalPreloadKindReport{Kind: target.kind, Order: target.order, LocalPreloadCounts: counts[i]})
			report.add(counts[i])
		}
		if err != nil {
			return report, err
		}
		start = end
	}
	return report, nil
}

// preloadLocalBatch 加载一批 kind；返回每个 kind 的计数（与 batch 同序）。枚举失败时等已发出的加载结束再返回错误。
func preloadLocalBatch(ctx context.Context, lister entityIDLister, load func(context.Context, int64) error, batch []localPreloadTarget) ([]LocalPreloadCounts, error) {
	counts := make([]LocalPreloadCounts, len(batch))
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, localPreloadConcurrency)
	)
	record := func(index int, field func(*LocalPreloadCounts)) {
		mu.Lock()
		field(&counts[index])
		mu.Unlock()
	}
	for index, target := range batch {
		kindLabel := strconv.Itoa(int(target.kind))
		for after := int64(0); ; {
			ids, next, err := lister.ListEntityIDs(ctx, target.key, after, localPreloadPage)
			if err != nil {
				wg.Wait()
				return counts, fmt.Errorf("dataengine preload: enumerate kind %d in %s/%s: %w", target.kind, target.key.Database, target.key.Resource, err)
			}
			for _, id := range ids {
				record(index, func(c *LocalPreloadCounts) { c.Listed++ })
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					wg.Wait()
					return counts, ctx.Err()
				}
				wg.Add(1)
				go func(index int, id int64) {
					defer wg.Done()
					defer func() { <-sem }()
					err := load(ctx, id)
					var result string
					switch {
					case err == nil:
						result = "ok"
						record(index, func(c *LocalPreloadCounts) { c.Loaded++ })
					case errors.Is(err, entity.ErrAuthorityEntityDeleted):
						result = "deleted"
						record(index, func(c *LocalPreloadCounts) { c.Deleted++ })
					case errors.Is(err, entity.ErrAuthorityEntityNotFound):
						result = "missing"
						record(index, func(c *LocalPreloadCounts) { c.Missing++ })
					default:
						result = "error"
						record(index, func(c *LocalPreloadCounts) { c.Failed++ })
						slog.Error("dataengine preload: loading an entity failed; it is left to on-demand loading", "kind", target.kind, "entity", id, "err", err)
					}
					metrics.IncCounter(metricLocalPreload, metrics.Labels{"kind": kindLabel, "result": result}, 1)
				}(index, id)
			}
			if next == 0 {
				break
			}
			after = next
		}
	}
	wg.Wait()
	return counts, nil
}
