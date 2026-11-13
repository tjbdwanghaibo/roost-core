package entity

import (
	"bytes"
	"fmt"
	"go/format"
	"path/filepath"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/genutil"
)

// lifecycleFile 是每个实体包的显式建 / 读 / 删入口（v1.47.0 起由生成器产出；此前是 `roost add lifecycle` 写进 game/lifecycle/
// 的一次性脚手架）。实现在 core 的 wiring/entitylifecycle（泛型），这里只按实体名起名字。
const lifecycleFile = "lifecycle_gen.go"

// lifecycleNames 是一个实体在包里的入口名：包里只有一个实体时用短名（player.LifecycleFromRegistry），
// 有几个实体时带实体名（<Name>LifecycleFromRegistry），避免互相覆盖。
type lifecycleNames struct {
	Entity, Type, New, FromRegistry, NotFound, Label string
}

func newLifecycleNames(entityName string, short bool) lifecycleNames {
	label := strings.ReplaceAll(toSnake(entityName), "_", " ")
	if short {
		return lifecycleNames{Entity: entityName, Type: "Lifecycle", New: "NewLifecycle", FromRegistry: "LifecycleFromRegistry", NotFound: "ErrNotFound", Label: label}
	}
	return lifecycleNames{Entity: entityName, Type: entityName + "Lifecycle", New: "New" + entityName + "Lifecycle",
		FromRegistry: entityName + "LifecycleFromRegistry", NotFound: "Err" + entityName + "NotFound", Label: label}
}

// generateLifecycle 写出 dir/lifecycle_gen.go：entities 是本包全部实体（按名字排序）。
func generateLifecycle(dir, pkg string, entities []EntityDef, force bool) (string, bool, error) {
	var b bytes.Buffer
	b.WriteString(entityGeneratedHeader + "\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString("import (\n\t\"errors\"\n\n")
	b.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/app\"\n")
	b.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n")
	b.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/wiring/entitylifecycle\"\n)\n\n")
	for _, ent := range entities {
		names := newLifecycleNames(ent.Name, len(entities) == 1)
		kind := ent.EntityKind
		fmt.Fprintf(&b, "// %s 是 Get 找不到 %s 时返回的错误（errors.Is 判断）。\n", names.NotFound, ent.Name)
		fmt.Fprintf(&b, "var %s = errors.New(%q)\n\n", names.NotFound, names.Label+" lifecycle: Entity not found")
		fmt.Fprintf(&b, "// %s 是 %s 的显式建 / 读 / 删边界（登录建角色、确保单例、运维删档）。普通读写走生成的 Nest sender。\n", names.Type, ent.Name)
		fmt.Fprintf(&b, "// 语义见 wiring/entitylifecycle：GetOrCreate 不重建删过档的实体，Create 只用于发号器给的新号。\n")
		fmt.Fprintf(&b, "type %s = entitylifecycle.Lifecycle[*%s]\n\n", names.Type, ent.Name)
		fmt.Fprintf(&b, "// %s 用一份 ManagerAccess 构造 %s。\n", names.New, names.Type)
		fmt.Fprintf(&b, "func %s(access *entity.ManagerAccess) (*%s, error) {\n", names.New, names.Type)
		fmt.Fprintf(&b, "\treturn entitylifecycle.New[*%s](access, %s, %q, %s)\n}\n\n", ent.Name, kind, names.Label, names.NotFound)
		fmt.Fprintf(&b, "// %s 是常规入口：Nest 以 entity.runtime 发布 ManagerAccess，这里从 App 的 Registry 取。\n", names.FromRegistry)
		fmt.Fprintf(&b, "func %s(registry *app.Registry) (*%s, error) {\n", names.FromRegistry, names.Type)
		fmt.Fprintf(&b, "\treturn entitylifecycle.FromRegistry[*%s](registry, %s, %q, %s)\n}\n\n", ent.Name, kind, names.Label, names.NotFound)
	}
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return "", false, fmt.Errorf("format %s: %w", lifecycleFile, err)
	}
	path := filepath.Join(dir, lifecycleFile)
	changed, err := genutil.WriteIfChanged(path, formatted, force)
	return path, changed, err
}

// retireLifecycle 删掉不再有实体的包里生成的 lifecycle_gen.go（只删带生成头的）。
func retireLifecycle(dir string) (bool, error) {
	return retireEntityFile(filepath.Join(dir, lifecycleFile))
}
