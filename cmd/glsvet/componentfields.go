package main

// 组件字段写提示（维护者决定 A1 盲区，2026-10-06 第十三轮，docs/feature/A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md）。
//
// A1 要求事务里会改的状态放在 DAO，由 DAO 的回滚统一兜住（docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md）。
// componentUndoHints 只抓“组件自己登记 undo”；组件把可变状态放在普通字段里、也不登记 undo 时，事务失败
// 这些字段不回滚，之前没有任何提示。这里补上：组件方法（初始化 / 装配钩子除外）给组件自身字段赋值，
// 或改字段里的 map / slice 元素，而该字段既不是 DAO 句柄、也没有 //roost:cache 标注、也不是函数类型，
// 就打印 hint:。与 componentUndoHints 一样只提示、不计入违例、不改退出码；同样跟进一层同包包级 helper
// （口径与 RR-20261006-13 一致）。
//
// 只按语法判断，没有类型信息，所以：
//   - “写”指以 recv.field 为根的赋值左值（=、op=、++/--，含 recv.field[k]、recv.field.x、*recv.field），
//     以及内建 delete / clear 的第一个参数。先取到局部变量再改（m := c.items; m[k] = v）、经方法调用改
//     （c.items.Add(x)）看不见。
//   - DAO 句柄：字段类型名（去掉指针、包名、类型参数）以 Dao 或 DAO 结尾。
//   - 函数类型字段（func(...) 字面量或同包 `type X func(...)`）装的是行为（投影、回调），由装配方法装上，
//     不是事务状态，不提示（combatcomponent.ProjectAttributes）。
//   - 只认组件结构体里直接声明的字段；嵌入类型提升上来的字段不认。

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// cacheDirective 标注一个可以不随事务回滚的组件字段：缓存、可从 DAO 重建的派生索引等。写在字段声明
// 上一行或行尾。被标注的字段在事务失败后可能与 DAO 不一致，读它的代码要能容忍或自行校验。
const cacheDirective = "roost:cache"

// componentLifecycleMethods 是组件的初始化 / 装配钩子：框架在实体建好（新建或加载）时调用
// OnInitFinish，销毁时调用 OnDestroy（entity.ComponentInterfaceBase），都不在业务事务里，写字段不提示。
// 构造组件的工厂函数（entity.RegisterComponentFactory 的回调、NewXComponent）不是方法，本来就不检查。
var componentLifecycleMethods = map[string]bool{
	"OnInitFinish": true,
	"OnDestroy":    true,
}

// componentField 描述组件结构体里的一个字段是否豁免写提示。
type componentField struct {
	exempt bool
}

// componentTypes 返回本包的组件类型：名字以 Component 结尾，或嵌入了 ComponentBase（与 A1 undo 提示同一口径）。
// 值是该类型的结构体定义；不是结构体的组件类型值为 nil。
func componentTypes(pkg *ast.Package) map[string]*ast.StructType {
	components := make(map[string]*ast.StructType)
	for _, file := range pkg.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok {
				return true
			}
			structType, _ := spec.Type.(*ast.StructType)
			if strings.HasSuffix(spec.Name.Name, "Component") {
				components[spec.Name.Name] = structType
				return true
			}
			if structType != nil {
				for _, field := range structType.Fields.List {
					if len(field.Names) == 0 && embedsComponentBase(field.Type) {
						components[spec.Name.Name] = structType
					}
				}
			}
			return true
		})
	}
	return components
}

// packageFuncTypes 返回本包里底层是函数类型的具名类型。
func packageFuncTypes(pkg *ast.Package) map[string]bool {
	funcTypes := make(map[string]bool)
	for _, file := range pkg.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			if spec, ok := node.(*ast.TypeSpec); ok {
				if _, isFunc := spec.Type.(*ast.FuncType); isFunc {
					funcTypes[spec.Name.Name] = true
				}
			}
			return true
		})
	}
	return funcTypes
}

// componentFields 返回每个组件类型直接声明的字段及其是否豁免。
func componentFields(pkg *ast.Package, components map[string]*ast.StructType) map[string]map[string]componentField {
	funcTypes := packageFuncTypes(pkg)
	fields := make(map[string]map[string]componentField)
	for name, structType := range components {
		if structType == nil {
			continue
		}
		byName := make(map[string]componentField)
		for _, field := range structType.Fields.List {
			exempt := hasCacheDirective(field) || isDaoHandleType(field.Type) || isFuncFieldType(field.Type, funcTypes)
			for _, fieldName := range field.Names {
				byName[fieldName.Name] = componentField{exempt: exempt}
			}
		}
		fields[name] = byName
	}
	return fields
}

// hasCacheDirective 报告字段的文档注释（上一行）或行尾注释里有没有 //roost:cache。
func hasCacheDirective(field *ast.Field) bool {
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if text == cacheDirective || strings.HasPrefix(text, cacheDirective+" ") {
				return true
			}
		}
	}
	return false
}

// isDaoHandleType 报告字段类型是不是 DAO 句柄：去掉指针、包名与类型参数后的类型名以 Dao 或 DAO 结尾。
func isDaoHandleType(expression ast.Expr) bool {
	name := typeBaseName(expression)
	return strings.HasSuffix(name, "Dao") || strings.HasSuffix(name, "DAO")
}

func typeBaseName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.StarExpr:
		return typeBaseName(typed.X)
	case *ast.SelectorExpr:
		return typed.Sel.Name
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return typeBaseName(typed.X)
	case *ast.IndexListExpr:
		return typeBaseName(typed.X)
	}
	return ""
}

// isFuncFieldType 报告字段是不是函数类型：func 字面量，或同包底层为函数的具名类型。
func isFuncFieldType(expression ast.Expr, funcTypes map[string]bool) bool {
	switch typed := expression.(type) {
	case *ast.FuncType:
		return true
	case *ast.Ident:
		return funcTypes[typed.Name]
	}
	return false
}

// writtenField 返回表达式写到的 root.field 的字段名：沿下标、选择子、解引用、括号往里剥，直到 root.field。
// 不以 root 为根时返回空串。
func writtenField(expression ast.Expr, root string) string {
	for {
		switch typed := expression.(type) {
		case *ast.ParenExpr:
			expression = typed.X
		case *ast.StarExpr:
			expression = typed.X
		case *ast.IndexExpr:
			expression = typed.X
		case *ast.IndexListExpr:
			expression = typed.X
		case *ast.SliceExpr:
			expression = typed.X
		case *ast.SelectorExpr:
			if identifier, ok := typed.X.(*ast.Ident); ok && identifier.Name == root {
				return typed.Sel.Name
			}
			expression = typed.X
		default:
			return ""
		}
	}
}

// fieldWrite 是函数体里一次对 root.field 的写。
type fieldWrite struct {
	position token.Pos
	field    string
}

// fieldWrites 找出 body 里所有以 root.field 为根的写（见文件头的“写”）。
func fieldWrites(body *ast.BlockStmt, root string) []fieldWrite {
	if root == "" || root == "_" {
		return nil
	}
	var writes []fieldWrite
	add := func(position token.Pos, expression ast.Expr) {
		if field := writtenField(expression, root); field != "" {
			writes = append(writes, fieldWrite{position: position, field: field})
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			if typed.Tok == token.DEFINE {
				return true
			}
			for _, expression := range typed.Lhs {
				add(typed.Pos(), expression)
			}
		case *ast.IncDecStmt:
			add(typed.Pos(), typed.X)
		case *ast.RangeStmt:
			if typed.Tok == token.ASSIGN {
				for _, expression := range []ast.Expr{typed.Key, typed.Value} {
					if expression != nil {
						add(typed.Pos(), expression)
					}
				}
			}
		case *ast.CallExpr:
			if identifier, ok := typed.Fun.(*ast.Ident); ok && (identifier.Name == "delete" || identifier.Name == "clear") && len(typed.Args) > 0 {
				add(typed.Pos(), typed.Args[0])
			}
		}
		return true
	})
	return writes
}

// firstUnexemptWrite 返回 writes 里第一个写到未豁免字段的位置；字段不是 fields 里直接声明的不算。
func firstUnexemptWrite(writes []fieldWrite, fields map[string]componentField) (fieldWrite, bool) {
	for _, write := range writes {
		if field, known := fields[write.field]; known && !field.exempt {
			return write, true
		}
	}
	return fieldWrite{}, false
}

// packageFieldWriteHelpers 返回同包里直接写组件未豁免字段的包级函数：函数名 → “参数.字段”。
// 只看类型是本包组件（或其指针）的参数；只跟一层（与 packageUndoHelpers 一致）。
func packageFieldWriteHelpers(pkg *ast.Package, fields map[string]map[string]componentField) map[string]string {
	helpers := make(map[string]string)
	for _, file := range pkg.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil || function.Type.Params == nil {
				continue
			}
			for _, parameter := range function.Type.Params.List {
				componentFields, isComponent := fields[receiverTypeName(parameter.Type)]
				if !isComponent {
					continue
				}
				for _, name := range parameter.Names {
					if write, found := firstUnexemptWrite(fieldWrites(function.Body, name.Name), componentFields); found {
						helpers[function.Name.Name] = name.Name + "." + write.field
						break
					}
				}
				if helpers[function.Name.Name] != "" {
					break
				}
			}
		}
	}
	return helpers
}

// componentFieldHints 报告组件方法（初始化 / 装配钩子除外）写组件自身未豁免字段的地方，含经同包包级 helper 写的。
func componentFieldHints(fileSet *token.FileSet, pkg *ast.Package) []string {
	components := componentTypes(pkg)
	fields := componentFields(pkg, components)
	helpers := packageFieldWriteHelpers(pkg, fields)
	const advice = "a failed transaction does not roll it back; keep transaction state in the DAO (a nopersist field if it must not be stored), or mark a cache field //roost:cache"
	var hints []string
	for _, file := range pkg.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || len(function.Recv.List) == 0 || function.Body == nil || componentLifecycleMethods[function.Name.Name] {
				continue
			}
			receiver := receiverTypeName(function.Recv.List[0].Type)
			ownFields, isComponent := fields[receiver]
			if !isComponent {
				continue
			}
			for _, write := range fieldWrites(function.Body, receiverName(function)) {
				if field, known := ownFields[write.field]; known && !field.exempt {
					hints = append(hints, fmt.Sprintf("%s: hint: component %s.%s writes field %s outside the DAO; %s",
						fileSet.Position(write.position), receiver, function.Name.Name, write.field, advice))
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if identifier, ok := call.Fun.(*ast.Ident); ok && helpers[identifier.Name] != "" {
					hints = append(hints, fmt.Sprintf("%s: hint: component %s.%s writes %s outside the DAO through %s; %s",
						fileSet.Position(call.Pos()), receiver, function.Name.Name, helpers[identifier.Name], identifier.Name, advice))
				}
				return true
			})
		}
	}
	return hints
}
