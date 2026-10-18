package roost

import (
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Saga 步骤运行时与性能报告的链接：生成的骨架注释与 roost help 引用同一份。
const (
	sagaGuideURL        = "https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/framework/guide/06-saga.md"
	sagaCapacityDocURL  = "https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/performance/SAGA-D12-20261018.md"
	sagaCapacitySummary = "one process runs about 14-18 native step commands per second on each durable; native-step sagas start to saturate at about 10/s forward and 5/s fully compensated, and past that step timeouts retry and amplify the load"
)

// sagaStep 是 -steps 里的一步：名字、是否原生步骤（-native），以及原生步骤的两个 Nest handler 名。
type sagaStep struct {
	snake, pascal string
	native        bool
}

// sagaHandlerTarget 是原生步骤 handler 锁的实体与组件（与 add handler 同一套解析）。
type sagaHandlerTarget struct {
	entity, entityPascal, component string
}

// parseSagaSteps 按书写顺序解析 -steps 与 -native：-steps 的顺序就是 Definition.Steps 的下标顺序（Saga 按下标推进、
// 按下标倒序补偿），蛇形化后同名的两步报错；-native 必须是 -steps 的子集，同样不许重复。
func parseSagaSteps(name string, steps, native []string) ([]sagaStep, error) {
	if len(steps) == 0 {
		return nil, fmt.Errorf("saga %s requires -steps", name)
	}
	out := make([]sagaStep, 0, len(steps))
	index := make(map[string]int, len(steps))
	for _, stepName := range steps {
		stepSnake := toSnake(stepName)
		if !validName(stepSnake) {
			return nil, fmt.Errorf("invalid saga step %q", stepName)
		}
		if _, seen := index[stepSnake]; seen {
			return nil, fmt.Errorf("duplicate saga step %q in -steps: each step needs its own name, listed in execution order", stepSnake)
		}
		index[stepSnake] = len(out)
		out = append(out, sagaStep{snake: stepSnake, pascal: toPascal(stepName)})
	}
	names := make([]string, len(out))
	for i, step := range out {
		names[i] = step.snake
		// 补偿方向的主题常量、登记名与 handler 文件都是 <step>Compensation / <step>_compensation：另一步叫这个名字就撞上。
		if _, clash := index[step.snake+"_compensation"]; clash {
			return nil, fmt.Errorf("saga step %q collides with the compensation of step %q (Topic%sCompensation); rename it", step.snake+"_compensation", step.snake, step.pascal)
		}
	}
	seenNative := make(map[string]bool, len(native))
	for _, stepName := range native {
		stepSnake := toSnake(stepName)
		position, ok := index[stepSnake]
		if !ok {
			return nil, fmt.Errorf("-native step %q is not in -steps (%s): -native names which of the saga's steps are Nest transactions", stepName, strings.Join(names, ", "))
		}
		if seenNative[stepSnake] {
			return nil, fmt.Errorf("duplicate saga step %q in -native", stepSnake)
		}
		seenNative[stepSnake] = true
		out[position].native = true
	}
	return out, nil
}

func (step sagaStep) handler(sagaPascal string) string { return sagaPascal + step.pascal }
func (step sagaStep) compensationHandler(sagaPascal string) string {
	return sagaPascal + step.pascal + "Compensation"
}

// sagaPaths 是一次 add saga 写的全部业务文件（只建一次）：定义、步骤运行时与原生步骤的 Nest handler（正向、补偿各一个）。
func sagaPaths(snake string, steps []sagaStep) []string {
	paths := []string{"saga/" + snake + "/definition.go", "saga/" + snake + "/steps.go"}
	for _, step := range steps {
		if step.native {
			paths = append(paths, "game/handler/"+snake+"_"+step.snake+".go", "game/handler/"+snake+"_"+step.snake+"_compensation.go")
		}
	}
	return paths
}

// planSaga 生成 add saga 的全部文件内容。任何校验失败都在写盘之前返回，错误输入不改动工程。
func planSaga(root string, m Manifest, o AddOptions, service string) ([]syncChange, error) {
	snake, pascal := toSnake(o.Name), toPascal(o.Name)
	steps, err := parseSagaSteps(o.Name, o.Steps, o.Native)
	if err != nil {
		return nil, err
	}
	anyNative := false
	for _, step := range steps {
		anyNative = anyNative || step.native
	}
	var target sagaHandlerTarget
	if anyNative {
		target, err = resolveSagaHandlerTarget(root, m, o, service)
		if err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(o.Entity) != "" || strings.TrimSpace(o.Component) != "" {
		return nil, fmt.Errorf("saga %s: -entity / -component choose what the native steps' handlers lock; give them together with -native", snake)
	}
	bodies := map[string]string{
		"saga/" + snake + "/definition.go": renderSagaDefinition(snake, steps),
		"saga/" + snake + "/steps.go":      renderSagaSteps(m.Project.Module, service, snake, pascal, steps, target),
	}
	for _, step := range steps {
		if !step.native {
			continue
		}
		bodies["game/handler/"+snake+"_"+step.snake+".go"] = renderSagaNativeHandler(m.Project.Module, snake, pascal, step, target, false)
		bodies["game/handler/"+snake+"_"+step.snake+"_compensation.go"] = renderSagaNativeHandler(m.Project.Module, snake, pascal, step, target, true)
	}
	if anyNative {
		if err := rejectExistingHandlers(root, pascal, steps); err != nil {
			return nil, err
		}
	}
	paths := sagaPaths(snake, steps)
	changes := make([]syncChange, 0, len(paths))
	for _, rel := range paths {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); err == nil {
			return nil, fmt.Errorf("%s already exists", rel)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		formatted, err := format.Source([]byte(bodies[rel]))
		if err != nil {
			return nil, fmt.Errorf("format %s: %w", rel, err)
		}
		changes = append(changes, syncChange{rel: rel, path: abs, body: formatted})
	}
	return changes, nil
}

// resolveSagaHandlerTarget 复用 add handler 的实体 / 组件解析，并要求跑这些步骤的服务带 Nest（步骤体经 Nest Client
// 发起事务）。
func resolveSagaHandlerTarget(root string, m Manifest, o AddOptions, service string) (sagaHandlerTarget, error) {
	entityName, componentName, err := resolveNestHandlerTarget(root, m, o.Entity, o.Component)
	if err != nil {
		return sagaHandlerTarget{}, fmt.Errorf("saga %s -native: %w", toSnake(o.Name), err)
	}
	resolved, err := resolveMods(effectiveServiceMods(m, service))
	if err != nil {
		return sagaHandlerTarget{}, err
	}
	if !contains(resolved, "nest") {
		return sagaHandlerTarget{}, fmt.Errorf("saga %s -native: service %s runs the step consumers and needs the nest Mod for the native steps' transactions; run: roost add mod nest -service %s", toSnake(o.Name), service, service)
	}
	return sagaHandlerTarget{entity: entityName, entityPascal: toPascal(entityName), component: toPascal(componentName)}, nil
}

// rejectExistingHandlers 拒绝与已有 Nest handler 同名的原生步骤 handler（nest 生成器按函数名登记，同名会让
// 生成失败或登记到别人的 handler）。
func rejectExistingHandlers(root, sagaPascal string, steps []sagaStep) error {
	entries, err := os.ReadDir(filepath.Join(root, "game", "handler"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var sources []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, "game", "handler", entry.Name()))
		if err != nil {
			return err
		}
		sources = append(sources, entry.Name()+"\x00"+string(raw))
	}
	for _, step := range steps {
		if !step.native {
			continue
		}
		for _, name := range []string{step.handler(sagaPascal), step.compensationHandler(sagaPascal)} {
			declaration := regexp.MustCompile(`(?m)^func handler` + name + `\(`)
			for _, source := range sources {
				file, body, _ := strings.Cut(source, "\x00")
				if declaration.MatchString(body) {
					return fmt.Errorf("Nest handler handler%s already exists in game/handler/%s; rename the saga or the step", name, file)
				}
			}
		}
	}
	return nil
}

func renderSagaDefinition(snake string, steps []sagaStep) string {
	var definitionSteps, topics strings.Builder
	for _, step := range steps {
		// No budget here: Timeout / MaxAttempts / Backoff come from the saga
		// Mod's config (saga.step_defaults, overridden per step under
		// saga.steps.<type>.<step>), filled in by Engine.Register.
		fmt.Fprintf(&definitionSteps, "\t\t{Name: %q, ForwardTopic: Topic%s, CompensateTopic: Topic%sCompensation},\n", step.snake, step.pascal, step.pascal)
		fmt.Fprintf(&topics, "\t// Topic%s is the forward topic of step %s.\n", step.pascal, step.snake)
		fmt.Fprintf(&topics, "\tTopic%s = %q\n", step.pascal, snake+"."+step.snake)
		fmt.Fprintf(&topics, "\t// Topic%sCompensation is its compensation topic.\n", step.pascal)
		fmt.Fprintf(&topics, "\tTopic%sCompensation = %q\n", step.pascal, snake+"."+step.snake+".compensate")
	}
	return fmt.Sprintf(`package %s

import (
	"context"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

const (
	Type = %q
	Version uint32 = 1
)

// The step topics. steps.go (generated next to this file) registers a
// consumer for each of them with roost-core wiring/saga Steps: Steps.Native
// for a step whose business is a Nest transaction (its Nest handler takes a
// saga.NativeStep and calls step.Complete inside the transaction), Steps.Mongo
// for any other step. Both inboxes let at most one attempt of an operation
// take effect (roost-core SAGA.md, 原生步骤执行契约 / Mongo 步骤). For a Mongo
// step that covers what its handler writes through the Mongo transaction it
// is given; a call into another service is outside that transaction and still
// has to be idempotent by Command.IdempotencyKey.
const (
%s)

// Definition lists the steps. Their timeout and retry budget is configuration,
// not code: saga.step_defaults gives every step its Timeout, MaxAttempts and
// backoff, and saga.steps.%s.<step>.<field> overrides one step (see the
// roost-core docs/framework/SAGA.md, step budgets). One operation of a step may take up
// to MaxAttempts attempts; the framework makes sure at most one of them takes
// effect (roost-core SAGA.md, 原生步骤执行契约; Mongo steps since 2026-10-06).
func Definition() saga.Definition {
	return saga.Definition{Type: Type, Version: Version, Steps: []saga.Step{
%s	}}
}

// Definitions must retain every version which still has non-terminal records.
// When changing step order or compensation semantics, keep the old definition
// here and make Definition return the new version.
func Definitions() []saga.Definition { return []saga.Definition{Definition()} }

func Register(engine *saga.Engine) error {
	for _, definition := range Definitions() {
		if err := engine.Register(definition); err != nil {
			return err
		}
	}
	return nil
}

// EmitStart is the production entry point from a Nest handler: the Saga start
// intent and current Entity mutations are committed in the same Nest WAL record.
func EmitStart(businessKey string, state []byte, deadline time.Time) error {
	return saga.EmitStart(saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}

// Start is for durable consumers, administration and recovery paths which are
// already outside a Nest transaction. Calls are idempotent for one intent.
func Start(ctx context.Context, engine *saga.Engine, businessKey string, state []byte, deadline time.Time) (saga.Record, error) {
	return engine.StartSaga(ctx, saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}
`, snake, snake, topics.String(), snake, definitionSteps.String())
}

// renderSagaSteps 生成 saga/<name>/steps.go：状态编码、步骤体、按主题常量的登记与启动入口。业务文件，只建一次。
func renderSagaSteps(module, service, snake, pascal string, steps []sagaStep, target sagaHandlerTarget) string {
	anyNative, anyMongo := false, false
	var nativeNames, mongoNames []string
	for _, step := range steps {
		if step.native {
			anyNative = true
			nativeNames = append(nativeNames, step.snake)
		} else {
			anyMongo = true
			mongoNames = append(mongoNames, step.snake)
		}
	}
	var imports strings.Builder
	imports.WriteString("\t\"context\"\n\t\"encoding/json\"\n\t\"fmt\"\n\n")
	if anyNative {
		fmt.Fprintf(&imports, "\t%s %q\n", target.entity, module+"/game/entities/"+target.entity)
		fmt.Fprintf(&imports, "\tsyncsender %q\n", module+"/game/handler/syncsender")
	}
	imports.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/app\"\n")
	if anyNative {
		imports.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n")
		imports.WriteString("\tcorenest \"github.com/tjbdwanghaibo/roost-core/framework/nest\"\n")
	}
	imports.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/framework/saga\"\n")
	if anyNative {
		imports.WriteString("\t\"github.com/tjbdwanghaibo/roost-core/wiring/mods\"\n")
	}
	imports.WriteString("\tkitsaga \"github.com/tjbdwanghaibo/roost-core/wiring/saga\"\n")

	var kinds strings.Builder
	switch {
	case anyNative && anyMongo:
		fmt.Fprintf(&kinds, "// Native steps (Nest transactions on %s): %s. Mongo steps: %s.\n", target.entityPascal, strings.Join(nativeNames, ", "), strings.Join(mongoNames, ", "))
	case anyNative:
		fmt.Fprintf(&kinds, "// Every step is a native step (a Nest transaction on %s): %s.\n", target.entityPascal, strings.Join(nativeNames, ", "))
	default:
		fmt.Fprintf(&kinds, "// Every step is a Mongo step: %s. A step whose business is a Nest\n// transaction belongs on the native path instead (roost add saga ... -native <step>\n// generates it; for an existing saga copy the shape from another saga's steps.go).\n", strings.Join(mongoNames, ", "))
	}

	var deps strings.Builder
	deps.WriteString("// StepDeps is what the step bodies need from the service. StartSteps fills the\n// framework's part from the registry; add the business's own (a typed client of\n// another service for a Mongo step, say) and pass them from the service Init.\ntype StepDeps struct {\n")
	if anyNative {
		deps.WriteString("\t// Nest runs the native steps' transactions. Nil: StartSteps takes the\n\t// service's Nest client from the registry.\n\tNest corenest.Client\n")
	}
	deps.WriteString("\t// TODO: what the steps need besides that.\n}\n")

	var state strings.Builder
	state.WriteString("// State is this saga's data: what the start handler encodes with EncodeState and\n// passes to EmitStart, and what every step reads back from Command().Payload.\n// JSON, so a step in another language could read it. Add the fields the steps\n// need; a field a running saga was started without decodes as its zero value.\ntype State struct {\n")
	if anyNative {
		fmt.Fprintf(&state, "\t// %sID is the %s the native steps change (the id the %s was created\n\t// with, not the Nest entity id). TODO: most sagas name more than one, e.g.\n\t// From and To for a gift; give each step its own.\n\t%sID int64 `json:\"%s_id\"`\n", target.entityPascal, target.entityPascal, target.entityPascal, target.entityPascal, target.entity)
	} else {
		state.WriteString("\t// TODO: the saga's fields.\n")
	}
	state.WriteString("}\n")

	var register, bodies strings.Builder
	for _, step := range steps {
		forward, compensate := "run"+step.pascal, "compensate"+step.pascal
		if step.native {
			fmt.Fprintf(&register, "\tsteps.Native(%q, Topic%s, bodies.%s)\n", step.snake, step.pascal, forward)
			fmt.Fprintf(&register, "\tsteps.Native(%q, Topic%sCompensation, bodies.%s)\n", step.snake+"_compensation", step.pascal, compensate)
			writeNativeStepBody(&bodies, snake, pascal, step, target, false)
			writeNativeStepBody(&bodies, snake, pascal, step, target, true)
		} else {
			fmt.Fprintf(&register, "\tsteps.Mongo(%q, Topic%s, bodies.%s)\n", step.snake, step.pascal, forward)
			fmt.Fprintf(&register, "\tsteps.Mongo(%q, Topic%sCompensation, bodies.%s)\n", step.snake+"_compensation", step.pascal, compensate)
			writeMongoStepBody(&bodies, snake, step, false)
			writeMongoStepBody(&bodies, snake, step, true)
		}
	}

	var lookup strings.Builder
	if anyNative {
		lookup.WriteString(`	if deps.Nest == nil {
		client, ok := app.Lookup[corenest.Client](registry, mods.ModNest)
		if !ok || client == nil {
			return nil, fmt.Errorf("saga %s steps: the nest client is not available; the service running the native steps needs the nest Mod", Type)
		}
		deps.Nest = client
	}
`)
	}

	return fmt.Sprintf(`package %[1]s

import (
%[2]s)

// The step consumers of saga %[1]s (roost add saga; a business file: generated
// once, never rewritten by roost). Every step has a forward body and a
// compensation body, registered by RegisterSteps in the order of -steps (the
// order of Definition().Steps).
//
%[3]s//
// Wiring: the service owns Init and Shutdown and roost does not rewrite them.
// Start the consumers in Init (the saga Mod has started by then) and drain them
// in Shutdown:
//
//	// Init
//	steps, err := %[1]s.StartSteps(context.Background(), registry, %[1]s.StepDeps{})
//	// Shutdown: finish what is running within the stop budget, then stop.
//	if err := steps.Drain(ctx); err != nil { steps.Stop() }
//
// roost project doctor warns (saga-steps:%[1]s) while nothing calls StartSteps.
//
// Capacity (maintainer decision 2026-10-18: accepted and documented, not
// optimised): a step's durable is shared by every replica of the service and
// each replica takes one command at a time from it. One process runs about
// 14-18 native step commands per second on a durable; sagas made of native
// steps start to saturate at about 10/s forward and 5/s fully compensated, and
// past that step timeouts retry and amplify the load (each operation still
// takes effect exactly once, but most sagas end failed / compensated). Mongo
// steps measured 20/s forward without saturating. More load needs more
// processes: shard by sid (each process owns its sids' entities) and add an
// admission check on the native steps (kitsaga.WithAdmit, below) so a command
// runs on the process that owns its entity. Measurements:
// %[5]s
// Guide: %[6]s §2.1

// StepsGroup names the step consumers: each step's JetStream durable is
// <group>-<step name>, shared by every replica of the service, and the Mongo
// steps' inbox is _<group>_step_inbox in the saga database. Do not rename it
// once deployed: commands left on the old durables are not handed to new ones.
const StepsGroup = %[7]q

%[8]s
%[9]s
// EncodeState encodes the saga's data for EmitStart.
func EncodeState(state State) ([]byte, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("saga %%s: encode state: %%w", Type, err)
	}
	return raw, nil
}

// DecodeState reads the saga's data back from a step command's payload.
func DecodeState(payload []byte) (State, error) {
	var state State
	if err := json.Unmarshal(payload, &state); err != nil {
		return State{}, fmt.Errorf("saga %%s: decode state: %%w", Type, err)
	}
	return state, nil
}

// StartSteps registers this saga's steps and starts their consumers; call it
// from the service Init and Drain the returned Steps in Shutdown.
func StartSteps(ctx context.Context, registry *app.Registry, deps StepDeps) (*kitsaga.Steps, error) {
	if registry == nil {
		return nil, fmt.Errorf("saga %%s steps: nil registry", Type)
	}
%[10]s	steps, err := kitsaga.NewSteps(registry, StepsGroup)
	if err != nil {
		return nil, fmt.Errorf("saga %%s steps: %%w", Type, err)
	}
	RegisterSteps(steps, deps)
	if err := steps.Start(ctx); err != nil {
		return nil, fmt.Errorf("saga %%s steps: %%w", Type, err)
	}
	return steps, nil
}

// RegisterSteps adds every step of this saga to steps, by the topic constants
// in definition.go, so a durable and its filter cannot drift apart. Use it
// instead of StartSteps to put several sagas on one Steps.
//
// No admission is set: every replica takes any command. That is right for one
// process per service. With several processes that each own some entities
// (players bound to a sid), a native step that runs on the wrong process loads
// another process's entity: give each Native call
// kitsaga.WithAdmit(func(ctx context.Context, command saga.Command) error { ... })
// that refuses a command whose entity this process does not own (the refused
// delivery is offered to another replica), as game-demo's gift saga does.
func RegisterSteps(steps *kitsaga.Steps, deps StepDeps) {
	bodies := stepBodies{deps: deps}
%[11]s}

%[13]stype stepBodies struct{ deps StepDeps }
%[12]s`, snake, imports.String(), kinds.String(), sagaCapacitySummary, sagaCapacityDocURL, sagaGuideURL, service+"-"+snake, deps.String(), state.String(), lookup.String(), register.String(), bodies.String(), stepContract(anyNative, anyMongo))
}

// stepContract 是 stepBodies 上方的契约说明，只写本 saga 用到的步骤种类。
func stepContract(anyNative, anyMongo bool) string {
	var out strings.Builder
	out.WriteString("// stepBodies holds the step bodies and what they need.\n")
	if anyNative {
		out.WriteString(`//
// A native step body hands the step to its Nest handler (game/handler), which
// changes the entity and calls step.Complete in the same transaction; the
// framework lets at most one attempt of the operation take effect, so neither
// needs idempotency of its own. A body returning an error means the outcome is
// unknown (the Nest commit may have happened): the attempt waits for its
// receipt until the command deadline and the coordinator retries after the step
// timeout. A payload that does not decode is such an error too; refusing needs
// the transaction (step.Complete(false, reason) in the handler).
`)
	}
	if anyMongo {
		out.WriteString(`//
// A Mongo step body runs inside a Mongo transaction (its ctx): what it writes
// through ctx commits together with the command's receipt, so that part takes
// effect at most once. A call into another service is outside the transaction
// and an attempt fenced at commit may already have made it: make it idempotent
// by command.IdempotencyKey (pass it as the call's request id). A coded business
// refusal is saga.Completion{Success: false, Error: reason}, not retried; an
// error rolls the transaction back and is retried with backoff.
`)
	}
	out.WriteString(`//
// A refused forward step makes the coordinator compensate the steps that
// completed; a refused compensation is manual_required for an operator.
`)
	return out.String()
}

func writeNativeStepBody(out *strings.Builder, snake, sagaPascal string, step sagaStep, target sagaHandlerTarget, compensation bool) {
	method, handler, what := "run"+step.pascal, step.handler(sagaPascal), "step "+step.snake
	if compensation {
		method, handler, what = "compensate"+step.pascal, step.compensationHandler(sagaPascal), "the compensation of "+step.snake
	}
	fmt.Fprintf(out, `
// %[1]s is %[2]s: handler%[3]s runs it as a Nest transaction.
func (bodies stepBodies) %[1]s(ctx context.Context, step saga.NativeStep) error {
	state, err := DecodeState(step.Command().Payload)
	if err != nil {
		return err
	}
	// The %[4]s this step changes. Nest locks it and, if it is not loaded in
	// this process, loads it on its io pool first.
	entityID, err := entity.BuildEntityID(state.%[4]sID, %[5]s.EntityKind%[4]s)
	if err != nil {
		return err
	}
	return syncsender.New%[3]sSender(bodies.deps.Nest).Sync_%[3]s(ctx, entityID, step)
}
`, method, what, handler, target.entityPascal, target.entity)
}

func writeMongoStepBody(out *strings.Builder, snake string, step sagaStep, compensation bool) {
	method, what := "run"+step.pascal, "step "+step.snake
	refusal := snake + "." + step.snake + " is not implemented"
	if compensation {
		method, what = "compensate"+step.pascal, "the compensation of "+step.snake
		refusal = snake + "." + step.snake + " compensation is not implemented"
	}
	fmt.Fprintf(out, `
// %[1]s is %[2]s, run inside a Mongo transaction (ctx).
func (bodies stepBodies) %[1]s(ctx context.Context, command saga.Command) (saga.Completion, error) {
	state, err := DecodeState(command.Payload)
	if err != nil {
		return saga.Completion{Success: false, Error: err.Error()}, nil
	}
	_ = state
	// TODO: the step's business. Until it is written the step refuses, so a saga
	// cannot report success for work nobody did.
	return saga.Completion{Success: false, Error: %[3]q}, nil
}
`, method, what, refusal)
}

// renderSagaNativeHandler 生成原生步骤的 Nest handler 骨架（game/handler/<saga>_<step>[_compensation].go，业务文件）。
func renderSagaNativeHandler(module, snake, sagaPascal string, step sagaStep, target sagaHandlerTarget, compensation bool) string {
	handler, what := step.handler(sagaPascal), "step "+step.snake
	refusal := snake + "." + step.snake + " is not implemented"
	if compensation {
		handler, what = step.compensationHandler(sagaPascal), "the compensation of step "+step.snake
		refusal = snake + "." + step.snake + " compensation is not implemented"
	}
	refusedMeans := "the coordinator then compensates\n// the steps that completed"
	if compensation {
		refusedMeans = "a refused compensation is\n// manual_required for an operator"
	}
	return fmt.Sprintf(`package handler

import (
	%[1]s %[2]q
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// handler%[3]s is %[4]s of saga %[5]s,
// a native step (roost add saga -native; a business file). It is one Nest transaction:
// change the %[6]s through target.%[7]sComp(), then call step.Complete in the
// same transaction. The change, the command's receipt and the completion the
// coordinator waits for are one WAL record: a redelivery replays the stored
// completion instead of running again, and a crash before the commit leaves
// none of the three (roost-core SAGA.md, 原生步骤执行契约).
//
// A business refusal also commits: return step.Complete(false, reason) without
// changing the %[6]s; %[8]s. Return an error only for an
// infrastructure failure: the outcome is then unknown and the coordinator
// retries after the step timeout. Add parameters after target as the step needs them and pass
// them from the step body in saga/%[5]s/steps.go; roost generate updates the
// Sender.
//
//roost:nest rollback=undo durability=strict
func handler%[3]s(target %[1]s.I%[7]sEntity, step saga.NativeStep) error {
	// TODO: call target.%[7]sComp().<BusinessMethod>(); on a coded business
	// error return step.Complete(false, reason). Until then the step refuses.
	return step.Complete(false, %[9]q)
}
`, target.entity, module+"/game/entities/"+target.entity, handler, what, snake, target.entityPascal, target.component, refusedMeans, refusal)
}

// addSaga 是 roost add saga：校验与生成全部文件内容（planSaga）之后才写盘，再把 saga Mod 接到服务上并同步工程
// （同步里的 generate 为新的原生步骤 handler 生成 Sender）。同步失败时撤回本次写的文件。
func addSaga(root string, m Manifest, options AddOptions) ([]string, error) {
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, err
	}
	service := toSnake(options.Service)
	if service == "" {
		services := sortedServiceNames(m)
		if len(services) != 1 {
			return nil, fmt.Errorf("saga requires -service when the project has multiple services")
		}
		service = services[0]
	}
	spec, ok := m.Services[service]
	if !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	changes, err := planSaga(root, m, options, service)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(changes))
	for i, change := range changes {
		paths[i] = change.rel
	}
	backups, err := captureFiles(root, paths)
	if err != nil {
		return nil, err
	}
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	created, err := captureFiles(root, paths)
	if err != nil {
		return paths, restoreFiles(root, backups, err)
	}
	previous := manifestWithService(m, service, ServiceSpec{Framework: spec.Framework, Mods: append([]string(nil), spec.Mods...), Uses: spec.Uses, Rpcs: spec.Rpcs, UsesRpcs: spec.UsesRpcs})
	spec.Mods = uniqueSorted(append(append([]string(nil), spec.Mods...), "saga"))
	m.Services[service] = spec
	m.Features = uniqueSorted(append(m.Features, "saga"))
	m.Sagas = uniqueSorted(append(m.Sagas, toSnake(options.Name)))
	if err := commitManifestSync(root, manifestBefore, m); err != nil {
		return paths, restoreFilesIfCurrent(root, backups, created, err)
	}
	paths = append(paths, "roost.yaml", "internal/bootstrap/generated.go")
	// The saga Mod (and whatever it pulled in) gets its config section in
	// the service's configs, like a Mod chosen at project creation.
	configs, err := appendModConfigSections(root, previous, m, service, options.Warnings)
	if err != nil {
		return paths, err
	}
	return append(paths, configs...), nil
}

// printSagaNext 是 add saga 之后的提示：服务的 Init / Shutdown 归业务，生成器不改写，只给要加的两行与容量。
func printSagaNext(stdout io.Writer, name string, native bool) {
	snake := toSnake(name)
	if native {
		fmt.Fprintf(stdout, "next: write the native steps' business in game/handler/%s_*.go (change the entity, then step.Complete in the same transaction), the step bodies and State in saga/%s/steps.go, then roost generate\n", snake, snake)
	} else {
		fmt.Fprintf(stdout, "next: write the step bodies and State in saga/%s/steps.go (every step is a Mongo step; -native <step> puts a Nest-transaction step on the native path)\n", snake)
	}
	fmt.Fprintf(stdout, "next: start the step consumers in the service (roost does not rewrite Init / Shutdown):\n")
	fmt.Fprintf(stdout, "  Init:     steps, err := %s.StartSteps(context.Background(), registry, %s.StepDeps{})   // import <module>/saga/%s\n", snake, snake, snake)
	fmt.Fprintf(stdout, "  Shutdown: if err := steps.Drain(ctx); err != nil { steps.Stop() }\n")
	fmt.Fprintf(stdout, "next: start a saga from a Nest handler: state, _ := %s.EncodeState(%s.State{...}); %s.EmitStart(businessKey, state, deadline)\n", snake, snake, snake)
	fmt.Fprintf(stdout, "capacity: %s; more load needs more processes sharded by sid (see roost help saga)\n", sagaCapacitySummary)
	fmt.Fprintln(stdout, "guide: "+sagaGuideURL)
}
