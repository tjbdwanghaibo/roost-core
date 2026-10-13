package combatcomponent

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

type mapResolver map[skill.EntityID]*CombatComponent

func (m mapResolver) CombatComponent(id skill.EntityID) (*CombatComponent, bool) {
	component, ok := m[id]
	return component, ok
}

type testRevision struct {
	revision skill.WorldRevision
	events   []EffectEvent
}

func (r *testRevision) CurrentRevision() skill.WorldRevision { return r.revision }
func (r *testRevision) CommitEffect(events []EffectEvent) skill.CommitReceipt {
	r.revision++
	r.events = append(r.events, events...)
	return skill.CommitReceipt{Revision: r.revision}
}

func newAdapterFixture() (*HostAdapter, *testRevision, *CombatComponent, *CombatComponent) {
	attacker := NewCombatComponent(NewCombatDao(1, "game"))
	attacker.dao.combatant = combat.Combatant{Alive: true, Health: 80, MaxHealth: 80, Penetration: 10}
	defender := NewCombatComponent(NewCombatDao(2, "game"))
	defender.dao.combatant = combat.Combatant{Alive: true, Health: 100, MaxHealth: 100, Shield: 20, Armor: 40}
	defender.dao.attributes.SetBase(5, 50) // mana pool
	revision := &testRevision{}
	adapter := &HostAdapter{
		Resolver:  mapResolver{1: attacker, 2: defender},
		Revision:  revision,
		Committer: &combatRecordingCommitter{},
		ResourceAttribute: func(resource string, handle skill.ResourceHandle) (combat.AttributeID, bool) {
			if resource == "mana" || handle == 5 {
				return 5, true
			}
			return 0, false
		},
	}
	return adapter, revision, attacker, defender
}

func TestHostAdapterAppliesDamageWithEvents(t *testing.T) {
	adapter, revision, _, defender := newAdapterFixture()
	result, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.DamageCommand{Source: 1, Target: 2, Amount: 100, DamageType: 1, CanCritical: true}})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	payload := result.Payload.(skill.DamageEffectResult)
	if !payload.Succeeded || payload.Result.HealthDamage == 0 || payload.Result.Absorbed != 20 {
		t.Fatalf("payload = %+v", payload)
	}
	if defender.Combatant().Shield != 0 {
		t.Fatal("shield not consumed")
	}
	kinds := make([]string, 0, len(revision.events))
	for _, event := range revision.events {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 3 || kinds[0] != "damage_resolved" || kinds[1] != "shield_absorbed" || kinds[2] != "shield_broken" {
		t.Fatalf("events = %v", kinds)
	}
	if result.Commit.Revision != 1 || !result.Commit.Changed {
		t.Fatalf("receipt = %+v", result.Commit)
	}
	// Unknown target fails without a revision bump.
	miss, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.DamageCommand{Source: 1, Target: 99, Amount: 10}})
	if err != nil || !handled || miss.Payload.(skill.DamageEffectResult).Succeeded || revision.revision != 1 {
		t.Fatalf("missing target: %+v revision=%d err=%v", miss.Payload, revision.revision, err)
	}
	// Non-combat payloads pass through unhandled.
	if _, handled, _ := adapter.Apply(skill.EffectCommand{Payload: skill.TeleportCommand{Target: 2}}); handled {
		t.Fatal("teleport handled by combat adapter")
	}
}

func TestHostAdapterHealShieldAndReads(t *testing.T) {
	adapter, revision, attacker, _ := newAdapterFixture()
	attacker.dao.combatant.Health -= 30
	healResult, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.HealCommand{Target: 1, Amount: 100}})
	if err != nil || !handled || healResult.Payload.(skill.HealEffectResult).Result.Effective != 30 {
		t.Fatalf("heal = %+v err=%v", healResult.Payload, err)
	}
	shieldResult, _, err := adapter.Apply(skill.EffectCommand{Payload: skill.ShieldCommand{Target: 1, Amount: 15, DurationTicks: 10}})
	if err != nil || shieldResult.Payload.(skill.ShieldEffectResult).Result.Added != 15 {
		t.Fatalf("shield = %+v err=%v", shieldResult.Payload, err)
	}
	if got := len(revision.events); got != 2 {
		t.Fatalf("events = %d", got)
	}
	read, handled, err := adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "mana"}})
	if err != nil || !handled {
		t.Fatalf("read err=%v handled=%v", err, handled)
	}
	if value, ok := read.Value.Int(); !ok || value != 50 {
		t.Fatalf("mana read = %+v", read.Value)
	}
	if _, handled, _ := adapter.Read(skill.ReadRequest{Payload: skill.PositionRead{Entity: 2}}); handled {
		t.Fatal("position read handled by combat adapter")
	}
}

func TestHostAdapterShieldReportsEffectiveDeltaAndSkipsNoOpCommit(t *testing.T) {
	adapter, revision, attacker, _ := newAdapterFixture()
	attacker.dao.combatant.Shield = math.MaxInt64 - 5

	result, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.ShieldCommand{Target: 1, Amount: 10}})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	payload := result.Payload.(skill.ShieldEffectResult)
	if payload.Result.Added != 5 || attacker.Combatant().Shield != math.MaxInt64 {
		t.Fatalf("shield result=%+v state=%d", payload.Result, attacker.Combatant().Shield)
	}
	if !result.Commit.Changed || result.Commit.Revision != 1 || len(revision.events) != 1 {
		t.Fatalf("first receipt=%+v events=%d", result.Commit, len(revision.events))
	}

	noOp, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.ShieldCommand{Target: 1, Amount: 10}})
	if err != nil || !handled {
		t.Fatalf("no-op handled=%v err=%v", handled, err)
	}
	if noOp.Payload.(skill.ShieldEffectResult).Result.Added != 0 || noOp.Commit.Changed {
		t.Fatalf("no-op result=%+v receipt=%+v", noOp.Payload, noOp.Commit)
	}
	if revision.revision != 1 || len(revision.events) != 1 {
		t.Fatalf("no-op advanced authority: revision=%d events=%d", revision.revision, len(revision.events))
	}
}

func TestHostAdapterPayCostsIsAtomic(t *testing.T) {
	adapter, revision, _, defender := newAdapterFixture()
	payment := skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: 30}, {Resource: "mana", Amount: 15}}}
	receipt, err := adapter.PayCosts(payment)
	if err != nil || !receipt.Changed || defender.AttributeBase(5) != 5 {
		t.Fatalf("pay = %+v base=%d err=%v", receipt, defender.AttributeBase(5), err)
	}
	// Insufficient totals must not partially deduct.
	if _, err := adapter.PayCosts(payment); !errors.Is(err, skill.ErrInsufficientResource) {
		t.Fatalf("expected insufficient, got %v", err)
	}
	if defender.AttributeBase(5) != 5 {
		t.Fatalf("partial deduction: base=%d", defender.AttributeBase(5))
	}
	if _, err := adapter.PayCosts(skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "unknown", Amount: 1}}}); err == nil {
		t.Fatal("unmapped resource accepted")
	}
	if revision.revision != 1 {
		t.Fatalf("revision = %d, want 1", revision.revision)
	}
}

// Resource commands touch attribute bases directly; every refusal below is
// what keeps a skill from minting or overspending a pool. Each is pinned, and
// each must leave the base and the revision untouched.
func TestResourceCommandRefusesEachIllegalOperation(t *testing.T) {
	cases := []struct {
		name    string
		command skill.ResourceCommand
		check   func(t *testing.T, err error)
	}{
		{"negative spend", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "spend", Amount: -1}, func(t *testing.T, err error) {
			if !errors.Is(err, skill.ErrInsufficientResource) {
				t.Fatalf("err = %v, want ErrInsufficientResource", err)
			}
		}},
		{"spend beyond the pool", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "spend", Amount: 51}, func(t *testing.T, err error) {
			if !errors.Is(err, skill.ErrInsufficientResource) {
				t.Fatalf("err = %v, want ErrInsufficientResource", err)
			}
		}},
		{"set below zero", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "set", Amount: -3}, func(t *testing.T, err error) {
			if !errors.Is(err, skill.ErrInsufficientResource) {
				t.Fatalf("err = %v, want ErrInsufficientResource", err)
			}
		}},
		{"unknown operation", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "multiply", Amount: 2}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), `unsupported resource operation "multiply"`) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"unmapped handle", skill.ResourceCommand{Target: 2, Resource: 9, Operation: "add", Amount: 1}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "resource handle 9 has no attribute mapping") {
				t.Fatalf("err = %v", err)
			}
		}},
		{"entity without combat component", skill.ResourceCommand{Target: 99, Resource: 5, Operation: "add", Amount: 1}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "entity 99 has no combat component") {
				t.Fatalf("err = %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, revision, _, defender := newAdapterFixture()
			_, handled, err := adapter.Apply(skill.EffectCommand{Payload: tc.command})
			if !handled {
				t.Fatal("resource command not handled")
			}
			tc.check(t, err)
			if defender.AttributeBase(5) != 50 || revision.revision != 0 {
				t.Fatalf("a refused resource command changed state: base=%d revision=%d", defender.AttributeBase(5), revision.revision)
			}
		})
	}
}

// PayCosts is atomic and strict: a negative entry, a total that overflows, or
// an entity without a component must refuse before any deduction.
func TestPayCostsRefusesNegativeOverflowingAndComponentlessPayments(t *testing.T) {
	cases := []struct {
		name    string
		payment skill.CostPayment
		text    string
	}{
		{"negative cost", skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: -5}}}, "negative cost"},
		{"overflowing total", skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: math.MaxInt64}, {Resource: "mana", Amount: 1}}}, "cost total overflows"},
		{"entity without component", skill.CostPayment{Entity: 99, Entries: []skill.CostEntry{{Resource: "mana", Amount: 1}}}, "entity 99 has no combat component"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, revision, _, defender := newAdapterFixture()
			_, err := adapter.PayCosts(tc.payment)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("PayCosts = %v, want %q", err, tc.text)
			}
			if defender.AttributeBase(5) != 50 || revision.revision != 0 {
				t.Fatalf("a refused payment changed state: base=%d revision=%d", defender.AttributeBase(5), revision.revision)
			}
		})
	}
}

// HostAdapter 声明它负责的那部分能力表（Catalog 里可读的属性、有映射的资源、资源 operation，接了 StatusBridge 时
// 加属性修正），业务 Host 把它与自己负责的部分（运动、衍生物、召唤物）用 skill.MergeHostCapabilities 合起来声明；
// Catalog 外的属性 handle 被拒绝，不读出 0（维护者决定 B3 ③）。

// businessHost 是最小的业务 Host：战斗取值交给 HostAdapter，其余交给 MemoryHost。
type businessHost struct {
	*skill.MemoryHost
	adapter *HostAdapter
}

func (host *businessHost) HostCapabilities() skill.HostCapabilityTable {
	// 业务自己负责的部分：运动、衍生物、召唤物（这里由 MemoryHost 实现）。
	own := skill.HostCapabilityTable{HostCapabilityCatalog: skill.FullHostCapabilityCatalog()}
	own.ResourceOperations, own.ModifierOperations = nil, nil
	return skill.MergeHostCapabilities(host.adapter.HostCapabilities(), own)
}

func (host *businessHost) Read(request skill.ReadRequest) (skill.ReadResult, error) {
	if result, handled, err := host.adapter.Read(request); handled {
		return result, err
	}
	return host.MemoryHost.Read(request)
}

func (host *businessHost) PayCosts(payment skill.CostPayment) (skill.CommitReceipt, error) {
	return host.adapter.PayCosts(payment)
}

func (host *businessHost) Apply(command skill.EffectCommand) (skill.EffectResult, error) {
	if result, handled, err := host.adapter.Apply(command); handled {
		return result, err
	}
	return host.MemoryHost.Apply(command)
}

func newBusinessHost(environment skill.CompileEnvironment, withStatus bool) (*businessHost, *CombatComponent) {
	component := NewCombatComponent(NewCombatDao(1, "game"))
	component.dao.combatant = combat.Combatant{Alive: true, Health: 100, MaxHealth: 100}
	component.dao.attributes.SetBase(50, 40) // mana pool
	revision := &testRevision{}
	resolver := mapResolver{1: component}
	adapter := &HostAdapter{
		Resolver: resolver, Revision: revision, Committer: &combatRecordingCommitter{}, Catalog: environment.Gameplay,
		ResourceAttribute: func(resource string, handle skill.ResourceHandle) (combat.AttributeID, bool) {
			if resource == "mana" || handle == 1 {
				return 50, true
			}
			return 0, false
		},
	}
	if withStatus {
		adapter.Status = &StatusBridge{Resolver: resolver, Revision: revision, Committer: &combatRecordingCommitter{}, Catalog: environment.Gameplay, CurrentTick: func() skill.Tick { return 0 }}
	}
	memory := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	memory.ConfigureGameplayCatalog(environment.Gameplay)
	memory.UpsertEntity(skill.MemoryEntity{ID: 1, Alive: true})
	return &businessHost{MemoryHost: memory, adapter: adapter}, component
}

// 业务 Host 声明的每一项都真的支持，并覆盖默认环境。
func TestBusinessHostOverHostAdapterMatchesItsCapabilities(t *testing.T) {
	environment := skill.DefaultCompileEnvironment()
	host, component := newBusinessHost(environment, true)
	if err := skill.CheckHostCapabilities(host, environment.Gameplay, skill.HostCapabilityProbe{Entity: 1}); err != nil {
		t.Fatal(err)
	}
	if err := skill.HostSupportsEnvironment(host, environment); err != nil {
		t.Fatal(err)
	}
	if got := component.AttributeBase(50); got != 40 {
		t.Fatalf("conformance probe changed mana to %d", got)
	}
}

// 没接 StatusBridge：HostAdapter 不声明属性修正，环境取自它的表时 attribute_modifier 在编译期被拒；
// 覆盖检查点名缺的 operation。
func TestHostAdapterWithoutStatusBridgeDeclaresNoModifiers(t *testing.T) {
	environment := skill.DefaultCompileEnvironment()
	host, _ := newBusinessHost(environment, false)
	err := skill.HostSupportsEnvironment(host, environment)
	if !errors.Is(err, skill.ErrHostCapabilityMissing) || !strings.Contains(err.Error(), `modifier_operation "add"`) || !strings.Contains(err.Error(), `modifier_operation "mul_bp"`) {
		t.Fatalf("HostSupportsEnvironment = %v, want the two modifier operations named", err)
	}
	if err := skill.CheckHostCapabilities(host, environment.Gameplay, skill.HostCapabilityProbe{Entity: 1}); err != nil {
		t.Fatalf("declared capabilities: %v", err)
	}
}

// Catalog 里有、ResourceAttribute 没映射的资源：HostAdapter 不声明它，启动时的覆盖检查点名它，
// 而不是等某个技能付费时才报 “has no attribute mapping”。
func TestHostAdapterUnmappedResourceIsNamedAtStartup(t *testing.T) {
	environment := skill.DefaultCompileEnvironment()
	environment.Gameplay.Resources.Entries = append(append([]skill.ResourceCatalogEntry(nil), environment.Gameplay.Resources.Entries...), skill.ResourceCatalogEntry{Handle: 2, Key: "rage", Maximum: 100})
	environment.Digest = skill.AuthorityDigest(environment)
	host, _ := newBusinessHost(environment, true)
	err := skill.HostSupportsEnvironment(host, environment)
	if !errors.Is(err, skill.ErrHostCapabilityMissing) || !strings.Contains(err.Error(), `resource "rage"`) {
		t.Fatalf("HostSupportsEnvironment = %v, want resource \"rage\" named", err)
	}
	if err := skill.CheckHostCapabilities(host, environment.Gameplay, skill.HostCapabilityProbe{Entity: 1}); err != nil {
		t.Fatalf("an undeclared resource must be refused, not answered: %v", err)
	}
}

// Catalog 外的属性 handle 被拒绝，不再读出 0。
func TestHostAdapterRefusesAttributesOutsideTheCatalog(t *testing.T) {
	adapter, _, _, _ := newAdapterFixture()
	adapter.Catalog = skill.DefaultCompileEnvironment().Gameplay
	read, handled, err := adapter.Read(skill.ReadRequest{Payload: skill.AttributeRead{Entity: 2, Attribute: 99}})
	if !handled || !errors.Is(err, skill.ErrHostCapabilityMissing) {
		value, _ := read.Value.Int()
		t.Fatalf("AttributeRead handle 99: handled=%v value=%d err=%v, want ErrHostCapabilityMissing", handled, value, err)
	}
	if _, _, err := adapter.Read(skill.ReadRequest{Payload: skill.AttributeRead{Entity: 2, Attribute: 1}}); err != nil {
		t.Fatalf("readable catalog attribute: %v", err)
	}
}

func TestNoChangeCombatDoesNotMarkDirty(t *testing.T) {
	for _, action := range []string{"ignored_buff", "dodged", "zero_damage"} {
		t.Run(action, func(t *testing.T) {
			_, _, _, target := newAdapterFixture()
			spec := combat.BuffSpec{ID: 1, StackPolicy: combat.BuffIgnore}
			if action == "ignored_buff" {
				target.dao.buffs.Apply(spec, 0, 1)
			}
			if action == "dodged" {
				target.dao.combatant.Dodge = true
			}
			target.dao.CleanDirty()
			_, err := nest.RunDetachedTransaction(context.Background(), &combatRecordingCommitter{}, "combat_no_change", func() (any, error) {
				if action == "ignored_buff" {
					target.ApplyBuff(spec, 0, 1)
				} else {
					amount := int64(5)
					if action == "zero_damage" {
						amount = 0
					}
					target.ApplyDamage(nil, combat.DamageInput{Amount: amount}, nil)
				}
				if target.dao.tracker.HasSyncDirty() {
					t.Errorf("%s marked unchanged DAO dirty", action)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAbsentResourceMappingReturnsErrors(t *testing.T) {
	for _, action := range []string{"read", "pay", "apply"} {
		t.Run(action, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("nil mapping panicked: %v", value)
				}
			}()
			adapter, _, _, _ := newAdapterFixture()
			adapter.ResourceAttribute = nil
			var err error
			switch action {
			case "read":
				_, _, err = adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "mana"}})
			case "pay":
				_, err = adapter.PayCosts(skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: 1}}})
			case "apply":
				_, _, err = adapter.Apply(skill.EffectCommand{Payload: skill.ResourceCommand{Target: 2, Resource: 5, Operation: "add", Amount: 1}})
			}
			if err == nil {
				t.Fatal("missing mapping accepted")
			}
		})
	}
}

func TestResourceReadMatchesSpendableBase(t *testing.T) {
	adapter, _, _, target := newAdapterFixture()
	target.dao.attributes.Grant(1, combat.Modifier{Attribute: 5, Flat: 30})
	read, _, err := adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "mana"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := read.Value.Int(); got != 50 {
		t.Fatalf("reported spendable pool=%d want=50", got)
	}
}

func TestHostAdapterRejectsUnsupportedCombatCatalog(t *testing.T) {
	for _, kind := range []string{"formula", "damage", "element"} {
		t.Run(kind, func(t *testing.T) {
			adapter, _, _, target := newAdapterFixture()
			command := skill.DamageCommand{Source: 1, Target: 2, Amount: 5, DamageType: 1}
			switch kind {
			case "formula":
				adapter.Catalog.Combat.FormulaPolicy = "unknown"
			case "damage":
				adapter.Catalog.Combat.DamageTypes = []skill.DamageTypeHandle{2}
			case "element":
				adapter.Catalog.Elements.Entries = []skill.ElementCatalogEntry{{Handle: 1, Key: "fire"}}
			}
			_, _, err := adapter.Apply(skill.EffectCommand{Payload: command})
			if err == nil || target.Combatant().Shield != 20 {
				t.Fatalf("unsupported %s accepted: %v", kind, err)
			}
		})
	}
}

func TestStatusStackOverflowAndPermanentRefresh(t *testing.T) {
	for _, action := range []string{"add_stacks", "refresh"} {
		t.Run(action, func(t *testing.T) {
			bridge, _, target, _ := newBridgeFixture()
			id, _ := target.dao.buffs.Apply(combat.BuffSpec{ID: 10, MaxStacks: 3, DurationTicks: 0}, 0, 1)
			_, _, err := bridge.Apply(skill.EffectCommand{Payload: skill.ModifyStatusInstanceCommand{Owner: 1, Status: skill.StatusInstanceRef{ID: skill.NewStatusInstanceID(uint64(id)), Target: 2}, Operation: action, Value: math.MaxInt64}})
			if err != nil {
				t.Fatal(err)
			}
			active := target.ActiveBuffs()
			if len(active) != 1 {
				t.Fatalf("overflow removed buff: %v", active)
			}
			if action == "add_stacks" && active[0].Stacks != 3 {
				t.Fatalf("stacks=%d", active[0].Stacks)
			}
			if action == "refresh" && active[0].DueTick != 0 {
				t.Fatalf("permanent expires at %d", active[0].DueTick)
			}
		})
	}
}
