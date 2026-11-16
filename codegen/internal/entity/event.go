package entity

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/genutil"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
)

// 实体事件的监听登记（docs/maintenance/FEATURE-2026-11-16-tx-event.md §2.3、§4）。
//
// 事件是 <模块>/game/event 里导出的结构体，事件名就是结构体名。实体方法、实体包里定义的组件的方法、实体挂载的
// 组件包里组件类型的方法，标了 //roost:listen [priority=N] 就是监听：签名固定为一个按值传入的 game/event 结构体参数、
// 返回 error。生成器为每个有监听的实体写 <实体>_event_gen.go，init 里按执行顺序调用
// entity.RegisterEventListener；core 只按登记先后执行，顺序全部在这里定：
//
//  1. priority 大的先（缺省 0，-1000～1000）；
//  2. 同优先级时实体自己的方法先；
//  3. 再按组件初始化顺序：组件类型号升序（与 framework/entity 的 sortedComponentTypes 一致）；
//  4. 同一接收者按方法名。
//
// 组件挂在几种实体上，就在每种实体的生成文件里各登记一次。组件包里的组件必须按组件包写法挂（字段类型 *<包>.<类型>、
// comp:"<包>.CompType<X>"，模块清单写 component_package）：旧写法在实体包里声明别名 type XComponent = <包>.Component
// 再挂别名，监听对不上这次挂载，生成失败（整理轮 §3.2 第 4 项）；组件确实没有实体挂载时只提示。
//
// 生成器另外扫描监听方法体（以及同一接收者上被它直接调用的
// 方法）里的 Publish(event.X{...})，画出“事件 → 监听 → 再发布的事件”的图，有环时打印提示；运行时的深度与次数上限
// 才是最终的保证。
const (
	eventDirRel    = "game/event"
	eventGenSuffix = "_event_gen.go"
	listenMinPrio  = -1000
	listenMaxPrio  = 1000
)

// eventListener 是一个 //roost:listen 方法。
type eventListener struct {
	Pkg, PkgPath string // 接收者所在包的包名与导入路径
	Recv, Method string
	Event        string
	Priority     int
	Pos          string   // file:line，报错用
	Publishes    []string // 静态扫描到的再发布事件（排序、去重）
}

// Label 是登记时的 listener 名："<包>.<接收者>.<方法>"。
func (l eventListener) Label() string { return l.Pkg + "." + l.Recv + "." + l.Method }

// entityListener 是登记到某个实体上的一条监听。
type entityListener struct {
	eventListener
	Own       bool   // 实体自己的方法
	CompOrder int64  // 组件类型号（Own 时不用）
	Getter    string // 组件 getter（<X>Comp），Own 时为空
}

// eventEntityKey 标识一个实体：实体包目录 + 结构体名。
type eventEntityKey struct{ Dir, Name string }

// eventPlan 是一个生成工程里全部实体的监听登记计划。
type eventPlan struct {
	EventImport string
	byEntity    map[eventEntityKey][]entityListener
	Notes       []string // warning / hint，按文本排序
}

// eventMount 是一个组件字段挂在一个实体上。
type eventMount struct {
	entity eventEntityKey
	comp   ComponentField
}

// buildEventPlan 扫描 root（module 路径 module）下的事件、实体与组件包，返回登记计划；规则违反时返回全部错误。
func buildEventPlan(root, module string) (*eventPlan, error) {
	plan := &eventPlan{EventImport: module + "/" + eventDirRel, byEntity: map[eventEntityKey][]entityListener{}}
	events, err := scanEventTypes(filepath.Join(root, filepath.FromSlash(eventDirRel)))
	if err != nil {
		return nil, err
	}
	entitiesRoot := filepath.Join(root, "game", "entities")
	var entityDirs []string
	if _, statErr := os.Stat(entitiesRoot); statErr == nil {
		if entityDirs, err = findEntityDirs(entitiesRoot); err != nil {
			return nil, err
		}
	}
	pkgPathOf := func(dir string) string {
		rel, relErr := filepath.Rel(root, dir)
		if relErr != nil || strings.HasPrefix(rel, "..") {
			return ""
		}
		return module + "/" + filepath.ToSlash(rel)
	}
	dirOf := func(pkgPath string) string {
		rel, ok := strings.CutPrefix(pkgPath, module+"/")
		if !ok {
			return ""
		}
		return filepath.Join(root, filepath.FromSlash(rel))
	}

	var problems []string
	entityTypes := map[[2]string]eventEntityKey{} // (包路径, 类型名) → 实体
	mounts := map[[2]string][]eventMount{}        // (包路径, 组件类型名) → 挂载
	coldMounts := map[[2]string]bool{}
	aliasMounts := map[[2]string][]aliasMount{} // (包路径, 组件类型名) → 经实体包别名挂它的字段
	scanDirs := map[string]string{}             // 目录 → 包路径
	for _, dir := range entityDirs {
		entities, _, err := parseDir(dir)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", dir, err)
		}
		aliases, err := entityTypeAliases(dir)
		if err != nil {
			return nil, err
		}
		pkgPath := pkgPathOf(dir)
		scanDirs[dir] = pkgPath
		for _, ent := range entities {
			key := eventEntityKey{Dir: dir, Name: ent.Name}
			entityTypes[[2]string{pkgPath, ent.Name}] = key
			seen := map[[2]string]string{}
			for _, comp := range ent.Components {
				compPkg := comp.TypePkg
				if compPkg == "" {
					compPkg = pkgPath
				}
				recv := lastTypeSegment(comp.TypeName)
				id := [2]string{compPkg, recv}
				if target, isAlias := aliases[recv]; isAlias && comp.TypePkg == "" {
					// 旧写法：挂的是实体包里的别名，组件真正的包另记一份，那个包里的监听在下面报错；包目录也加进扫描范围
					// （game/modules 下的组件包不在 game/components 的遍历里）。
					aliasMounts[target] = append(aliasMounts[target], aliasMount{
						where:    fmt.Sprintf("%s.%s (alias %s.%s)", ent.Name, comp.FieldName, filepath.Base(dir), recv),
						compType: lastTypeSegment(comp.CompType),
					})
					if targetDir := dirOf(target[0]); targetDir != "" {
						scanDirs[targetDir] = target[0]
					}
				}
				if other, dup := seen[id]; dup {
					problems = append(problems, fmt.Sprintf("%s: entity %s mounts component %s.%s twice (fields %s and %s); its listeners would be registered twice",
						dir, ent.Name, compPkg, recv, other, comp.FieldName))
					continue
				}
				seen[id] = comp.FieldName
				if comp.Cold {
					coldMounts[id] = true
					continue
				}
				mounts[id] = append(mounts[id], eventMount{entity: key, comp: comp})
				if compDir := dirOf(compPkg); compDir != "" {
					scanDirs[compDir] = compPkg
				}
			}
		}
	}
	componentsRoot := filepath.Join(root, "game", "components")
	if _, statErr := os.Stat(componentsRoot); statErr == nil {
		walkErr := filepath.WalkDir(componentsRoot, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != componentsRoot && (entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".")) {
					return filepath.SkipDir
				}
				scanDirs[path] = pkgPathOf(path)
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}

	dirs := make([]string, 0, len(scanDirs))
	for dir := range scanDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	compOrders := map[[2]string]int64{} // (包路径, 常量名) → 类型号
	byReceiverEvent := map[string]string{}
	for _, dir := range dirs {
		scan, err := scanListenerDir(dir, scanDirs[dir], plan.EventImport, events)
		if err != nil {
			return nil, err
		}
		problems = append(problems, scan.problems...)
		for _, listener := range scan.listeners {
			id := [2]string{listener.PkgPath, listener.Recv}
			dupKey := listener.PkgPath + "." + listener.Recv + "/" + listener.Event
			if other, dup := byReceiverEvent[dupKey]; dup {
				problems = append(problems, fmt.Sprintf("%s: %s.%s listens to %s again (already %s); one receiver has one listener per event",
					listener.Pos, listener.Recv, listener.Method, listener.Event, other))
				continue
			}
			byReceiverEvent[dupKey] = listener.Method + " at " + listener.Pos
			if old := aliasMounts[id]; len(old) > 0 {
				// 别名挂载对不上组件包里的接收者：不报错的话，这些实体上的监听会悄悄缺失。
				where := make([]string, 0, len(old))
				for _, mount := range old {
					where = append(where, mount.where)
				}
				problems = append(problems, fmt.Sprintf("%s: %s listens to %s, but %s mount%s the component through an alias in the entity package (the old way), so the listener cannot be registered there; "+
					"mount it the component-package way: field type *%s.%s with comp:\"%s.%s\" (component_package in the module manifest), and delete the alias and its CompType constant from the entity package",
					listener.Pos, listener.Label(), listener.Event, strings.Join(where, ", "), pluralS(len(old)), listener.Pkg, listener.Recv, listener.Pkg, old[0].compType))
				continue
			}
			if key, ok := entityTypes[id]; ok {
				plan.byEntity[key] = append(plan.byEntity[key], entityListener{eventListener: listener, Own: true})
				continue
			}
			if coldMounts[id] && len(mounts[id]) == 0 {
				problems = append(problems, fmt.Sprintf("%s: %s.%s listens on a cold component (comp:\"...,cold\"): it may be absent when the event is published; listen on the entity or make the component hot",
					listener.Pos, listener.Recv, listener.Method))
				continue
			}
			if len(mounts[id]) == 0 {
				if scan.components[listener.Recv] {
					plan.Notes = append(plan.Notes, fmt.Sprintf("warning: %s: component %s.%s listens to %s but no entity mounts it; the listener is not registered",
						listener.Pos, listener.Pkg, listener.Recv, listener.Event))
				} else {
					problems = append(problems, fmt.Sprintf("%s: //roost:listen on %s.%s: the receiver is neither an entity nor a component mounted by an entity",
						listener.Pos, listener.Recv, listener.Method))
				}
				continue
			}
			for _, mount := range mounts[id] {
				order, err := componentTypeNumber(mount, compOrders, dirOf)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", listener.Pos, err))
					continue
				}
				plan.byEntity[mount.entity] = append(plan.byEntity[mount.entity], entityListener{
					eventListener: listener, CompOrder: order, Getter: mount.comp.GetterName,
				})
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		joined := make([]error, 0, len(problems))
		for _, problem := range problems {
			joined = append(joined, errors.New(problem))
		}
		return nil, errors.Join(joined...)
	}
	for key, listeners := range plan.byEntity {
		sort.SliceStable(listeners, func(i, j int) bool { return listenerLess(listeners[i], listeners[j]) })
		plan.byEntity[key] = listeners
	}
	plan.Notes = append(plan.Notes, eventCycleHints(plan.byEntity)...)
	sort.Strings(plan.Notes)
	return plan, nil
}

// aliasMount 是旧写法的一次挂载：实体字段的类型是实体包里指向组件包的别名。
type aliasMount struct {
	where    string // "<实体>.<字段> (alias <实体包>.<别名>)"
	compType string // 实体包里的类型号常量名，例如 CompTypeActivity
}

// pluralS 让动词随主语单复数：一个挂载用 mounts，多个用 mount。
func pluralS(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

// entityTypeAliases 返回实体包里指向别的包的类型别名：别名名 → (包路径, 类型名)。旧写法用它把组件包的组件挂到实体上
// （type ActivityComponent = activity.Component）。
func entityTypeAliases(dir string) (map[string][2]string, error) {
	aliases := map[string][2]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || isGeneratedWireFile(name) {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		imports := collectImports(file)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				if !ts.Assign.IsValid() {
					continue
				}
				selector, ok := ts.Type.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok {
					continue
				}
				if imp, found := imports[pkg.Name]; found {
					aliases[ts.Name.Name] = [2]string{imp.Path, selector.Sel.Name}
				}
			}
		}
	}
	return aliases, nil
}

// listenerLess 是同一事件里的执行顺序（见文件头）；不同事件之间按事件名排，生成文件稳定。
func listenerLess(a, b entityListener) bool {
	switch {
	case a.Event != b.Event:
		return a.Event < b.Event
	case a.Priority != b.Priority:
		return a.Priority > b.Priority
	case a.Own != b.Own:
		return a.Own
	case !a.Own && a.CompOrder != b.CompOrder:
		return a.CompOrder < b.CompOrder
	case a.Recv != b.Recv:
		return a.Recv < b.Recv
	default:
		return a.Method < b.Method
	}
}

// componentTypeNumber 读组件类型号常量的整数值（const CompTypeX entity.ComponentType = N）。
func componentTypeNumber(mount eventMount, cache map[[2]string]int64, dirOf func(string) string) (int64, error) {
	pkgPath, dir := mount.comp.CompPkg, ""
	if pkgPath == "" {
		dir = mount.entity.Dir
	} else {
		dir = dirOf(pkgPath)
	}
	name := lastTypeSegment(mount.comp.CompType)
	key := [2]string{dir, name}
	if value, ok := cache[key]; ok {
		return value, nil
	}
	values, err := integerConsts(dir)
	if err != nil {
		return 0, err
	}
	value, ok := values[name]
	if !ok {
		return 0, fmt.Errorf("cannot read the type number of %s (want `const %s entity.ComponentType = <integer>` in %s): listeners are ordered by it", mount.comp.CompType, name, dir)
	}
	cache[key] = value
	return value, nil
}

// integerConsts 读 dir 里写成整数字面量的常量。
func integerConsts(dir string) (map[string]int64, error) {
	out := map[string]int64{}
	if dir == "" {
		return out, nil
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
						if n, err := strconv.ParseInt(lit.Value, 0, 64); err == nil {
							out[name.Name] = n
						}
					}
				}
			}
		}
	}
	return out, nil
}

// scanEventTypes 读 game/event 里导出的结构体名（事件名）。目录不存在时返回空。
func scanEventTypes(dir string) (map[string]bool, error) {
	events := map[string]bool{}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				if _, isStruct := ts.Type.(*ast.StructType); isStruct && ts.Name.IsExported() {
					events[ts.Name.Name] = true
				}
			}
		}
	}
	return events, nil
}

// listenerDirScan 是一个包目录里的监听扫描结果。
type listenerDirScan struct {
	listeners  []eventListener
	problems   []string
	components map[string]bool // 本包里嵌入了 ComponentBase 的结构体（组件类型）
}

// listenMarker 返回 //roost:listen 标记后面的选项文本。
func listenMarker(text string) (string, bool) {
	rest, ok := marker.Cut(text, marker.Listen.Kind)
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return rest, true
}

// methodRef 是包里的一个方法声明与它所在文件的导入表。
type methodRef struct {
	decl    *ast.FuncDecl
	imports map[string]string
}

// scanListenerDir 扫一个包目录里的 //roost:listen：校验位置、选项与签名，并静态找出每个监听再发布的事件。
func scanListenerDir(dir, pkgPath, eventImport string, events map[string]bool) (listenerDirScan, error) {
	scan := listenerDirScan{components: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return scan, nil
		}
		return scan, err
	}
	type marked struct {
		fn       *ast.FuncDecl
		options  string
		pos      string
		imports  map[string]string
		fileName string
	}
	var found []marked
	methods := map[string]map[string]methodRef{}
	pkgName := ""
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || isGeneratedWireFile(name) {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return scan, fmt.Errorf("parse %s: %w", path, err)
		}
		if pkgName == "" {
			pkgName = file.Name.Name
		}
		imports := map[string]string{}
		for alias, imp := range collectImports(file) {
			imports[alias] = imp.Path
		}
		position := func(pos token.Pos) string {
			p := fset.Position(pos)
			return fmt.Sprintf("%s:%d", p.Filename, p.Line)
		}
		attached := map[*ast.Comment]bool{}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) == 1 {
					if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
						if methods[recv] == nil {
							methods[recv] = map[string]methodRef{}
						}
						methods[recv][d.Name.Name] = methodRef{decl: d, imports: imports}
					}
				}
				if d.Doc == nil {
					continue
				}
				count := 0
				for _, comment := range d.Doc.List {
					options, ok := listenMarker(comment.Text)
					if !ok {
						continue
					}
					attached[comment] = true
					if count++; count > 1 {
						scan.problems = append(scan.problems, fmt.Sprintf("%s: //roost:listen is given twice on %s", position(comment.Pos()), d.Name.Name))
						continue
					}
					found = append(found, marked{fn: d, options: options, pos: position(comment.Pos()), imports: imports, fileName: name})
				}
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					if st, ok := ts.Type.(*ast.StructType); ok && embedsComponentBase(st) {
						scan.components[ts.Name.Name] = true
					}
				}
			}
		}
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if _, ok := listenMarker(comment.Text); ok && !attached[comment] {
					scan.problems = append(scan.problems, fmt.Sprintf("%s: //roost:listen must sit in the doc comment of a method", position(comment.Pos())))
				}
			}
		}
	}
	for _, mark := range found {
		listener, problem := validateListener(mark.fn, mark.options, mark.pos, mark.imports, eventImport, events)
		if problem != "" {
			scan.problems = append(scan.problems, problem)
			continue
		}
		listener.Pkg, listener.PkgPath = pkgName, pkgPath
		listener.Publishes = publishedEvents(mark.fn, mark.imports, eventImport, methods)
		scan.listeners = append(scan.listeners, listener)
	}
	return scan, nil
}

// validateListener 校验 //roost:listen 的选项与方法签名。
func validateListener(fn *ast.FuncDecl, options, pos string, imports map[string]string, eventImport string, events map[string]bool) (eventListener, string) {
	if fn.Recv == nil {
		return eventListener{}, fmt.Sprintf("%s: //roost:listen on function %s: put it on a method of an entity or of a component an entity mounts", pos, fn.Name.Name)
	}
	recv := receiverTypeName(fn.Recv.List[0].Type)
	if recv == "" {
		return eventListener{}, fmt.Sprintf("%s: //roost:listen on %s: the receiver must be a plain (pointer to a) named type", pos, fn.Name.Name)
	}
	listener := eventListener{Recv: recv, Method: fn.Name.Name, Pos: pos}
	parsed, err := marker.Listen.Parse(options)
	if err != nil {
		return eventListener{}, fmt.Sprintf("%s: %v", pos, err)
	}
	if raw, ok := parsed["priority"]; ok {
		priority, err := strconv.Atoi(raw)
		if err != nil || priority < listenMinPrio || priority > listenMaxPrio {
			return eventListener{}, fmt.Sprintf("%s: //roost:listen priority=%s on %s.%s: want an integer from %d to %d", pos, raw, recv, fn.Name.Name, listenMinPrio, listenMaxPrio)
		}
		listener.Priority = priority
	}
	wrongSignature := func(why string) (eventListener, string) {
		return eventListener{}, fmt.Sprintf("%s: //roost:listen %s.%s: %s; a listener is func (r *R) M(ev event.X) error with X a struct in %s",
			pos, recv, fn.Name.Name, why, eventImport)
	}
	params := fn.Type.Params
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return wrongSignature("it must take exactly one parameter")
	}
	paramType := params.List[0].Type
	if star, ok := paramType.(*ast.StarExpr); ok {
		paramType = star.X
		if sel, ok := paramType.(*ast.SelectorExpr); ok && imports[identName(sel.X)] == eventImport {
			return wrongSignature("the event is passed by pointer; take it by value")
		}
	}
	sel, ok := paramType.(*ast.SelectorExpr)
	if !ok || imports[identName(sel.X)] != eventImport {
		return wrongSignature("the parameter is not a type from game/event")
	}
	if !events[sel.Sel.Name] {
		return wrongSignature(fmt.Sprintf("event.%s is not an exported struct in game/event", sel.Sel.Name))
	}
	results := fn.Type.Results
	if results == nil || len(results.List) != 1 || len(results.List[0].Names) > 1 || identName(results.List[0].Type) != "error" {
		return wrongSignature("it must return exactly one error")
	}
	listener.Event = sel.Sel.Name
	return listener, ""
}

func identName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// embedsComponentBase 报告结构体是否嵌入了 entity.ComponentBase（组件类型的标志）。
func embedsComponentBase(st *ast.StructType) bool {
	for _, field := range st.Fields.List {
		if len(field.Names) != 0 {
			continue
		}
		if strings.HasSuffix(strings.TrimPrefix(exprToString(field.Type), "*"), "ComponentBase") {
			return true
		}
	}
	return false
}

// publishedEvents 找出方法体里（以及同一接收者上被它直接调用、再往下直接调用的方法里）Publish(event.X{...}) 发布的事件。
// 只认字面量写在调用里的形式；经变量、别的包或接口发布的看不到，所以这只是提示。
func publishedEvents(fn *ast.FuncDecl, imports map[string]string, eventImport string, methods map[string]map[string]methodRef) []string {
	recv := receiverTypeName(fn.Recv.List[0].Type)
	found := map[string]bool{}
	visited := map[string]bool{}
	var walk func(decl *ast.FuncDecl, imports map[string]string)
	walk = func(decl *ast.FuncDecl, imports map[string]string) {
		if decl.Body == nil || visited[decl.Name.Name] {
			return
		}
		visited[decl.Name.Name] = true
		recvName := ""
		if names := decl.Recv.List[0].Names; len(names) == 1 {
			recvName = names[0].Name
		}
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Publish" && len(call.Args) == 1 {
				if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
					if typ, ok := lit.Type.(*ast.SelectorExpr); ok && imports[identName(typ.X)] == eventImport {
						found[typ.Sel.Name] = true
					}
				}
			}
			if recvName != "" && identName(sel.X) == recvName {
				if callee, ok := methods[recv][sel.Sel.Name]; ok {
					walk(callee.decl, callee.imports)
				}
			}
			return true
		})
	}
	walk(fn, imports)
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// eventCycleHints 按实体画“事件 → 监听 → 再发布的事件”的图，列出其中的环（同一条环在几种实体上出现时合成一条）。
func eventCycleHints(byEntity map[eventEntityKey][]entityListener) []string {
	type cycleInfo struct {
		entities  []string
		listeners []string
	}
	cycles := map[string]*cycleInfo{}
	for key, listeners := range byEntity {
		edges := map[string]map[string][]string{} // 事件 → 再发布的事件 → 监听名
		for _, listener := range listeners {
			for _, next := range listener.Publishes {
				if edges[listener.Event] == nil {
					edges[listener.Event] = map[string][]string{}
				}
				edges[listener.Event][next] = append(edges[listener.Event][next], listener.Label())
			}
		}
		for _, cycle := range elementaryCycles(edges) {
			text := strings.Join(append(append([]string(nil), cycle...), cycle[0]), " → ")
			info := cycles[text]
			if info == nil {
				info = &cycleInfo{}
				for i, event := range cycle {
					info.listeners = append(info.listeners, edges[event][cycle[(i+1)%len(cycle)]]...)
				}
				cycles[text] = info
			}
			info.entities = append(info.entities, key.Name)
		}
	}
	var out []string
	for text, info := range cycles {
		sort.Strings(info.entities)
		out = append(out, fmt.Sprintf("hint: event cycle on %s: %s (listeners %s); the runtime depth/repeat limits stop a real loop, a data-bounded re-entry (level-up granting exp) is fine",
			strings.Join(info.entities, ", "), text, strings.Join(info.listeners, ", ")))
	}
	sort.Strings(out)
	return out
}

// elementaryCycles 列出有向图的全部简单环，每条从环上名字最小的事件开始。
func elementaryCycles(edges map[string]map[string][]string) [][]string {
	nodes := map[string]bool{}
	for from, tos := range edges {
		nodes[from] = true
		for to := range tos {
			nodes[to] = true
		}
	}
	sorted := make([]string, 0, len(nodes))
	for node := range nodes {
		sorted = append(sorted, node)
	}
	sort.Strings(sorted)
	var out [][]string
	for _, start := range sorted {
		var path []string
		onPath := map[string]bool{}
		var visit func(node string)
		visit = func(node string) {
			path = append(path, node)
			onPath[node] = true
			nexts := make([]string, 0, len(edges[node]))
			for next := range edges[node] {
				nexts = append(nexts, next)
			}
			sort.Strings(nexts)
			for _, next := range nexts {
				switch {
				case next == start:
					out = append(out, append([]string(nil), path...))
				case next > start && !onPath[next]:
					visit(next)
				}
			}
			onPath[node] = false
			path = path[:len(path)-1]
		}
		visit(start)
	}
	return out
}

// eventFileFor 是实体的事件登记文件名。
func eventFileFor(entityName string) string { return toSnake(entityName) + eventGenSuffix }

// generateEventFile 写出实体的 <实体>_event_gen.go。
func generateEventFile(dir, pkg string, ent EntityDef, listeners []entityListener, eventImport string, force bool) (string, bool, error) {
	var b bytes.Buffer
	b.WriteString(entityGeneratedHeader + "\n")
	fmt.Fprintf(&b, "package %s\n\nimport (\n", pkg)
	fmt.Fprintf(&b, "\t%q\n", eventImport)
	if alias := qualifier(ent.EntityKind); alias != "" {
		for _, imp := range ent.Imports {
			if imp.Alias == alias {
				fmt.Fprintf(&b, "\t%s %q\n", imp.Alias, imp.Path)
			}
		}
	}
	b.WriteString("\n\t\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n)\n\n")
	fmt.Fprintf(&b, "// init 登记 %s 的实体事件监听（//roost:listen）。登记顺序就是执行顺序：同一事件里 priority 大的先；\n", ent.Name)
	fmt.Fprintf(&b, "// 同优先级时 %s 自己的方法先，再按组件初始化顺序（组件类型号升序），同一接收者按方法名。\n", ent.Name)
	b.WriteString("func init() {\n")
	for _, listener := range listeners {
		call := "e." + listener.Method + "(ev)"
		if !listener.Own {
			call = "e." + listener.Getter + "()." + listener.Method + "(ev)"
		}
		fmt.Fprintf(&b, "\t// priority=%d\n", listener.Priority)
		fmt.Fprintf(&b, "\tentity.RegisterEventListener(%s, %q, %q,\n\t\tfunc(e *%s, ev event.%s) error { return %s })\n",
			ent.EntityKind, listener.Event, listener.Label(), ent.Name, listener.Event, call)
	}
	b.WriteString("}\n")
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return "", false, fmt.Errorf("format %s: %w", eventFileFor(ent.Name), err)
	}
	path := filepath.Join(dir, eventFileFor(ent.Name))
	changed, err := genutil.WriteIfChanged(path, formatted, force)
	return path, changed, err
}

// retireEventFiles 删掉 dir 里不在 keep 中的生成的 *_event_gen.go（实体没有监听了、改名或删掉了）。
func retireEventFiles(dir string, keep map[string]bool, stdout interface{ Write([]byte) (int, error) }) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*"+eventGenSuffix))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if keep[filepath.Base(path)] {
			continue
		}
		removed, err := retireEntityFile(path)
		if err != nil {
			return err
		}
		if removed {
			fmt.Fprintf(stdout, "removed: %s\n", path)
		}
	}
	return nil
}
