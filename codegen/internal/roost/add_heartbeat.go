package roost

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
)

// roost add heartbeat / roost add behavior / roost add timedomain：实体心跳、行为组件与场景时间域。
//
//	roost add heartbeat --entity <Entity> [--service <service>] [--id <component-id>]
//	roost add behavior  --entity <Entity> [--ai] [--state] [--service <service>] [--id <component-id>]
//	roost add timedomain --entity <Scene> [--id <component-id>]
//
// 归属：
//
//	roost.yaml heartbeats.<entity> / time_domains.<entity>           声明（组件类型号、服务、行为选项、DAO）
//	roost.yaml services.<service>.mods                               add 时加 heartbeat（Mod 依赖 nest）
//	game/entities/<entity>/entity.go                                 add 时加组件字段（之后归业务）
//	db/def/<entity>.go（timedomain）                                 add 时加三个 nopersist,sync 字段（之后归业务）
//	game/heartbeat/driver_gen.go                                     工程唯一的心跳驱动              project sync 重写
//	game/entities/<entity>/heartbeat_component_gen.go                心跳宿主组件                    project sync 重写
//	game/entities/<entity>/behavior_component_gen.go                 actionflow.Behavior（+ AI）     project sync 重写
//	game/entities/<entity>/timedomain_component_gen.go               timedomain.Domain 与 DAO 适配    project sync 重写
//	internal/bootstrap/generated.go                                  wiring/heartbeat 装驱动          project sync 重写
//	game/entities/<entity>/heartbeat_beats.go                        本实体自己的心跳登记             只在缺失时创建
//	game/entities/<entity>/behavior_actions.go                       动作种类、组、注册表、示例动作   只在缺失时创建
//	game/entities/<entity>/behavior_strategy.go（--ai）              示例 AI 策略                     只在缺失时创建
//	game/entities/<entity>/states.go（--state）                      gameplay/state 的 Catalog        只在缺失时创建
//	game/handler/<entity>_behavior.go                                示例 Nest handler                只在缺失时创建
//	game/entities/<entity>/timedomain_config.go                      时间域的倍率范围                 只在缺失时创建
//
// 运行期（调度、投递、跳拍、行为控制、时间域）都在 core 的 gameplay/*；生成物只是组件与装配的适配。
//
// 装配取舍（按需装配，不缺省装配）：heartbeat Mod 只装在有心跳实体的服务上。实体侧本来就是零开销（没有登记、或登记
// 都停着的实例不占调度），但驱动 goroutine 每 10ms 走一格（空转也一样），每个 Mod 让 shutdown.total_timeout 多 3s
// 保底；所以与 entitytimer 一样，由 add 命令在需要时把 Mod 加进服务（也可以 roost add mod heartbeat 手工装）。

const (
	heartbeatDriverDir   = "game/heartbeat"
	heartbeatDriverAlias = "gameheartbeat"
	heartbeatDocURL      = "https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/framework/guide/10-time.md#4a-实体心跳周期推进"
	behaviorDocURL       = "https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结"
	timeDomainDocURL     = "https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/framework/guide/10-time.md#4d-场景时间域变速"
)

// HeartbeatSpec 是一个实体的心跳宿主组件（roost add heartbeat），可选带行为组件（roost add behavior）。
type HeartbeatSpec struct {
	// ID 是 HeartbeatComponent 的组件类型号（ids.component 段内）。
	ID int64 `yaml:"id"`
	// Service 是运行该实体 Nest 的服务：它装 heartbeat Mod（wiring/heartbeat）。
	Service string `yaml:"service"`
	// Behavior 非空时实体还有行为组件（actionflow.Behavior，可选 AI 心跳与状态目录）。
	Behavior *BehaviorSpec `yaml:"behavior,omitempty"`
}

// BehaviorSpec 是行为组件的选项。
type BehaviorSpec struct {
	// ID 是 BehaviorComponent 的组件类型号。
	ID int64 `yaml:"id"`
	// AI：组件同时构造 ai.NewBeatController（AI 决策用独立的 "ai" 心跳），并生成示例策略。
	AI bool `yaml:"ai,omitempty"`
	// State：生成 gameplay/state 的 Catalog 声明骨架（states.go）。
	State bool `yaml:"state,omitempty"`
}

// TimeDomainSpec 是一个场景实体的时间域组件（roost add timedomain）。
type TimeDomainSpec struct {
	// ID 是 TimeDomainComponent 的组件类型号。
	ID int64 `yaml:"id"`
	// Dao 是存放三个时间域字段的 DAO 类型（如 SceneDao）。
	Dao string `yaml:"dao"`
}

// heartbeatNames 是一个实体的心跳 / 行为 / 时间域条目展开后的全部名字，渲染与 add 共用。
type heartbeatNames struct {
	Module     string
	Project    string
	Entity     string // snake：monster
	EntityType string // Monster
	Var        string // monster（lowerCamel，局部名）
}

func newHeartbeatNames(m Manifest, entityName string) heartbeatNames {
	snake := toSnake(entityName)
	pascal := toPascal(snake)
	return heartbeatNames{Module: m.Project.Module, Project: m.Project.Name, Entity: snake, EntityType: pascal, Var: strings.ToLower(pascal[:1]) + pascal[1:]}
}

func (n heartbeatNames) dir() string { return "game/entities/" + n.Entity + "/" }
func (n heartbeatNames) heartbeatComponentFile() string {
	return n.dir() + "heartbeat_component_gen.go"
}
func (n heartbeatNames) heartbeatBeatsFile() string { return n.dir() + "heartbeat_beats.go" }
func (n heartbeatNames) behaviorComponentFile() string {
	return n.dir() + "behavior_component_gen.go"
}
func (n heartbeatNames) behaviorActionsFile() string  { return n.dir() + "behavior_actions.go" }
func (n heartbeatNames) behaviorStrategyFile() string { return n.dir() + "behavior_strategy.go" }
func (n heartbeatNames) statesFile() string           { return n.dir() + "states.go" }
func (n heartbeatNames) behaviorHandlerFile() string {
	return "game/handler/" + n.Entity + "_behavior.go"
}
func (n heartbeatNames) timeDomainComponentFile() string {
	return n.dir() + "timedomain_component_gen.go"
}
func (n heartbeatNames) timeDomainConfigFile() string { return n.dir() + "timedomain_config.go" }
func (n heartbeatNames) startHandler() string         { return "StartExample" + n.EntityType + "Action" }
func (n heartbeatNames) strategyHandler() string      { return "StartExample" + n.EntityType + "Strategy" }

func heartbeatDriverFile() string { return heartbeatDriverDir + "/driver_gen.go" }

// validateHeartbeat 是 heartbeats.<entity> 的静态检查；实体本身是否存在由 add 检查。
func (m Manifest) validateHeartbeat(entityName string) error {
	spec := m.Heartbeats[entityName]
	var joined error
	if !validName(entityName) || toSnake(entityName) != entityName {
		joined = errors.Join(joined, fmt.Errorf("heartbeats: invalid entity name %q (snake_case)", entityName))
	}
	if spec.ID <= 0 {
		joined = errors.Join(joined, fmt.Errorf("heartbeats.%s.id must be a positive component type id", entityName))
	}
	if spec.Behavior != nil && (spec.Behavior.ID <= 0 || spec.Behavior.ID == spec.ID) {
		joined = errors.Join(joined, fmt.Errorf("heartbeats.%s.behavior.id must be a positive component type id other than heartbeats.%s.id", entityName, entityName))
	}
	service, exists := m.Services[spec.Service]
	switch {
	case !exists:
		joined = errors.Join(joined, fmt.Errorf("heartbeats.%s.service %q is not a service of this project", entityName, spec.Service))
	case service.Framework != "":
		joined = errors.Join(joined, fmt.Errorf("heartbeats.%s.service %q hosts a framework service; heartbeats run where the entity's Nest runs", entityName, spec.Service))
	default:
		if resolved, err := resolveMods(effectiveServiceMods(m, spec.Service)); err == nil && !contains(resolved, "heartbeat") {
			joined = errors.Join(joined, fmt.Errorf("heartbeats.%s.service %q has no heartbeat mod; the heartbeat driver is installed there (roost add mod heartbeat -service %s)", entityName, spec.Service, spec.Service))
		}
	}
	for _, feature := range []string{"entity", "nest"} {
		if !contains(m.Features, feature) {
			joined = errors.Join(joined, fmt.Errorf("heartbeats.%s needs the %s feature", entityName, feature))
		}
	}
	return joined
}

// validateTimeDomain 是 time_domains.<entity> 的静态检查。
func (m Manifest) validateTimeDomain(entityName string) error {
	spec := m.TimeDomains[entityName]
	var joined error
	if !validName(entityName) || toSnake(entityName) != entityName {
		joined = errors.Join(joined, fmt.Errorf("time_domains: invalid entity name %q (snake_case)", entityName))
	}
	if spec.ID <= 0 {
		joined = errors.Join(joined, fmt.Errorf("time_domains.%s.id must be a positive component type id", entityName))
	}
	if spec.Dao == "" {
		joined = errors.Join(joined, fmt.Errorf("time_domains.%s.dao names the DAO type that stores the domain fields", entityName))
	}
	for _, feature := range []string{"entity", "dao"} {
		if !contains(m.Features, feature) {
			joined = errors.Join(joined, fmt.Errorf("time_domains.%s needs the %s feature", entityName, feature))
		}
	}
	return joined
}

// manifestComponentIDs 是 roost.yaml 已经登记的组件类型号（生成文件可能还没写出来）。
func manifestComponentIDs(m Manifest) map[int64]string {
	used := map[int64]string{}
	for name, spec := range m.Timers {
		used[spec.ID] = "timers." + name
	}
	for name, spec := range m.Heartbeats {
		used[spec.ID] = "heartbeats." + name
		if spec.Behavior != nil {
			used[spec.Behavior.ID] = "heartbeats." + name + ".behavior"
		}
	}
	for name, spec := range m.TimeDomains {
		used[spec.ID] = "time_domains." + name
	}
	return used
}

// allocateComponentIDs 分配 n 个组件类型号：explicit 非零时它是第一个（校验范围与占用），其余从段内最小的空号起。
func allocateComponentIDs(root string, m Manifest, n int, explicit int64) ([]int64, error) {
	uses, err := ScanIDs(root)
	if err != nil {
		return nil, err
	}
	used := manifestComponentIDs(m)
	for _, use := range uses {
		if use.Kind == "component" {
			used[use.ID] = use.File
		}
	}
	var out []int64
	if explicit != 0 {
		if err := validateExplicitID(m, "component", "", explicit); err != nil {
			return nil, err
		}
		if owner, taken := used[explicit]; taken {
			return nil, fmt.Errorf("component id %d is already used by %s", explicit, owner)
		}
		used[explicit] = "this command"
		out = append(out, explicit)
	}
	space := m.IDs["component"]
	for id := space.Min; id <= space.Max && len(out) < n; id++ {
		if _, taken := used[id]; !taken && id > 0 {
			used[id] = "this command"
			out = append(out, id)
		}
	}
	if len(out) < n {
		return nil, fmt.Errorf("id space component [%d, %d] has no free id left", space.Min, space.Max)
	}
	return out, nil
}

// requestedEntity 取 add 命令的实体：--entity 或位置参数，两者都给时必须一致。
func requestedEntity(command string, options AddOptions) (string, error) {
	requested := strings.TrimSpace(options.Entity)
	if requested == "" {
		requested = strings.TrimSpace(options.Name)
	} else if name := strings.TrimSpace(options.Name); name != "" && toSnake(name) != toSnake(requested) {
		return "", fmt.Errorf("roost add %s takes the entity once: got %q and --entity %q", command, name, requested)
	}
	if requested == "" {
		return "", fmt.Errorf("roost add %s needs the entity: roost add %s --entity <Entity>", command, command)
	}
	return requested, nil
}

// componentSlotFree 拒绝实体上已有的同名组件（类型常量已声明、或组件字段已在）。
func componentSlotFree(entityDir string, host entity.TimerHost, entityType, compType string) error {
	declared, err := packageDeclaresConst(entityDir, "CompType"+compType)
	if err != nil {
		return err
	}
	if declared || contains(host.Components, "CompType"+compType) {
		return fmt.Errorf("entity %s already has a component %s (CompType%s)", entityType, compType, compType)
	}
	return nil
}

func rejectExistingFiles(root string, rels ...string) error {
	for _, rel := range rels {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			return fmt.Errorf("%s already exists; remove it first (it is created by this command)", rel)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// rejectExistingHandlerNames 拒绝与 game/handler 里已有 Nest handler 同名的示例 handler。
func rejectExistingHandlerNames(root string, names ...string) error {
	entries, err := os.ReadDir(filepath.Join(root, "game", "handler"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, "game", "handler", entry.Name()))
		if err != nil {
			return err
		}
		for _, name := range names {
			if regexp.MustCompile(`(?m)^func handler` + name + `\(`).Match(raw) {
				return fmt.Errorf("Nest handler handler%s already exists in game/handler/%s; rename it before adding the behavior", name, entry.Name())
			}
		}
	}
	return nil
}

// addHeartbeat 实现 roost add heartbeat 与 roost add behavior（withBehavior）。先改实体（加组件字段），再写
// roost.yaml（条目 + 服务的 heartbeat Mod）并 project sync 渲染生成部分；sync 失败时实体文件一并还原。
func addHeartbeat(root string, m Manifest, options AddOptions, withBehavior bool) ([]string, error) {
	command := "heartbeat"
	if withBehavior {
		command = "behavior"
	}
	requested, err := requestedEntity(command, options)
	if err != nil {
		return nil, err
	}
	if !withBehavior && (options.AI || options.State) {
		return nil, errors.New("--ai and --state are options of roost add behavior")
	}
	for _, feature := range []string{"entity", "nest"} {
		if !hasFeature(m, feature) {
			return nil, fmt.Errorf("roost add %s needs the %s feature in roost.yaml", command, feature)
		}
	}
	entityName, entityPath, err := resolveEntitySource(root, requested)
	if err != nil {
		return nil, err
	}
	names := newHeartbeatNames(m, entityName)
	existing, hasHost := m.Heartbeats[entityName]
	switch {
	case hasHost && !withBehavior:
		return nil, fmt.Errorf("entity %s already has a heartbeat host (roost.yaml heartbeats.%s); register more beats in %s", names.EntityType, entityName, names.heartbeatBeatsFile())
	case hasHost && existing.Behavior != nil:
		return nil, fmt.Errorf("entity %s already has a behavior component (roost.yaml heartbeats.%s.behavior); add actions to %s", names.EntityType, entityName, names.behaviorActionsFile())
	}
	service := existing.Service
	if !hasHost || options.Service != "" {
		picked, err := nestService(m, options.Service, "roost add "+command, "the heartbeat driver delivers beats through Nest")
		if err != nil {
			return nil, err
		}
		if hasHost && picked != existing.Service {
			return nil, fmt.Errorf("entity %s's heartbeat host is on service %s (roost.yaml heartbeats.%s.service), not %s", names.EntityType, existing.Service, entityName, picked)
		}
		service = picked
	}
	entityDir := filepath.Dir(entityPath)
	host, err := entity.InspectComponentHost(entityDir, names.EntityType)
	if err != nil {
		return nil, err
	}
	var files []string
	count := 0
	if !hasHost {
		if err := componentSlotFree(entityDir, host, names.EntityType, "Heartbeat"); err != nil {
			return nil, err
		}
		files = append(files, names.heartbeatComponentFile(), names.heartbeatBeatsFile())
		count++
	}
	if withBehavior {
		if err := componentSlotFree(entityDir, host, names.EntityType, "Behavior"); err != nil {
			return nil, err
		}
		files = append(files, names.behaviorComponentFile(), names.behaviorActionsFile(), names.behaviorHandlerFile())
		handlers := []string{names.startHandler()}
		if options.AI {
			files = append(files, names.behaviorStrategyFile())
			handlers = append(handlers, names.strategyHandler())
		}
		if options.State {
			files = append(files, names.statesFile())
		}
		if err := rejectExistingHandlerNames(root, handlers...); err != nil {
			return nil, err
		}
		count++
	}
	if err := rejectExistingFiles(root, files...); err != nil {
		return nil, err
	}
	// --id 是本命令主组件的号：add heartbeat 给心跳宿主，add behavior 给行为组件（宿主随后取空号）。
	ids, err := allocateComponentIDs(root, m, count, options.ID)
	if err != nil {
		return nil, err
	}
	spec := existing
	spec.Service = service
	if withBehavior {
		behaviorID := ids[0]
		if !hasHost {
			// 自动分配时宿主取小号：组件按类型号初始化，宿主的 onHeartbeatInit 先于行为组件运行。
			spec.ID = ids[1]
			if options.ID == 0 {
				spec.ID, behaviorID = ids[0], ids[1]
			}
		}
		spec.Behavior = &BehaviorSpec{ID: behaviorID, AI: options.AI, State: options.State}
	} else {
		spec.ID = ids[0]
	}

	entityBefore, err := os.ReadFile(entityPath)
	if err != nil {
		return nil, err
	}
	entityAfter := entityBefore
	if !hasHost {
		if entityAfter, _, err = wireComponentField(entityAfter, entityPath, names.EntityType, "heartbeat", "Heartbeat"); err != nil {
			return nil, err
		}
	}
	if withBehavior {
		if entityAfter, _, err = wireComponentField(entityAfter, entityPath, names.EntityType, "behavior", "Behavior"); err != nil {
			return nil, err
		}
	}
	entityRel := relativeSlash(root, entityPath)
	backups, err := captureFiles(root, []string{entityRel})
	if err != nil {
		return nil, err
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	if err := commitSyncChanges([]syncChange{{rel: entityRel, path: entityPath, before: entityBefore, body: entityAfter, existed: true}}); err != nil {
		return nil, err
	}
	edited, err := captureFiles(root, []string{entityRel})
	if err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	serviceSpec := m.Services[service]
	previous := manifestWithService(m, service, ServiceSpec{Framework: serviceSpec.Framework, PortBlock: serviceSpec.PortBlock, Mods: append([]string(nil), serviceSpec.Mods...), Uses: serviceSpec.Uses, Rpcs: serviceSpec.Rpcs, UsesRpcs: serviceSpec.UsesRpcs})
	addedMod := false
	if resolved, err := resolveMods(effectiveServiceMods(m, service)); err == nil && !contains(resolved, "heartbeat") {
		serviceSpec.Mods = uniqueSorted(append(append([]string(nil), serviceSpec.Mods...), "heartbeat"))
		m.Services = maps.Clone(m.Services)
		m.Services[service] = serviceSpec
		addedMod = true
	}
	m.Heartbeats = maps.Clone(m.Heartbeats)
	if m.Heartbeats == nil {
		m.Heartbeats = map[string]HeartbeatSpec{}
	}
	m.Heartbeats[entityName] = spec
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return nil, restoreFilesIfCurrent(root, backups, edited, err)
	}
	paths := []string{"roost.yaml", entityRel}
	if addedMod {
		paths = append(paths, heartbeatDriverFile())
	}
	paths = append(paths, files...)
	paths = append(paths, "internal/bootstrap/generated.go")
	if addedMod {
		configs, err := appendModConfigSections(root, previous, m, service, options.Warnings)
		if err != nil {
			return paths, err
		}
		paths = append(paths, configs...)
	}
	return paths, nil
}

// addTimeDomain 实现 roost add timedomain：DAO 定义加三个 nopersist,sync 字段、实体加组件字段，再写 roost.yaml 并
// project sync。
func addTimeDomain(root string, m Manifest, options AddOptions) ([]string, error) {
	requested, err := requestedEntity("timedomain", options)
	if err != nil {
		return nil, err
	}
	for _, feature := range []string{"entity", "dao"} {
		if !hasFeature(m, feature) {
			return nil, fmt.Errorf("roost add timedomain needs the %s feature in roost.yaml", feature)
		}
	}
	entityName, entityPath, err := resolveEntitySource(root, requested)
	if err != nil {
		return nil, err
	}
	names := newHeartbeatNames(m, entityName)
	if _, ok := m.TimeDomains[entityName]; ok {
		return nil, fmt.Errorf("entity %s already has a time domain (roost.yaml time_domains.%s)", names.EntityType, entityName)
	}
	entityDir := filepath.Dir(entityPath)
	host, err := entity.InspectComponentHost(entityDir, names.EntityType)
	if err != nil {
		return nil, err
	}
	dao, err := hostMainDao(entityName, host, "the time domain fields are stored in it")
	if err != nil {
		return nil, err
	}
	if err := componentSlotFree(entityDir, host, names.EntityType, "TimeDomain"); err != nil {
		return nil, err
	}
	if err := rejectExistingFiles(root, names.timeDomainComponentFile(), names.timeDomainConfigFile()); err != nil {
		return nil, err
	}
	ids, err := allocateComponentIDs(root, m, 1, options.ID)
	if err != nil {
		return nil, err
	}
	daoPath, daoBefore, daoAfter, err := addTimeDomainDaoFields(root, dao)
	if err != nil {
		return nil, err
	}
	entityBefore, err := os.ReadFile(entityPath)
	if err != nil {
		return nil, err
	}
	entityAfter, _, err := wireComponentField(entityBefore, entityPath, names.EntityType, "timeDomain", "TimeDomain")
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
	edits := []syncChange{{rel: entityRel, path: entityPath, before: entityBefore, body: entityAfter, existed: true}}
	if !bytes.Equal(daoBefore, daoAfter) {
		edits = append(edits, syncChange{rel: daoRel, path: daoPath, before: daoBefore, body: daoAfter, existed: true})
	}
	if err := commitSyncChanges(edits); err != nil {
		return nil, err
	}
	edited, err := captureFiles(root, []string{daoRel, entityRel})
	if err != nil {
		return nil, restoreFiles(root, backups, err)
	}
	m.TimeDomains = maps.Clone(m.TimeDomains)
	if m.TimeDomains == nil {
		m.TimeDomains = map[string]TimeDomainSpec{}
	}
	m.TimeDomains[entityName] = TimeDomainSpec{ID: ids[0], Dao: dao}
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return nil, restoreFilesIfCurrent(root, backups, edited, err)
	}
	return []string{"roost.yaml", daoRel, entityRel, names.timeDomainComponentFile(), names.timeDomainConfigFile()}, nil
}

// hostMainDao 选实体的主 DAO：唯一的 DAO，或几个里面字段名为 dao 的那个。
func hostMainDao(entityName string, host entity.TimerHost, why string) (string, error) {
	pick := func(dao entity.DaoField) string { return lastSegment(strings.TrimPrefix(dao.TypeName, "*")) }
	switch len(host.Daos) {
	case 0:
		return "", fmt.Errorf("entity %s has no DAO; %s. Run: roost add dao %s --entity %s", toPascal(entityName), why, toPascal(entityName), toPascal(entityName))
	case 1:
		return pick(host.Daos[0]), nil
	}
	for _, dao := range host.Daos {
		if dao.FieldName == "dao" {
			return pick(dao), nil
		}
	}
	return "", fmt.Errorf("entity %s has %d DAOs and none is its main DAO (field dao); %s", toPascal(entityName), len(host.Daos), why)
}

// timeDomainFields 是时间域在 DAO 里的三个字段（名字 / 类型 / bson 键）。nopersist,sync：随同步下发给客户端、不落库
// （随进程结束的副本场景；要持久的场景把 nopersist 改成 persist，见 guide §4D）。
var timeDomainFields = []struct{ name, typ, key string }{
	{"TimeAnchorBusiness", "int64", "time_anchor_business"},
	{"TimeAnchorDomain", "int64", "time_anchor_domain"},
	{"TimeRate", "float64", "time_rate"},
}

// addTimeDomainDaoFields 在 DAO 定义里加三个字段（文本插在结构体右括号前）。三个都已按同样类型存在时不改；
// 只有一部分、或类型不同时拒绝。
func addTimeDomainDaoFields(root, daoType string) (string, []byte, []byte, error) {
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
		structure := findStruct(file, daoType)
		if structure == nil {
			continue
		}
		found := 0
		for _, field := range structure.Fields.List {
			for _, ident := range field.Names {
				for _, want := range timeDomainFields {
					if ident.Name != want.name {
						continue
					}
					if got := exprString(fset, field.Type); got != want.typ {
						return "", nil, nil, fmt.Errorf("%s: %s.%s already exists as %s; the time domain needs it as %s", relativeSlash(root, path), daoType, ident.Name, got, want.typ)
					}
					found++
				}
			}
		}
		if found == len(timeDomainFields) {
			return path, raw, raw, nil
		}
		if found != 0 {
			return "", nil, nil, fmt.Errorf("%s: %s has only some of TimeAnchorBusiness / TimeAnchorDomain / TimeRate; add all three or none", relativeSlash(root, path), daoType)
		}
		var text strings.Builder
		text.WriteString("\n\t// 场景时间域（roost add timedomain 加入，gameplay/timedomain.State）：域时间 = AnchorDomain + (业务时间 − AnchorBusiness)\n")
		text.WriteString("\t// × Rate。随同步下发给客户端外推域时间；nopersist：随进程结束的副本不落库，要持久的场景改成 persist。\n")
		for _, field := range timeDomainFields {
			fmt.Fprintf(&text, "\t%s %s `bson:\"%s\" dao:\"nopersist,sync\"`\n", field.name, field.typ, field.key)
		}
		closing := fset.Position(structure.Fields.Closing).Offset
		edited := append(append(append([]byte(nil), raw[:closing]...), []byte(text.String())...), raw[closing:]...)
		formatted, err := format.Source(edited)
		if err != nil {
			return "", nil, nil, fmt.Errorf("format %s: %w", relativeSlash(root, path), err)
		}
		return path, raw, formatted, nil
	}
	return "", nil, nil, fmt.Errorf("DAO %s is not defined in db/def; the time domain fields are stored in it", daoType)
}

// renderHeartbeatFiles 是心跳驱动、各实体的心跳宿主 / 行为组件 / 时间域的生成部分与业务文件（project sync 调用）。
func renderHeartbeatFiles(m Manifest, addGo func(path, body string, owned bool) error) error {
	type file struct {
		path, body string
		owned      bool
	}
	var out []file
	if contains(allProjectMods(m), "heartbeat") {
		out = append(out, file{heartbeatDriverFile(), renderHeartbeatDriver(m), true})
	}
	for _, name := range slices.Sorted(maps.Keys(m.Heartbeats)) {
		spec := m.Heartbeats[name]
		n := newHeartbeatNames(m, name)
		out = append(out,
			file{n.heartbeatComponentFile(), renderHeartbeatComponent(n, spec), true},
			file{n.heartbeatBeatsFile(), renderHeartbeatBeats(n), false})
		if b := spec.Behavior; b != nil {
			out = append(out,
				file{n.behaviorComponentFile(), renderBehaviorComponent(n, *b), true},
				file{n.behaviorActionsFile(), renderBehaviorActions(n, *b), false},
				file{n.behaviorHandlerFile(), renderBehaviorHandlers(n, *b), false})
			if b.AI {
				out = append(out, file{n.behaviorStrategyFile(), renderBehaviorStrategy(n), false})
			}
			if b.State {
				out = append(out, file{n.statesFile(), renderStates(n), false})
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(m.TimeDomains)) {
		spec := m.TimeDomains[name]
		n := newHeartbeatNames(m, name)
		out = append(out,
			file{n.timeDomainComponentFile(), renderTimeDomainComponent(n, spec), true},
			file{n.timeDomainConfigFile(), renderTimeDomainConfig(n), false})
	}
	for _, f := range out {
		if err := addGo(f.path, f.body, f.owned); err != nil {
			return err
		}
	}
	return nil
}

// printHeartbeatNext 是 add heartbeat / behavior / timedomain 之后的提示。服务的 Init / Shutdown 不用改：心跳驱动由
// bootstrap 里的 heartbeat Mod 在 Nest 之后启动、之前停止，组件随实体加载 / 卸载自己登记、离开。
func printHeartbeatNext(stdout io.Writer, kind string, withAI bool) {
	fmt.Fprintln(stdout, "next: roost generate (the entity generator adds the component getters, the nest generator the example handler's Sender)")
	switch kind {
	case "heartbeat":
		fmt.Fprintln(stdout, "next: register the entity's own beats in game/entities/<entity>/heartbeat_beats.go (onHeartbeatInit); start / stop them in Nest handlers with <Entity>.HeartbeatComp().Beat(name)")
		fmt.Fprintln(stdout, "init/shutdown: nothing to add; the heartbeat Mod in the generated bootstrap starts the driver after Nest and stops it before Nest")
		fmt.Fprintln(stdout, "guide: "+heartbeatDocURL)
	case "behavior":
		fmt.Fprintln(stdout, "next: define the action kinds and actions in game/entities/<entity>/behavior_actions.go; start them in a Nest handler as its last step (game/handler/<entity>_behavior.go has an example)")
		if withAI {
			fmt.Fprintln(stdout, "next: set a strategy (behavior_strategy.go has an example) in onBehaviorReady or a Nest handler; without one the AI heartbeat does not run")
		}
		fmt.Fprintln(stdout, "init/shutdown: nothing to add; the heartbeat Mod in the generated bootstrap starts the driver after Nest and stops it before Nest")
		fmt.Fprintln(stdout, "guide: "+behaviorDocURL)
	case "timedomain":
		fmt.Fprintln(stdout, "next: change the rate in the scene's own Nest handlers with <Scene>.TimeDomainComp().Domain().SetRate(r); members read Domain().Now() and pass the domain to their behavior (behavior_actions.go timeDomain)")
		fmt.Fprintln(stdout, "init/shutdown: nothing to add; the domain is built when the scene loads")
		fmt.Fprintln(stdout, "guide: "+timeDomainDocURL)
	}
}
