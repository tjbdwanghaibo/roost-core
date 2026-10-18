package roost

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sagaNativeFixture 是能生成原生步骤的新手工程：configdata 工程 + add access player（带来 nest）+ Player 实体、
// Wallet 组件与 Player DAO。整包生成一次，用例各拿私有副本。
var sagaNativeFixture struct {
	once sync.Once
	root string
	err  error
}

func copyOfSagaNativeProject(t *testing.T) string {
	t.Helper()
	sagaNativeFixture.once.Do(func() {
		source, err := generatedProjectFixture("configdata")
		if err != nil {
			sagaNativeFixture.err = err
			return
		}
		projectFixturesMu.Lock()
		root := filepath.Join(projectFixtureBase, "saga-native", "planet")
		projectFixturesMu.Unlock()
		if err := copyProjectTree(source, root); err != nil {
			sagaNativeFixture.err = err
			return
		}
		for _, options := range []AddOptions{
			{Kind: "access", Name: "player", Service: "game"},
			{Kind: "entity", Name: "Player"},
			{Kind: "component", Name: "Wallet", Entity: "Player"},
			{Kind: "dao", Name: "Player", Entity: "Player"},
		} {
			options.Warnings = &bytes.Buffer{}
			if _, err := Add(root, options); err != nil {
				sagaNativeFixture.err = err
				return
			}
		}
		sagaNativeFixture.root = root
	})
	if sagaNativeFixture.err != nil {
		t.Fatal(sagaNativeFixture.err)
	}
	target := filepath.Join(t.TempDir(), "planet")
	if err := copyProjectTree(sagaNativeFixture.root, target); err != nil {
		t.Fatal(err)
	}
	return target
}

// runGeneratedGo 在生成工程里运行 go（GOWORK=off、-mod=mod 由 useLocalRoostCore 的 tidy 保证），失败时带输出。
func runGeneratedGo(t *testing.T, root string, args ...string) string {
	t.Helper()
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s in the generated project: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// add saga -native（D12 方向 B）：生成物、归属与登记顺序。原生步骤各有正向与补偿两个 Nest handler（参数带
// saga.NativeStep、事务里 step.Complete），sync 为它们生成 Sender；steps.go 按 -steps 顺序、按主题常量登记，
// 原生步骤用 Steps.Native、其余 Steps.Mongo；definition.go 不再有第二套 Subscribe* 写法。
func TestAddSagaNativeGeneratesStepsHandlersAndSenders(t *testing.T) {
	t.Parallel()
	root := copyOfSagaNativeProject(t)
	var out bytes.Buffer
	if err := Run([]string{"add", "saga", "Gift", "-steps", "debit,credit,audit", "-native", "credit,debit", "-component", "Wallet", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add saga -native: %v\n%s", err, out.String())
	}
	for _, rel := range []string{
		"saga/gift/definition.go", "saga/gift/steps.go",
		"game/handler/gift_debit.go", "game/handler/gift_debit_compensation.go",
		"game/handler/gift_credit.go", "game/handler/gift_credit_compensation.go",
		// roost add saga 的同步跑了 generate：原生步骤的 Sender 已经在，steps.go 能直接编译。
		"game/handler/syncsender/gift_debit_nest_gen.go", "game/handler/syncsender/gift_credit_compensation_nest_gen.go",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was not generated: %v\n%s", rel, err, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "game", "handler", "gift_audit.go")); !os.IsNotExist(err) {
		t.Errorf("audit is a Mongo step, yet a Nest handler was generated for it (err=%v)", err)
	}

	definition := readProjectFile(t, root, "saga/gift/definition.go")
	if strings.Contains(definition, "func Subscribe") || strings.Contains(definition, "SubscribeMongoStep") {
		t.Errorf("definition.go still generates the Subscribe* helpers, a second way next to steps.go:\n%s", definition)
	}

	steps := readProjectFile(t, root, "saga/gift/steps.go")
	register := regexp.MustCompile(`steps\.(Native|Mongo)\("(\w+)", (Topic\w+),`).FindAllStringSubmatch(steps, -1)
	var got []string
	for _, match := range register {
		got = append(got, match[1]+" "+match[2]+" "+match[3])
	}
	want := []string{
		"Native debit TopicDebit", "Native debit_compensation TopicDebitCompensation",
		"Native credit TopicCredit", "Native credit_compensation TopicCreditCompensation",
		"Mongo audit TopicAudit", "Mongo audit_compensation TopicAuditCompensation",
	}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("RegisterSteps registrations = %v, want %v (in -steps order, native by -native, whatever order -native lists them)", got, want)
	}
	for _, want := range []string{
		`const StepsGroup = "game-gift"`,
		"func StartSteps(ctx context.Context, registry *app.Registry, deps StepDeps) (*kitsaga.Steps, error)",
		"func RegisterSteps(steps *kitsaga.Steps, deps StepDeps)",
		"syncsender.NewGiftDebitSender(bodies.deps.Nest).Sync_GiftDebit(ctx, entityID, step)",
		"syncsender.NewGiftCreditCompensationSender(bodies.deps.Nest).Sync_GiftCreditCompensation(ctx, entityID, step)",
		"entity.BuildEntityID(state.PlayerID, player.EntityKindPlayer)",
		"kitsaga.WithAdmit",
		// 容量决定写在骨架里：数字、分片、报告链接。
		"14-18 native step commands per second", "10/s forward and 5/s fully compensated", "shard by sid", sagaCapacityDocURL,
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps.go lacks %q:\n%s", want, steps)
		}
	}

	debit := readProjectFile(t, root, "game/handler/gift_debit.go")
	for _, want := range []string{"//roost:nest rollback=undo durability=strict", "func handlerGiftDebit(target player.IWalletEntity, step saga.NativeStep) error", "target.WalletComp()", `step.Complete(false, "gift.debit is not implemented")`} {
		if !strings.Contains(debit, want) {
			t.Errorf("gift_debit.go lacks %q:\n%s", want, debit)
		}
	}
	if compensation := readProjectFile(t, root, "game/handler/gift_credit_compensation.go"); !strings.Contains(compensation, "func handlerGiftCreditCompensation(target player.IWalletEntity, step saga.NativeStep) error") || !strings.Contains(compensation, "manual_required") {
		t.Errorf("gift_credit_compensation.go is not the compensation handler skeleton:\n%s", compensation)
	}
	// 服务的 Init / Shutdown 归业务：生成器不改写，只在 next 提示里给两行与容量。
	if service := readProjectFile(t, root, "internal/service/game/service.go"); strings.Contains(service, "StartSteps") {
		t.Errorf("add saga rewrote the service's Init:\n%s", service)
	}
	for _, want := range []string{"gift.StartSteps(context.Background(), registry, gift.StepDeps{})", "steps.Drain(ctx)", "capacity:", "sharded by sid", "gift.EmitStart("} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("add saga next hints lack %q:\n%s", want, out.String())
		}
	}
	manifest := mustManifest(t, root)
	if !contains(manifest.Services["game"].Mods, "saga") || !contains(manifest.Sagas, "gift") {
		t.Errorf("saga was not wired into the manifest: %+v", manifest)
	}
}

// 没有 -native 时全部是 Mongo 步骤，与之前等价（少了 Subscribe* 辅助），也不需要 Entity。
func TestAddSagaWithoutNativeRegistersMongoSteps(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "saga")
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "Trade", Steps: []string{"reserve", "deliver"}}); err != nil {
		t.Fatal(err)
	}
	steps := readProjectFile(t, root, "saga/trade/steps.go")
	if strings.Contains(steps, "steps.Native(") || strings.Contains(steps, "syncsender") || strings.Contains(steps, "corenest") {
		t.Errorf("a saga without -native registers native steps or needs Nest:\n%s", steps)
	}
	for _, want := range []string{`steps.Mongo("reserve", TopicReserve, bodies.runReserve)`, `steps.Mongo("deliver_compensation", TopicDeliverCompensation, bodies.compensateDeliver)`, "command.IdempotencyKey", `"trade.reserve is not implemented"`} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps.go lacks %q:\n%s", want, steps)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "game", "handler"))
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "trade_") {
				t.Errorf("a saga without -native generated handler %s", entry.Name())
			}
		}
	}
}

// 错误输入在写盘之前被拒绝：-native 不是 -steps 的子集、重复、没有组件、只给 -entity 不给 -native、服务没有 Nest、
// 已存在的 saga 与同名 handler。每种情形之后工程逐字节不变。
func TestAddSagaNativeRejectsBadInputWithoutTouchingTheProject(t *testing.T) {
	t.Parallel()
	root := copyOfSagaNativeProject(t)
	if err := os.WriteFile(filepath.Join(root, "game", "handler", "old_debit.go"), []byte("package handler\n\nfunc handlerOldDebit() error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := projectFileHashes(t, root)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"native not in steps", []string{"Gift", "-steps", "debit,credit", "-native", "debit,refund", "-component", "Wallet"}, `-native step "refund" is not in -steps (debit, credit)`},
		{"native duplicate", []string{"Gift", "-steps", "debit,credit", "-native", "debit,Debit", "-component", "Wallet"}, `duplicate saga step "debit" in -native`},
		{"steps duplicate", []string{"Gift", "-steps", "debit,credit,debit", "-native", "debit", "-component", "Wallet"}, `duplicate saga step "debit" in -steps`},
		{"step named like a compensation", []string{"Gift", "-steps", "debit,debit_compensation", "-component", "Wallet", "-native", "debit"}, `saga step "debit_compensation" collides with the compensation of step "debit"`},
		{"native without component", []string{"Gift", "-steps", "debit,credit", "-native", "debit"}, "handler requires --component <name>"},
		{"unknown component", []string{"Gift", "-steps", "debit", "-native", "debit", "-component", "Bag"}, `component "bag" is not attached to Entity "player"`},
		{"entity without native", []string{"Gift", "-steps", "debit", "-component", "Wallet"}, "give them together with -native"},
		{"handler name taken", []string{"Old", "-steps", "debit", "-native", "debit", "-component", "Wallet"}, "handlerOldDebit already exists in game/handler/old_debit.go"},
	} {
		var out bytes.Buffer
		err := Run(append([]string{"add", "saga"}, append(tc.args, "--root", root)...), &out, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
		assertProjectUnchanged(t, root, before)
	}

	// 同名 handler 不在约定的文件名里也拒绝：nest 生成器按函数名登记。
	if err := os.WriteFile(filepath.Join(root, "game", "handler", "elsewhere.go"), []byte("package handler\n\nfunc handlerPayCharge() error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before = projectFileHashes(t, root)
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "Pay", Steps: []string{"charge"}, Native: []string{"charge"}, Component: "Wallet"}); err == nil || !strings.Contains(err.Error(), "handlerPayCharge already exists in game/handler/elsewhere.go") {
		t.Errorf("a Nest handler of the same name in another file: err = %v", err)
	}
	assertProjectUnchanged(t, root, before)

	// 第二次 add 同一个 saga：拒绝，第一次的生成物（已被业务改过）不动。
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "Gift", Steps: []string{"debit"}, Native: []string{"debit"}, Component: "Wallet"}); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(root, "saga", "gift", "steps.go")
	if err := os.WriteFile(edited, append([]byte(readProjectFile(t, root, "saga/gift/steps.go")), []byte("\n// business edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	before = projectFileHashes(t, root)
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "Gift", Steps: []string{"debit", "credit"}, Native: []string{"debit"}, Component: "Wallet"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second add saga Gift: err = %v, want already exists", err)
	}
	assertProjectUnchanged(t, root, before)
}

// 服务没有 Nest 时 -native 被拒绝（步骤体经 Nest Client 发起事务），且不留文件。
func TestAddSagaNativeNeedsNestInTheStepService(t *testing.T) {
	t.Parallel()
	root := copyOfSagaNativeProject(t)
	var out bytes.Buffer
	if err := Run([]string{"add", "service", "ledger", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add service: %v\n%s", err, out.String())
	}
	before := projectFileHashes(t, root)
	_, err := Add(root, AddOptions{Kind: "saga", Name: "Gift", Service: "ledger", Steps: []string{"debit"}, Native: []string{"debit"}, Component: "Wallet"})
	if err == nil || !strings.Contains(err.Error(), "service ledger runs the step consumers and needs the nest Mod") {
		t.Fatalf("native steps on a service without nest: err = %v", err)
	}
	assertProjectUnchanged(t, root, before)
}

// doctor 的 saga-steps / effect-consumer（只读 WARN）：生成了却没在服务里启动时提示，启动并排空后 OK。
func TestDoctorWarnsWhileStepsOrEffectConsumersAreNotStarted(t *testing.T) {
	t.Parallel()
	root := copyOfSagaNativeProject(t)
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "Gift", Steps: []string{"debit", "audit"}, Native: []string{"debit"}, Component: "Wallet"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "effect", Name: "player.level_up", Consumer: "game"}); err != nil {
		t.Fatal(err)
	}
	items := func() map[string]CheckItem {
		found := map[string]CheckItem{}
		for _, item := range checkStepAndEffectWiring(root, mustManifest(t, root)) {
			found[item.Name] = item
		}
		return found
	}
	got := items()
	if item := got["saga-steps:gift"]; item.Status != StatusWarn || !strings.Contains(item.Detail, "gift.StartSteps(context.Background(), registry, gift.StepDeps{})") {
		t.Errorf("saga-steps:gift before wiring = %+v, want WARN with the Init line", item)
	}
	if item := got["effect-consumer:game/startPlayerLevelUpEffectConsumer"]; item.Status != StatusWarn || !strings.Contains(item.Detail, "subscription.Drain()") {
		t.Errorf("effect-consumer before wiring = %+v, want WARN with the Init / Shutdown lines", item)
	}

	// 只启动不排空：仍 WARN。
	service := `package game

import (
	"context"

	gift "example.com/planet/saga/gift"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
)

type Service struct {
	steps    *kitsaga.Steps
	levelUps fnats.IJetStreamSubscription
}

func New() *Service                    { return &Service{} }
func (*Service) Name() app.ServiceName { return app.ServiceName("game") }
func (s *Service) Init(registry *app.Registry) error {
	steps, err := gift.StartSteps(context.Background(), registry, gift.StepDeps{})
	if err != nil {
		return err
	}
	s.steps = steps
	s.levelUps, err = startPlayerLevelUpEffectConsumer(context.Background(), registry)
	return err
}
func (*Service) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *Service) Shutdown(ctx context.Context) error {
	s.levelUps.Drain()
	return nil
}
`
	writeProjectFile(t, root, "internal/service/game/service.go", service)
	got = items()
	if item := got["saga-steps:gift"]; item.Status != StatusWarn || !strings.Contains(item.Detail, "nothing in its package calls Drain") {
		t.Errorf("saga-steps:gift started without Drain = %+v, want WARN", item)
	}
	if item := got["effect-consumer:game/startPlayerLevelUpEffectConsumer"]; item.Status != StatusOK {
		t.Errorf("effect-consumer after wiring = %+v, want OK", item)
	}
	writeProjectFile(t, root, "internal/service/game/service.go", strings.Replace(service, "	s.levelUps.Drain()\n", "	s.levelUps.Drain()\n	_ = s.steps.Drain(ctx)\n", 1))
	if item := items()["saga-steps:gift"]; item.Status != StatusOK || !strings.Contains(item.Detail, "internal/service/game/service.go") {
		t.Errorf("saga-steps:gift after wiring = %+v, want OK naming the service file", item)
	}
}

// 生成工程能编译：add saga -native 与 add effect 的骨架（没填业务）在 go build / vet 下通过，服务按 next 提示接线后
// 也通过，doctor 不再 WARN。编译当前工作树的 roost-core。
func TestGeneratedSagaNativeAndEffectSkeletonsCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated project")
	}
	t.Parallel()
	root := copyOfSagaNativeProject(t)
	var out bytes.Buffer
	for _, args := range [][]string{
		{"add", "saga", "Gift", "-steps", "debit,credit,audit", "-native", "debit,credit", "-component", "Wallet"},
		{"add", "saga", "Trade", "-steps", "reserve,deliver"},
		{"add", "effect", "player.level_up", "-consumer", "game"},
	} {
		if err := Run(append(args, "--root", root), &out, &out); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
	}
	useLocalRoostCore(t, root)
	runGeneratedGo(t, root, "build", "./...")
	runGeneratedGo(t, root, "vet", "./...")
	writeProjectFile(t, root, "internal/service/game/service.go", `package game

import (
	"context"
	"errors"

	gift "example.com/planet/saga/gift"
	trade "example.com/planet/saga/trade"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
)

type Service struct {
	gift, trade *kitsaga.Steps
	levelUps    fnats.IJetStreamSubscription
}

func New() *Service                    { return &Service{} }
func (*Service) Name() app.ServiceName { return app.ServiceName("game") }
func (s *Service) Init(registry *app.Registry) error {
	var err error
	if s.gift, err = gift.StartSteps(context.Background(), registry, gift.StepDeps{}); err != nil {
		return err
	}
	if s.trade, err = trade.StartSteps(context.Background(), registry, trade.StepDeps{}); err != nil {
		return err
	}
	s.levelUps, err = startPlayerLevelUpEffectConsumer(context.Background(), registry)
	return err
}
func (*Service) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (s *Service) Shutdown(ctx context.Context) error {
	if s.levelUps != nil {
		s.levelUps.Drain()
	}
	return errors.Join(s.gift.Drain(ctx), s.trade.Drain(ctx))
}
`)
	runGeneratedGo(t, root, "build", "./...")
	runGeneratedGo(t, root, "vet", "./...")
	items := mustDoctorItems(t, root)
	for _, name := range []string{"saga-steps:gift", "saga-steps:trade", "effect-consumer:game/startPlayerLevelUpEffectConsumer", "compile:go-list"} {
		if item, ok := items[name]; !ok || item.Status != StatusOK {
			t.Errorf("doctor %s = %+v (present=%v), want OK", name, item, ok)
		}
	}
}

// replaceOnce 把 body 里恰好一处 old 换成 new；找不到或不止一处时失败（骨架的形状变了，要同步本测试）。
func replaceOnce(t *testing.T, rel, body, old, new string) string {
	t.Helper()
	if n := strings.Count(body, old); n != 1 {
		t.Fatalf("%s: %q occurs %d times, want 1; the generated skeleton changed shape", rel, old, n)
	}
	return strings.Replace(body, old, new, 1)
}

// replaceInFunc 只在 body 里 signature 开头的函数体内替换（到下一个顶格 "}" 为止）。
func replaceInFunc(t *testing.T, rel, body, signature, old, new string) string {
	t.Helper()
	start := strings.Index(body, signature)
	if start < 0 {
		t.Fatalf("%s: no %q; the generated skeleton changed shape", rel, signature)
	}
	end := start + strings.Index(body[start:], "\n}\n")
	return body[:start] + replaceOnce(t, rel, body[start:end], old, new) + body[end:]
}

// fillSagaNativeBusiness 是新手“填业务”的一步：testdata/saga_native 按原路径覆盖（DAO 字段、钱包逻辑、handler、服务
// Init / Shutdown、真实依赖用例），生成的 steps.go、effect 载荷与消费者只在 TODO 处小改，其余保持生成的样子。
func fillSagaNativeBusiness(t *testing.T, root, daoDatabase string) {
	t.Helper()
	overlay := filepath.Join("testdata", "saga_native")
	err := filepath.WalkDir(overlay, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(path) == "README.md" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(overlay, path)
		if err != nil {
			return err
		}
		writeProjectFile(t, root, filepath.ToSlash(rel), strings.ReplaceAll(string(raw), "SAGAGEN_DB", daoDatabase))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	const stepsRel = "saga/gift/steps.go"
	steps := readProjectFile(t, root, stepsRel)
	steps = replaceOnce(t, stepsRel, steps, "\t\"github.com/tjbdwanghaibo/roost-core/framework/saga\"\n",
		"\t\"github.com/tjbdwanghaibo/roost-core/framework/saga\"\n\tfmongo \"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo\"\n\t\"go.mongodb.org/mongo-driver/v2/bson\"\n\tdb \"example.com/planet/db\"\n")
	steps = replaceOnce(t, stepsRel, steps, "\t// TODO: what the steps need besides that.\n", "\t// Mongo writes the audit step's record.\n\tMongo fmongo.IMongo\n")
	steps = replaceOnce(t, stepsRel, steps, "\tPlayerID int64 `json:\"player_id\"`\n", "\tFrom   int64 `json:\"from\"`\n\tTo     int64 `json:\"to\"`\n\tAmount int64 `json:\"amount\"`\n")
	for _, method := range []struct{ name, who string }{{"runDebit", "From"}, {"compensateDebit", "From"}, {"runCredit", "To"}, {"compensateCredit", "To"}} {
		signature := "func (bodies stepBodies) " + method.name + "("
		steps = replaceInFunc(t, stepsRel, steps, signature, "state.PlayerID", "state."+method.who)
		steps = replaceInFunc(t, stepsRel, steps, signature, "(ctx, entityID, step)", "(ctx, entityID, state.Amount, step)")
	}
	steps = replaceInFunc(t, stepsRel, steps, "func (bodies stepBodies) runAudit(", `	_ = state
	// TODO: the step's business. Until it is written the step refuses, so a saga
	// cannot report success for work nobody did.
	return saga.Completion{Success: false, Error: "gift.audit is not implemented"}, nil`, `	if _, err := bodies.deps.Mongo.Database(db.PlayerDaoDBName).Collection("gift_audit").InsertOne(ctx,
		bson.M{"_id": command.IdempotencyKey, "saga_id": command.SagaID, "from": state.From, "to": state.To, "amount": state.Amount}); err != nil {
		return saga.Completion{}, err
	}
	return saga.Completion{Success: true}, nil`)
	writeProjectFile(t, root, stepsRel, steps)

	const effectRel = "game/effects/player_level_up.go"
	writeProjectFile(t, root, effectRel, replaceOnce(t, effectRel, readProjectFile(t, root, effectRel),
		"\t// TODO: the effect's fields, e.g. PlayerID int64 `json:\"player_id\"`.\n", "\tPlayerID int64 `json:\"player_id\"`\n\tLevel    int32 `json:\"level\"`\n"))

	const consumerRel = "internal/service/game/player_level_up_consumer.go"
	consumer := readProjectFile(t, root, consumerRel)
	consumer = replaceOnce(t, consumerRel, consumer, "\t\"github.com/tjbdwanghaibo/roost-core/framework/app\"\n",
		"\tdb \"example.com/planet/db\"\n\t\"github.com/tjbdwanghaibo/roost-core/framework/app\"\n\tfmongo \"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo\"\n\t\"github.com/tjbdwanghaibo/roost-core/wiring/mods\"\n\t\"go.mongodb.org/mongo-driver/v2/bson\"\n")
	consumer = replaceOnce(t, consumerRel, consumer, "\tconsumer := kitdataengine.EffectConsumer{", `	mongoClient, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
	if !ok || mongoClient == nil {
		return nil, fmt.Errorf("level-up notices: mongo is not available")
	}
	notices := mongoClient.Database(db.PlayerDaoDBName).Collection("level_up_notices")
	consumer := kitdataengine.EffectConsumer{`)
	consumer = replaceOnce(t, consumerRel, consumer, `		_ = event
		// TODO: act on the effect. Until this is written the effect is redelivered
		// with backoff instead of being acknowledged unhandled.
		return fmt.Errorf("player.level_up consumer is not implemented; effect %s will be redelivered", envelope.EffectID)`, `		_, err := notices.InsertOne(ctx, bson.M{"_id": envelope.EffectID, "player_id": event.PlayerID, "level": event.Level})
		return err`)
	writeProjectFile(t, root, consumerRel, consumer)
}

// 新手流程从零走一遍并在真实依赖上验收（D12 方向 B）：add access / entity / component / dao / handler → add saga -native
// （送礼两步原生 + 一步 Mongo）→ add effect（升级通知）→ 填业务 → generate / build / vet / doctor → 生成工程里的
// TestSagaNativeStepsAndEffectRealDependencies 在真实 Mongo / NATS / DataEngine / Nest 上跑一笔正向、一笔补偿与一条
// effect 消费，核对恰好一次。DAO 库名是编译期常量，这里换成本次唯一的名字，用例结束删库删流。
//
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh，持有 remote-acceptance.lock；scripts/test-demo-generated.sh 调用。
func TestGeneratedSagaNativeStepsAndEffectConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("generates, compiles and runs a project against real dependencies")
	}
	if os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI") == "" || os.Getenv("ROOST_DATAENGINE_IT_NATS_URL") == "" {
		t.Skip("isolated Mongo / NATS not configured (source ~/.roost-it/roost-dataengine-it/env.sh)")
	}
	root := copyOfSagaNativeProject(t)
	var out bytes.Buffer
	for _, args := range [][]string{
		{"add", "handler", "StartGift", "--entity", "Player", "--component", "Wallet"},
		{"add", "handler", "LevelUp", "--entity", "Player", "--component", "Wallet"},
		{"add", "saga", "Gift", "-steps", "debit,credit,audit", "-native", "debit,credit", "-component", "Wallet"},
		{"add", "effect", "player.level_up", "-consumer", "game"},
	} {
		if err := Run(append(args, "--root", root), &out, &out); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
	}
	fillSagaNativeBusiness(t, root, fmt.Sprintf("sagagen_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000))
	if err := Run([]string{"generate", "--root", root}, &out, &out); err != nil {
		t.Fatalf("roost generate after filling the business: %v\n%s", err, out.String())
	}
	useLocalRoostCore(t, root)
	runGeneratedGo(t, root, "build", "./...")
	runGeneratedGo(t, root, "vet", "./...")
	items, _ := doctorItems(t, root)
	for name, item := range items {
		if item.Status == StatusFail {
			t.Errorf("doctor %s FAIL: %s", name, item.Detail)
		}
	}
	for _, name := range []string{"saga-steps:gift", "effect-consumer:game/startPlayerLevelUpEffectConsumer"} {
		if item := items[name]; item.Status != StatusOK {
			t.Errorf("doctor %s = %+v, want OK", name, item)
		}
	}
	t.Log(runGeneratedGo(t, root, "test", "-race", "-count=1", "-timeout", "6m", "-v", "-run", "^TestSagaNativeStepsAndEffectRealDependencies$", "./internal/bootstrap"))
}
