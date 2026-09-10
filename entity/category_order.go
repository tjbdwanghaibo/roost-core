package entity

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// EntityCategoryRemote is the framework-reserved category for kinds whose
// authoritative state lives behind a distributed ownership guard.
//
// A category's VALUE is its lock rank, acquired lowest first. Remote is fixed at
// the bottom because that one rank has a physical reason rather than a
// business convention: a remote-managed entity's ownership lock is taken at
// the top of a dispatch, and acquiring it while a local entity mutex is
// already held would hold that mutex across a network round trip, stalling
// every other user of that entity behind one backend hiccup.
//
// Application categories therefore start at EntityCategoryRemote + 1, numbered
// in the order they must be locked.
const EntityCategoryRemote EntityCategory = 1

// The recommended taxonomy. Values are lock ranks, acquired lowest first, and
// the ordering below is the one the framework is designed around:
//
//	Remote        distributed ownership; must be first, see above
//	World         world/scene objects, contended between many players
//	PlayerScoped  a player's outward representation, taken before the player
//	Player        the player entity itself
//	Other         anything else
//
// They are constants, not a requirement: category is a full uint8 now, so a
// project may insert its own values between these or continue past Other. What
// IS a requirement is that remote-managed kinds sit in EntityCategoryRemote,
// which ValidateEntityRegistry checks.
const (
	EntityCategoryWorld        EntityCategory = EntityCategoryRemote + 1
	EntityCategoryPlayerScoped EntityCategory = EntityCategoryRemote + 2
	EntityCategoryPlayer       EntityCategory = EntityCategoryRemote + 3
	EntityCategoryOther        EntityCategory = EntityCategoryRemote + 4
)

// EntityCategoryUnknown is the rank given to an id whose kind this process does
// not link, and is reserved: no kind may be registered in it.
//
// Ranking last is the conservative answer. A rank is only ever compared, and
// holding the last rank permits acquiring nothing further, so an id the process
// cannot reason about can never be the reason another acquisition is refused
// out of order.
const EntityCategoryUnknown EntityCategory = 255

// EntityCategoryDef declares one category.
//
// There is no order field: the category's value is its order. Name exists only
// so logs and errors can say "world" instead of "2".
type EntityCategoryDef struct {
	Category EntityCategory
	Name     string
}

// categoryTaxonomy is the declared category set. It is replaced wholesale, so
// a lock-free reader sees one self-consistent set.
type categoryTaxonomy struct {
	names map[EntityCategory]string
	// max is the highest declared value, which is the rank given to a kind
	// this process does not know: locking last is the conservative answer,
	// because holding it permits acquiring nothing further.
	max EntityCategory
}

var (
	categoryMu sync.Mutex // writers only
	taxonomy   atomic.Pointer[categoryTaxonomy]
	// lockRankByKind is the per-kind rank derived at registration time. Zero
	// means "not derived", which is also the state of every kind while no
	// taxonomy is declared, so the legacy path stays in charge then.
	lockRankByKind [1 << EntityKindBits]atomic.Uint32
)

// RegisterEntityCategories names the application's categories so logs, errors
// and ValidateEntityRegistry can talk about them. It does not establish the
// lock order: a category's value is its rank whether or not it is named here.
func RegisterEntityCategories(defs ...EntityCategoryDef) error {
	if len(defs) == 0 {
		return fmt.Errorf("entity: no categories to register")
	}
	categoryMu.Lock()
	defer categoryMu.Unlock()

	next := &categoryTaxonomy{names: make(map[EntityCategory]string, len(defs))}
	if current := taxonomy.Load(); current != nil {
		for category, name := range current.names {
			next.names[category] = name
			if category > next.max {
				next.max = category
			}
		}
	}
	for _, def := range defs {
		if def.Category == EntityCategoryNone {
			return fmt.Errorf("entity: category must not be none")
		}
		if def.Category == EntityCategoryUnknown {
			return fmt.Errorf("entity: category %d is reserved for kinds this process does not link", EntityCategoryUnknown)
		}
		if existing, ok := next.names[def.Category]; ok && existing != def.Name {
			return fmt.Errorf("entity: category %d already declared as %q, new name %q", def.Category, existing, def.Name)
		}
		next.names[def.Category] = def.Name
		if def.Category > next.max {
			next.max = def.Category
		}
	}
	taxonomy.Store(next)

	// Kinds may have been registered before the categories were declared, so
	// derive every rank rather than only the new ones.
	registryMu.Lock()
	defer registryMu.Unlock()
	for kind := range lockRankByKind {
		refreshLockRankLocked(EntityKind(kind))
	}
	return nil
}

func MustRegisterEntityCategories(defs ...EntityCategoryDef) {
	if err := RegisterEntityCategories(defs...); err != nil {
		panic(err)
	}
}

// EntityCategoryName is the declared name of a category, for diagnostics. It
// returns the decimal value when the category was never declared.
func EntityCategoryName(category EntityCategory) string {
	if declared := taxonomy.Load(); declared != nil {
		if name, ok := declared.names[category]; ok && name != "" {
			return name
		}
	}
	if name, ok := recommendedCategoryNames[category]; ok {
		return name
	}
	return fmt.Sprintf("%d", category)
}

// recommendedCategoryNames makes diagnostics readable without a declaration.
// It is only a fallback for EntityCategoryName; it does not declare anything,
// so ValidateEntityRegistry's set-level checks stay opt-in.
var recommendedCategoryNames = map[EntityCategory]string{
	EntityCategoryRemote:       "remote",
	EntityCategoryWorld:        "world",
	EntityCategoryPlayerScoped: "player_scoped",
	EntityCategoryPlayer:       "player",
	EntityCategoryOther:        "other",
	EntityCategoryUnknown:      "unknown",
}

// refreshLockRankLocked derives one kind's lock rank. Callers hold registryMu.
//
// Declaring categories is not a precondition: the rank is the category the kind
// was registered with, whether or not the application also gave that category a
// name. Declaring only adds names and lets ValidateEntityRegistry check the set.
func refreshLockRankLocked(kind EntityKind) {
	entry := kindEntryOf(kind)
	if entry == nil {
		lockRankByKind[kind].Store(0)
		return
	}
	rank := entry.category
	if entry.policy.RemoteManaged() {
		// Enforced rather than trusted: a managed kind declared in some other
		// category would otherwise be locked after local entities, which is the
		// one ordering that must never happen. ValidateEntityRegistry reports
		// the declaration as an error so the mistake is still visible.
		rank = EntityCategoryRemote
	}
	lockRankByKind[kind].Store(uint32(rank))
}

// lockRankOf reports a kind's derived rank, or false when none was derived.
func lockRankOf(kind EntityKind) (int, bool) {
	if rank := lockRankByKind[kind].Load(); rank != 0 {
		return int(rank), true
	}
	return 0, false
}

// ValidateEntityRegistry checks the registry against the declared taxonomy and
// reports every problem it finds at once. Call it at the end of bootstrap.
//
// It validates without freezing the registry. The write-freeze half of the
// planned seal belongs with the step that makes the generator the only writer;
// until then a late registration is legal and this function can be called again.
//
// Declaring categories is optional: without it the set-level checks are
// skipped, but the per-kind invariants are still enforced.
func ValidateEntityRegistry() error {
	declared := taxonomy.Load()

	var problems []error
	if declared != nil {
		minDeclared := EntityCategory(0)
		for category := range declared.names {
			if minDeclared == 0 || category < minDeclared {
				minDeclared = category
			}
		}
		if _, ok := declared.names[EntityCategoryRemote]; !ok {
			problems = append(problems, fmt.Errorf("category %d (remote) is not declared, so nothing pins the rank that must be acquired first", EntityCategoryRemote))
		} else if minDeclared != EntityCategoryRemote {
			problems = append(problems, fmt.Errorf("category %d (%s) ranks before remote; remote must be the lowest declared category", minDeclared, EntityCategoryName(minDeclared)))
		}
	}

	for kind := 0; kind < len(lockRankByKind); kind++ {
		entry := kindEntryOf(EntityKind(kind))
		if entry == nil {
			continue
		}
		if entry.category == EntityCategoryUnknown {
			problems = append(problems, fmt.Errorf("kind %d is in category %d, which is reserved for kinds this process does not link", kind, EntityCategoryUnknown))
		}
		if declared != nil {
			if _, ok := declared.names[entry.category]; !ok {
				problems = append(problems, fmt.Errorf("kind %d is in category %d, which is not declared", kind, entry.category))
			}
		}
		if entry.policy.RemoteManaged() && entry.category != EntityCategoryRemote {
			problems = append(problems, fmt.Errorf("kind %d is remote-managed but declared in category %d (%s); it must be in category %d (remote), which is the rank acquired first",
				kind, entry.category, EntityCategoryName(entry.category), EntityCategoryRemote))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("entity registry is inconsistent: %w", joinCategoryProblems(problems))
}

func joinCategoryProblems(problems []error) error {
	joined := problems[0]
	for _, problem := range problems[1:] {
		joined = fmt.Errorf("%w; %w", joined, problem)
	}
	return joined
}
