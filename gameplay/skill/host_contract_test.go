package skill

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestNilHostAdvanceAndPassiveReturnError(t *testing.T) {
	program := *summonCostingProgram(t)
	program.activationKind = "passive"
	for _, name := range []string{"advance", "passive"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Errorf("nil host caused panic: %v", value)
				}
			}()
			runtime := NewRuntime(nil, RuntimeOptions{})
			var err error
			if name == "advance" {
				err = runtime.Advance(1)
			} else {
				_, err = runtime.ActivatePassive(&program, EventContext{EventID: 1, RootEventID: 1, Source: 1})
			}
			if !errors.Is(err, ErrProgramInvariant) {
				t.Fatalf("nil host error=%v", err)
			}
		})
	}
}

func TestCheckpointCapabilityFailureKeepsSentinel(t *testing.T) {
	flow := `{"flow":"wait","ticks":5,"then":{"flow":"finish"}}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("restore-cost", `[{"resource":"mana","amount":1}]`, flow)), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })
	_, err = RestoreRuntime(&emptyTableHost{MemoryHost: host}, RuntimeOptions{}, checkpoint, resolver)
	if !errors.Is(err, ErrCheckpointProgram) || !errors.Is(err, ErrHostCapabilityMissing) {
		t.Fatalf("checkpoint lost capability sentinel: %v", err)
	}
}

func TestHostAdmissionCacheRemainsBounded(t *testing.T) {
	runtime := NewRuntime(runtimeTestHost(DefaultCompileEnvironment()), RuntimeOptions{})
	for i := range 4096 {
		program := &Program{id: fmt.Sprintf("generated-%d", i), hostRequirements: []HostCapability{{Kind: HostCapabilitySummon}}}
		if err := runtime.admitHostCapabilitiesLocked(program); err != nil {
			t.Fatal(err)
		}
	}
	if len(runtime.hostAdmitted) > 1024 {
		t.Fatalf("host admission retained %d one-shot programs", len(runtime.hostAdmitted))
	}
}

func hostCapabilitySkill(id, costs, flow string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.hostcap.` + id + `","name":"HostCap","description":"Host capability.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":` + costs + `,"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
}

// summonSpawnFlow：summon 带一个 projectile 衍生物，motion 由参数给出。
func summonSpawnFlow(spawn string) string {
	return `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":` + spawn + `},{"flow":"finish"}]}`
}

func withoutHostCapabilities(table HostCapabilityTable, drop ...HostCapability) HostCapabilityTable {
	result := HostCapabilityTable{HostCapabilityCatalog: HostCapabilityCatalog{Revision: table.Revision}}
	for _, item := range table.Items() {
		dropped := false
		for _, candidate := range drop {
			dropped = dropped || candidate == item
		}
		if !dropped {
			result = MergeHostCapabilities(result, tableOf(item))
		}
	}
	return result
}

func tableOf(item HostCapability) HostCapabilityTable {
	var table HostCapabilityTable
	column, _ := hostCapabilityColumnOf(item.Kind)
	if column.keys == nil {
		table.Summon = true
	} else {
		*column.keys(&table) = []string{item.Key}
	}
	return table
}

// environmentForHost 是业务的做法：环境里的 Host 段取自 Host 声明的表。
func environmentForHost(table HostCapabilityTable) CompileEnvironment {
	environment := DefaultCompileEnvironment()
	environment.Host = table.HostCapabilityCatalog
	environment.Digest = AuthorityDigest(environment)
	return environment
}

// noCollisionHost 不支持 collision 步骤，声明与行为一致。
type noCollisionHost struct{ *MemoryHost }

func (host *noCollisionHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if _, ok := command.Motion.(CollisionMotionStep); ok {
		return SpawnStepResult{}, fmt.Errorf("host: collision motion is not supported")
	}
	return host.MemoryHost.StepSpawn(command, state)
}

func (host *noCollisionHost) HostCapabilities() HostCapabilityTable {
	return withoutHostCapabilities(host.MemoryHost.HostCapabilities(), HostCapability{HostCapabilityMotionStep, "collision"})
}

// noSteeringHost 不接受 steering 步骤（RR-20261006-37）：Runtime 对每个运动衍生物每步都发
// SteeringMotionStep，不论定义写没写 steering。
type noSteeringHost struct{ *MemoryHost }

func (host *noSteeringHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if _, ok := command.Motion.(SteeringMotionStep); ok {
		return SpawnStepResult{}, fmt.Errorf("host: steering motion is not supported")
	}
	return host.MemoryHost.StepSpawn(command, state)
}

func (host *noSteeringHost) HostCapabilities() HostCapabilityTable {
	return withoutHostCapabilities(host.MemoryHost.HostCapabilities(), HostCapability{HostCapabilityMotionStep, "steering"})
}

// noModifierHost 不支持属性修正，声明与行为一致。
type noModifierHost struct{ *MemoryHost }

func (host *noModifierHost) Apply(command EffectCommand) (EffectResult, error) {
	if _, ok := command.Payload.(AttributeModifierCommand); ok {
		return EffectResult{}, fmt.Errorf("host: attribute modifiers are not supported")
	}
	return host.MemoryHost.Apply(command)
}

func (host *noModifierHost) HostCapabilities() HostCapabilityTable {
	return withoutHostCapabilities(host.MemoryHost.HostCapabilities(), HostCapability{HostCapabilityModifierOperation, "add"}, HostCapability{HostCapabilityModifierOperation, "mul_bp"})
}

type hostCapabilityCase struct {
	name    string
	missing HostCapability
	path    string
	costs   string
	flow    string
	host    func(CompileEnvironment) (Host, *MemoryHost)
}

func hostCapabilityCases() []hostCapabilityCase {
	collision := summonSpawnFlow(`{"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"collision":{"layers":["terrain"],"response":"stop"},"completion":{"type":"end"}}}`)
	return []hostCapabilityCase{
		{
			name: "summon on a host without owned entities", missing: HostCapability{Kind: HostCapabilitySummon}, path: "$.phases[0].on.enter.steps[0].effect",
			costs: `[{"resource":"mana","amount":10}]`,
			flow:  `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10}},{"flow":"finish"}]}`,
			host: func(environment CompileEnvironment) (Host, *MemoryHost) {
				inner := runtimeTestHost(environment)
				return &hostWithoutOwnedContract{inner: inner}, inner
			},
		},
		{
			name: "collision motion on a host without collision", missing: HostCapability{HostCapabilityMotionStep, "collision"}, path: "$.phases[0].on.enter.steps[0].spawn.motion.collision",
			costs: `[{"resource":"mana","amount":10}]`, flow: collision,
			host: func(environment CompileEnvironment) (Host, *MemoryHost) {
				inner := runtimeTestHost(environment)
				return &noCollisionHost{MemoryHost: inner}, inner
			},
		},
		{
			// RR-20261006-37：定义没写 steering，修前在关掉 steering 槽位的环境里照样编译。
			name: "moving spawn without steering on a host without steering steps", missing: HostCapability{HostCapabilityMotionStep, "steering"}, path: "$.phases[0].on.enter.steps[0].spawn.motion.steering",
			costs: `[{"resource":"mana","amount":10}]`,
			flow:  summonSpawnFlow(`{"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}}`),
			host: func(environment CompileEnvironment) (Host, *MemoryHost) {
				inner := runtimeTestHost(environment)
				return &noSteeringHost{MemoryHost: inner}, inner
			},
		},
		{
			name: "attribute modifier on a host without modifiers", missing: HostCapability{HostCapabilityModifierOperation, "add"}, path: "$.phases[0].on.enter.steps[0].effect.operation",
			costs: `[{"resource":"mana","amount":10}]`,
			flow:  `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"attribute_modifier","target":"$caster","attribute":"move_speed","operation":"add","value":5,"duration_ticks":10}},{"flow":"finish"}]}`,
			host: func(environment CompileEnvironment) (Host, *MemoryHost) {
				inner := runtimeTestHost(environment)
				return &noModifierHost{MemoryHost: inner}, inner
			},
		},
	}
}

// 编译能过、Host 不支持的取值：环境取自 Host 声明的表时在编译期被拒，并点名缺哪一项。
func TestHostCapabilityMissingIsRejectedAtCompileTime(t *testing.T) {
	for _, test := range hostCapabilityCases() {
		t.Run(test.name, func(t *testing.T) {
			host, _ := test.host(DefaultCompileEnvironment())
			environment := environmentForHost(host.(HostCapabilityProvider).HostCapabilities())
			_, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("compile", test.costs, test.flow)), environment)
			want := "environment host capability table lacks " + test.missing.String()
			found := false
			for _, diagnostic := range diagnostics {
				if diagnostic.Code == DiagnosticHostCapabilityMissing && diagnostic.Path == test.path && diagnostic.Message == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostics = %v, want %s at %s: %q", diagnostics, DiagnosticHostCapabilityMissing, test.path, want)
			}
			// 同一定义在完整的默认环境里能编译：缺的只是这个 Host 的能力。
			_, diagnostics = Compile(mustParseJSON(t, hostCapabilitySkill("compile", test.costs, test.flow)), DefaultCompileEnvironment())
			requireNoErrors(t, diagnostics)
		})
	}
}

// 用别的环境编译出的 Program 交给能力不够的 Host：Runtime 在启动时按同一张表拒绝，不扣费、
// 不到施法中途才失败。
func TestRuntimeRefusesProgramsOutsideTheHostTable(t *testing.T) {
	for _, test := range hostCapabilityCases() {
		t.Run(test.name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			program, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("runtime", test.costs, test.flow)), environment)
			requireNoErrors(t, diagnostics)
			host, inner := test.host(environment)
			runtime := NewRuntime(host, RuntimeOptions{})
			_, err := runtime.Activate(program, CastInput{Caster: 1})
			if !errors.Is(err, ErrHostCapabilityMissing) || !errors.Is(err, ErrHostContractViolation) || !strings.Contains(err.Error(), test.missing.String()) {
				t.Fatalf("Activate err = %v, want ErrHostCapabilityMissing naming %s", err, test.missing)
			}
			if mana := inner.ResourceForTest(1, "mana"); mana != 100 {
				t.Fatalf("mana = %d after a refused start, want 100 (nothing paid)", mana)
			}
			if err := runtime.RegisterAbility(AbilityRegistration{Owner: 1, Handle: 1, Program: program}); !errors.Is(err, ErrHostCapabilityMissing) {
				t.Fatalf("RegisterAbility err = %v, want ErrHostCapabilityMissing", err)
			}
		})
	}
}

// checkpoint 恢复到能力不够的 Host 上同样被拒绝。
func TestRestoreRefusesProgramsOutsideTheHostTable(t *testing.T) {
	flow := `{"flow":"wait","ticks":5,"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10}},{"flow":"finish"}]}}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("restore", `[]`, flow)), environment)
	requireNoErrors(t, diagnostics)
	inner := runtimeTestHost(environment)
	runtime := NewRuntime(inner, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })
	if _, err := RestoreRuntime(inner, RuntimeOptions{}, checkpoint, resolver); err != nil {
		t.Fatalf("restore onto the full host: %v", err)
	}
	_, err = RestoreRuntime(&hostWithoutOwnedContract{inner: inner}, RuntimeOptions{}, checkpoint, resolver)
	if !errors.Is(err, ErrCheckpointProgram) || !strings.Contains(err.Error(), "summon") {
		t.Fatalf("restore onto a host without summons: err = %v, want ErrCheckpointProgram naming summon", err)
	}
}

func hostCapabilityProbeHost(environment CompileEnvironment) *MemoryHost {
	host := NewMemoryHost(AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	host.ConfigureGameplayCatalog(environment.Gameplay)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{1: 1, 2: 2, 3: 3}})
	return host
}

// MemoryHost 声明的每一项都真的支持，表外的属性 / 资源被拒绝。
func TestMemoryHostCapabilitiesMatchItsBehavior(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := hostCapabilityProbeHost(environment)
	if err := CheckHostCapabilities(host, environment.Gameplay, HostCapabilityProbe{Entity: 1}); err != nil {
		t.Fatal(err)
	}
	if err := HostSupportsEnvironment(host, environment); err != nil {
		t.Fatalf("MemoryHost does not cover the default environment: %v", err)
	}
	if mana := host.ResourceForTest(1, "mana"); mana != 100 {
		t.Fatalf("conformance probe changed mana to %d", mana)
	}
}

// 守卫能红：声明与行为不一致的 Host（变异）都被 CheckHostCapabilities 点名。
type overDeclaredHost struct {
	*MemoryHost
	extra HostCapabilityTable
}

func (host *overDeclaredHost) HostCapabilities() HostCapabilityTable {
	return MergeHostCapabilities(host.MemoryHost.HostCapabilities(), host.extra)
}

type declaredSummonWithoutContract struct{ hostWithoutOwnedContract }

func (host *declaredSummonWithoutContract) HostCapabilities() HostCapabilityTable {
	return host.inner.HostCapabilities()
}

type lyingCollisionHost struct{ noCollisionHost }

func (host *lyingCollisionHost) HostCapabilities() HostCapabilityTable {
	return host.MemoryHost.HostCapabilities()
}

type lyingModifierHost struct{ noModifierHost }

func (host *lyingModifierHost) HostCapabilities() HostCapabilityTable {
	return host.MemoryHost.HostCapabilities()
}

type outsideCatalogAttributeHost struct{ *MemoryHost }

func (host *outsideCatalogAttributeHost) Read(request ReadRequest) (ReadResult, error) {
	if attribute, ok := request.Payload.(AttributeRead); ok && attribute.Attribute == 4 {
		attribute.Attribute = 1 // 故意把未声明 handle 当成合法字段，保留负例语义。
		request.Payload = attribute
	}
	return host.MemoryHost.Read(request)
}

func TestCheckHostCapabilitiesCatchesMisdeclaredHosts(t *testing.T) {
	environment := DefaultCompileEnvironment()
	extended := environment.Gameplay
	extended.Resources.Entries = append(append([]ResourceCatalogEntry(nil), extended.Resources.Entries...), ResourceCatalogEntry{Handle: 2, Key: "rage", Maximum: 100})
	for _, test := range []struct {
		name    string
		host    func() Host
		catalog GameplayCatalog
		want    string
	}{
		{"declares collision but StepSpawn refuses it", func() Host {
			return &lyingCollisionHost{noCollisionHost{MemoryHost: hostCapabilityProbeHost(environment)}}
		}, environment.Gameplay, `motion_step "collision": declared but StepSpawn`},
		{"declares modifiers but Apply refuses them", func() Host {
			return &lyingModifierHost{noModifierHost{MemoryHost: hostCapabilityProbeHost(environment)}}
		}, environment.Gameplay, `modifier_operation "add": declared but Apply`},
		{"declares summon without OwnedEntityRuntimeHost", func() Host {
			return &declaredSummonWithoutContract{hostWithoutOwnedContract{inner: hostCapabilityProbeHost(environment)}}
		}, environment.Gameplay, "summon: declared but"},
		{"declares a resource it does not know", func() Host {
			return &overDeclaredHost{MemoryHost: hostCapabilityProbeHost(environment), extra: HostCapabilityTable{Resources: []string{"rage"}}}
		}, extended, `resource "rage": declared but`},
		{"answers attributes outside the catalog with 0", func() Host {
			host := NewMemoryHost(AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Resources: map[string]int64{"mana": 100}})
			return &outsideCatalogAttributeHost{MemoryHost: host}
		}, environment.Gameplay, `attribute "<handle 4>": outside the catalog but Read(AttributeRead) answered`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckHostCapabilities(test.host(), test.catalog, HostCapabilityProbe{Entity: 1})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CheckHostCapabilities = %v, want a failure containing %q", err, test.want)
			}
		})
	}
}

// 环境的 Host 段是封闭集合的子集，算进 authority digest。
func TestEnvironmentHostCapabilityCatalogIsValidatedAndDigested(t *testing.T) {
	base := DefaultCompileEnvironment()
	narrowed := base
	narrowed.Host = withoutHostCapabilities(HostCapabilityTableOf(base), HostCapability{HostCapabilityMotionStep, "carry"}).HostCapabilityCatalog
	if AuthorityDigest(narrowed) == base.Digest {
		t.Fatal("narrowing the host capability catalog did not change the authority digest")
	}
	for _, test := range []struct {
		name   string
		mutate func(*HostCapabilityCatalog)
		path   string
	}{
		{"unknown motion step", func(c *HostCapabilityCatalog) { c.MotionSteps = append(c.MotionSteps, "teleport") }, "$.host.motion_steps[6]"},
		{"duplicate spawn kind", func(c *HostCapabilityCatalog) { c.SpawnKinds = append(c.SpawnKinds, "dash") }, "$.host.spawn_kinds"},
		{"runtime-applied numeric field", func(c *HostCapabilityCatalog) { c.SpawnNumericFields = append(c.SpawnNumericFields, "speed") }, "$.host.spawn_numeric_fields[3]"},
		{"minion without summon", func(c *HostCapabilityCatalog) { c.Summon = false }, "$.host.summon"},
		{"missing revision", func(c *HostCapabilityCatalog) { c.Revision = "" }, "$.host.revision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			environment.Host = cloneHostCapabilityCatalog(environment.Host)
			test.mutate(&environment.Host)
			environment.Digest = AuthorityDigest(environment)
			_, diagnostics := Compile(mustParseJSON(t, minimalSkillJSON), environment)
			for _, diagnostic := range diagnostics {
				if diagnostic.Code == DiagnosticCatalogHostPolicy && diagnostic.Path == test.path {
					return
				}
			}
			t.Fatalf("diagnostics = %v, want %s at %s", diagnostics, DiagnosticCatalogHostPolicy, test.path)
		})
	}
}

// 编译器对每一项需求都查表：对每个种子、默认表的每一项，从环境里去掉这一项（属性改成不可读）
// 后重新编译——这一项在 Program 的需求里时必须报 HOST_CAPABILITY_MISSING 并点名它，且只报它；
// 不在需求里时必须照常编译、需求不变。
func TestCompilerConsultsTheTableForEveryRequirement(t *testing.T) {
	base := DefaultCompileEnvironment()
	items := HostCapabilityTableOf(base).Items()
	checked := 0
	for _, seed := range mutationSeeds(t) {
		definition, err := Parse(seed.data)
		if err != nil {
			t.Fatalf("%s: %v", seed.name, err)
		}
		program, diagnostics := Compile(definition, base)
		requireNoErrors(t, diagnostics)
		required := make(map[HostCapability]bool, len(program.hostRequirements))
		for _, item := range program.hostRequirements {
			required[item] = true
		}
		for _, item := range items {
			if item.Kind == HostCapabilityResource {
				continue // 资源列就是 catalog 本身，删掉条目是 catalog 引用错误，由 CAPABILITY_UNKNOWN 管
			}
			environment := withoutHostCapability(base, item)
			narrowed, diagnostics := Compile(definition, environment)
			checked++
			if !required[item] {
				if diagnosticsHaveErrors(diagnostics) {
					t.Errorf("%s: removing unused %s broke compilation: %v", seed.name, item, diagnostics)
				} else if fmt.Sprint(narrowed.hostRequirements) != fmt.Sprint(program.hostRequirements) {
					t.Errorf("%s: removing unused %s changed requirements to %v", seed.name, item, narrowed.hostRequirements)
				}
				continue
			}
			if len(diagnostics) == 0 {
				t.Errorf("%s: removing required %s still compiled", seed.name, item)
				continue
			}
			for _, diagnostic := range diagnostics {
				if diagnostic.Code != DiagnosticHostCapabilityMissing || diagnostic.Message != "environment host capability table lacks "+item.String() {
					t.Errorf("%s: removing required %s: diagnostic %v, want only HOST_CAPABILITY_MISSING naming it", seed.name, item, diagnostic)
				}
			}
		}
	}
	if checked < 500 {
		t.Fatalf("checked %d removals, the seed set shrank", checked)
	}
	t.Logf("checked %d removals", checked)
}

func withoutHostCapability(base CompileEnvironment, item HostCapability) CompileEnvironment {
	environment := base
	environment.Host = withoutHostCapabilities(HostCapabilityTableOf(base), item).HostCapabilityCatalog
	if item.Kind == HostCapabilitySpawnKind && item.Key == "minion" {
		environment.Host.Summon = base.Host.Summon
	}
	if item.Kind == HostCapabilitySummon {
		environment.Host.SpawnKinds = withoutString(environment.Host.SpawnKinds, "minion")
	}
	if item.Kind == HostCapabilityAttribute {
		environment.Gameplay.Attributes.Entries = append([]AttributeCatalogEntry(nil), base.Gameplay.Attributes.Entries...)
		for index := range environment.Gameplay.Attributes.Entries {
			if environment.Gameplay.Attributes.Entries[index].Key == item.Key {
				environment.Gameplay.Attributes.Entries[index].Readable = false
			}
		}
	}
	environment.Digest = AuthorityDigest(environment)
	return environment
}

func withoutString(values []string, drop string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != drop {
			result = append(result, value)
		}
	}
	return result
}

// requirementRecordingHost 把 Runtime 发给 Host 的每次取值换算成能力表的项。
type requirementRecordingHost struct {
	*MemoryHost
	catalog GameplayCatalog
	asked   map[HostCapability]string
}

func (host *requirementRecordingHost) ask(kind HostCapabilityKind, key, call string) {
	if host.asked == nil {
		host.asked = make(map[HostCapability]string)
	}
	host.asked[HostCapability{Kind: kind, Key: key}] = call
}

func (host *requirementRecordingHost) attributeKey(handle AttributeHandle) string {
	for _, entry := range host.catalog.Attributes.Entries {
		if entry.Handle == handle {
			return entry.Key
		}
	}
	return fmt.Sprintf("<handle %d>", handle)
}

func (host *requirementRecordingHost) resourceKey(name string, handle ResourceHandle) string {
	if name != "" {
		return name
	}
	for _, entry := range host.catalog.Resources.Entries {
		if entry.Handle == handle {
			return entry.Key
		}
	}
	return fmt.Sprintf("<handle %d>", handle)
}

func (host *requirementRecordingHost) Read(request ReadRequest) (ReadResult, error) {
	switch payload := request.Payload.(type) {
	case AttributeRead:
		host.ask(HostCapabilityAttribute, host.attributeKey(payload.Attribute), "Read")
	case ResourceRead:
		host.ask(HostCapabilityResource, payload.Resource, "Read")
	}
	return host.MemoryHost.Read(request)
}

func (host *requirementRecordingHost) Select(request SelectRequest) (SelectResult, error) {
	if _, owned := request.Shape.(OwnedEntitiesSelectShape); owned {
		host.ask(HostCapabilitySummon, "", "Select")
	}
	for _, filter := range request.Filters {
		if typed, ok := filter.(AttributeSelectFilter); ok {
			host.ask(HostCapabilityAttribute, host.attributeKey(typed.Attribute), "Select")
		}
	}
	return host.MemoryHost.Select(request)
}

func (host *requirementRecordingHost) PayCosts(payment CostPayment) (CommitReceipt, error) {
	for _, entry := range payment.Entries {
		host.ask(HostCapabilityResource, host.resourceKey(entry.Resource, entry.Handle), "PayCosts")
	}
	return host.MemoryHost.PayCosts(payment)
}

func (host *requirementRecordingHost) Apply(command EffectCommand) (EffectResult, error) {
	switch payload := command.Payload.(type) {
	case ResourceCommand:
		host.ask(HostCapabilityResource, host.resourceKey("", payload.Resource), "Apply")
		host.ask(HostCapabilityResourceOperation, payload.Operation, "Apply")
	case AttributeModifierCommand:
		host.ask(HostCapabilityModifierOperation, payload.Operation, "Apply")
	case SummonCommand, OwnedEntityCommand:
		host.ask(HostCapabilitySummon, "", fmt.Sprintf("Apply(%T)", payload))
	}
	return host.MemoryHost.Apply(command)
}

func (host *requirementRecordingHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	steps := map[string]bool{}
	switch command.Motion.(type) {
	case FrameMotionStep:
		steps["frame"] = true
	case SteeringMotionStep:
		steps["steering"] = true
	case OffsetsMotionStep:
		steps["offsets"] = true
	case CollisionMotionStep:
		steps["collision"] = true
	case CarryMotionStep:
		steps["carry"] = true
	case CompletionMotionStep:
		steps["completion"] = true
	}
	for step := range steps {
		host.ask(HostCapabilityMotionStep, step, "StepSpawn")
	}
	for field, value := range map[string]int64{"turn_rate_mdeg_per_tick": command.Numeric.TurnRateMDegPerTick, "return_speed_bp": command.Numeric.ReturnSpeedBP, "collision_force": command.Numeric.CollisionForce} {
		if value != 0 {
			host.ask(HostCapabilitySpawnNumericField, field, "StepSpawn")
		}
	}
	return host.MemoryHost.StepSpawn(command, state)
}

func (host *requirementRecordingHost) PreviewOwnedSummon(command SummonCommand) (OwnedSummonPreview, error) {
	host.ask(HostCapabilitySummon, "", "PreviewOwnedSummon")
	return host.MemoryHost.PreviewOwnedSummon(command)
}

func (host *requirementRecordingHost) CommitOwnedSummon(id OwnedSummonTransactionID) error {
	host.ask(HostCapabilitySummon, "", "CommitOwnedSummon")
	return host.MemoryHost.CommitOwnedSummon(id)
}

// Runtime 发给 Host 的取值都在 Program 的需求里：编译器收集的需求覆盖 Runtime 的实际调用，
// 编译期按表放行的 Program 不会在运行期向 Host 要表外的东西。
func TestRuntimeAsksHostOnlyForCompiledRequirements(t *testing.T) {
	asked := 0
	for _, seed := range mutationSeeds(t) {
		t.Run(seed.name, func(t *testing.T) {
			definition, err := Parse(seed.data)
			if err != nil {
				t.Fatal(err)
			}
			environment := DefaultCompileEnvironment()
			program, diagnostics := Compile(definition, environment)
			requireNoErrors(t, diagnostics)
			host := &requirementRecordingHost{MemoryHost: runtimeTestHost(environment), catalog: environment.Gameplay}
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{1: 10, 2: 10, 3: 10}})
			runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(23)})
			fixture := seed.fixture
			if fixture.passive {
				_, _ = runtime.ActivatePassive(program, EventContext{EventID: 1, RootEventID: 1, Source: 2, Owner: 1, Target: 1, Result: fixture.passiveResult})
			} else if castID, err := runtime.Activate(program, fixture.input); err == nil && fixture.release {
				_ = runtime.Release(castID)
			}
			for tick := Tick(0); tick <= 12; tick++ {
				_ = runtime.Advance(tick)
			}
			required := HostCapabilityTable{}
			for _, item := range program.hostRequirements {
				required = MergeHostCapabilities(required, tableOf(item))
			}
			for item, call := range host.asked {
				asked++
				if !required.Has(item) {
					t.Errorf("runtime asked the host for %s via %s, but the compiled requirements %v do not list it", item, call, program.hostRequirements)
				}
			}
		})
	}
	if asked == 0 {
		t.Fatal("no host capability was exercised")
	}
}

// hostOnlySpawnNumericFields 与 Runtime 的用法一致：Runtime 用来算运动的数值属性有基础值表达式，
// 只交给 Host 的没有。
func TestHostOnlySpawnNumericFieldsMatchRuntime(t *testing.T) {
	spawns := map[string]string{
		"speed":                       `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`,
		"radius":                      `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"completion":{"type":"end"}}`,
		"arc_height":                  `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"parabola","destination":"$caster.position","height":10,"duration_ticks":10},"completion":{"type":"end"}}`,
		"turn_rate_mdeg_per_tick":     `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":10},"trajectory":{"type":"linear","speed":10},"completion":{"type":"end"}}`,
		"angular_speed_mdeg_per_tick": `"kind":"orbit","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"completion":{"type":"end"}}`,
		"offset_amplitude":            `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"zigzag","amplitude":2,"period_ticks":2}],"completion":{"type":"end"}}`,
		"offset_radius":               `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"offsets":[{"type":"circular","radius":2,"angular_speed":1000}],"completion":{"type":"end"}}`,
		"return_speed_bp":             `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"completion":{"type":"boomerang","max_return_ticks":10}}`,
		"collision_force":             `"kind":"projectile","duration_ticks":10,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":10},"collision":{"layers":["terrain"],"response":"stop"},"completion":{"type":"end"}}`,
	}
	for _, policy := range defaultSpawnPropertyCatalog().Properties {
		spawn, found := spawns[policy.Key]
		if !found {
			t.Fatalf("spawn property %q has no motion fixture; add one", policy.Key)
		}
		program, diagnostics := compileNumericSkill(t, numericSpawnSkillJSON(spawn, ""))
		requireNoErrors(t, diagnostics)
		hostOnly := true
		bound := false
		for _, property := range program.spawnProperties {
			if property.handle != policy.Handle {
				continue
			}
			for _, binding := range property.slotBindings {
				value, ok := spawnNumericBinding(program.spawnTemplates[0].motion, binding)
				bound = bound || ok
				if ok && value != nil {
					hostOnly = false
				}
			}
		}
		if !bound {
			t.Fatalf("spawn property %q is not bound by its motion fixture", policy.Key)
		}
		if hostOnly != containsString(hostOnlySpawnNumericFields, policy.Key) {
			t.Errorf("spawn property %q: runtime treats it as host-only=%v, hostOnlySpawnNumericFields says %v", policy.Key, hostOnly, !hostOnly)
		}
	}
}

// 编译器只经 HostCapabilityTableOf 读取 Host 能力：编译器源文件不直接读环境的 Host 段、属性的
// Readable，也不再自己列 motion 步骤或资源 / 修正 operation 是否被 Host 支持。
func TestCompilerReadsHostCapabilitiesOnlyThroughTheTable(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	direct := regexp.MustCompile(`environment\.Host\b|\.Readable\b|HostFeatures|EnabledSlots|hostOnlySpawnNumericFields`)
	for _, file := range files {
		name := file.Name()
		compiler := strings.HasPrefix(name, "compile_") || strings.HasPrefix(name, "lower")
		if !compiler || strings.HasSuffix(name, "_test.go") || !strings.HasSuffix(name, ".go") || name == "compile_environment.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for index, line := range strings.Split(string(data), "\n") {
			if name == "compile_host_capability.go" && strings.Contains(line, "hostOnlySpawnNumericFields") {
				continue // 收集需求时判断哪些数值属性要 Host 声明，同一张表的封闭集合
			}
			if name == "compile_authority.go" && (strings.Contains(line, "Host: environment.Host}") || strings.Contains(line, "validateHostCapabilityCatalog(environment.Host,")) {
				continue // 环境自身的 digest 与校验，不是编译决策
			}
			if direct.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				t.Errorf("%s:%d reads host capability data outside HostCapabilityTableOf: %s", name, index+1, strings.TrimSpace(line))
			}
		}
	}
}

func summonCostingProgram(t *testing.T) *Program {
	t.Helper()
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10}},{"flow":"finish"}]}`
	program, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("required", `[{"resource":"mana","amount":10}]`, flow)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	return program
}

// 能力表是 Host 接口的一部分：不声明能力的 Host 编译不过，Runtime 没有可以跳过核对的 Host。
func TestHostInterfaceRequiresTheCapabilityTable(t *testing.T) {
	host := reflect.TypeOf((*Host)(nil)).Elem()
	provider := reflect.TypeOf((*HostCapabilityProvider)(nil)).Elem()
	if !host.Implements(provider) {
		t.Fatal("skill.Host does not include HostCapabilities(): a Host may omit its capability table and skip admission")
	}
}

// 包装型 Host（RecordingHost / ReplayHost）转发底层 Host 的表：底层如实声明没有召唤物时，
// 包装后同样在准入处被拒，不扣费；底层能力齐全时包装后照常施法，回放与录制一致。
func TestWrappingHostsForwardTheWrappedCapabilities(t *testing.T) {
	program := summonCostingProgram(t)
	environment := DefaultCompileEnvironment()

	inner := runtimeTestHost(environment)
	recording := NewRecordingHost(&hostWithoutOwnedContract{inner: inner})
	_, err := NewRuntime(recording, RuntimeOptions{}).Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostCapabilityMissing) || !strings.Contains(err.Error(), "summon") {
		t.Fatalf("RecordingHost over a host without summons: err = %v, want ErrHostCapabilityMissing naming summon", err)
	}
	if mana := inner.ResourceForTest(1, "mana"); mana != 100 {
		t.Fatalf("mana = %d after a refused start, want 100 (nothing paid)", mana)
	}
	replay := NewReplayHost(inner.AuthorityIdentity(), recording.Records())
	_, err = NewRuntime(replay, RuntimeOptions{}).Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostCapabilityMissing) || !strings.Contains(err.Error(), "summon") {
		t.Fatalf("ReplayHost of that recording: err = %v, want the same refusal", err)
	}
	if err := replay.AssertComplete(); err != nil {
		t.Fatal(err)
	}

	full := runtimeTestHost(environment)
	recording = NewRecordingHost(&ownedSpawnTestHost{MemoryHost: full})
	if got, want := recording.HostCapabilities(), full.HostCapabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecordingHost table = %v, want the wrapped host's %v", got.Items(), want.Items())
	}
}

// 声明了空表（什么都不支持）的 Host：带能力需求的 Program 一律在准入处被拒，点名缺的每一项。
func TestEmptyTableRefusesEveryRequirement(t *testing.T) {
	program := summonCostingProgram(t)
	inner := runtimeTestHost(DefaultCompileEnvironment())
	_, err := NewRuntime(&emptyTableHost{MemoryHost: inner}, RuntimeOptions{}).Activate(program, CastInput{Caster: 1})
	if !errors.Is(err, ErrHostCapabilityMissing) || !strings.Contains(err.Error(), "summon") || !strings.Contains(err.Error(), `resource "mana"`) {
		t.Fatalf("empty table: err = %v, want ErrHostCapabilityMissing naming summon and resource \"mana\"", err)
	}
	if mana := inner.ResourceForTest(1, "mana"); mana != 100 {
		t.Fatalf("mana = %d after a refused start, want 100", mana)
	}
}

// 守卫：skill 的非测试源码里不再有对 HostCapabilityProvider 的类型断言——那正是“没实现就跳过”
// 分支的形状。能力表从 Host 接口直接取。
func TestNoHostCapabilitySkipBranch(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	assertion := regexp.MustCompile(`\.\(\s*HostCapabilityProvider\s*\)`)
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for index, line := range strings.Split(string(data), "\n") {
			if assertion.MatchString(line) {
				t.Errorf("%s:%d asserts HostCapabilityProvider; every Host declares its table, read host.HostCapabilities() directly: %s", name, index+1, strings.TrimSpace(line))
			}
		}
	}
}

// emptyTableHost 声明空表。
type emptyTableHost struct{ *MemoryHost }

func (*emptyTableHost) HostCapabilities() HostCapabilityTable { return HostCapabilityTable{} }

type neutralProbeHost struct {
	*MemoryHost
	multiplier int64
}

func (host *neutralProbeHost) Apply(command EffectCommand) (EffectResult, error) {
	if modifier, ok := command.Payload.(AttributeModifierCommand); ok && modifier.Operation == "mul_bp" {
		host.multiplier = modifier.Value
	}
	return host.MemoryHost.Apply(command)
}

func TestCapabilityProbeIsNeutralAndStopsItsSpawn(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := &neutralProbeHost{MemoryHost: hostCapabilityProbeHost(environment), multiplier: -1}
	if err := CheckHostCapabilities(host, environment.Gameplay, HostCapabilityProbe{Entity: 1}); err != nil {
		t.Fatal(err)
	}
	if host.multiplier != 10000 {
		t.Errorf("mul_bp probe=%d want neutral 10000", host.multiplier)
	}
	for id, spawn := range host.spawns {
		if spawn.active {
			t.Errorf("capability probe left active spawn %d", id)
		}
	}
}

type nameOnlyPaymentHost struct{ *MemoryHost }

func (host *nameOnlyPaymentHost) PayCosts(payment CostPayment) (CommitReceipt, error) {
	for _, entry := range payment.Entries {
		if entry.Resource == "" {
			return CommitReceipt{}, errors.New("only resource names supported")
		}
	}
	return host.MemoryHost.PayCosts(payment)
}
func TestCapabilityProbeUsesRuntimeHandleOnlyPayment(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := &nameOnlyPaymentHost{MemoryHost: hostCapabilityProbeHost(environment)}
	if err := CheckHostCapabilities(host, environment.Gameplay, HostCapabilityProbe{Entity: 1}); err == nil {
		t.Fatal("name-only Host passed probe although Runtime pays with handles only")
	}
}

func TestDefaultMemoryHostSupportsItsDeclaredResourceHandles(t *testing.T) {
	program := summonCostingProgram(t)
	host := NewMemoryHost(program.AuthorityIdentity())
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Resources: map[string]int64{"mana": 100}})
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatalf("default advertised catalog cannot execute cost/summon: %v", err)
	}
	if mana := host.ResourceForTest(1, "mana"); mana != 90 {
		t.Fatalf("mana=%d", mana)
	}
}
