package roost

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

// wireModuleSync 只补目标 Entity 的标记；已有明确关闭或另一份 packer 时拒绝，避免静默换掉业务权限。
// 调用方已把 entity.go 纳入安装回滚，默认自身 packer 由后续 generate 正式退役。
func wireModuleSync(root string, spec ModuleSpec) error {
	_, path, err := resolveEntitySource(root, spec.Entity)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := moduleSyncMarker(raw, path, spec.Entity, spec.SyncPacker)
	if err != nil {
		return err
	}
	return writeAtomic(path, updated, 0644)
}

func moduleSyncMarker(raw []byte, path, entityName, packer string) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.TYPE {
			continue
		}
		for _, definition := range group.Specs {
			typ, ok := definition.(*ast.TypeSpec)
			if !ok || typ.Name.Name != entityName {
				continue
			}
			comments := typ.Doc
			if comments == nil {
				comments = group.Doc
			}
			if comments == nil {
				break
			}
			for _, comment := range comments.List {
				if !strings.HasPrefix(comment.Text, "//roost:entity ") {
					continue
				}
				fields := strings.Fields(comment.Text)
				haveSync, havePacker := false, false
				for _, field := range fields[1:] {
					if strings.HasPrefix(field, "sync=") {
						if field != "sync=true" {
							return nil, fmt.Errorf("%s: %s explicitly disables sync", path, entityName)
						}
						haveSync = true
					}
					if strings.HasPrefix(field, "subjectPacker=") {
						if field != "subjectPacker="+packer {
							return nil, fmt.Errorf("%s: %s already has %s; keep the business packer and integrate the module views explicitly", path, entityName, field)
						}
						havePacker = true
					}
				}
				text := comment.Text
				if !haveSync {
					text += " sync=true"
				}
				if !havePacker {
					text += " subjectPacker=" + packer
				}
				start, end := fset.Position(comment.Pos()).Offset, fset.Position(comment.End()).Offset
				result := append([]byte(nil), raw[:start]...)
				result = append(result, text...)
				result = append(result, raw[end:]...)
				return result, nil
			}
		}
	}
	return nil, fmt.Errorf("%s: %s has no roost entity marker", path, entityName)
}
