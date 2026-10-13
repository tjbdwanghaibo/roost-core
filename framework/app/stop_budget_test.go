package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// Mod 通过 StopBudget 声明所需停机预算，App 在 shutdown.total_timeout 内优先分配给它，其余 Mod 均分剩余。
// 历史：RR-20260926-42。

// budgetRecorder 记录每个 Mod 在 StopWithContext 入口看到的剩余截止时长。
type budgetRecorder struct {
	mu     sync.Mutex
	budget map[ModName]time.Duration
	// noDeadline 记录没有截止时间的调用（兼容 Stop 路径的 background context）。
	noDeadline map[ModName]bool
}

func newBudgetRecorder() *budgetRecorder {
	return &budgetRecorder{budget: make(map[ModName]time.Duration), noDeadline: make(map[ModName]bool)}
}

func (r *budgetRecorder) record(name ModName, ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		r.noDeadline[name] = true
		return
	}
	r.budget[name] = time.Until(deadline)
}

func (r *budgetRecorder) get(name ModName) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.budget[name]
}

// plainStopMod 不声明停机预算。
type plainStopMod struct {
	name ModName
	rec  *budgetRecorder
}

func (m plainStopMod) Name() ModName           { return m.name }
func (m plainStopMod) Init(*viper.Viper) error { return nil }
func (m plainStopMod) Provide(*Registry) error { return nil }
func (m plainStopMod) Start() error            { return nil }
func (m plainStopMod) Stop()                   {}
func (m plainStopMod) StopWithContext(ctx context.Context) error {
	m.rec.record(m.name, ctx)
	return nil
}

// declaringStopMod 声明停机预算（dataengine 的形状）。
type declaringStopMod struct {
	plainStopMod
	declared time.Duration
}

func (m declaringStopMod) StopBudget() time.Duration { return m.declared }

func stopBudgetMods(order []ModName, declaring ModName, declared time.Duration, rec *budgetRecorder) []Mod {
	mods := make([]Mod, len(order))
	for i, name := range order {
		plain := plainStopMod{name: name, rec: rec}
		if name == declaring {
			mods[i] = declaringStopMod{plainStopMod: plain, declared: declared}
			continue
		}
		mods[i] = plain
	}
	return mods
}

var reproStopOrders = [][]ModName{
	{"redis", "mongo", "nats", "dataengine", "nest"},
	{"redis", "mongo", "nats", "remote_entity", "dataengine", "nest", "saga"},
	{"etcd", "redis", "mongo", "nats", "lock", "remote_entity", "dataengine", "nest", "saga", "syncbus"},
}

// 调度抖动与 -race 的余量；预算以秒计，毫秒级误差不影响结论。
const stopBudgetTolerance = 250 * time.Millisecond

func assertBudgetNear(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got > want || got < want-stopBudgetTolerance {
		t.Fatalf("%s budget=%v, want %v (tolerance %v)", what, got.Round(time.Millisecond), want, stopBudgetTolerance)
	}
}

// 总 30s、dataengine 声明 30s 的三种组合：轮到 dataengine 时其后分别还有 3 / 4 / 6 个未声明 Mod，上限 30 − 3s × 未声明数，
// 得 21s / 18s / 12s。
func TestGeneratedDefaultsGrantDataEngineItsScaledDeclaredBudget(t *testing.T) {
	const total, declared = 30 * time.Second, 30 * time.Second
	for i, want := range []time.Duration{21 * time.Second, 18 * time.Second, 12 * time.Second} {
		rec := newBudgetRecorder()
		runStopBudgetPlan(t, total, stopBudgetMods(reproStopOrders[i], "dataengine", declared, rec))
		assertBudgetNear(t, fmt.Sprintf("%d mods: dataengine", len(reproStopOrders[i])), rec.get("dataengine"), want)
	}
}

// 服务专属 Mod 先停、共享 Mod 后停，两段共用一个 shutdown.total_timeout：先停的一段把后面共享 Mod 的声明预算算进去，否则会把
// 总时长全部拿走。后一段未声明的 Mod 不参与前一段的均分，所以 service_only 拿到 总时长 - 声明预算。
func TestServiceModStopPlansAroundLaterSharedDeclaredBudget(t *testing.T) {
	const total, declared = 30 * time.Second, 20 * time.Second
	rec := newBudgetRecorder()
	shared := stopBudgetMods([]ModName{"mongo", "dataengine"}, "dataengine", declared, rec)
	service := []Mod{plainStopMod{name: "service_only", rec: rec}}
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	if err := stopModsReverseBefore(ctx, service, shared, "test stop (service-specific)"); err != nil {
		t.Fatal(err)
	}
	if err := stopModsReverseWithContext(ctx, shared, "test stop"); err != nil {
		t.Fatal(err)
	}
	assertBudgetNear(t, "service_only", rec.get("service_only"), total-declared)
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), declared)
}

// 没有总截止时间的兼容路径（启动失败时的 stopModsReverse）：声明的 Mod 用自己的预算，
// 其余沿用 defaultModStopTimeout。
func TestDeclaredStopBudgetAppliesWithoutTotalDeadline(t *testing.T) {
	const declared = 20 * time.Second
	rec := newBudgetRecorder()
	stopModsReverse(stopBudgetMods(reproStopOrders[0], "dataengine", declared, rec), "test stop")
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), declared)
	assertBudgetNear(t, "nest", rec.get("nest"), defaultModStopTimeout)
}

type quickService struct{}

func (quickService) Name() ServiceName              { return "game" }
func (quickService) Init(*Registry) error           { return nil }
func (quickService) Serve(context.Context) error    { return nil }
func (quickService) Shutdown(context.Context) error { return nil }

// 经 App.Execute 的正式停机路径：服务专属 Mod 先停，共享的 dataengine 声明的预算仍然生效。
func TestAppShutdownGrantsDeclaredStopBudgetAcrossModGroups(t *testing.T) {
	const total, declared = 30 * time.Second, 20 * time.Second
	rec := newBudgetRecorder()
	a := New("roost-test", "0.0.0")
	a.Mods(stopBudgetMods([]ModName{"mongo", "nats", "dataengine", "nest"}, "dataengine", declared, rec)...)
	a.RegisterServer("game", quickService{}, plainStopMod{name: "service_only", rec: rec})
	a.cfg.Set("log.file", false)
	a.cfg.Set("log.dir", t.TempDir())
	a.cfg.Set("shutdown.total_timeout", total)
	a.RootCmd().SetArgs([]string{"game"})
	if err := a.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// service_only 先停，只能用“总时长 - 声明预算”；dataengine 拿满声明值。
	if got := rec.get("service_only"); got > total-declared {
		t.Fatalf("service-specific mod budget=%v ate into the declared %v", got, declared)
	}
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), declared)
}

// 停机预算规则：先按声明值分配，上限为“剩余总时长 − 每个未声明 Mod 的固定保底”；只有声明值之和超过上限才按比例缩放声明值；
// 保底是固定下限、不参与缩放；总时长连所有 Mod 的保底都给不起时均分并告警。保底为 3s（与生成配置 nest.request_timeout 同值：
// Nest 排空已准入请求的上限）。
// 历史：RR-20260926-51。

// captureWarnings 把 slog 默认 logger 换成只收 Warn 及以上的文本 logger，测试结束恢复。
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func runStopBudgetPlan(t *testing.T, total time.Duration, mods []Mod) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	if err := stopModsReverseWithContext(ctx, mods, "test stop"); err != nil {
		t.Fatal(err)
	}
}

var auditStopOrder = []ModName{"etcd", "redis", "mongo", "nats", "lock", "remote_entity", "dataengine", "nest", "saga", "syncbus", "gateway"}

// 总 50s 足以覆盖 dataengine 声明的 30s 与其后 6 个未声明 Mod 的保底（6 × 3s）：dataengine 拿到声明值，不被一起缩放。
func TestDeclaredStopBudgetIsGrantedWhenTotalCoversItAndTheFloors(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	runStopBudgetPlan(t, 50*time.Second, stopBudgetMods(auditStopOrder, "dataengine", 30*time.Second, rec))
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), 30*time.Second)
	// 先停的 gateway 规划时声明值（30s）超过上限 50 − 10 × 3 = 20s，只拿固定保底。
	assertBudgetNear(t, "gateway", rec.get("gateway"), 3*time.Second)
	if strings.Contains(logs.String(), "dataengine") {
		t.Fatalf("dataengine got its declared budget but a scale-down warning was logged:\n%s", logs.String())
	}
}

// 声明值偏大（120s）、总时长 30s：先停的 nest 保有固定保底 3s；dataengine 拿 30 − 3 × 3 = 21s 并告警。
func TestLargeDeclaredStopBudgetKeepsTheFloorOfEarlierMods(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	runStopBudgetPlan(t, 30*time.Second, stopBudgetMods([]ModName{"redis", "mongo", "nats", "dataengine", "nest"}, "dataengine", 120*time.Second, rec))
	assertBudgetNear(t, "nest", rec.get("nest"), 3*time.Second)
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), 21*time.Second)
	// dataengine 立即结束后，其余 3 个未声明 Mod 均分剩下的约 30s。
	assertBudgetNear(t, "nats", rec.get("nats"), 10*time.Second)
	if !strings.Contains(logs.String(), "dataengine") || !strings.Contains(logs.String(), "scaled") {
		t.Fatalf("no scale-down warning for dataengine; logs:\n%s", logs.String())
	}
}

// 生成默认值 shutdown.total_timeout=60s、dataengine.shutdown_timeout=30s：三种 Mod 组合下 dataengine 都拿到声明值，先停的 Mod
// 均分 60 − 30 = 30s（5 / 7 / 10 个 Mod 时 7.5s / 5s / 3.33s），每个未声明 Mod 都不低于 3s 保底，停机不告警。
func TestGeneratedDefaultShutdownBudgetsCoverEveryMod(t *testing.T) {
	logs := captureWarnings(t)
	const total, declared = 60 * time.Second, 30 * time.Second
	for _, order := range reproStopOrders {
		rec := newBudgetRecorder()
		runStopBudgetPlan(t, total, stopBudgetMods(order, "dataengine", declared, rec))
		label := fmt.Sprintf("%d mods: ", len(order))
		assertBudgetNear(t, label+"dataengine", rec.get("dataengine"), declared)
		first := order[len(order)-1]
		assertBudgetNear(t, label+string(first), rec.get(first), (total-declared)/time.Duration(len(order)-1))
		for _, name := range order {
			if name != "dataengine" && rec.get(name) < 3*time.Second-stopBudgetTolerance {
				t.Fatalf("%s%s budget=%v is below the 3s floor", label, name, rec.get(name).Round(time.Millisecond))
			}
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("the generated defaults logged a scale-down warning:\n%s", logs.String())
	}
}

// 总时长连每个 Mod 的保底都给不起（10s < 5 × 3s）：均分并告警，声明的 Mod 也不例外。
func TestStopBudgetsSplitEvenlyWhenTotalCannotCoverTheFloors(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	runStopBudgetPlan(t, 10*time.Second, stopBudgetMods(reproStopOrders[0], "dataengine", 30*time.Second, rec))
	assertBudgetNear(t, "nest", rec.get("nest"), 2*time.Second)
	// nest 立即结束，剩约 10s、4 个 Mod，仍给不起保底：均分 2.5s。
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), 2500*time.Millisecond)
	if !strings.Contains(logs.String(), "floor") {
		t.Fatalf("no warning that the total cannot cover the per-mod floor; logs:\n%s", logs.String())
	}
}

// 没有任何 Mod 声明预算时按“剩余 / 本段剩余 Mod 数”均分，包括总时长低于保底之和的情形。
func TestUndeclaredStopBudgetsKeepTheEvenSplit(t *testing.T) {
	order := reproStopOrders[0]
	for _, total := range []time.Duration{30 * time.Second, 10 * time.Second} {
		rec := newBudgetRecorder()
		runStopBudgetPlan(t, total, stopBudgetMods(order, "", 0, rec))
		for i := len(order) - 1; i >= 0; i-- {
			// 前面的 Mod 立即结束，剩余时间几乎不变：第 k 个停止的 Mod 拿 total / 剩余数。
			assertBudgetNear(t, fmt.Sprintf("total %v: %s", total, order[i]), rec.get(order[i]), total/time.Duration(i+1))
		}
	}
}

// 生成配置按服务实际注册的 Mod 计算 shutdown.total_timeout = 声明预算之和 + 3s × 未声明 Mod 数 + 5s（余量给先于 Mod 运行的
// Service.Shutdown）。真实 game-demo 的 game 服务注册 23 个 Mod，dataengine 声明 30s、accessplayertcp 声明 10s，生成值是
// 30s + 10s + 3s × 21 + 5s = 108s；这个总时长覆盖每个 Mod 的保底与声明预算。
// codegen/internal/roost/shutdown_test.go 现在从生成工程的 bootstrap 数出 24 个 Mod（加了按亲和键路由所需的 etcd），
// 钉住 30s + 10s + 3s × 22 + 5s + 3s 单实例锁释放 = 114s；本文件仍按上面 23 个 Mod 的清单与 108s 规划。
// 历史：RR-20260926-66、RR-20260927-05。
const (
	generatedGameShutdownTotal  = 108 * time.Second
	generatedShutdownMargin     = 5 * time.Second
	generatedDataEngineDeclared = 30 * time.Second
	generatedPlayerTCPDeclared  = 10 * time.Second
)

// generatedDeclared 是生成的 game 服务里声明停机预算的 Mod 及其声明值。
var generatedDeclared = map[ModName]time.Duration{"dataengine": generatedDataEngineDeclared, "accessplayertcp": generatedPlayerTCPDeclared}

// game-demo game 服务的 Mod（生成 bootstrap 的注册顺序）与共享 Mod。
var (
	generatedGameServiceMods = []ModName{"configdata", "mongo", "nats", "dataengine", "nest", "redis", "syncbus", "remote_entity", "saga",
		"c_account", "c_activity", "c_chat", "c_global", "c_mail", "c_match", "c_platform", "c_rank", "c_session", "accessplayer", "accessplayertcp"}
	generatedSharedMods = []ModName{"lock", "ops", "statslog"}
)

// withDataEngineAt 把 dataengine 挪到服务专属段的第 position 位（依赖排序可能改变它在停止顺序中的位置，
// 每个位置都要成立）。
func withDataEngineAt(position int) []ModName {
	out := make([]ModName, 0, len(generatedGameServiceMods))
	for _, name := range generatedGameServiceMods {
		if name != "dataengine" {
			out = append(out, name)
		}
	}
	out = append(out[:position], append([]ModName{"dataengine"}, out[position:]...)...)
	return out
}

func buildPlanMods(names []ModName) []Mod {
	mods := make([]Mod, len(names))
	for i, name := range names {
		plain := plainStopMod{name: name}
		if declared, ok := generatedDeclared[name]; ok {
			mods[i] = declaringStopMod{plainStopMod: plain, declared: declared}
			continue
		}
		mods[i] = plain
	}
	return mods
}

// 模拟时钟：用 modStopBudget 按 App 的停止顺序逐个规划。Service.Shutdown 用掉 0 或整个余量；
// Mod 要么立即结束，要么 dataengine 之前的每个 Mod 用满给它的预算（最坏情况：dataengine 最晚拿到剩余）。
// 每个 Mod 都要拿到至少 3s 保底，dataengine 拿满声明的 30s、accessplayertcp 拿满 10s，且全程不进入缩放 / 均分分支（不告警）。
func TestGeneratedGameServiceTotalCoversEveryModFloorAndTheDeclaredBudget(t *testing.T) {
	assertGameServiceBudgets(t, generatedGameShutdownTotal)
}

func assertGameServiceBudgets(t *testing.T, total time.Duration) {
	t.Helper()
	for position := range generatedGameServiceMods {
		for _, spent := range []time.Duration{0, generatedShutdownMargin} {
			for _, consume := range []string{"instant", "full-before-dataengine"} {
				name := fmt.Sprintf("dataengine@%d/service-shutdown=%v/%s", position, spent, consume)
				service, shared := buildPlanMods(withDataEngineAt(position)), buildPlanMods(generatedSharedMods)
				remaining := total - spent
				var problems []string
				for segment, mods := range [][]Mod{service, shared} {
					var later []Mod
					if segment == 0 {
						later = shared
					}
					reachedDataEngine := false
					for i := len(mods) - 1; i >= 0; i-- {
						budget, plan := modStopBudget(remaining, mods[i], mods[:i], later)
						mod := mods[i].Name()
						if plan != stopBudgetDeclaredGranted {
							problems = append(problems, fmt.Sprintf("%s plan=%d (warns)", mod, plan))
						}
						if mod == "dataengine" {
							reachedDataEngine = true
						}
						if declared, ok := generatedDeclared[mod]; ok {
							if budget != declared {
								problems = append(problems, fmt.Sprintf("%s=%v, want %v", mod, budget, declared))
							}
						} else if budget < undeclaredModStopFloor {
							problems = append(problems, fmt.Sprintf("%s=%v below the %v floor", mod, budget, undeclaredModStopFloor))
						}
						if consume == "full-before-dataengine" && segment == 0 && !reachedDataEngine {
							remaining -= budget
						}
					}
				}
				if len(problems) > 0 {
					t.Errorf("total=%v %s:\n  %s", total, name, strings.Join(problems, "\n  "))
				}
			}
		}
	}
}

// 真实时钟、正式停机入口：服务专属段 + 共享段，全部 Mod 立即结束，不出预算告警。
func TestGeneratedGameServiceStopsWithoutBudgetWarnings(t *testing.T) {
	logs := captureWarnings(t)
	rec := newBudgetRecorder()
	service := stopBudgetMods(generatedGameServiceMods, "dataengine", generatedDataEngineDeclared, rec)
	for i, mod := range service {
		if mod.Name() == "accessplayertcp" {
			service[i] = declaringStopMod{plainStopMod: plainStopMod{name: mod.Name(), rec: rec}, declared: generatedPlayerTCPDeclared}
		}
	}
	shared := stopBudgetMods(generatedSharedMods, "", 0, rec)
	ctx, cancel := context.WithTimeout(context.Background(), generatedGameShutdownTotal)
	defer cancel()
	if err := stopModsReverseBefore(ctx, service, shared, "svc"); err != nil {
		t.Fatal(err)
	}
	if err := stopModsReverseWithContext(ctx, shared, "shared"); err != nil {
		t.Fatal(err)
	}
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), generatedDataEngineDeclared)
	assertBudgetNear(t, "accessplayertcp", rec.get("accessplayertcp"), generatedPlayerTCPDeclared)
	for _, name := range append(append([]ModName{}, generatedGameServiceMods...), generatedSharedMods...) {
		if got := rec.get(name); got < undeclaredModStopFloor-250*time.Millisecond {
			t.Errorf("%s budget=%v, below the %v floor", name, got, undeclaredModStopFloor)
		}
	}
	if logs.Len() > 0 {
		t.Fatalf("stopping the generated game service logged warnings:\n%s", logs.String())
	}
}
