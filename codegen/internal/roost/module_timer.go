package roost

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	entitygen "github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
)

// ModuleTimerPlan 是模块对实体共享定时器的接线；类型号只在该实体内唯一。
// 同一次安装的模块共用这一份计划，保证分号、写入与 roost.yaml 的最终记录一致。
type ModuleTimerPlan struct {
	Entity string    `json:"entity"`
	Spec   TimerSpec `json:"spec"`
	Create bool      `json:"create"`
	used   map[int32]bool
	names  map[string]bool
}

func modulePlanHasTimers(plan ModulePlan) bool {
	for _, item := range plan.Modules {
		if item.Timer != nil {
			return true
		}
	}
	return false
}

func planModuleTimer(root string, manifest Manifest, service string, spec ModuleSpec, newEntity bool, ids *moduleIDAllocator, plans map[string]*ModuleTimerPlan, allocated map[string]map[string]int64) (*ModuleTimerPlan, error) {
	name := toSnake(spec.Entity)
	plan := plans[name]
	if plan == nil {
		timer, exists := manifest.Timers[name]
		if exists && timer.Service != service {
			return nil, fmt.Errorf("timers.%s belongs to service %s, not %s", name, timer.Service, service)
		}
		if !exists {
			id, err := ids.next("component")
			if err != nil {
				return nil, err
			}
			dao := spec.Entity + "Dao"
			if !newEntity {
				_, file, err := resolveEntitySource(root, spec.Entity)
				if err != nil {
					return nil, err
				}
				host, err := entitygen.InspectTimerHost(filepath.Dir(file), spec.Entity)
				if err != nil {
					return nil, err
				}
				dao, err = timerHostDao(name, host)
				if err != nil {
					return nil, err
				}
			}
			timer = TimerSpec{Component: timerDefaultComponent, ID: id, Service: service, Dao: dao}
			n := newTimerNames(manifest.Project.Module, name, timer)
			for _, rel := range []string{n.componentFile(), n.handlersFile(), n.nodeFile(), n.tickFile(), "game/entities/" + name + "/timer_component.go"} {
				if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
					return nil, fmt.Errorf("timer scaffold conflicts with %s; declare the existing timer in roost.yaml", rel)
				} else if !os.IsNotExist(err) {
					return nil, err
				}
			}
		}
		timer.Types = maps.Clone(timer.Types)
		if timer.Types == nil {
			timer.Types = map[string]int32{}
		}
		used, names, err := scanModuleTimerConstants(filepath.Join(root, "game/entities", name))
		if err != nil {
			return nil, err
		}
		// 新组件沿用 add timer 的业务示例；不把它的 1 号发给真实模块。
		if !exists {
			if names["CompTypeTimer"] {
				return nil, fmt.Errorf("entity %s already declares CompTypeTimer; declare its timer in roost.yaml or choose another component", name)
			}
			used[1] = true
		}
		for typeName, id := range timer.Types {
			used[id] = true
			names[typeName] = true
		}
		for moduleName, record := range manifest.Modules {
			if record.TimerEntity != name {
				continue
			}
			for typeName, id := range record.IDs["timer"] {
				if id <= 0 || id > math.MaxInt32 || timer.Types[typeName] != int32(id) {
					return nil, fmt.Errorf("module %s timer %s disagrees with timers.%s.types", moduleName, typeName, name)
				}
				used[int32(id)] = true
				names[typeName] = true
			}
		}
		plan = &ModuleTimerPlan{Entity: name, Spec: timer, Create: !exists, used: used, names: names}
		plans[name] = plan
	}
	allocated["timer"] = map[string]int64{}
	for _, typeName := range slices.Sorted(slices.Values(spec.TimerTypes)) {
		if plan.names[typeName] {
			return nil, fmt.Errorf("timer type name %s already exists in entity %s", typeName, spec.Entity)
		}
		var id int32
		for candidate := int64(1); candidate <= math.MaxInt32; candidate++ {
			if !plan.used[int32(candidate)] {
				id = int32(candidate)
				break
			}
		}
		if id == 0 {
			return nil, fmt.Errorf("timer types exhausted for %s", spec.Entity)
		}
		plan.used[id] = true
		plan.names[typeName] = true
		plan.Spec.Types[typeName] = id
		allocated["timer"][typeName] = int64(id)
	}
	return plan, nil
}

// scanModuleTimerConstants 只求实体包的常量值，不把尚未生成齐的工程当成可编译包。
// go/types 可解析 int32、别名、iota 与本包常量表达式；每个 Register 的类型实参必须能静态求值。
// 外部常量/运行期表达式解析不出时拒绝分号，不能猜一个“看起来空闲”的号。
func scanModuleTimerConstants(dir string) (map[int32]bool, map[string]bool, error) {
	used, names := map[int32]bool{}, map[string]bool{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return used, names, nil
	}
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return used, names, nil
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	config := types.Config{Error: func(error) {}}
	_, _ = config.Check(files[0].Name.Name, fset, files, info)
	for ident, obj := range info.Defs {
		if obj == nil {
			continue
		}
		if obj.Parent() != nil && obj.Parent().Parent() == types.Universe {
			names[ident.Name] = true
		}
		value, ok := obj.(*types.Const)
		if !ok {
			continue
		}
		basic, ok := value.Type().Underlying().(*types.Basic)
		if !ok || basic.Kind() != types.Int32 {
			continue
		}
		number, known := constant.Int64Val(value.Val())
		if !known || number < math.MinInt32 || number > math.MaxInt32 {
			return nil, nil, fmt.Errorf("%s: cannot resolve int32 constant %s", fset.Position(ident.Pos()), ident.Name)
		}
		if number > 0 {
			used[int32(number)] = true
		}
	}
	unresolved := checkTimerRegistrationScope(fset, files)
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Register" || len(call.Args) != 2 {
				return true
			}
			value := info.Types[call.Args[0]].Value
			var number int64
			var known bool
			if value != nil {
				number, known = constant.Int64Val(value)
			}
			if !known || number <= 0 || number > math.MaxInt32 {
				unresolved = errors.Join(unresolved, fmt.Errorf("%s: cannot resolve Register timer type; use a positive local int32 constant", fset.Position(call.Pos())))
			} else {
				used[int32(number)] = true
			}
			return true
		})
	}
	return used, names, unresolved
}

// wireModuleTimer 与模块模板同处一个回滚边界。只改业务定义与登记函数，所有框架生成物
// 由 GenerateTransactional 中的 project sync 生成，失败不会留下半套 bootstrap / DAO。
func wireModuleTimer(root string, manifest Manifest, item ModulePlanItem) error {
	plan := item.Timer
	n := newTimerNames(manifest.Project.Module, plan.Entity, plan.Spec)
	_, entityPath, err := resolveEntitySource(root, item.spec.Entity)
	if err != nil {
		return err
	}
	host, err := entitygen.InspectTimerHost(filepath.Dir(entityPath), item.spec.Entity)
	if err != nil {
		return err
	}
	dao, err := timerHostDao(plan.Entity, host)
	if err != nil {
		return err
	}
	if dao != plan.Spec.Dao {
		return fmt.Errorf("timer DAO changed from %s to %s", plan.Spec.Dao, dao)
	}
	// 同一批多个模块可能共享本次新建的组件；字段与登记追加均幂等。
	if plan.Create {
		daoPath, _, body, err := addTimerDaoFields(root, n)
		if err != nil {
			return err
		}
		if err := writeAtomic(daoPath, body, 0644); err != nil {
			return err
		}
		raw, err := os.ReadFile(entityPath)
		if err != nil {
			return err
		}
		body, err = wireTimerComponent(raw, entityPath, n)
		if err != nil {
			return err
		}
		if err := writeAtomic(entityPath, body, 0644); err != nil {
			return err
		}
	}
	file := filepath.Join(root, n.handlersFile())
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) && plan.Create {
		raw = []byte(renderTimerHandlers(n))
	} else if err != nil {
		return err
	}
	body, err := appendModuleTimerRegistration(raw, file, n, item.spec.Component)
	if err != nil {
		return err
	}
	return writeAtomic(file, body, 0644)
}

func appendModuleTimerRegistration(raw []byte, path string, n timerNames, moduleComponent string) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	wanted := "register" + n.CompType + "Handlers"
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != wanted || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		pointer, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		target, ok := pointer.X.(*ast.Ident)
		if !ok || target.Name != n.CompType+"Component" {
			continue
		}
		if fn.Body == nil || len(fn.Recv.List[0].Names) != 1 || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
			return nil, fmt.Errorf("%s: %s must have a named receiver and handlers parameter", path, wanted)
		}
		receiver, param := fn.Recv.List[0].Names[0].Name, fn.Type.Params.List[0].Names[0].Name
		call := fmt.Sprintf("%s.register%sTimers(%s)", receiver, moduleComponent, param)
		for _, stmt := range fn.Body.List {
			if expr, ok := stmt.(*ast.ExprStmt); ok && exprString(fset, expr.X) == call {
				return raw, nil
			}
		}
		// 不把登记偷偷放进 return 后面或分支里；这类自定义登记需业务先整理成顺序函数。
		for _, stmt := range fn.Body.List {
			if _, ok := stmt.(*ast.ExprStmt); !ok {
				return nil, fmt.Errorf("%s: %s has control flow; keep registrations as direct calls before installing timer modules", path, wanted)
			}
		}
		offset := fset.Position(fn.Body.Rbrace).Offset
		body := append(slices.Clone(raw[:offset]), []byte("\n\t"+call+"\n")...)
		body = append(body, raw[offset:]...)
		return format.Source(body)
	}
	return nil, fmt.Errorf("%s: cannot find %s on %sComponent", path, wanted, n.CompType)
}

// 已分配类型既是模块归属，也是持久期限的解码键。手改清单不能让这两份记录悄悄分叉。
func (m Manifest) validateModuleTimerRecords() error {
	var joined error
	owners := map[string]string{}
	for moduleName, record := range m.Modules {
		for name, id := range record.IDs["timer"] {
			timer, exists := m.Timers[record.TimerEntity]
			if !exists || id <= 0 || id > math.MaxInt32 || timer.Types[name] != int32(id) || timer.Service != record.Service {
				joined = errors.Join(joined, fmt.Errorf("module %s timer %s disagrees with timers.%s.types / service", moduleName, name, record.TimerEntity))
			}
			key := fmt.Sprintf("%s:%d", record.TimerEntity, id)
			if previous, exists := owners[key]; exists {
				joined = errors.Join(joined, fmt.Errorf("modules %s and %s both own timer %s", previous, moduleName, key))
			}
			owners[key] = moduleName
		}
	}
	return joined
}

// 不允许把登记表交给无法分析的外部代码，或存到其他变量里绕过检查。
// 本包的登记方法可以互相调用；每个方法仍须显式接收 *entitytimer.Handlers。
func checkTimerRegistrationScope(fset *token.FileSet, files []*ast.File) error {
	type registration struct {
		fn        *ast.FuncDecl
		parameter *ast.Ident
	}
	var registrations []registration
	helpers := map[string]int{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, field := range fn.Type.Params.List {
				pointer, ok := field.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				selector, ok := pointer.X.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Handlers" {
					continue
				}
				for _, name := range field.Names {
					registrations = append(registrations, registration{fn, name})
					helpers[fn.Name.Name]++
				}
			}
		}
	}
	var joined error
	for _, item := range registrations {
		allowed := map[token.Pos]bool{}
		same := func(expr ast.Expr) bool { id, ok := expr.(*ast.Ident); return ok && id.Obj == item.parameter.Obj }
		ast.Inspect(item.fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Register" && same(selector.X) && len(call.Args) == 2 {
				allowed[selector.X.Pos()] = true
			}
			helper := ""
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				helper = fn.Name
			case *ast.SelectorExpr:
				// 只允许本方法的接收者，外部包恰好有同名函数也不能算成本包登记。
				if item.fn.Recv != nil && len(item.fn.Recv.List[0].Names) == 1 {
					if receiver, ok := fn.X.(*ast.Ident); ok && receiver.Obj == item.fn.Recv.List[0].Names[0].Obj {
						helper = fn.Sel.Name
					}
				}
			}
			if helpers[helper] == 1 {
				for _, arg := range call.Args {
					if same(arg) {
						allowed[arg.Pos()] = true
					}
				}
			}
			return true
		})
		ast.Inspect(item.fn.Body, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if ok && ident.Obj == item.parameter.Obj && !allowed[ident.Pos()] {
				joined = errors.Join(joined, fmt.Errorf("%s: timer handlers escape static registration; keep registrations in local typed methods", fset.Position(ident.Pos())))
			}
			return true
		})
	}
	return joined
}
