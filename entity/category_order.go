package entity

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// EntityCategoryRemote is the framework-reserved category for kinds whose
// authoritative state lives behind a distributed ownership guard.
//
// Once an application declares its categories (RegisterEntityCategories), a
// category's VALUE is its lock rank, acquired lowest first. Remote is fixed at
// the bottom because that one rank has a physical reason rather than a
// business convention: a remote-managed entity's ownership lock is taken at
// the top of a dispatch, and acquiring it while a local entity mutex is
// already held would hold that mutex across a network round trip, stalling
// every other user of that entity behind one backend hiccup.
//
// Application categories therefore start at EntityCategoryRemote + 1, numbered
// in the order they must be locked. An application that has NOT declared its
// categories is unaffected by any of this and keeps using GetEntityGroupFunc.
const EntityCategoryRemote EntityCategory = 1

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

// RegisterEntityCategories declares the application's categories and, with
// them, the lock order. Declaring any category switches lock-order decisions
// from GetEntityGroupFunc to the declared values.
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
	return fmt.Sprintf("%d", category)
}

// refreshLockRankLocked derives one kind's lock rank. Callers hold registryMu.
func refreshLockRankLocked(kind EntityKind) {
	if taxonomy.Load() == nil {
		// No taxonomy: the legacy hook decides, and a stored rank would
		// silently take precedence over it.
		lockRankByKind[kind].Store(0)
		return
	}
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

// unknownKindLockRank is the rank for an ID whose kind this process does not
// link. A cross-server ID that carries the remote bit is ranked with remote;
// anything else is ranked last.
func unknownKindLockRank(guid int64) (int, bool) {
	declared := taxonomy.Load()
	if declared == nil {
		return 0, false
	}
	if GetEntityRemoteFromID(guid) {
		return int(EntityCategoryRemote), true
	}
	return int(declared.max), true
}

// ValidateEntityRegistry checks the registry against the declared taxonomy and
// reports every problem it finds at once. Call it at the end of bootstrap.
//
// It validates without freezing the registry. The write-freeze half of the
// planned seal belongs with the step that makes the generator the only writer;
// until then a late registration is legal and this function can be called again.
//
// With no taxonomy declared there is nothing to check and it returns nil.
func ValidateEntityRegistry() error {
	declared := taxonomy.Load()
	if declared == nil {
		return nil
	}

	var problems []error
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

	for kind := 0; kind < len(lockRankByKind); kind++ {
		entry := kindEntryOf(EntityKind(kind))
		if entry == nil {
			continue
		}
		if _, ok := declared.names[entry.category]; !ok {
			problems = append(problems, fmt.Errorf("kind %d is in category %d, which is not declared", kind, entry.category))
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
