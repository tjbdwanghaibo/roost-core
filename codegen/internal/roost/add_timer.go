package roost

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
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
)

// roost add timer --entity <Entity> [--component Timer] [--service <service>]（DESIGN-TODO D11，维护者 2026-10-17
// 决定方案 B）：按实体的持久定时器。
//
// 归属：
//
//	roost.yaml timers.<entity>                                  声明（组件名、组件类型号、服务、节点所在 DAO）
//	db/def/<entity>.go（实体的 DAO 定义）                         add 时加两个字段：节点 map 与种子（之后归业务）
//	game/entities/<entity>/entity.go                             add 时加组件字段（之后归业务）
//	db/def/<entity>_<component>_node_gen.go                      节点类型                       project sync 重写
//	game/entities/<entity>/<component>_component_gen.go          组件、DAO 适配、驱动变量       project sync 重写
//	game/handler/tick_<entity>_<component>_gen.go(+_test.go)     到期 Tick 的 Nest handler      project sync 重写
//	internal/bootstrap/generated.go                              wiring/entitytimer 装配驱动    project sync 重写
//	game/entities/<entity>/<component>_handlers.go               定时器类型与业务回调           只在缺失时创建一次
//
// 运行期（索引、投递、离线语义）在 core 的 framework/nest/entitytimer；生成物只是 DAO 与 Nest handler 的适配。

const (
	timerDefaultComponent = "timer"
	entityTimerImport     = "github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
	entityTimerWiring     = "github.com/tjbdwanghaibo/roost-core/wiring/entitytimer"
	entityTimerDocURL     = "https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/framework/guide/10-time.md#4-按实体的持久定时器"
)

// timerNames 是一个 timers 条目展开后的全部名字，渲染与 add 共用。
type timerNames struct {
	Module      string
	Entity      string // snake：player
	EntityType  string // Player
	Comp        string // snake：timer
	CompType    string // Timer
	ID          int64
	Dao         string // PlayerDao
	DaoGetter   string // Dao
	Node        string // PlayerTimerNode
	MapField    string // Timers
	MapKey      string // timers
	SeedField   string // TimerSeed
	SeedKey     string // timer_seed
	Handler     string // handlerTickPlayerTimer
	HandlerFile string // tick_player_timer_gen
	DriverName  string // player.timer
}

func newTimerNames(module, entityName string, spec TimerSpec) timerNames {
	comp := toSnake(spec.Component)
	if comp == "" {
		comp = timerDefaultComponent
	}
	entitySnake := toSnake(entityName)
	n := timerNames{
		Module: module, Entity: entitySnake, EntityType: toPascal(entitySnake),
		Comp: comp, CompType: toPascal(comp), ID: spec.ID, Dao: spec.Dao,
	}
	n.DaoGetter = timerDaoGetter(n.EntityType, spec.Dao)
	n.Node = n.EntityType + n.CompType + "Node"
	n.MapField, n.MapKey = n.CompType+"s", comp+"s"
	n.SeedField, n.SeedKey = n.CompType+"Seed", comp+"_seed"
	n.Handler = "handlerTick" + n.EntityType + n.CompType
	n.HandlerFile = "tick_" + entitySnake + "_" + comp + "_gen"
	n.DriverName = entitySnake + "." + comp
	return n
}

// timerDaoGetter 与 entity 生成器的 DAO getter 同一规则（daoGetterName）：类型名去掉实体名前缀，空了就是 Dao。
func timerDaoGetter(entityType, daoType string) string {
	name := strings.TrimPrefix(strings.TrimPrefix(daoType, "*"), "db.")
	name = strings.TrimPrefix(name, entityType)
	if name == "" {
		return "Dao"
	}
	return name
}

func (n timerNames) componentFile() string {
	return "game/entities/" + n.Entity + "/" + n.Comp + "_component_gen.go"
}
func (n timerNames) handlersFile() string {
	return "game/entities/" + n.Entity + "/" + n.Comp + "_handlers.go"
}
func (n timerNames) nodeFile() string { return "db/def/" + n.Entity + "_" + n.Comp + "_node_gen.go" }
func (n timerNames) tickFile() string { return "game/handler/" + n.HandlerFile + ".go" }
func (n timerNames) tickTestFile() string {
	return "game/handler/" + n.HandlerFile + "_test.go"
}

func sortedTimerEntities(m Manifest) []string {
	names := make([]string, 0, len(m.Timers))
	for name := range m.Timers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateTimer 是 timers.<entity> 的静态检查；实体本身（存在、不是 Remote / noPersist、DAO 对得上）由 add 检查。
func (m Manifest) validateTimer(entityName string) error {
	spec := m.Timers[entityName]
	var joined error
	if !validName(entityName) || toSnake(entityName) != entityName {
		joined = errors.Join(joined, fmt.Errorf("timers: invalid entity name %q (snake_case)", entityName))
	}
	if component := spec.Component; component != "" && (!validName(component) || toSnake(component) != component) {
		joined = errors.Join(joined, fmt.Errorf("timers.%s.component %q is not a snake_case name", entityName, component))
	}
	if spec.ID <= 0 {
		joined = errors.Join(joined, fmt.Errorf("timers.%s.id must be a positive component type id", entityName))
	}
	if spec.Dao == "" {
		joined = errors.Join(joined, fmt.Errorf("timers.%s.dao names the DAO type that stores the nodes", entityName))
	}
	service, exists := m.Services[spec.Service]
	switch {
	case !exists:
		joined = errors.Join(joined, fmt.Errorf("timers.%s.service %q is not a service of this project", entityName, spec.Service))
	case service.Framework != "":
		joined = errors.Join(joined, fmt.Errorf("timers.%s.service %q hosts a framework service; timers run where the entity's Nest runs", entityName, spec.Service))
	default:
		if resolved, err := resolveMods(effectiveServiceMods(m, spec.Service)); err == nil && !contains(resolved, "nest") {
			joined = errors.Join(joined, fmt.Errorf("timers.%s.service %q has no nest mod; the timer driver delivers ticks through Nest", entityName, spec.Service))
		}
	}
	for _, feature := range []string{"entity", "nest", "dao"} {
		if !contains(m.Features, feature) {
			joined = errors.Join(joined, fmt.Errorf("timers.%s needs the %s feature", entityName, feature))
		}
	}
	return joined
}

// timerServices 返回在 service 上装的定时器（按实体名排序）。
func timerServices(m Manifest, service string) []timerNames {
	var out []timerNames
	for _, name := range sortedTimerEntities(m) {
		if spec := m.Timers[name]; spec.Service == service {
			out = append(out, newTimerNames(m.Project.Module, name, spec))
		}
	}
	return out
}

// addTimer 实现 roost add timer。先改两个业务文件（DAO 定义加字段、实体加组件字段），再写 roost.yaml 并
// project sync 渲染生成部分；sync 失败时两个业务文件一并还原。
func addTimer(root string, m Manifest, options AddOptions) ([]string, error) {
	requested := strings.TrimSpace(options.Entity)
	if requested == "" {
		requested = strings.TrimSpace(options.Name)
	} else if name := strings.TrimSpace(options.Name); name != "" && toSnake(name) != toSnake(requested) {
		return nil, fmt.Errorf("roost add timer takes the entity once: got %q and --entity %q", name, requested)
	}
	if requested == "" {
		return nil, errors.New("roost add timer needs the entity: roost add timer --entity <Entity> [--component Timer]")
	}
	for _, feature := range []string{"entity", "nest", "dao"} {
		if !hasFeature(m, feature) {
			return nil, fmt.Errorf("roost add timer needs the %s feature in roost.yaml", feature)
		}
	}
	entityName, entityPath, err := resolveEntitySource(root, requested)
	if err != nil {
		return nil, err
	}
	if existing, ok := m.Timers[entityName]; ok {
		comp := existing.Component
		if comp == "" {
			comp = timerDefaultComponent
		}
		return nil, fmt.Errorf("entity %s already has a timer component (%s, roost.yaml timers.%s); one component holds every timer type of an entity: add a type to game/entities/%s/%s_handlers.go", entityName, comp, entityName, entityName, comp)
	}
	comp := toSnake(options.Component)
	if comp == "" {
		comp = timerDefaultComponent
	}
	if !validName(comp) {
		return nil, fmt.Errorf("invalid timer component name %q", options.Component)
	}
	service, err := timerService(m, options.Service)
	if err != nil {
		return nil, err
	}
	host, err := entity.InspectTimerHost(filepath.Dir(entityPath), toPascal(entityName))
	if err != nil {
		return nil, err
	}
	dao, err := timerHostDao(entityName, host)
	if err != nil {
		return nil, err
	}
	id := options.ID
	if id == 0 {
		if id, err = NextID(root, m, "component", ""); err != nil {
			return nil, err
		}
	} else if err := validateExplicitID(m, "component", "", id); err != nil {
		return nil, err
	}
	spec := TimerSpec{Component: comp, ID: id, Service: service, Dao: dao}
	names := newTimerNames(m.Project.Module, entityName, spec)
	// 实体上已经有同名组件字段时，只有它正是这个定时器组件的空位（字段在、类型常量还没人声明，game-demo 的 World
	// 模板就是这样）才接受；类型常量已由别的文件声明说明那是另一个组件。
	if declared, err := packageDeclaresConst(filepath.Dir(entityPath), "CompType"+names.CompType); err != nil {
		return nil, err
	} else if declared || (contains(host.Components, "CompType"+names.CompType) && !entityHasTimerField(host, names)) {
		return nil, fmt.Errorf("entity %s already has a component %s (CompType%s); choose another name with --component", names.EntityType, names.CompType, names.CompType)
	}
	for _, rel := range []string{names.componentFile(), names.handlersFile(), names.nodeFile(), names.tickFile(), "game/entities/" + entityName + "/" + comp + "_component.go"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			return nil, fmt.Errorf("%s already exists; remove it or choose another name with --component", rel)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if options.ID != 0 {
		uses, err := ScanIDs(root)
		if err != nil {
			return nil, err
		}
		for _, use := range uses {
			if use.Kind == "component" && use.ID == id {
				return nil, fmt.Errorf("component id %d is already used by %s", id, use.File)
			}
		}
	}

	daoPath, daoBefore, daoAfter, err := addTimerDaoFields(root, names)
	if err != nil {
		return nil, err
	}
	entityBefore, err := os.ReadFile(entityPath)
	if err != nil {
		return nil, err
	}
	entityAfter, err := wireTimerComponent(entityBefore, entityPath, names)
	if err != nil {
		return nil, err
	}
	daoRel, entityRel := relativeSlash(root, daoPath), relativeSlash(root, entityPath)
	backups, err := captureFiles(root, []string{daoRel, entityRel})
	if err != nil {
		return nil, err
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	var edits []syncChange
	if !bytes.Equal(daoBefore, daoAfter) {
		edits = append(edits, syncChange{rel: daoRel, path: daoPath, before: daoBefore, body: daoAfter, existed: true})
	}
	if !bytes.Equal(entityBefore, entityAfter) {
		edits = append(edits, syncChange{rel: entityRel, path: entityPath, before: entityBefore, body: entityAfter, existed: true})
	}
	if err := commitSyncChanges(edits); err != nil {
		return nil, err
	}
	edited, err := captureFiles(root, []string{daoRel, entityRel})
	if err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	if m.Timers == nil {
		m.Timers = make(map[string]TimerSpec)
	}
	m.Timers[entityName] = spec
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		delete(m.Timers, entityName)
		return nil, restoreFilesIfCurrent(root, backups, edited, err)
	}
	return []string{
		"roost.yaml", daoRel, entityRel,
		names.nodeFile(), names.componentFile(), names.handlersFile(), names.tickFile(), names.tickTestFile(),
		"internal/bootstrap/generated.go",
	}, nil
}

// timerService 选运行该实体 Nest 的服务：--service，或工程里唯一一个装了 nest 的业务服务。
func timerService(m Manifest, requested string) (string, error) {
	var candidates []string
	for _, name := range sortedServiceNames(m) {
		if m.isFrameworkService(name) {
			continue
		}
		if resolved, err := resolveMods(effectiveServiceMods(m, name)); err == nil && contains(resolved, "nest") {
			candidates = append(candidates, name)
		}
	}
	if requested = toSnake(requested); requested != "" {
		if _, ok := m.Services[requested]; !ok {
			return "", fmt.Errorf("unknown service %q", requested)
		}
		if !contains(candidates, requested) {
			return "", fmt.Errorf("service %q does not run Nest; the timer driver delivers ticks through Nest (roost add mod nest -service %s)", requested, requested)
		}
		return requested, nil
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", errors.New("roost add timer needs a service with the nest mod; run: roost add mod nest -service <service>")
	default:
		return "", fmt.Errorf("services %s all run Nest; choose the one that loads this entity with --service", strings.Join(candidates, ", "))
	}
}

// timerHostDao 选存放节点的 DAO：实体唯一的 DAO，或几个里面与实体同名的那个（字段 dao）。
func timerHostDao(entityName string, host entity.TimerHost) (string, error) {
	var local []entity.DaoField
	for _, dao := range host.Daos {
		local = append(local, dao)
	}
	pick := func(dao entity.DaoField) string { return lastSegment(strings.TrimPrefix(dao.TypeName, "*")) }
	switch len(local) {
	case 0:
		return "", fmt.Errorf("entity %s has no DAO; timer nodes are stored in it. Run: roost add dao %s --entity %s", toPascal(entityName), toPascal(entityName), toPascal(entityName))
	case 1:
		return pick(local[0]), nil
	}
	for _, dao := range local {
		if dao.FieldName == "dao" {
			return pick(dao), nil
		}
	}
	names := make([]string, 0, len(local))
	for _, dao := range local {
		names = append(names, dao.FieldName+" "+pick(dao))
	}
	return "", fmt.Errorf("entity %s has %d DAOs (%s) and none is its main DAO (field dao); timer nodes need one DAO", toPascal(entityName), len(local), strings.Join(names, ", "))
}

// entityHasTimerField：实体上 CompType<C> 的字段就是 *<C>Component。
func entityHasTimerField(host entity.TimerHost, names timerNames) bool {
	for _, comp := range host.ComponentFields {
		if comp.CompType == "CompType"+names.CompType && strings.TrimPrefix(comp.TypeName, "*") == names.CompType+"Component" {
			return true
		}
	}
	return false
}

// packageDeclaresConst 报告 dir 包的非测试文件是否在包级声明了常量 name。
func packageDeclaresConst(dir, name string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, file), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				for _, ident := range spec.(*ast.ValueSpec).Names {
					if ident.Name == name {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

func lastSegment(typeName string) string {
	if index := strings.LastIndex(typeName, "."); index >= 0 {
		return typeName[index+1:]
	}
	return typeName
}

// addTimerDaoFields 在 DAO 定义里加节点 map 与种子两个字段（文本插在结构体的右括号前，保留业务注释）。
// 两个字段已经按同样的类型存在时不改（game-demo 的 World 模板自带它们）；名字被占用、类型不同则拒绝。
func addTimerDaoFields(root string, names timerNames) (string, []byte, []byte, error) {
	dir := filepath.Join(root, "db", "def")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, nil, fmt.Errorf("read DAO definitions: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", nil, nil, err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			return "", nil, nil, fmt.Errorf("parse %s: %w", relativeSlash(root, path), err)
		}
		structure := findStruct(file, names.Dao)
		if structure == nil {
			continue
		}
		mapType, seedType := "map[int64]*"+names.Node, "int64"
		found := 0
		for _, field := range structure.Fields.List {
			for _, ident := range field.Names {
				var want string
				switch ident.Name {
				case names.MapField:
					want = mapType
				case names.SeedField:
					want = seedType
				default:
					continue
				}
				if got := exprString(fset, field.Type); got != want {
					return "", nil, nil, fmt.Errorf("%s: %s.%s already exists as %s; the timer component needs it as %s (choose another name with --component)", relativeSlash(root, path), names.Dao, ident.Name, got, want)
				}
				found++
			}
		}
		if found == 2 {
			return path, raw, raw, nil
		}
		if found != 0 {
			return "", nil, nil, fmt.Errorf("%s: %s has only one of %s / %s; add both or neither", relativeSlash(root, path), names.Dao, names.MapField, names.SeedField)
		}
		closing := fset.Position(structure.Fields.Closing).Offset
		fields := fmt.Sprintf(`
	// %[1]s 是 %[2]sComponent 的定时器节点（roost add timer 加入）：节点 ID -> 节点。这份 map 就是全部期限，
	// 增删改都在触发它的 Nest 事务里经组件写回，随事务提交 / 回滚；没有别的存储。
	%[1]s map[int64]*%[3]s `+"`bson:\"%[4]s\" dao:\"persist,map=fast\"`"+`
	// %[5]s 是分配过的最大节点 ID，持久化：重启后不会把已经交出去的 ID 再分配一次。
	%[5]s int64 `+"`bson:\"%[6]s\" dao:\"persist\"`"+`
`, names.MapField, names.CompType, names.Node, names.MapKey, names.SeedField, names.SeedKey)
		edited := append(append(append([]byte(nil), raw[:closing]...), []byte(fields)...), raw[closing:]...)
		formatted, err := format.Source(edited)
		if err != nil {
			return "", nil, nil, fmt.Errorf("format %s: %w", relativeSlash(root, path), err)
		}
		return path, raw, formatted, nil
	}
	return "", nil, nil, fmt.Errorf("DAO %s is not defined in db/def; the timer nodes are stored in it", names.Dao)
}

func findStruct(file *ast.File, name string) *ast.StructType {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			if typeSpec, ok := spec.(*ast.TypeSpec); ok && typeSpec.Name.Name == name {
				structure, _ := typeSpec.Type.(*ast.StructType)
				return structure
			}
		}
	}
	return nil
}

func exprString(fset *token.FileSet, expr ast.Expr) string {
	var b bytes.Buffer
	_ = format.Node(&b, fset, expr)
	return b.String()
}

// wireTimerComponent 给实体加组件字段；已经有同一组件字段（game-demo 的 World 模板）时不改。
func wireTimerComponent(raw []byte, path string, names timerNames) ([]byte, error) {
	_, _, _, structure, err := parseEntityAggregate(raw, path, names.EntityType)
	if err != nil {
		return nil, err
	}
	if structHasTagValue(structure, "comp", "CompType"+names.CompType) {
		return raw, nil
	}
	updated, _, err := wireComponentField(raw, path, names.EntityType, names.Comp, names.CompType)
	return updated, err
}

// renderTimerFiles 是一个 timers 条目的生成部分与业务文件（project sync 调用）。
func renderTimerFiles(m Manifest, addGo func(path, body string, owned bool) error) error {
	for _, name := range sortedTimerEntities(m) {
		names := newTimerNames(m.Project.Module, name, m.Timers[name])
		for _, file := range []struct {
			path  string
			body  string
			owned bool
		}{
			{names.nodeFile(), renderTimerNode(names), true},
			{names.componentFile(), renderTimerComponent(names), true},
			{names.tickFile(), renderTimerTick(names), true},
			{names.tickTestFile(), renderTimerTickTest(names), true},
			{names.handlersFile(), renderTimerHandlers(names), false},
		} {
			if err := addGo(file.path, file.body, file.owned); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderTimerNode(n timerNames) string {
	return fmt.Sprintf(`%[1]s
package dbdef

// %[2]s 是 %[3]s 的 %[4]sComponent 存的一个定时器节点（roost add timer 生成，project sync 重写）：
// infra/base/timer.Node 去掉只在内存里的部分（堆下标、回调）。回调不能存，类型号就是重启后找回回调的依据。
// 期限按毫秒存：组件按毫秒取业务时间，重建前后的触发顺序一致。
type %[2]s struct {
	Type int32 `+"`bson:\"type\"`"+`
	// Priority 只在期限相同时起作用：小的先触发，相同时按登记顺序（节点 ID）。
	Priority     int32  `+"`bson:\"priority\"`"+`
	Param1       int64  `+"`bson:\"param1\"`"+`
	Param2       int64  `+"`bson:\"param2\"`"+`
	Payload      string `+"`bson:\"payload,omitempty\"`"+`
	EndUnixMilli int64  `+"`bson:\"end_unix_milli\"`"+`
	DelayMillis  int64  `+"`bson:\"delay_millis\"`"+`
}
`, generatedHeader, n.Node, n.EntityType, n.CompType)
}

func renderTimerComponent(n timerNames) string {
	return fmt.Sprintf(`%[1]s
// roost add timer --entity %[3]s 生成，project sync 重写；定时器类型与业务回调在 %[4]s_handlers.go（业务文件）。
// 运行期在 core 的 framework/nest/entitytimer：%[8]s
package %[2]s

import (
	"fmt"
	"time"

	db %[5]q
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
	"github.com/tjbdwanghaibo/roost-core/infra/base/timer"
)

//roost:component type=%[6]d
const CompType%[7]s entity.ComponentType = %[6]d

// %[7]sTickHandler 是到期时驱动投递的 Nest handler（game/handler/%[9]s.go）。
const %[7]sTickHandler = %[10]q

// %[7]sDriver 是 %[3]s 的定时器驱动：进程内按“已加载实例的最早期限”排的索引，到期才给那一个实体投递一次
// Tick，不按在线人数轮询。生成的 bootstrap 用 wiring/entitytimer 把它接到 Nest 与 App 业务时钟上。
var %[7]sDriver = entitytimer.NewDriver(entitytimer.Config{Name: %[11]q, Handler: nest.NewHandlerName(%[7]sTickHandler)})

// %[7]sComponent 是 %[3]s 的持久定时器。
//
//   - 期限只在 DAO 里（%[12]s.%[13]s / %[14]s）：外层调用从 DAO 现建调度器，Tick 回调中的组件操作共享当前视图；增删改经钩子写回，随调用所在的
//     Nest 事务提交或回滚。组件自己不存任何事务会改的状态。
//   - 到期动作在实体的 Nest 锁内执行（Tick handler），回调只改 DAO、记 effect，不做 I/O。
//   - 离线语义（维护者 2026-10-17 决定）：框架不追帧。实体不在内存期间到期的节点，加载后只触发一次，不论错过
//     多少个周期；回调参数 entitytimer.Fire 带原定期限 Deadline、触发时刻 Now、加载时刻 LoadedAt、补触发标志
//     Overdue 与间隔 Interval，补算由业务自己做。回调返回正值是下次间隔（从 Now 起算），0 结束。
//   - 驱动不唤醒不在内存的实体：离线实体到期只在下次加载时补触发。
//
// 方法只能在持有实体锁时调用（Nest handler 里）。
type %[7]sComponent struct {
	entity.ComponentBase
	owner *%[3]s
	host  *entitytimer.Host
}

// I%[7]sEntity 是 Nest handler 使用本组件的锁安全视图。
type I%[7]sEntity interface {
	entity.IThreadSafeEntity
	%[7]sComp() *%[7]sComponent
}

func init() {
	entity.RegisterComponentFactory(CompType%[7]s, func(owner any, _ *entity.EntityCreateParam) (entity.ComponentInterfaceBase, error) {
		typed, ok := owner.(*%[3]s)
		if !ok {
			return nil, fmt.Errorf("%[4]s component: owner %%T is not *%[3]s", owner)
		}
		component := &%[7]sComponent{owner: typed}
		component.host = %[7]sDriver.NewHost(typed, %[15]sStore{owner: typed}, component.register%[7]sHandlers)
		return component, nil
	})
}

func (component *%[7]sComponent) Name() string { return %[4]q }

// Owner 只在实体存活且被 Nest 加锁时可用。
func (component *%[7]sComponent) Owner() *%[3]s { return component.owner }

// OnInitFinish：加载时记下加载时刻、把最早期限交给驱动；已经过期的节点因此在加载后被投递一次 Tick 补触发。
// 不写 DAO，加载不产生写入。回调登记有误（类型号非正、重复）时实体初始化失败。
func (component *%[7]sComponent) OnInitFinish(_ *entity.EntityCreateParam, isCreate bool) error {
	return component.host.OnInitFinish(isCreate)
}

// OnDestroy：卸载、删档或撤销新建时，实例永久离开驱动索引。
func (component *%[7]sComponent) OnDestroy(entity.EntityDestroyReason) { component.host.OnDestroy() }

// New%[7]s 登记一个 delay 之后到期的定时器，返回节点 ID（实体内唯一、重启后不复用）；参数不合法时返回 0。
func (component *%[7]sComponent) New%[7]s(delay time.Duration, timerType int32, param1, param2 int64, payload []byte) int64 {
	return component.host.NewTimer(delay, timerType, param1, param2, payload)
}

// New%[7]sWithPriority 另外指定同一期限内的触发顺序（小的先触发）。
func (component *%[7]sComponent) New%[7]sWithPriority(delay time.Duration, timerType int32, priority int32, param1, param2 int64, payload []byte) int64 {
	return component.host.NewTimerWithPriority(delay, timerType, priority, param1, param2, payload)
}

// New%[7]sAt 登记一个在 at 到期的定时器（按毫秒）；at 已过去时下一次 Tick 触发。
func (component *%[7]sComponent) New%[7]sAt(at time.Time, timerType int32, param1, param2 int64, payload []byte) int64 {
	return component.host.NewTimerAt(at, timerType, param1, param2, payload)
}

// Change%[7]s 把节点改到现在起 delay 之后；节点不存在时返回 false。
func (component *%[7]sComponent) Change%[7]s(id int64, delay time.Duration) bool {
	return component.host.ChangeTimer(id, delay)
}

// Remove%[7]s 取消节点；返回 true 之后它不再触发。
func (component *%[7]sComponent) Remove%[7]s(id int64) bool { return component.host.RemoveTimer(id) }

// %[7]s 返回一个节点的当前状态。
func (component *%[7]sComponent) %[7]s(id int64) (timer.Node, bool) { return component.host.Timer(id) }

// %[7]ss 返回全部节点，按触发顺序。
func (component *%[7]sComponent) %[7]ss() []timer.Node { return component.host.Timers() }

// Tick 触发 now 时已到期的节点，返回剩下的节点数（生成的 Tick handler 调用）。
func (component *%[7]sComponent) Tick(now time.Time) int { return component.host.Tick(now) }

// %[15]sStore 把 %[12]s 的两个字段适配成 entitytimer.Store；写入走生成的 mutator，随事务回滚。
type %[15]sStore struct{ owner *%[3]s }

func (store %[15]sStore) SavedTimers() []timer.Node {
	dao := store.owner.%[16]s()
	saved := make([]timer.Node, 0, dao.%[13]sLen())
	dao.Range%[13]s(func(id int64, stored *db.%[17]s) bool {
		if stored == nil {
			return true
		}
		saved = append(saved, timer.Node{
			ID: id, Type: stored.GetType(), Priority: stored.GetPriority(),
			Param1: stored.GetParam1(), Param2: stored.GetParam2(),
			Payload: []byte(stored.GetPayload()),
			End:     time.UnixMilli(stored.GetEndUnixMilli()),
			Delay:   time.Duration(stored.GetDelayMillis()) * time.Millisecond,
		})
		return true
	})
	return saved
}

func (store %[15]sStore) TimerSeed() int64 { return store.owner.%[16]s().Get%[14]s() }

func (store %[15]sStore) SetTimerSeed(seed int64) { store.owner.%[16]s().Set%[14]s(seed) }

func (store %[15]sStore) DeleteTimer(id int64) { store.owner.%[16]s().Del%[13]s(id) }

func (store %[15]sStore) PutTimer(node timer.Node) {
	stored := &db.%[17]s{}
	stored.SetType(node.Type)
	stored.SetPriority(node.Priority)
	stored.SetParam1(node.Param1)
	stored.SetParam2(node.Param2)
	stored.SetPayload(string(node.Payload))
	stored.SetEndUnixMilli(node.End.UnixMilli())
	stored.SetDelayMillis(node.Delay.Milliseconds())
	store.owner.%[16]s().Set%[13]s(node.ID, stored)
}
`, generatedHeader, n.Entity, n.EntityType, n.Comp, n.Module+"/db", n.ID, n.CompType, entityTimerDocURL,
		n.HandlerFile, n.Handler, n.DriverName, n.Dao, n.MapField, n.SeedField, strings.ToLower(n.EntityType[:1])+n.EntityType[1:]+n.CompType, n.DaoGetter, n.Node)
}

func renderTimerTick(n timerNames) string {
	return fmt.Sprintf(`%[1]s
package handler

import (
	"time"

	%[2]s %[3]q
)

// %[4]s 触发 %[5]s 已到期的定时器（roost add timer 生成，project sync 重写）。
//
// 由 %[2]s.%[6]sDriver 在最早期限到了时投递（只投到期的那一个实体），在实体的 Nest 锁与事务内执行：
// 回调改的东西与被触发节点的删除 / 续期是同一条 WAL 记录，提交后驱动才看到新的最早期限。nowUnixMilli 是
// 驱动投递时的业务时间。返回剩下的节点数。
//
//roost:nest rollback=undo durability=strict
func %[4]s(target %[2]s.I%[6]sEntity, nowUnixMilli int64) (int32, error) {
	return int32(target.%[6]sComp().Tick(time.UnixMilli(nowUnixMilli))), nil
}
`, generatedHeader, n.Entity, n.Module+"/game/entities/"+n.Entity, n.Handler, n.EntityType, n.CompType)
}

func renderTimerTickTest(n timerNames) string {
	return fmt.Sprintf(`%[1]s
package handler

import (
	"testing"

	%[2]s %[3]q
)

// 驱动投递的 handler 名必须就是 Nest 生成器从 %[4]s 得出的名字，否则到期消息投给一个不存在的 handler。
func Test%[5]s%[6]sTickHandlerMatchesTheDriver(t *testing.T) {
	if got, want := handlerName%[7]s.String(), %[2]s.%[6]sTickHandler; got != want {
		t.Fatalf("nest handler %%q, driver delivers to %%q", got, want)
	}
	if got := %[2]s.%[6]sDriver.Handler().String(); got != %[2]s.%[6]sTickHandler {
		t.Fatalf("driver handler %%q", got)
	}
}
`, generatedHeader, n.Entity, n.Module+"/game/entities/"+n.Entity, n.Handler, n.EntityType, n.CompType, strings.TrimPrefix(n.Handler, "handler"))
}

func renderTimerHandlers(n timerNames) string {
	return fmt.Sprintf(`package %[1]s

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
)

// %[2]s 的定时器类型。类型号随节点存进 DAO，重启后靠它找回回调，所以发布后不要改号；下线一种类型时先让
// 存量节点到期或清掉（没有回调的存量类型加载时告警，到期时丢弃并计 timer.unhandled_dropped_total）。
// 这个文件归业务：roost add timer 只在缺失时创建，project sync 不改写。
const (
	// %[3]sTypeExample 是示例，换成自己的类型（从 1 开始，必须为正）。
	%[3]sTypeExample int32 = 1
)

// register%[3]sHandlers 登记每种类型的回调；组件构造时调用一次。
func (component *%[3]sComponent) register%[3]sHandlers(handlers *entitytimer.Handlers) {
	handlers.Register(%[3]sTypeExample, component.on%[3]sExample)
}

// on%[3]sExample 在节点到期时执行：实体的 Nest 锁与事务内，只改 DAO、记 effect，不做 I/O、不阻塞。
//
// 离线语义：实体不在内存期间到期的节点，加载后只触发这一次（fire.Overdue 为 true），框架不补触发错过的周期。
// 需要补算时用 fire.Deadline（原定期限）、fire.Now（实际触发时刻）与 fire.Interval（间隔），例如
// missed := fire.Now.Sub(fire.Deadline) / fire.Interval。
// 返回正值是下次间隔（从 fire.Now 起算），返回 0 结束这个定时器。
func (component *%[3]sComponent) on%[3]sExample(fire entitytimer.Fire) time.Duration {
	_ = fire
	return 0
}
`, n.Entity, n.EntityType, n.CompType)
}
