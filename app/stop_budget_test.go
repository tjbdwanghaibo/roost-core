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

// RR-20260926-42（REPRO-2026-09-26-04 §8 改写）：Mod 通过 StopBudget 声明所需停机预算，
// App 在 shutdown.total_timeout 内优先分配给它，其余 Mod 均分剩余；总时长不足时按比例
// 缩放并告警。修前 dataengine 只拿到“剩余 / 剩余 mod 数”（总 30s 时 5/7/10 个 mod 分别
// 7.5s/6s/4.29s），dataengine.shutdown_timeout 不生效。

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

func TestDeclaredStopBudgetIsHonoredWithinTotalTimeout(t *testing.T) {
	// 90s 足够覆盖 20s 声明 + 最多 9 个未声明 Mod x 5s 基准，三种组合都不需要缩放。
	const total, declared = 90 * time.Second, 20 * time.Second
	for _, order := range reproStopOrders {
		rec := newBudgetRecorder()
		ctx, cancel := context.WithTimeout(context.Background(), total)
		err := stopModsReverseWithContext(ctx, stopBudgetMods(order, "dataengine", declared, rec), "test stop")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		assertBudgetNear(t, "dataengine", rec.get("dataengine"), declared)
		// 在 dataengine 之前停止的 Mod 均分“总时长 - 声明预算”，不会吃掉它的预留。
		undeclared := time.Duration(len(order) - 1)
		first := order[len(order)-1]
		assertBudgetNear(t, string(first), rec.get(first), (total-declared)/undeclared)
	}
}

func TestStopBudgetsScaleProportionallyWhenTotalIsInsufficient(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	// 生成配置的默认值：shutdown.total_timeout=30s，dataengine.shutdown_timeout=30s。
	const total, declared = 30 * time.Second, 30 * time.Second
	order := reproStopOrders[0]
	rec := newBudgetRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), total)
	err := stopModsReverseWithContext(ctx, stopBudgetMods(order, "dataengine", declared, rec), "test stop")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	// 先停 nest：需求 = 30s 声明 + 4 个未声明 Mod x defaultModStopTimeout(5s) = 50s > 30s，
	// 按 30/50 缩放，nest 拿 3s。nest 立即结束后为 dataengine 重新规划：剩余仍约 30s，
	// 需求 = 30s + 3 x 5s = 45s，dataengine 拿 30 x 30/45 = 20s（修前 7.5s）。
	scaledShare := func(want, need time.Duration) time.Duration {
		return time.Duration(float64(want) * float64(total) / float64(need))
	}
	assertBudgetNear(t, "nest", rec.get("nest"), scaledShare(defaultModStopTimeout, declared+4*defaultModStopTimeout))
	assertBudgetNear(t, "dataengine", rec.get("dataengine"), scaledShare(declared, declared+3*defaultModStopTimeout))
	if !strings.Contains(logs.String(), "dataengine") || !strings.Contains(logs.String(), "scaled") {
		t.Fatalf("no scale-down warning for dataengine; logs:\n%s", logs.String())
	}
}

// REPRO §8 的三种组合，用生成配置的默认值（总 30s、dataengine 30s）：修前 7.5s / 6s / 4.29s。
// 轮到 dataengine 时其后分别还有 3 / 4 / 6 个未声明 Mod，需求 45s / 50s / 60s，按比例得 20s / 18s / 15s。
func TestGeneratedDefaultsGrantDataEngineItsScaledDeclaredBudget(t *testing.T) {
	const total, declared = 30 * time.Second, 30 * time.Second
	for i, want := range []time.Duration{20 * time.Second, 18 * time.Second, 15 * time.Second} {
		rec := newBudgetRecorder()
		ctx, cancel := context.WithTimeout(context.Background(), total)
		err := stopModsReverseWithContext(ctx, stopBudgetMods(reproStopOrders[i], "dataengine", declared, rec), "test stop")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		assertBudgetNear(t, fmt.Sprintf("%d mods: dataengine", len(reproStopOrders[i])), rec.get("dataengine"), want)
	}
}

// 服务专属 Mod 先停、共享 Mod 后停，两段共用一个 shutdown.total_timeout：先停的一段
// 必须把后面共享 Mod 的声明预算算进去，否则它会把总时长全部拿走。后一段未声明的 Mod
// 不参与前一段的均分（与修前相同），所以 service_only 拿到 总时长 - 声明预算。
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
