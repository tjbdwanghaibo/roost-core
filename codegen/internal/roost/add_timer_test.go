package roost

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTimerProject 是带 Nest、有 Player 实体与 PlayerDao 的最小工程（roost add timer 的前提）。
func newTimerProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root, Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}}); err != nil {
		t.Fatal(err)
	}
	for _, options := range []AddOptions{
		{Kind: "entity", Name: "Player"},
		{Kind: "dao", Name: "Player", Entity: "Player"},
	} {
		if _, err := Add(root, options); err != nil {
			t.Fatalf("add %s: %v", options.Kind, err)
		}
	}
	return root
}

// roost add timer 的生成物形状与归属：生成部分带生成头、project sync 重写；业务回调文件与改过的 DAO 定义、
// entity.go 归业务，sync 不改；roost.yaml 记下组件名、类型号、服务与 DAO；bootstrap 装上驱动。
func TestAddTimerGeneratesOwnedPartsAndBusinessCallbacks(t *testing.T) {
	t.Parallel()
	root := newTimerProject(t)
	paths, err := Add(root, AddOptions{Kind: "timer", Entity: "Player", Warnings: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"roost.yaml", "db/def/player.go", "game/entities/player/entity.go",
		"db/def/player_timer_node_gen.go", "game/entities/player/timer_component_gen.go",
		"game/entities/player/timer_handlers.go", "game/handler/tick_player_timer_gen.go",
		"game/handler/tick_player_timer_gen_test.go", "internal/bootstrap/generated.go",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v\nwant %v", paths, want)
	}
	owned := []string{"db/def/player_timer_node_gen.go", "game/entities/player/timer_component_gen.go", "game/handler/tick_player_timer_gen.go", "game/handler/tick_player_timer_gen_test.go"}
	for _, rel := range owned {
		if body := readProjectFile(t, root, rel); !strings.HasPrefix(body, generatedHeader) {
			t.Fatalf("%s is generator-owned but has no generated header:\n%s", rel, body)
		}
	}
	handlers := readProjectFile(t, root, "game/entities/player/timer_handlers.go")
	if hasGeneratedHeader("x.go", []byte(handlers)) || !strings.Contains(handlers, "func (component *TimerComponent) registerTimerHandlers(handlers *entitytimer.Handlers)") ||
		!strings.Contains(handlers, "fire.Overdue") {
		t.Fatalf("business callbacks file:\n%s", handlers)
	}
	component := readProjectFile(t, root, "game/entities/player/timer_component_gen.go")
	for _, fragment := range []string{
		"//roost:component type=2000", "const TimerTickHandler = \"handlerTickPlayerTimer\"",
		"var TimerDriver = entitytimer.NewDriver(entitytimer.Config{Name: \"player.timer\"",
		"TimerDriver.NewHost(typed, playerTimerStore{owner: typed}, component.registerTimerHandlers)",
		"func (component *TimerComponent) NewTimer(", "func (component *TimerComponent) NewTimerAt(", "func (component *TimerComponent) Tick() int",
		"dao.RangeTimers(", "Overdue",
	} {
		if !strings.Contains(component, fragment) {
			t.Fatalf("component misses %q:\n%s", fragment, component)
		}
	}
	tick := readProjectFile(t, root, "game/handler/tick_player_timer_gen.go")
	if !strings.Contains(tick, "//roost:nest rollback=undo durability=strict\nfunc handlerTickPlayerTimer(target player.ITimerEntity) (int32, error)") {
		t.Fatalf("tick handler:\n%s", tick)
	}
	dao := readProjectFile(t, root, "db/def/player.go")
	if !strings.Contains(dao, "Timers map[int64]*PlayerTimerNode `bson:\"timers\" dao:\"persist,map=fast\"`") || !strings.Contains(dao, "TimerSeed int64 `bson:\"timer_seed\" dao:\"persist\"`") {
		t.Fatalf("DAO definition:\n%s", dao)
	}
	if entityGo := readProjectFile(t, root, "game/entities/player/entity.go"); !strings.Contains(entityGo, "timer *TimerComponent `comp:\"CompTypeTimer\"`") {
		t.Fatalf("entity.go:\n%s", entityGo)
	}
	if manifest := readProjectFile(t, root, "roost.yaml"); !strings.Contains(manifest, "timers:\n    player:\n        component: timer\n        id: 2000\n        service: game\n        dao: PlayerDao\n") {
		t.Fatalf("roost.yaml:\n%s", manifest)
	}
	if bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go"); !strings.Contains(bootstrap, "kitentitytimer.NewMod(EntityAccess, player.TimerDriver)") ||
		!strings.Contains(bootstrap, `kitentitytimer "github.com/tjbdwanghaibo/roost-core/wiring/entitytimer"`) {
		t.Fatalf("bootstrap:\n%s", bootstrap)
	}

	// project sync：生成部分被改坏了就重写回来；业务回调文件里的改动保留。
	tickPath := filepath.Join(root, "game", "handler", "tick_player_timer_gen.go")
	if err := os.WriteFile(tickPath, []byte(generatedHeader+"\npackage handler\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handlersPath := filepath.Join(root, "game", "entities", "player", "timer_handlers.go")
	edited := strings.Replace(handlers, "TimerTypeExample int32 = 1", "TimerTypeBuffExpire int32 = 1\n\tTimerTypeExample int32 = 2", 1)
	if err := os.WriteFile(handlersPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "game/handler/tick_player_timer_gen.go"); got != tick {
		t.Fatalf("project sync did not restore the generated tick handler:\n%s", got)
	}
	if got := readProjectFile(t, root, "game/entities/player/timer_handlers.go"); got != edited {
		t.Fatalf("project sync rewrote the business callbacks file")
	}
	// 业务回调文件删掉之后 sync 重新创建一份骨架（只在缺失时创建）。
	if err := os.Remove(handlersPath); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "game/entities/player/timer_handlers.go"); got != handlers {
		t.Fatalf("a missing callbacks file was not recreated from the scaffold")
	}
}

// --component 改名：所有名字跟着组件走，类型号避开已用的。
func TestAddTimerComponentNameAndExplicitID(t *testing.T) {
	t.Parallel()
	root := newTimerProject(t)
	if _, err := Add(root, AddOptions{Kind: "timer", Entity: "Player", Component: "BuffClock", ID: 2042, Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	component := readProjectFile(t, root, "game/entities/player/buff_clock_component_gen.go")
	for _, fragment := range []string{"//roost:component type=2042", "type BuffClockComponent struct", "func (component *BuffClockComponent) NewBuffClock(", "var BuffClockDriver =", "Name: \"player.buff_clock\"", "dao.RangeBuffClocks("} {
		if !strings.Contains(component, fragment) {
			t.Fatalf("component misses %q", fragment)
		}
	}
	if dao := readProjectFile(t, root, "db/def/player.go"); !strings.Contains(dao, "BuffClocks map[int64]*PlayerBuffClockNode `bson:\"buff_clocks\"") || !strings.Contains(dao, "BuffClockSeed int64 `bson:\"buff_clock_seed\"") {
		t.Fatalf("DAO definition:\n%s", dao)
	}
	if _, err := os.Stat(filepath.Join(root, "game", "handler", "tick_player_buff_clock_gen.go")); err != nil {
		t.Fatal(err)
	}
}

// 拒绝的输入都在改任何文件之前报出，并说明原因与改法。
func TestAddTimerRefusesWhatItCannotSupport(t *testing.T) {
	t.Parallel()
	root := newTimerProject(t)
	for _, options := range []AddOptions{
		// Guild 改成 remote=managed：写它的 Nest 服务要有 Remote 运行时，否则 generate 拒绝（remoteRuntimeHosts）。
		{Kind: "mod", Name: "remote_entity", Service: "game", Warnings: io.Discard},
		{Kind: "entity", Name: "Npc"},
		{Kind: "entity", Name: "Guild"},
		{Kind: "dao", Name: "Guild", Entity: "Guild"},
		{Kind: "entity", Name: "Monster"},
		{Kind: "dao", Name: "Monster", Entity: "Monster"},
		{Kind: "entity", Name: "Hero"},
		{Kind: "dao", Name: "Hero", Entity: "Hero"},
		{Kind: "component", Name: "Timer", Entity: "Hero"},
	} {
		if _, err := Add(root, options); err != nil {
			t.Fatalf("add %s %s: %v", options.Kind, options.Name, err)
		}
	}
	editFile := func(rel, old, replacement string) {
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
	editFile("game/entities/guild/entity.go", "category=entity.EntityCategoryOther", "category=entity.EntityCategoryRemote remote=managed")
	editFile("game/entities/guild/entity.go", "*entity.EntityBase", "*entity.RemoteEntityBase")
	editFile("game/entities/monster/entity.go", "category=entity.EntityCategoryOther", "category=entity.EntityCategoryOther noPersist=true lifetime=ephemeral")
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
		{"no entity", AddOptions{Kind: "timer"}, "needs the entity"},
		{"two entity names", AddOptions{Kind: "timer", Name: "Npc", Entity: "Player"}, "takes the entity once"},
		{"unknown entity", AddOptions{Kind: "timer", Entity: "Ghost"}, `entity "ghost" does not exist`},
		{"no DAO", AddOptions{Kind: "timer", Entity: "Npc"}, "has no DAO"},
		{"remote managed", AddOptions{Kind: "timer", Entity: "Guild"}, "not supported on Remote-managed entities"},
		{"not persisted", AddOptions{Kind: "timer", Entity: "Monster"}, "noPersist=true"},
		{"component name taken", AddOptions{Kind: "timer", Entity: "Hero"}, "already has a component Timer"},
		{"unknown service", AddOptions{Kind: "timer", Entity: "Player", Service: "lobby"}, `unknown service "lobby"`},
		{"bad component", AddOptions{Kind: "timer", Entity: "Player", Component: "9x"}, "invalid timer component name"},
		{"id out of range", AddOptions{Kind: "timer", Entity: "Player", ID: 9}, "outside"},
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

	// 重复 add：同一实体只有一个定时器组件，再加一种类型写在业务回调文件里。
	if _, err := Add(root, AddOptions{Kind: "timer", Entity: "Player"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "timer", Entity: "Player", Component: "Other"}); err == nil || !strings.Contains(err.Error(), "already has a timer component (timer") {
		t.Fatalf("second add = %v", err)
	}
}

// roost.yaml 手改出的不一致在加载时报出。
func TestManifestRejectsAnInconsistentTimerEntry(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, []string{"entity", "nest", "dao"})
	m.Timers = map[string]TimerSpec{"Player": {Component: "timer", ID: 0, Service: "lobby"}}
	err := m.Validate()
	for _, want := range []string{`invalid entity name "Player"`, "timers.Player.id must be a positive", "timers.Player.dao names the DAO", `timers.Player.service "lobby" is not a service`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %v, want %q", err, want)
		}
	}
	m.Timers = map[string]TimerSpec{"player": {Component: "timer", ID: 2000, Service: "game", Dao: "PlayerDao"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), `timers.player.service "game" has no nest mod`) {
		t.Fatalf("Validate() without nest = %v", err)
	}
}

// 真实生成工程：roost add timer --entity Player，新手第四轮的“限时 Buff 到期”加一个周期收益，经正式 Nest 引擎、
// wiring/entitytimer 装配与生成的 Tick handler 跑一遍（DataEngine 用不落库 committer + 内存权威 loader：卸载时
// 记下 DAO 的持久字节，冷加载时按 RestorePersisted 水合，与 DataEngine 仓库同一发布路径）。时钟是可控业务时钟，
// 驱动循环只在第一段运行，之后由用例显式 DeliverDue，事件顺序由用例决定。
func TestEntityTimerGeneratedProjectEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	root := newTimerProject(t)
	modPath := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	mod = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(mod, nil)
	mod = append(mod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(modPath, mod, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run([]string{"add", "timer", "--entity", "Player", "--root", root}, &out, &out); err != nil {
		t.Fatalf("roost add timer: %v\n%s", err, out.String())
	}
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dao := readProjectFile(t, root, "db/def/player.go")
	dao = strings.Replace(dao, "\n\t// Timers 是", timerE2EDaoFields+"\n\t// Timers 是", 1)
	write("db/def/player.go", dao)
	write("game/entities/player/timer_handlers.go", timerE2EHandlers)
	write("game/handler/player_timers.go", timerE2ENestHandlers)
	write("internal/bootstrap/entity_timer_e2e_test.go", timerE2ETest)
	out.Reset()
	if err := Run([]string{"generate", "--root", root}, &out, &out); err != nil {
		t.Fatalf("roost generate: %v\n%s", err, out.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	for _, args := range [][]string{
		{"build", "./..."}, {"vet", "./..."},
		{"test", "-race", "-count=1", "./game/..."},
		{"test", "-race", "-count=1", "-run", "TestEntityTimerEndToEnd", "-v", "./internal/bootstrap"},
	} {
		command := exec.CommandContext(ctx, "go", args...)
		command.Dir = root
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		if args[len(args)-1] == "./internal/bootstrap" {
			if !strings.Contains(string(output), "--- PASS: TestEntityTimerEndToEnd") {
				t.Fatalf("the generated end-to-end test did not run:\n%s", output)
			}
			t.Logf("%s", output)
		}
	}
}

// 业务字段：Buff 状态与周期收益，以及最近一次周期回调收到的 Fire（给用例核对离线补触发的参数）。
const timerE2EDaoFields = `
	BuffActive   bool  ` + "`bson:\"buff_active\" dao:\"persist\"`" + `
	BuffExpired  int64 ` + "`bson:\"buff_expired\" dao:\"persist\"`" + `
	IncomeFires  int64 ` + "`bson:\"income_fires\" dao:\"persist\"`" + `
	Gold         int64 ` + "`bson:\"gold\" dao:\"persist\"`" + `
	LastOverdue  bool  ` + "`bson:\"last_overdue\" dao:\"persist\"`" + `
	LastDeadline int64 ` + "`bson:\"last_deadline\" dao:\"persist\"`" + `
	LastNow      int64 ` + "`bson:\"last_now\" dao:\"persist\"`" + `
	LastInterval int64 ` + "`bson:\"last_interval\" dao:\"persist\"`"

const timerE2EHandlers = `package player

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
)

const (
	// TimerTypeBuffExpire：限时 Buff（双倍金币卡）到期。
	TimerTypeBuffExpire int32 = 1
	// TimerTypeIncome：每分钟一次收益；离线错过的周期由回调自己补算（框架只触发一次）。
	TimerTypeIncome int32 = 2
)

const incomeInterval = time.Minute

func (component *TimerComponent) registerTimerHandlers(handlers *entitytimer.Handlers) {
	handlers.Register(TimerTypeBuffExpire, component.onBuffExpire)
	handlers.Register(TimerTypeIncome, component.onIncome)
}

func (component *TimerComponent) onBuffExpire(entitytimer.Fire) time.Duration {
	dao := component.owner.Dao()
	dao.SetBuffActive(false)
	dao.SetBuffExpired(dao.GetBuffExpired() + 1)
	return 0
}

func (component *TimerComponent) onIncome(fire entitytimer.Fire) time.Duration {
	dao := component.owner.Dao()
	missed := int64(0)
	if fire.Overdue && fire.Interval > 0 {
		missed = int64(fire.Now.Sub(fire.Deadline) / fire.Interval)
	}
	dao.SetGold(dao.GetGold() + 1 + missed)
	dao.SetIncomeFires(dao.GetIncomeFires() + 1)
	dao.SetLastOverdue(fire.Overdue)
	dao.SetLastDeadline(fire.Deadline.UnixMilli())
	dao.SetLastNow(fire.Now.UnixMilli())
	dao.SetLastInterval(fire.Interval.Milliseconds())
	return incomeInterval
}
`

const timerE2ENestHandlers = `package handler

import (
	"fmt"
	"time"

	player "example.com/planet/game/entities/player"
)

//roost:nest rollback=undo durability=strict
func handlerArmBuff(target player.ITimerEntity, seconds int64) (int64, error) {
	target.TimerComp().Owner().Dao().SetBuffActive(true)
	return target.TimerComp().NewTimer(time.Duration(seconds)*time.Second, player.TimerTypeBuffExpire, 0, 0, nil), nil
}

//roost:nest rollback=undo durability=strict
func handlerArmIncome(target player.ITimerEntity) (int64, error) {
	return target.TimerComp().NewTimer(time.Minute, player.TimerTypeIncome, 0, 0, nil), nil
}

//roost:nest rollback=undo durability=strict
func handlerTimerState(target player.ITimerEntity) (string, error) {
	dao := target.TimerComp().Owner().Dao()
	return fmt.Sprintf("buff=%v expired=%d income=%d gold=%d overdue=%v deadline=%d now=%d interval=%d nodes=%d",
		dao.GetBuffActive(), dao.GetBuffExpired(), dao.GetIncomeFires(), dao.GetGold(), dao.GetLastOverdue(),
		dao.GetLastDeadline(), dao.GetLastNow(), dao.GetLastInterval(), len(target.TimerComp().Timers())), nil
}
`

const timerE2ETest = `package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"example.com/planet/db"
	player "example.com/planet/game/entities/player"
	registry "example.com/planet/internal/registry"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	kitentitytimer "github.com/tjbdwanghaibo/roost-core/wiring/entitytimer"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
)

type noDurableStore struct{}

func (noDurableStore) NestOptions() []corenest.NestOption {
	return []corenest.NestOption{corenest.NestOptionWithTransactionCommitter(noDurableStore{})}
}
func (noDurableStore) Commit(context.Context, corenest.CommitRecord) error { return nil }

// authority 是内存里的“存储”：卸载时记下 DAO 的持久字节，冷加载按 RestorePersisted 水合后经 EntityManager 发布。
type authority struct {
	mu      sync.Mutex
	docs    map[int64][]byte
	deleted map[int64]bool
	loads   int
}

func (a *authority) save(id int64, raw []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.docs[id] = raw
}

func (a *authority) LoadEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	a.mu.Lock()
	a.loads++
	raw, ok := a.docs[id]
	deleted := a.deleted[id]
	a.mu.Unlock()
	if deleted {
		return nil, engine.ErrEntityAggregateDeleted
	}
	if !ok {
		return nil, engine.ErrEntityAggregateNotFound
	}
	restored := db.NewPlayerDao()
	restored.SetId(id)
	if err := restored.RestorePersisted(raw, db.PlayerDaoSchemaVersion, 1); err != nil {
		return nil, err
	}
	var created entity.IThreadSafeEntity
	var createErr error
	err := entity.RunLocal(ctx, func() {
		created, createErr = EntityManager.Create(&entity.EntityCreateParam{
			IsCreate: false, Kind: kind, Id: id, Category: entity.MustEntityCategoryOfKind(kind),
			Dao: map[string]entity.DaoInterface{db.PlayerDaoCollection: restored},
		})
	})
	return created, errors.Join(err, createErr)
}

func (a *authority) loadCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.loads
}

type fakeBusinessClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeBusinessClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeBusinessClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

func TestEntityTimerEndToEnd(t *testing.T) {
	if err := registry.RegisterAll(); err != nil {
		t.Fatal(err)
	}
	store := &authority{docs: map[int64][]byte{}, deleted: map[int64]bool{}}
	if _, err := EntityAccess.ConfigureLoader(store); err != nil {
		t.Fatal(err)
	}
	if _, err := EntityManager.RegisterDeleteAdmitter(func(_ context.Context, value entity.IThreadSafeEntity, _ entity.EntityDestroyReason) (entity.DeleteAdmission, error) {
		store.mu.Lock()
		store.deleted[value.ID()] = true
		store.mu.Unlock()
		return entity.DeleteAdmissionImmediate, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := viper.New()
	reg := app.NewRegistry(cfg)
	if err := reg.Register(mods.ModDataEngine, noDurableStore{}); err != nil {
		t.Fatal(err)
	}
	// 与生成的 bootstrap 同样的两个构造。
	nestMod := kitnest.NewMod(EntityAccess)
	timerMod := kitentitytimer.NewMod(EntityAccess, player.TimerDriver)
	all := []app.Mod{nestMod, timerMod}
	for _, mod := range all {
		if err := mod.Init(cfg); err != nil {
			t.Fatalf("%s init: %v", mod.Name(), err)
		}
	}
	for _, mod := range all {
		if err := mod.Provide(reg); err != nil {
			t.Fatalf("%s provide: %v", mod.Name(), err)
		}
	}
	business := &fakeBusinessClock{now: time.UnixMilli(1_900_000_000_000)}
	player.TimerDriver.SetClock(business.Now)
	t.Cleanup(func() { player.TimerDriver.SetClock(fctx.Now) })
	for _, mod := range all {
		if err := mod.Start(); err != nil {
			t.Fatalf("%s start: %v", mod.Name(), err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = timerMod.StopWithContext(ctx)
		_ = nestMod.StopWithContext(ctx)
	})
	nest, ok := app.Lookup[*corenest.NestMgr](reg, mods.ModNest)
	if !ok {
		t.Fatal("nest engine not published")
	}
	ctx := context.Background()
	request := func(handler string, id int64, params ...any) any {
		t.Helper()
		value, err := nest.Request(ctx, corenest.NewHandlerName(handler), id, corenest.NewParams(params...))
		if err != nil {
			t.Fatalf("%s: %v", handler, err)
		}
		return value
	}
	state := func(id int64) string { t.Helper(); return request("handlerTimerState", id).(string) }
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	live := func(id int64) entity.IThreadSafeEntity { return EntityManager.Get(id) }

	created, err := EntityAccess.Create(&entity.EntityCreateParam{IsCreate: true, UniqueID: 42, Kind: player.EntityKindPlayer, Category: entity.MustEntityCategoryOfKind(player.EntityKindPlayer)})
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID()
	t0 := business.Now()

	// 1. 在线：Buff 5 秒后到期，驱动循环在期限到了时投递一次，回调在锁内把 Buff 关掉。
	if got := request("handlerArmBuff", id, int64(5)).(int64); got != 1 {
		t.Fatalf("first timer id = %d", got)
	}
	if due, ok := player.TimerDriver.DueOf(created); !ok || !due.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("indexed due after arming = %v %v", due, ok)
	}
	business.advance(5 * time.Second)
	waitFor("the buff to expire through the running driver loop", func() bool { return state(id) == "buff=false expired=1 income=0 gold=0 overdue=false deadline=0 now=0 interval=0 nodes=0" })
	waitFor("the tick's report to clear the index", func() bool { return player.TimerDriver.Pending() == 0 })
	// 之后由用例显式投递：停掉循环，事件顺序由用例决定。
	if err := timerMod.StopWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	deliver := func(want int) {
		t.Helper()
		if got := player.TimerDriver.DeliverDue(ctx); got != want {
			t.Fatalf("delivered %d ticks, want %d", got, want)
		}
	}
	settled := func(owner entity.IThreadSafeEntity, want time.Time) {
		t.Helper()
		waitFor(fmt.Sprintf("the index to settle on %v", want), func() bool {
			due, ok := player.TimerDriver.DueOf(owner)
			if want.IsZero() {
				return !ok && player.TimerDriver.Pending() == 0
			}
			return ok && due.Equal(want)
		})
	}

	// 2. 周期定时器按回调返回值续期（从触发时刻起算）。
	request("handlerArmIncome", id)
	armedIncome := business.Now()
	deliver(0)
	business.advance(time.Minute)
	deliver(1)
	settled(created, armedIncome.Add(2*time.Minute))
	if got := state(id); got != fmt.Sprintf("buff=false expired=1 income=1 gold=1 overdue=false deadline=%d now=%d interval=60000 nodes=1", armedIncome.Add(time.Minute).UnixMilli(), armedIncome.Add(time.Minute).UnixMilli()) {
		t.Fatalf("after the first income: %s", got)
	}

	// 3. 离线：卸载（先记下持久字节），错过十个周期期间不投递、不加载；加载后只触发一次，参数里有原定期限与补触发标志。
	missedDeadline := armedIncome.Add(2 * time.Minute)
	if err := nest.RunLocal(ctx, func() {
		store.save(id, created.(*player.Player).Dao().Marshal())
		if err := EntityAccess.Unload(ctx, created); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if live(id) != nil || player.TimerDriver.Pending() != 0 {
		t.Fatalf("after unload: live %v, pending %d", live(id) != nil, player.TimerDriver.Pending())
	}
	loadsBefore := store.loadCount()
	business.advance(10*time.Minute + 30*time.Second)
	deliver(0)
	if live(id) != nil || store.loadCount() != loadsBefore {
		t.Fatal("the driver woke an unloaded entity")
	}
	loadedAt := business.Now()
	if got := state(id); got != fmt.Sprintf("buff=false expired=1 income=1 gold=1 overdue=false deadline=%d now=%d interval=60000 nodes=1", armedIncome.Add(time.Minute).UnixMilli(), armedIncome.Add(time.Minute).UnixMilli()) {
		t.Fatalf("after reload, before the catch-up tick: %s", got)
	}
	reloaded := live(id)
	if reloaded == nil || reloaded == created {
		t.Fatal("the state request did not load a new instance")
	}
	if due, ok := player.TimerDriver.DueOf(reloaded); !ok || !due.Equal(missedDeadline) {
		t.Fatalf("after load the index holds %v %v, want the missed deadline %v", due, ok, missedDeadline)
	}
	deliver(1)
	settled(reloaded, loadedAt.Add(time.Minute))
	// 错过 2m 那一次之后又过了 9 个整周期：回调自己补 9 个，框架只触发这一次。
	if got := state(id); got != fmt.Sprintf("buff=false expired=1 income=2 gold=11 overdue=true deadline=%d now=%d interval=60000 nodes=1", missedDeadline.UnixMilli(), loadedAt.UnixMilli()) {
		t.Fatalf("after the catch-up: %s", got)
	}
	deliver(0)

	// 4. 重启（重新加载）后 ID 不复用：ID 1、2 交出去过，下一个是 3。
	if got := request("handlerArmBuff", id, int64(30)).(int64); got != 3 {
		t.Fatalf("after reload the next timer id is %d, want 3", got)
	}

	// 5. 同一实体多个定时器：索引是最早的那个。
	request("handlerArmBuff", id, int64(10))
	request("handlerArmBuff", id, int64(20))
	settled(reloaded, loadedAt.Add(10*time.Second))
	business.advance(10 * time.Second)
	deliver(1)
	settled(reloaded, loadedAt.Add(20*time.Second))

	// 6. 删档：实例离开索引；之后到期不投递，直接发给它的 Tick 也不会把它加载回来触发。
	if err := nest.RunLocal(ctx, func() {
		if err := EntityAccess.Destroy(ctx, reloaded, entity.DestroyReasonCommon, true); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if player.TimerDriver.Pending() != 0 {
		t.Fatalf("a deleted entity is still indexed (%d)", player.TimerDriver.Pending())
	}
	business.advance(time.Hour)
	deliver(0)
	if _, err := nest.Request(ctx, corenest.NewHandlerName(player.TimerTickHandler), id, corenest.NewParams(business.Now().UnixMilli())); !errors.Is(err, entity.ErrAuthorityEntityDeleted) {
		t.Fatalf("a tick sent to the deleted entity = %v, want it refused as deleted", err)
	}
	if live(id) != nil {
		t.Fatal("the deleted entity came back")
	}
}
`
