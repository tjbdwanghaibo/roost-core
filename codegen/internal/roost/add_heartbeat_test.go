package roost

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newHeartbeatProject 是带 Nest、有 Monster（有 DAO）与 Scene（有 DAO）两个实体的最小工程。
func newHeartbeatProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root, Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}}); err != nil {
		t.Fatal(err)
	}
	for _, options := range []AddOptions{
		{Kind: "entity", Name: "Monster"},
		{Kind: "dao", Name: "Monster", Entity: "Monster"},
		{Kind: "entity", Name: "Scene"},
		{Kind: "dao", Name: "Scene", Entity: "Scene"},
	} {
		if _, err := Add(root, options); err != nil {
			t.Fatalf("add %s %s: %v", options.Kind, options.Name, err)
		}
	}
	return root
}

func editProjectFile(t *testing.T, root, rel, old, replacement string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(old)) {
		t.Fatalf("%s does not contain %q", rel, old)
	}
	if err := os.WriteFile(path, bytes.Replace(raw, []byte(old), []byte(replacement), 1), 0o644); err != nil {
		t.Fatal(err)
	}
}

// roost add behavior --ai --state 的生成物形状与归属：组件与驱动带生成头、project sync 重写；动作 / 策略 / 状态 /
// 示例 handler / 心跳登记是业务文件，sync 不改、缺失时重建；roost.yaml 记下组件号与选项，服务装上 heartbeat Mod，
// bootstrap 装驱动。
func TestAddBehaviorGeneratesOwnedPartsAndBusinessFiles(t *testing.T) {
	t.Parallel()
	root := newHeartbeatProject(t)
	paths, err := Add(root, AddOptions{Kind: "behavior", Entity: "Monster", AI: true, State: true, Warnings: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"roost.yaml", "game/entities/monster/entity.go", "game/heartbeat/driver_gen.go",
		"game/entities/monster/heartbeat_component_gen.go", "game/entities/monster/heartbeat_beats.go",
		"game/entities/monster/behavior_component_gen.go", "game/entities/monster/behavior_actions.go",
		"game/handler/monster_behavior.go", "game/entities/monster/behavior_strategy.go", "game/entities/monster/states.go",
		"internal/bootstrap/generated.go",
		// 服务新装了 heartbeat Mod：已有的服务配置补上它的配置段（heartbeat.delivery，缺省 dispatch）。
		"configs/service/config.game.yaml", "configs/service/config.game.prod.example.yaml", "deploy/k8s/base/secret.game.example.yaml",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v\nwant %v", paths, want)
	}
	for _, rel := range []string{"game/heartbeat/driver_gen.go", "game/entities/monster/heartbeat_component_gen.go", "game/entities/monster/behavior_component_gen.go"} {
		if body := readProjectFile(t, root, rel); !strings.HasPrefix(body, generatedHeader) {
			t.Fatalf("%s is generator-owned but has no generated header:\n%s", rel, body)
		}
	}
	business := []string{"game/entities/monster/heartbeat_beats.go", "game/entities/monster/behavior_actions.go", "game/entities/monster/behavior_strategy.go", "game/entities/monster/states.go", "game/handler/monster_behavior.go"}
	for _, rel := range business {
		if body := readProjectFile(t, root, rel); hasGeneratedHeader("x.go", []byte(body)) {
			t.Fatalf("%s is a business file but carries the generated header", rel)
		}
	}
	for rel, fragments := range map[string][]string{
		"game/heartbeat/driver_gen.go": {`var Driver = coreheartbeat.NewDriver(coreheartbeat.Config{Name: "planet"})`},
		"game/entities/monster/heartbeat_component_gen.go": {
			"//roost:component type=2000", "host: gameheartbeat.Driver.NewHost(typed)", "func (component *HeartbeatComponent) OnDestroy(entity.EntityDestroyReason) {\n\tcomponent.host.OnDestroy()",
			"return component.onHeartbeatInit(isCreate)", "func (component *HeartbeatComponent) Beat(name string) *heartbeat.Reg",
		},
		"game/entities/monster/behavior_component_gen.go": {
			"//roost:component type=2001", "ai.NewBeatController(", "ai.BeatConfig{Host: host, TimeDomain: domain}",
			"Beat:         &actionflow.BeatConfig{Host: host, TimeDomain: domain},", "Observer:     component.ai,",
			"Registry:     behaviorRegistry,", "return component.onBehaviorReady(isCreate)",
			"func (component *BehaviorComponent) OnDestroy(entity.EntityDestroyReason) {\n\tif component.ai != nil {\n\t\tcomponent.ai.Close(\"owner destroyed\")",
			"component.behavior.OnDestroy()",
		},
		"game/entities/monster/behavior_actions.go":  {"ActionExample actionflow.ActionKind = 1", "func NewExampleAction(param any) (actionflow.Action, error)", "a.waited += ctx.Elapsed", "func (component *BehaviorComponent) timeDomain() *timedomain.Domain"},
		"game/entities/monster/behavior_strategy.go": {"type ExampleStrategy struct", "ctx.ActionList.CreateAction(ActionExample"},
		"game/entities/monster/states.go":            {"StateExample state.ID = 1", "state.NewCatalog(defs...)"},
		"game/handler/monster_behavior.go": {
			"//roost:nest rollback=undo\nfunc handlerStartExampleMonsterAction(target monster.IBehaviorEntity, millis int64) (int64, error)",
			"func handlerStartExampleMonsterStrategy(target monster.IBehaviorEntity) (string, error)",
		},
		"game/entities/monster/entity.go":               {"heartbeat *HeartbeatComponent `comp:\"CompTypeHeartbeat\"`", "behavior  *BehaviorComponent  `comp:\"CompTypeBehavior\"`"},
		"roost.yaml":                                    {"heartbeats:\n    monster:\n        id: 2000\n        service: game\n        behavior:\n            id: 2001\n            ai: true\n            state: true\n", "            - heartbeat\n"},
		"configs/service/config.game.yaml":              {"\nheartbeat:\n", "  delivery: dispatch\n"},
		"configs/service/config.game.prod.example.yaml": {"\nheartbeat:\n", "  delivery: dispatch\n"},
		"internal/bootstrap/generated.go":               {"kitheartbeat.NewMod(EntityAccess, gameheartbeat.Driver)", `gameheartbeat "example.com/planet/game/heartbeat"`, `kitheartbeat "github.com/tjbdwanghaibo/roost-core/wiring/heartbeat"`},
	} {
		body := readProjectFile(t, root, rel)
		for _, fragment := range fragments {
			if !strings.Contains(body, fragment) {
				t.Fatalf("%s misses %q:\n%s", rel, fragment, body)
			}
		}
	}

	// project sync：生成部分被改坏了就重写回来；业务文件里的改动保留，删掉的业务文件按骨架重建。
	component := readProjectFile(t, root, "game/entities/monster/behavior_component_gen.go")
	if err := os.WriteFile(filepath.Join(root, "game", "entities", "monster", "behavior_component_gen.go"), []byte(generatedHeader+"\npackage monster\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	actions := readProjectFile(t, root, "game/entities/monster/behavior_actions.go")
	editProjectFile(t, root, "game/entities/monster/behavior_actions.go", "ActionExample actionflow.ActionKind = 1", "ActionExample actionflow.ActionKind = 1\n\tActionAttack  actionflow.ActionKind = 2")
	edited := readProjectFile(t, root, "game/entities/monster/behavior_actions.go")
	strategy := readProjectFile(t, root, "game/entities/monster/behavior_strategy.go")
	if err := os.Remove(filepath.Join(root, "game", "entities", "monster", "behavior_strategy.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "game/entities/monster/behavior_component_gen.go"); got != component {
		t.Fatalf("project sync did not restore the generated behavior component:\n%s", got)
	}
	if got := readProjectFile(t, root, "game/entities/monster/behavior_actions.go"); got != edited || got == actions {
		t.Fatal("project sync rewrote the business actions file")
	}
	if got := readProjectFile(t, root, "game/entities/monster/behavior_strategy.go"); got != strategy {
		t.Fatal("a missing strategy file was not recreated from the scaffold")
	}
}

// 不带 --ai / --state：没有 AI 字段与导入，不生成策略、状态与策略 handler。
func TestAddBehaviorWithoutOptions(t *testing.T) {
	t.Parallel()
	root := newHeartbeatProject(t)
	if _, err := Add(root, AddOptions{Kind: "behavior", Entity: "Monster", Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	component := readProjectFile(t, root, "game/entities/monster/behavior_component_gen.go")
	if strings.Contains(component, "gameplay/ai") || strings.Contains(component, "Observer") {
		t.Fatalf("behavior without --ai builds an AI:\n%s", component)
	}
	for _, rel := range []string{"game/entities/monster/behavior_strategy.go", "game/entities/monster/states.go"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("%s generated without its option (err %v)", rel, err)
		}
	}
	if handlers := readProjectFile(t, root, "game/handler/monster_behavior.go"); strings.Contains(handlers, "Strategy") {
		t.Fatalf("strategy handler without --ai:\n%s", handlers)
	}
	if manifest := readProjectFile(t, root, "roost.yaml"); !strings.Contains(manifest, "        behavior:\n            id: 2001\n") || strings.Contains(manifest, "ai: true") {
		t.Fatalf("roost.yaml:\n%s", manifest)
	}
}

// 先 add heartbeat，再 add behavior：第二次只加行为组件（不再加宿主、不重复加 Mod）；--id 给本命令的主组件。
// add timedomain 在 DAO 定义里加三个 nopersist,sync 字段、实体加组件字段。
func TestAddHeartbeatThenBehaviorAndTimeDomain(t *testing.T) {
	t.Parallel()
	root := newHeartbeatProject(t)
	paths, err := Add(root, AddOptions{Kind: "heartbeat", Entity: "Scene", ID: 2042, Warnings: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(paths, "game/heartbeat/driver_gen.go") || contains(paths, "game/entities/scene/behavior_component_gen.go") {
		t.Fatalf("add heartbeat paths = %v", paths)
	}
	if got := readProjectFile(t, root, "game/entities/scene/heartbeat_component_gen.go"); !strings.Contains(got, "//roost:component type=2042") {
		t.Fatalf("explicit id not used:\n%s", got)
	}
	paths, err = Add(root, AddOptions{Kind: "behavior", Entity: "Scene", Warnings: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if contains(paths, "game/heartbeat/driver_gen.go") || contains(paths, "game/entities/scene/heartbeat_component_gen.go") {
		t.Fatalf("second add re-created the host: %v", paths)
	}
	if config := readProjectFile(t, root, "configs/service/config.game.yaml"); strings.Count(config, "\nheartbeat:\n") != 1 {
		t.Fatalf("the heartbeat config section is not there exactly once:\n%s", config)
	}
	manifest := readProjectFile(t, root, "roost.yaml")
	if !strings.Contains(manifest, "    scene:\n        id: 2042\n        service: game\n        behavior:\n            id: 2000\n") || strings.Count(manifest, "- heartbeat") != 1 {
		t.Fatalf("roost.yaml:\n%s", manifest)
	}
	if _, err := Add(root, AddOptions{Kind: "timedomain", Entity: "Scene", Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	dao := readProjectFile(t, root, "db/def/scene.go")
	for _, field := range []string{
		"TimeAnchorBusiness int64   `bson:\"time_anchor_business\" dao:\"nopersist,sync\"`",
		"TimeAnchorDomain   int64   `bson:\"time_anchor_domain\" dao:\"nopersist,sync\"`",
		"TimeRate           float64 `bson:\"time_rate\" dao:\"nopersist,sync\"`",
	} {
		if !strings.Contains(dao, field) {
			t.Fatalf("DAO definition misses %q:\n%s", field, dao)
		}
	}
	component := readProjectFile(t, root, "game/entities/scene/timedomain_component_gen.go")
	for _, fragment := range []string{"//roost:component type=2001", "timedomain.New(sceneTimeDomainFields{owner: component.owner}, timeDomainConfig())", "dao.SetTimeRate(s.Rate)", "f.owner.Dao()"} {
		if !strings.Contains(component, fragment) {
			t.Fatalf("time domain component misses %q:\n%s", fragment, component)
		}
	}
	if entityGo := readProjectFile(t, root, "game/entities/scene/entity.go"); !strings.Contains(entityGo, "timeDomain *TimeDomainComponent `comp:\"CompTypeTimeDomain\"`") {
		t.Fatalf("entity.go:\n%s", entityGo)
	}
	if manifest := readProjectFile(t, root, "roost.yaml"); !strings.Contains(manifest, "time_domains:\n    scene:\n        id: 2001\n        dao: SceneDao\n") {
		t.Fatalf("roost.yaml:\n%s", manifest)
	}
}

// 拒绝的输入在改任何文件之前报错，工程逐字节不变。
func TestAddHeartbeatRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()
	root := newHeartbeatProject(t)
	for _, options := range []AddOptions{
		{Kind: "entity", Name: "Npc"},
		{Kind: "entity", Name: "Hero"},
		{Kind: "component", Name: "Behavior", Entity: "Hero"},
		{Kind: "entity", Name: "Boss"},
		{Kind: "dao", Name: "Boss", Entity: "Boss"},
		{Kind: "entity", Name: "Arena"},
		{Kind: "dao", Name: "Arena", Entity: "Arena"},
	} {
		if _, err := Add(root, options); err != nil {
			t.Fatalf("add %s %s: %v", options.Kind, options.Name, err)
		}
	}
	// Boss 的示例 handler 名已被占用；Arena 的 DAO 只有三个时间域字段之一。
	if err := os.WriteFile(filepath.Join(root, "game", "handler", "boss.go"), []byte("package handler\n\nfunc handlerStartExampleBossAction() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	editProjectFile(t, root, "db/def/arena.go", "type ArenaDao struct {", "type ArenaDao struct {\n\tTimeRate float64 `bson:\"time_rate\" dao:\"nopersist,sync\"`")
	snapshot, err := snapshotGenerated(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := readProjectFile(t, root, "roost.yaml")
	for _, tc := range []struct {
		name    string
		options AddOptions
		want    string
	}{
		{"no entity", AddOptions{Kind: "behavior"}, "needs the entity"},
		{"two entity names", AddOptions{Kind: "heartbeat", Name: "Npc", Entity: "Monster"}, "takes the entity once"},
		{"unknown entity", AddOptions{Kind: "behavior", Entity: "Ghost"}, `entity "ghost" does not exist`},
		{"ai on heartbeat", AddOptions{Kind: "heartbeat", Entity: "Monster", AI: true}, "options of roost add behavior"},
		{"component taken", AddOptions{Kind: "behavior", Entity: "Hero"}, "already has a component Behavior"},
		{"handler taken", AddOptions{Kind: "behavior", Entity: "Boss"}, "handlerStartExampleBossAction already exists"},
		{"unknown service", AddOptions{Kind: "heartbeat", Entity: "Monster", Service: "lobby"}, `unknown service "lobby"`},
		{"id out of range", AddOptions{Kind: "heartbeat", Entity: "Monster", ID: 9}, "outside"},
		{"time domain without DAO", AddOptions{Kind: "timedomain", Entity: "Npc"}, "has no DAO"},
		{"partial time domain fields", AddOptions{Kind: "timedomain", Entity: "Arena"}, "has only some of TimeAnchorBusiness"},
	} {
		if _, err := Add(root, tc.options); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	after, err := snapshotGenerated(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed := diffSnapshot(snapshot, after); len(changed) != 0 || readProjectFile(t, root, "roost.yaml") != manifest {
		t.Fatalf("a refused add changed files: %v", changed)
	}

	// 重复 add：宿主与行为组件各只有一个。
	if _, err := Add(root, AddOptions{Kind: "behavior", Entity: "Monster", Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "heartbeat", Entity: "Monster"}); err == nil || !strings.Contains(err.Error(), "already has a heartbeat host") {
		t.Fatalf("second heartbeat = %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "behavior", Entity: "Monster"}); err == nil || !strings.Contains(err.Error(), "already has a behavior component") {
		t.Fatalf("second behavior = %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "timedomain", Entity: "Scene", Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "timedomain", Entity: "Scene"}); err == nil || !strings.Contains(err.Error(), "already has a time domain") {
		t.Fatalf("second timedomain = %v", err)
	}
}

// roost.yaml 手改出的不一致在加载时报出；服务没有 heartbeat Mod 时拒绝。
func TestManifestRejectsAnInconsistentHeartbeatEntry(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata", "nest"}, []string{"entity", "nest", "dao"})
	m.Heartbeats = map[string]HeartbeatSpec{"Monster": {ID: 0, Service: "lobby", Behavior: &BehaviorSpec{ID: 0}}}
	m.TimeDomains = map[string]TimeDomainSpec{"scene": {ID: 0}}
	err := m.Validate()
	for _, want := range []string{
		`heartbeats: invalid entity name "Monster"`, "heartbeats.Monster.id must be a positive", "heartbeats.Monster.behavior.id must be a positive",
		`heartbeats.Monster.service "lobby" is not a service`, "time_domains.scene.id must be a positive", "time_domains.scene.dao names the DAO",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %v, want %q", err, want)
		}
	}
	m.Heartbeats = map[string]HeartbeatSpec{"monster": {ID: 2000, Service: "game"}}
	m.TimeDomains = nil
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), `heartbeats.monster.service "game" has no heartbeat mod`) {
		t.Fatalf("Validate() without the heartbeat mod = %v", err)
	}
	game := m.Services["game"]
	game.Mods = append(game.Mods, "heartbeat")
	m.Services["game"] = game
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate() with the heartbeat mod = %v", err)
	}
}

// roost add mod heartbeat 不需要条目：Mod 依赖 nest，生成驱动与 bootstrap 装配。
func TestHeartbeatModWithoutEntities(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root, Mods: []string{"configdata"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "heartbeat", Service: "game", Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go")
	nest, heartbeat := strings.Index(bootstrap, "kitnest.NewMod(EntityAccess)"), strings.Index(bootstrap, "kitheartbeat.NewMod(EntityAccess, gameheartbeat.Driver)")
	if nest < 0 || heartbeat < nest {
		t.Fatalf("bootstrap does not install nest and then heartbeat:\n%s", bootstrap)
	}
	if driver := readProjectFile(t, root, "game/heartbeat/driver_gen.go"); !strings.Contains(driver, "var Driver = coreheartbeat.NewDriver") {
		t.Fatalf("driver:\n%s", driver)
	}
}
