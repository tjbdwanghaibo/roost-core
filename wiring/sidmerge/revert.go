package sidmerge

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 回退（设计 §5.5）：停服窗口里 B 已正常停机、WAL 排空之后，把清单里的每份文档按 B 里的当前版本搬回 A 的源库
// （覆盖归档里的旧版本），再从 B 删掉。版本是从 A 的原值接着增长的，A 用新 WAL 目录启动后冷加载继续。
//
// 合服期间 B 里新产生、不在清单里的按 sid 独占文档，框架无法判断归属（进程只知道自己是 B）：回退不动它们，只在
// 报告里计数，由业务按自己的键规则处理（runbook 写明）。
//
// 先全部核对、再写：清单里有文档在 B 里找不到（被物理删除），或清单记的库名与现在按配置换算的不同，整体拒绝，
// 什么也不写。搬回是逐份的“覆盖 A、按版本删 B”：中途失败时已搬回的文档在 A、不在 B，原样重跑会把它们报成“B 里
// 找不到”——此时看报告里的 reverted 列表，确认后去掉清单里这些项再跑。

// RevertReport 是 Revert 的结论。
type RevertReport struct {
	From   int32   `json:"from"`
	Into   int32   `json:"into"`
	Checks []Check `json:"checks"`
	// Reverted 是搬回 A 的文档数；Missing 是 B 里找不到的清单项（有它们时什么也没写）。
	Reverted int      `json:"reverted"`
	Missing  []string `json:"missing,omitempty"`
	// NewInTarget 是 B 的按 sid 分库里不在清单上的文档（合服期间新产生的），按 "<逻辑库>/<集合>" 计数。
	NewInTarget map[string]int `json:"new_in_target,omitempty"`
}

// Revert 按清单把迁入 B 的文档搬回 A。plan 两侧的配置必须是合服时用的那两份（库名按它们重新换算后与清单比较）。
func Revert(ctx context.Context, plan Plan, manifest Manifest) (RevertReport, error) {
	ctx = withContext(ctx)
	report := RevertReport{From: plan.From.SID, Into: plan.Into.SID}
	if manifest.Version != ManifestVersion {
		return report, fmt.Errorf("sidmerge: manifest version %d is not supported (want %d)", manifest.Version, ManifestVersion)
	}
	if manifest.From != plan.From.SID || manifest.Into != plan.Into.SID {
		return report, fmt.Errorf("sidmerge: the manifest is for %d into %d, not %d into %d", manifest.From, manifest.Into, plan.From.SID, plan.Into.SID)
	}
	r, err := openRun(ctx, plan)
	if err != nil {
		return report, err
	}
	defer r.close()

	// B 必须已停、排空：搬走的文档正是它的数据。
	report.Checks = append(report.Checks, walCheck(ctx, r.into))
	databases := Check{Name: "manifest-databases", Status: StatusOK, Detail: "the manifest's databases match the configs"}
	byLogical := map[string]ScopedDatabase{}
	for _, entry := range manifest.Databases {
		current := r.scopedDatabase(entry.Logical)
		if current.Source != entry.Source || current.Target != entry.Target || current.SourceMongo != entry.SourceMongo || current.TargetMongo != entry.TargetMongo {
			databases.Status = StatusFail
			databases.Detail = fmt.Sprintf("logical database %q: the manifest moved %s (%s) into %s (%s), the configs now say %s (%s) into %s (%s); pass the configs used for the merge",
				entry.Logical, entry.Source, entry.SourceMongo, entry.Target, entry.TargetMongo, current.Source, current.SourceMongo, current.Target, current.TargetMongo)
			break
		}
		byLogical[entry.Logical] = current
	}
	report.Checks = append(report.Checks, databases)
	if err := notReady(report.Checks); err != nil {
		return report, err
	}

	// 核对：每份清单文档都在 B 里。
	listed := map[string]map[string]bool{} // "<逻辑库>/<集合>" → _id 集合
	for _, document := range manifest.Documents {
		scoped, ok := byLogical[document.Logical]
		if !ok {
			return report, fmt.Errorf("sidmerge: manifest document %s names logical database %q, which the manifest does not list", document.ID, document.Logical)
		}
		filter, err := idFilter(document.ID)
		if err != nil {
			return report, err
		}
		count, err := r.intoMongo.Database(scoped.Target).Collection(document.Collection).CountDocuments(ctx, filter)
		if err != nil {
			return report, err
		}
		if count == 0 {
			report.Missing = append(report.Missing, document.Logical+"/"+document.Collection+" "+document.ID)
		}
		key := document.Logical + "/" + document.Collection
		if listed[key] == nil {
			listed[key] = map[string]bool{}
		}
		listed[key][document.ID] = true
	}
	if len(report.Missing) > 0 {
		report.Checks = append(report.Checks, Check{Name: "manifest-documents", Status: StatusFail,
			Detail: fmt.Sprintf("%d manifest documents are not in B (listed under missing); nothing was written", len(report.Missing))})
		return report, notReady(report.Checks)
	}
	report.Checks = append(report.Checks, Check{Name: "manifest-documents", Status: StatusOK, Detail: fmt.Sprintf("all %d manifest documents are in B", len(manifest.Documents))})

	// 合服期间新产生的：只计数。
	report.NewInTarget = map[string]int{}
	for logical, scoped := range byLogical {
		names, err := collectionNames(ctx, r.intoMongo.Database(scoped.Target))
		if err != nil {
			return report, err
		}
		for _, name := range names {
			key := logical + "/" + name
			err := eachDocument(ctx, r.intoMongo.Database(scoped.Target).Collection(name), func(raw bson.Raw) error {
				if !listed[key][idString(raw)] {
					report.NewInTarget[key]++
				}
				return nil
			})
			if err != nil {
				return report, err
			}
		}
	}

	for _, document := range manifest.Documents {
		scoped := byLogical[document.Logical]
		if err := r.moveBack(ctx, scoped, document); err != nil {
			return report, fmt.Errorf("sidmerge: revert %s/%s %s after %d documents: %w", document.Logical, document.Collection, document.ID, report.Reverted, err)
		}
		report.Reverted++
	}
	return report, nil
}

// moveBack 把一份文档按 B 里的当前内容写回 A（覆盖归档），再按读到的版本从 B 删掉。
func (r *run) moveBack(ctx context.Context, scoped ScopedDatabase, document ManifestDocument) error {
	filter, err := idFilter(document.ID)
	if err != nil {
		return err
	}
	target := r.intoMongo.Database(scoped.Target).Collection(document.Collection)
	var current bson.Raw
	if err := target.FindOne(ctx, filter).Decode(&current); err != nil {
		return fmt.Errorf("read from %s: %w", scoped.Target, err)
	}
	source := r.fromMongo.Database(scoped.Source).Collection(document.Collection)
	if _, err := source.ReplaceOne(ctx, filter, current, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("write back to %s: %w", scoped.Source, err)
	}
	guard := append(bson.D(nil), filter...)
	if value, err := current.LookupErr("_version"); err == nil {
		guard = append(guard, bson.E{Key: "_version", Value: value})
	}
	deleted, err := target.DeleteOne(ctx, guard)
	if err != nil {
		return fmt.Errorf("delete from %s: %w", scoped.Target, err)
	}
	if deleted.DeletedCount != 1 {
		return errors.New("the document changed in " + scoped.Target + " while being reverted; is B running?")
	}
	return nil
}
