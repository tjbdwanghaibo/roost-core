package roost

import (
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/attribute"
)

// attributeDir 是属性 profile 所在的包，roost generate 的属性生成器与 project sync 写的 doc.go / runtime.go 都在这里。
const attributeDir = "game/gameplay/attribute"

// addAttribute 是属性生成器的上手入口（roost add attribute <Profile>）：写一个带 dirtyMask 的 profile 骨架（业务文件，
// 只建一次），立即跑属性生成器，再在 feature 还没开启时开启它（project sync 写出 doc.go 与受控的 runtime.go——生成
// 代码引用的 AttrID、AttributeProfile 等类型在那里）。任何一步失败都回滚已写文件，工程不留半成品。
// 生成器只生成框架内容：profile 的字段与派生公式是业务的，骨架给出示例后归项目所有。
func addAttribute(root string, m Manifest, o AddOptions) ([]string, error) {
	snake, pascal := toSnake(o.Name), toPascal(o.Name)
	if !validName(snake) {
		return nil, fmt.Errorf("invalid attribute profile name %q", o.Name)
	}
	dir := filepath.Join(root, filepath.FromSlash(attributeDir))
	profileRel := attributeDir + "/" + snake + ".go"
	generatedRel := attributeDir + "/gen_" + snake + "_attribute.go"
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(profileRel))); err == nil {
		return nil, fmt.Errorf("%s already exists", profileRel)
	}
	existing, err := attribute.Profiles(dir)
	if err != nil {
		return nil, fmt.Errorf("read the existing attribute profiles in %s (fix them first): %w", attributeDir, err)
	}
	index := 1
	for _, profile := range existing {
		if profile.Name == pascal {
			return nil, fmt.Errorf("attribute profile %s is already declared in %s", pascal, attributeDir)
		}
		index = max(index, profile.IndexBegin+profile.MaxCount)
	}
	body, err := format.Source([]byte(renderAttributeProfile(pascal, index, len(existing) == 0)))
	if err != nil {
		return nil, err
	}

	backups, err := captureFiles(root, []string{profileRel, generatedRel})
	if err != nil {
		return nil, err
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(root, filepath.FromSlash(profileRel)), body, 0o644); err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	if err := attribute.Run([]string{"-dir", dir}, io.Discard); err != nil {
		return nil, restoreFiles(root, backups, fmt.Errorf("generate %s: %w", generatedRel, err))
	}
	created := []string{profileRel, generatedRel}
	if hasFeature(m, "attribute") {
		return created, nil
	}
	after, err := captureFiles(root, []string{profileRel, generatedRel})
	if err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	m.Features = uniqueSorted(append(m.Features, "attribute"))
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return nil, restoreFilesIfCurrent(root, backups, after, err)
	}
	return append(created, ManifestName, attributeDir+"/doc.go", attributeDir+"/runtime.go"), nil
}

// renderAttributeProfile 是 profile 骨架。第一个 profile 带示例字段与一个派生公式；包里已有 profile 时只留
// dirtyMask 与 TODO（生成的 Attr<Field> 常量按字段名命名，示例字段会与已有 profile 撞名）。
func renderAttributeProfile(pascal string, index int, withExample bool) string {
	fields := "\t// TODO: integer attribute fields, e.g. MP int64. Field names must be unique\n\t// across the profiles of this package (the generated constants are Attr<Field>).\n"
	formula := ""
	if withExample {
		fields = "\tHP     int64\n\tAttack int64\n\tPower  int64 // derived: see _Power\n"
		formula = fmt.Sprintf(`
// _Power is Power's derived formula: the parameters name the fields it reads,
// and the generated setters recompute Power whenever HP or Attack changes.
// Cycles, unknown fields and type mismatches are rejected by roost generate.
func (p *%s) _Power(Attack int64, HP int64) int64 {
	return Attack*2 + HP/10
}
`, pascal)
	}
	return fmt.Sprintf(`package attribute

// %[1]s is an attribute profile (roost add attribute; a business file:
// generated once, yours afterwards). roost generate turns it into
// gen_<profile>_attribute.go: one AttrID and dirty-mask bit per field, typed
// setters, derived-value recomputation, Snapshot and container accessors.
//
// Every exported field is an attribute and must be an integer (AttrValue is
// int64; store a scaled integer for fractions); attr:"name" renames one,
// attr:"-" leaves it out. ids run from index= upward, at most max= fields;
// when one container holds several profiles give each its own index= range.
//
//roost:attribute index=%[2]d max=64
type %[1]s struct {
%[3]s
	dirtyMask uint64 // the generated setters mark changed fields here; keep it
}
%[4]s`, pascal, index, fields, formula)
}

var errAttributeRuntimeMissing = errors.New("the attribute feature is enabled but " + attributeDir + "/runtime.go is missing; run roost project sync (it writes the attribute runtime the generated code needs), or start with roost add attribute <Profile>")
