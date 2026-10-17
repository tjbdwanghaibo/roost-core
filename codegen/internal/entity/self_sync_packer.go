package entity

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// access.player.self_sync 的默认 packer（维护者 2026-10-17 决定）。
//
// roost 在 roost.yaml 打开 self_sync 时用 -self-sync <实体名> 调本生成器。对那个实体：
//
//   - 开关就是“这个实体要 Entity Sync”：标记没写 sync= 时按 sync=true 生成，没写 syncNamespace= 时取
//     实体名小写；显式写 sync=false 与开关矛盾，拒绝。
//   - 标记没写 subjectPacker= 时生成 <snake>_sync_packer_gen.go（New<Name>SyncPacker：视图集只有
//     policy.SelfProfile，包含实体唯一 DAO 的全部 sync 字段），wire 文件的 PackerFactory 指向它；
//     写了 subjectPacker= 时用业务的工厂，不生成、并删掉以前生成的那份。
//   - 不带 -self-sync（开关关闭）时不生成，以前生成的那份同样删掉；其余输出与开关加入前相同。
//
// 代价：标了 sync 的字段默认全部对玩家本人可见；要裁剪字段或别的视图（AOI 他人视图）就自己写 packer。

// selfSyncPackerSuffix 是默认 packer 的文件名后缀：player_sync_packer_gen.go。
const selfSyncPackerSuffix = "_sync_packer_gen.go"

// SelfSyncPlan 是 -self-sync 实体的就绪结论，本生成器与 roost doctor / generate 的前置检查共用。
type SelfSyncPlan struct {
	// Packer 是 wire 文件里 PackerFactory 的表达式：业务写的 subjectPacker，或生成的 New<Name>SyncPacker。
	Packer string
	// DefaultPacker 为 true 时由生成器写 PackerFile。
	DefaultPacker bool
	PackerFile    string
	// DaoType 是默认 packer 打包的 DAO（如 *db.PlayerDao）。
	DaoType string
	// NoSyncFields：默认 packer 的 DAO 在生成的 DAO 包里没有 SyncFields，即没有标 sync 的字段，
	// 客户端收不到任何字段。只有能读到 DAO 包源码（模块内）时才判断。
	NoSyncFields bool
}

// InspectSelfSync 读 dir 包里名为 name 的实体，按 -self-sync 的缺省规则给出结论；拒绝时错误里写明改法。
func InspectSelfSync(dir, name string) (SelfSyncPlan, error) {
	defs, _, err := parsePackage(dir)
	if err != nil {
		return SelfSyncPlan{}, err
	}
	for i := range defs.Entities {
		if defs.Entities[i].Name == name {
			return applySelfSyncDefaults(&defs.Entities[i], dir)
		}
	}
	return SelfSyncPlan{}, fmt.Errorf("%s: no //roost:entity struct named %s", filepath.ToSlash(dir), name)
}

// applySelfSyncDefaults 把开关的缺省写进 ent（Sync / SyncNamespace / SubjectPacker），返回要生成什么。
func applySelfSyncDefaults(ent *EntityDef, dir string) (SelfSyncPlan, error) {
	if ent.SyncSet && !ent.Sync {
		return SelfSyncPlan{}, fmt.Errorf("%s: the //roost:entity marker of %s says sync=false, but access.player.self_sync sends this entity to its player through Entity Sync; drop sync=false from the marker (self_sync turns Entity Sync on), or turn self_sync off in roost.yaml",
			filepath.ToSlash(ent.SourceFile), ent.Name)
	}
	ent.Sync = true
	if ent.SyncNamespace == "" {
		ent.SyncNamespace = strings.ToLower(ent.Name)
	}
	plan := SelfSyncPlan{Packer: ent.SubjectPacker}
	if ent.SubjectPacker != "" {
		return plan, nil
	}
	switch len(ent.Daos) {
	case 0:
		return SelfSyncPlan{}, fmt.Errorf(`entity %s has no DAO field, so the default self_sync packer has nothing to send; run: roost add dao %s --entity %s, then tag the fields the client should see as dao:"persist,sync"`,
			ent.Name, ent.Name, ent.Name)
	case 1:
	default:
		names := make([]string, 0, len(ent.Daos))
		for _, dao := range ent.Daos {
			names = append(names, dao.FieldName+" "+dao.TypeName)
		}
		return SelfSyncPlan{}, fmt.Errorf("entity %s has %d DAO fields (%s); the default self_sync packer sends exactly one DAO. Write your own packer whose view set accepts the profile Key \"self\" (policy.SelfProfile) and name it on the marker: subjectPacker=New%sSyncPacker",
			ent.Name, len(ent.Daos), strings.Join(names, ", "), ent.Name)
	}
	dao := ent.Daos[0]
	ent.SubjectPacker = "New" + ent.Name + "SyncPacker"
	plan.Packer = ent.SubjectPacker
	plan.DefaultPacker = true
	plan.PackerFile = toSnake(ent.Name) + selfSyncPackerSuffix
	plan.DaoType = dao.TypeName
	if pkgDir, typeName := daoSourceDir(*ent, dir, dao.TypeName); pkgDir != "" {
		has, err := declaresFunc(pkgDir, typeName+"SyncFields")
		if err != nil {
			return SelfSyncPlan{}, fmt.Errorf("entity %s: inspect DAO %s: %w", ent.Name, dao.TypeName, err)
		}
		plan.NoSyncFields = !has
	}
	return plan, nil
}

// NoSyncFieldsWarning 是默认 packer 的 DAO 没有 sync 字段时的提示（生成器输出与 doctor 共用）。
func (plan SelfSyncPlan) NoSyncFieldsWarning(entityName string) string {
	return fmt.Sprintf(`%s has no field tagged sync, so the player receives nothing about its own %s; tag the fields the client should see as dao:"persist,sync" in db/def, then run roost generate`,
		strings.TrimPrefix(plan.DaoType, "*"), entityName)
}

// validateEntitySync 是标记的 sync 参数一致性：写了 subjectPacker= 却没开 sync 的标记什么也不做，拒绝。
// 在 -self-sync 缺省之后执行，所以开关打开时 Player 只写 subjectPacker= 也行。
func validateEntitySync(ent EntityDef) error {
	if ent.SubjectPacker != "" && !ent.Sync {
		return fmt.Errorf("%s: //roost:entity has subjectPacker=%s but sync is not true", filepath.ToSlash(ent.SourceFile), ent.SubjectPacker)
	}
	return nil
}

// declaresFunc reports whether a non-test file in pkgDir declares a
// package-level function (not a method) with the given name.
func declaresFunc(pkgDir, name string) (bool, error) {
	entries, err := os.ReadDir(pkgDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, file), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// generateSelfSyncPacker 写默认 packer；内容不变时不写。
func generateSelfSyncPacker(ent EntityDef, plan SelfSyncPlan, pkg, dir string, force bool) (bool, error) {
	data := struct {
		Package, Name, Lower, Packer, DaoType, DaoGetter, Import string
	}{
		Package: pkg,
		Name:    ent.Name,
		Lower:   firstToLower(ent.Name),
		Packer:  plan.Packer,
		DaoType: derefType(plan.DaoType),
	}
	for _, dao := range ent.Daos {
		if dao.TypeName == plan.DaoType {
			data.DaoGetter = dao.GetterName
		}
	}
	if alias := qualifier(data.DaoType); alias != "" {
		for _, imp := range ent.Imports {
			if imp.Alias == alias {
				data.Import = fmt.Sprintf("%s %q", alias, imp.Path)
			}
		}
		if data.Import == "" {
			return false, fmt.Errorf("entity %s: DAO type %s: no import for %q", ent.Name, plan.DaoType, alias)
		}
	}
	var buf bytes.Buffer
	if err := selfSyncPackerTemplate.Execute(&buf, data); err != nil {
		return false, fmt.Errorf("self_sync packer template: %w", err)
	}
	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format self_sync packer: %w", err)
	}
	path := filepath.Join(dir, plan.PackerFile)
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.HasPrefix(existing, []byte(entityGeneratedHeader)) {
			return false, fmt.Errorf("%s exists but is not generated (no %q header); it is where the default self_sync packer goes. Rename your file, or name your packer on the marker: subjectPacker=%s",
				filepath.ToSlash(path), entityGeneratedHeader, plan.Packer)
		}
		if !force && bytes.Equal(existing, content) {
			return false, nil
		}
	}
	return true, os.WriteFile(path, content, 0o644)
}

// retireSelfSyncPackers 删掉 dir 里不再需要的、本生成器写的默认 packer（keep 是仍要的文件名）。
// 业务自己写的同名文件没有生成头，不动。
func retireSelfSyncPackers(dir, keep string, stdout io.Writer) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if name := entry.Name(); name != keep && strings.HasSuffix(name, selfSyncPackerSuffix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		removed, err := retireEntityFile(path)
		if err != nil {
			return fmt.Errorf("retire self_sync packer %s: %w", path, err)
		}
		if removed {
			fmt.Fprintf(stdout, "removed: %s\n", path)
		}
	}
	return nil
}

const entityGeneratedHeader = "// Code generated by tool/entity. DO NOT EDIT."

var selfSyncPackerTemplate = template.Must(template.New("self_sync_packer").Parse(entityGeneratedHeader + `

// 这是 access.player.self_sync 的默认 packer：roost.yaml 打开 self_sync、{{.Name}} 的 //roost:entity 标记
// 没写 subjectPacker= 时由生成器写出，roost generate / roost project sync 会重写，不要修改。
//
// 视图集只有自身视图 "self"（policy.SelfProfile，玩家本人），包含 {{.DaoType}} 全部标了 sync 的字段：
// 标了 sync 的字段默认全部对玩家本人可见。需要别的视图（AOI 里别人看到的视图、按字段裁剪）时自己写
// packer，在标记上写 subjectPacker=<你的工厂>，这个文件随之删除。

package {{.Package}}

import (
	"fmt"
{{if .Import}}
	{{.Import}}
{{- end}}
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
)

// {{.Lower}}SelfSyncCodec 是 "self" 视图载荷的编码号；载荷是 DAO 的 MarshalSync，客户端交给 ApplySync。
const {{.Lower}}SelfSyncCodec uint16 = 1

var {{.Lower}}SelfSyncViews, {{.Lower}}SelfSyncViewsErr = dataengine.NewSyncViewSet({{.Lower}}SelfSyncFields(),
	entity.NamedSyncView{Profile: policy.SelfProfile, Fields: []string{"*"}},
)

// {{.Lower}}SelfSyncFields 取 DAO 的 sync 字段表。DAO 没有标 sync 的字段时生成的 DAO 不带 SyncFields，
// 视图为空、客户端什么也收不到（roost doctor 给警告）；按方法取而不是直接调用，DAO 增删 sync 字段后
// 这个文件不用跟着变。
func {{.Lower}}SelfSyncFields() []dataengine.SyncFieldMeta {
	table, ok := any((*{{.DaoType}})(nil)).(interface{ SyncFields() []dataengine.SyncFieldMeta })
	if !ok {
		return nil
	}
	return table.SyncFields()
}

// {{.Packer}} 是 {{.Name}} 的 SubjectSyncPacker 工厂（wire 文件的 PackerFactory）。
func {{.Packer}}(value entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	owner, _ := value.(*{{.Name}})
	return {{.Lower}}SelfSyncPacker{owner: owner}
}

type {{.Lower}}SelfSyncPacker struct{ owner *{{.Name}} }

func (packer {{.Lower}}SelfSyncPacker) PackSubjectSnapshot(profile entity.SyncProfile) (entity.FrozenSyncPayload, error) {
	return packer.pack(profile, dataengine.AllFields)
}

func (packer {{.Lower}}SelfSyncPacker) PackSubjectDelta(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	return packer.pack(profile, mask)
}

// pack 只交出视图允许的字段；未知视图返回 entity.ErrSyncViewUnknown，不扩大字段权限。
func (packer {{.Lower}}SelfSyncPacker) pack(profile entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
	if {{.Lower}}SelfSyncViewsErr != nil {
		return entity.FrozenSyncPayload{}, fmt.Errorf("{{.Lower}} self_sync packer: %w", {{.Lower}}SelfSyncViewsErr)
	}
	if packer.owner == nil {
		return entity.FrozenSyncPayload{}, fmt.Errorf("{{.Lower}} self_sync packer: the entity is not a *{{.Name}}")
	}
	view, ok := {{.Lower}}SelfSyncViews.Lookup(profile)
	if !ok {
		return entity.FrozenSyncPayload{}, fmt.Errorf("%w: %+v", entity.ErrSyncViewUnknown, profile)
	}
	if mask &= view.Fields; mask == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	dao := packer.owner.{{.DaoGetter}}()
	if dao == nil {
		return entity.FrozenSyncPayload{}, nil
	}
	raw := dao.MarshalSync(mask)
	if len(raw) == 0 {
		return entity.FrozenSyncPayload{}, nil
	}
	return entity.TakeFrozenSyncPayload({{.Lower}}SelfSyncCodec, raw), nil
}
`))
