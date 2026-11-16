package protocol

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"path"
	"strconv"
	"strings"
)

// GoImport 是生成代码要用的一条 import。Name 为空时代码里用路径最后一段作包名（pb、fmt、playeraccess……）。
type GoImport struct {
	Name, Path string
}

// localName 是代码里引用这个包用的名字。
func (imp GoImport) localName() string {
	if imp.Name != "" {
		return imp.Name
	}
	return path.Base(imp.Path)
}

// goImportBlockPrefix 是 addGoImports 插入的 import 块的开头。IsControllerStubAppend 靠它认出插入的块，两处必须一致。
const goImportBlockPrefix = "\n\nimport (\n"

// AppendGoDecls 把 decls（一组声明的源码，会先 gofmt）追加到 src 末尾，并把缺的 import 作为一个新的 import 块
// 插在最后一个 import 声明之后（没有 import 时插在 package 子句之后）。src 原有的字节一个不改、顺序不变——
// 业务文件（game/controllers/<分组>.go）里协议生成器只做追加，不重排、不重新格式化业务写的内容。
//
// 同一个名字已经指向别的路径时报错（追加的代码会引用错包）；路径已在、名字相同时不重复插入。
func AppendGoDecls(src []byte, imports []GoImport, decls string) ([]byte, error) {
	formatted, err := format.Source([]byte(strings.TrimSpace(decls) + "\n"))
	if err != nil {
		return nil, fmt.Errorf("format appended declarations: %w", err)
	}
	out, err := addGoImports(src, imports)
	if err != nil {
		return nil, err
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, '\n')
	return append(out, formatted...), nil
}

// addGoImports 只插入缺的 import，返回新的源码；不缺时原样返回 src 的副本。
func addGoImports(src []byte, imports []GoImport) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("parse Go file before adding imports: %w", err)
	}
	have := map[string]string{} // 代码里的名字 → 路径
	for _, spec := range file.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		have[name] = importPath
	}
	var missing []GoImport
	seen := map[string]bool{}
	for _, imp := range imports {
		name := imp.localName()
		if seen[name] {
			continue
		}
		seen[name] = true
		if existing, ok := have[name]; ok {
			if existing != imp.Path {
				return nil, fmt.Errorf("the file already imports %q as %s; the appended code needs %s to be %q", existing, name, name, imp.Path)
			}
			continue
		}
		missing = append(missing, imp)
	}
	out := append([]byte(nil), src...)
	if len(missing) == 0 {
		return out, nil
	}
	insertAt := fset.Position(file.Name.End()).Offset
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			insertAt = fset.Position(gen.End()).Offset
		}
	}
	var block strings.Builder
	block.WriteString(goImportBlockPrefix)
	for _, imp := range missing {
		block.WriteString("\t")
		if imp.Name != "" {
			block.WriteString(imp.Name + " ")
		}
		block.WriteString(strconv.Quote(imp.Path) + "\n")
	}
	block.WriteString(")")
	result := make([]byte, 0, len(out)+block.Len())
	result = append(result, out[:insertAt]...)
	result = append(result, block.String()...)
	return append(result, out[insertAt:]...), nil
}

// dropUnusedGoImport 去掉 importPath 这条 import，前提是文件里已经没有用到它的名字（替换掉占位之后，占位独用的
// fmt 会变成未使用，编译不过）。只看标识符名字是否出现，判断偏保守：同名的局部标识符会让它留下。
func dropUnusedGoImport(src []byte, importPath string) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		for _, raw := range gen.Specs {
			spec := raw.(*ast.ImportSpec)
			value, _ := strconv.Unquote(spec.Path.Value)
			if value != importPath {
				continue
			}
			name := path.Base(value)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name == "_" || name == "." || goIdentUsed(file, name) {
				return src, nil
			}
			start, end := fset.Position(spec.Pos()).Offset, fset.Position(spec.End()).Offset
			if !gen.Lparen.IsValid() {
				start, end = fset.Position(gen.Pos()).Offset, fset.Position(gen.End()).Offset
			}
			// 连同这一行的缩进与行尾换行一起去掉。
			for start > 0 && (src[start-1] == '\t' || src[start-1] == ' ') {
				start--
			}
			if end < len(src) && src[end] == '\n' {
				end++
			}
			return append(append([]byte(nil), src[:start]...), src[end:]...), nil
		}
	}
	return src, nil
}

func goIdentUsed(file *ast.File, name string) bool {
	used := false
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			continue
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && ident.Name == name {
				used = true
			}
			return !used
		})
		if used {
			return true
		}
	}
	return false
}

// WriteGoImportBlock 写一个 import ( ... ) 块：标准库一组、其余一组，中间空一行（gofmt 只在组内排序）。
func WriteGoImportBlock(b *strings.Builder, imports []GoImport) {
	var std, other []GoImport
	for _, imp := range imports {
		first, _, _ := strings.Cut(imp.Path, "/")
		if strings.Contains(first, ".") {
			other = append(other, imp)
		} else {
			std = append(std, imp)
		}
	}
	b.WriteString("import (\n")
	for index, group := range [][]GoImport{std, other} {
		if index == 1 && len(std) > 0 && len(other) > 0 {
			b.WriteString("\n")
		}
		for _, imp := range group {
			b.WriteString("\t")
			if imp.Name != "" {
				b.WriteString(imp.Name + " ")
			}
			b.WriteString(strconv.Quote(imp.Path) + "\n")
		}
	}
	b.WriteString(")\n")
}
