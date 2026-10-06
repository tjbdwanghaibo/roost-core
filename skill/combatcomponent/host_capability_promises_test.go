package combatcomponent

import (
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/skill"

	"github.com/tjbdwanghaibo/roost-core/skill/combat"
)

// B3 ③：HostAdapter 声明它负责的那部分能力表（Catalog 里可读的属性、有映射的资源、资源
// operation，接了 StatusBridge 时加属性修正），业务 Host 把它与自己负责的部分（运动、衍生物、
// 召唤物）用 skill.MergeHostCapabilities 合起来声明。修前（基线 dac4f38c）HostAdapter 对 Catalog
// 外的属性 handle 读出 0、不报错（AttributeCurrent 对没有的通道返回 0）。

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

// Catalog 里有、ResourceAttribute 没映射的资源：HostAdapter 不声明它，启动时的覆盖检查点名它
// （修前要等某个技能付费时才报 “has no attribute mapping”）。
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
