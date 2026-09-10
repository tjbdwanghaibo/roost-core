package entity

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// EntityBuilderFunc creates an entity from params.
// The returned entity must satisfy IThreadSafeEntity.
type EntityBuilderFunc func(param *EntityCreateParam) (IThreadSafeEntity, error)

// DaoBuilderFunc creates a new DAO instance.
type DaoBuilderFunc func() DaoInterface

// EntityBuilderParam holds the builder configuration for a concrete entity kind.
type EntityBuilderParam struct {
	Category     EntityCategory    // ownership/access category
	Kind         EntityKind        // concrete entity definition
	Builder      EntityBuilderFunc // entity constructor (generated NewXxx)
	DaoBuilders  []DaoBuilderFunc  // DAO factories (one per DAO in entity)
	LoadPriority int               // lower = load first
	NoPersist    bool              // skip auto-persist
	RemotePolicy RemotePolicy      // explicit cross-server policy
	Lifetime     EntityLifetime    // memory/persistence lifecycle policy
	Sync         EntitySyncBuilderParam
}

// --- Global entity factory registry ---

// entityKindEntry is the whole record the framework keeps for one kind. It is
// immutable once published: every update stores a fresh copy, so a lock-free
// reader always observes one self-consistent record instead of three maps that
// could be read between two writes.
type entityKindEntry struct {
	category EntityCategory
	policy   RemotePolicy
	builder  *EntityBuilderParam // nil until RegisterEntityBuilder runs
}

// The registry is keyed by EntityKind, which is a uint8, so it is a fixed
// array rather than a map and a read is one atomic load with no lock.
//
// That matters because the lock-ordering path reads it WHILE entity mutexes
// are held: cmpGuidFunc calls GetEntityGroup twice per comparison when sorting
// entities to lock, maxLockedGroup calls it once per already-held lock, and
// broadcast bucketing calls it once per id. Taking a registry lock there costs
// a mutex per query and, worse, creates an "entity mutex then registry lock"
// acquisition edge that lets a registration stall lock-order decisions (M-01).
var (
	registryMu  sync.Mutex // writers only; readers are lock-free
	kindEntries [1 << EntityKindBits]atomic.Pointer[entityKindEntry]
)

func kindEntryOf(kind EntityKind) *entityKindEntry {
	return kindEntries[kind].Load()
}

// RegisterEntityBuilder registers a builder for a concrete entity kind.
// Call from the service bootstrap path. Panics on duplicate.
func RegisterEntityBuilder(param *EntityBuilderParam) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if param.Kind == EntityKindNone {
		panic("entity builder kind must not be none")
	}
	normalizeBuilderPolicy(param)
	if err := registerEntityKindDefinitionLocked(EntityKindDef{
		Kind:         param.Kind,
		Category:     param.Category,
		RemotePolicy: param.RemotePolicy,
	}); err != nil {
		panic(err)
	}
	// registerEntityKindDefinitionLocked has just created or validated the
	// entry, so it exists.
	entry := kindEntryOf(param.Kind)
	if entry.builder != nil {
		panic(fmt.Sprintf("duplicate entity builder for kind %d", param.Kind))
	}
	next := *entry
	next.builder = param
	kindEntries[param.Kind].Store(&next)
}

func RegisterEntityKindCategory(kind EntityKind, category EntityCategory) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	return registerEntityKindCategoryLocked(kind, category)
}

func MustRegisterEntityKindCategory(kind EntityKind, category EntityCategory) {
	if err := RegisterEntityKindCategory(kind, category); err != nil {
		panic(err)
	}
}

func RegisterEntityKindCategories(defs ...EntityKindCategory) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, def := range defs {
		if err := registerEntityKindCategoryLocked(def.Kind, def.Category); err != nil {
			return err
		}
	}
	return nil
}

func MustRegisterEntityKindCategories(defs ...EntityKindCategory) {
	if err := RegisterEntityKindCategories(defs...); err != nil {
		panic(err)
	}
}

func RegisterEntityKindDefs(defs ...EntityKindDef) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, def := range defs {
		if err := registerEntityKindDefinitionLocked(def); err != nil {
			return err
		}
	}
	return nil
}

func MustRegisterEntityKindDefs(defs ...EntityKindDef) {
	if err := RegisterEntityKindDefs(defs...); err != nil {
		panic(err)
	}
}

func EntityCategoryOfKind(kind EntityKind) (EntityCategory, bool) {
	if entry := kindEntryOf(kind); entry != nil {
		return entry.category, true
	}
	return EntityCategoryNone, false
}

func ResolveEntityKindCategory(kind EntityKind) (EntityCategory, error) {
	if kind == EntityKindNone {
		return EntityCategoryNone, fmt.Errorf("entity kind must not be none")
	}
	// EntityKind is uint8 and EntityKindBits is 8: every value fits the mask, so
	// there is no "kind above the mask" case to refuse (U-0099 / U-0123).
	category, ok := EntityCategoryOfKind(kind)
	if !ok {
		return EntityCategoryNone, fmt.Errorf("%w: kind %d category is not registered", ErrInvalidEntityID, kind)
	}
	return category, nil
}

func MustEntityCategoryOfKind(kind EntityKind) EntityCategory {
	category, err := ResolveEntityKindCategory(kind)
	if err != nil {
		panic(err)
	}
	return category
}

func registerEntityKindCategoryLocked(kind EntityKind, category EntityCategory) error {
	return registerEntityKindDefinitionLocked(EntityKindDef{Kind: kind, Category: category})
}

func registerEntityKindDefinitionLocked(def EntityKindDef) error {
	kind := def.Kind
	category := def.Category
	if kind == EntityKindNone {
		return fmt.Errorf("entity kind must not be none")
	}
	if category == EntityCategoryNone {
		return fmt.Errorf("entity category must not be none for kind %d", kind)
	}
	existing := kindEntryOf(kind)
	if existing == nil {
		kindEntries[kind].Store(&entityKindEntry{category: category, policy: def.RemotePolicy})
		refreshLockRankLocked(kind)
		return nil
	}
	if existing.category != category {
		return fmt.Errorf("entity kind %d category mismatch: registered=%d new=%d", kind, existing.category, category)
	}
	// A category-only registration carries policy none, so none is an "unknown
	// yet" value that a later definition may fill in. The reverse is a partial
	// re-declaration, not a downgrade request, so it is ignored rather than
	// refused. Anything else is two sources disagreeing.
	switch {
	case existing.policy == def.RemotePolicy:
		return nil
	case existing.policy == RemotePolicyNone:
		next := *existing
		next.policy = def.RemotePolicy
		kindEntries[kind].Store(&next)
		refreshLockRankLocked(kind)
		return nil
	case def.RemotePolicy == RemotePolicyNone:
		return nil
	default:
		return fmt.Errorf("entity kind %d remote policy mismatch: registered=%d new=%d", kind, existing.policy, def.RemotePolicy)
	}
}

// GetEntityBuilderParam retrieves the registered builder for an entity kind.
func GetEntityBuilderParam(kind EntityKind) *EntityBuilderParam {
	if entry := kindEntryOf(kind); entry != nil {
		return entry.builder
	}
	return nil
}

// GetAllEntityBuilders returns all registered builders.
func GetAllEntityBuilders() []*EntityBuilderParam {
	result := make([]*EntityBuilderParam, 0, 16)
	for i := range kindEntries {
		if entry := kindEntries[i].Load(); entry != nil && entry.builder != nil {
			result = append(result, entry.builder)
		}
	}
	return result
}

func ResetEntityRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i := range kindEntries {
		kindEntries[i].Store(nil)
		lockRankByKind[i].Store(0)
	}
}

func GetEntityKindRemotePolicy(kind EntityKind) RemotePolicy {
	// The builder shares this entry, so "no entry" also means "no builder":
	// the old fallback through GetEntityBuilderParam could never fire, because
	// RegisterEntityBuilder declares the kind before it stores the builder.
	if entry := kindEntryOf(kind); entry != nil {
		return entry.policy
	}
	return RemotePolicyNone
}

func IsEntityKindRemoteCapable(kind EntityKind) bool {
	return GetEntityKindRemotePolicy(kind).RemoteCapable()
}

func IsEntityKindRemoteManaged(kind EntityKind) bool {
	return GetEntityKindRemotePolicy(kind).RemoteManaged()
}

// --- Entity creation ---

func resolveEntityBuilder(param *EntityCreateParam) (*EntityBuilderParam, error) {
	if param == nil {
		return nil, fmt.Errorf("entity create param is nil")
	}
	if param.Kind == EntityKindNone {
		return nil, fmt.Errorf("entity kind must not be none")
	}
	bp := GetEntityBuilderParam(param.Kind)
	if bp == nil {
		return nil, fmt.Errorf("no entity builder registered for category %d kind %d", param.Category, param.Kind)
	}
	if param.Category == EntityCategoryNone {
		param.Category = bp.Category
	} else if bp.Category != EntityCategoryNone && param.Category != bp.Category {
		return nil, fmt.Errorf("entity builder kind %d category mismatch: param=%d builder=%d", param.Kind, param.Category, bp.Category)
	}
	return bp, nil
}

// BuildEntity creates an entity using the registered builder without adding it
// to the global manager or acquiring its guard lock. It is intended for
// loaders that need to finish construction before deciding how to publish the
// entity into memory.
func BuildEntity(param *EntityCreateParam) (IThreadSafeEntity, error) {
	bp, err := resolveEntityBuilder(param)
	if err != nil {
		return nil, err
	}

	if err := param.NormalizeID(param.Kind); err != nil {
		return nil, err
	}

	// Create DAOs for new entities
	if param.IsCreate && param.Dao == nil {
		param.Dao = make(map[string]DaoInterface)
		for _, daoBuilder := range bp.DaoBuilders {
			dao := daoBuilder()
			dao.SetId(param.StorageID())
			param.Dao[dao.CollName()] = dao
		}
	}

	// Build entity (components are created and InitAll is called inside Builder)
	e, err := bp.Builder(param)
	if err != nil {
		return nil, fmt.Errorf("build entity category %d: %w", param.Category, err)
	}
	if err := validateBuiltEntityPolicy(bp, e); err != nil {
		return nil, err
	}
	if param.RemoteRestore != nil {
		remote, ok := e.(IThreadSafeRemoteEntity)
		if !ok {
			return nil, fmt.Errorf("entity kind %d has remote restore state but is not remote-managed", param.Kind)
		}
		if err := remote.SetRemoteVersionVector(*param.RemoteRestore); err != nil {
			return nil, fmt.Errorf("entity kind %d restore remote version: %w", param.Kind, err)
		}
		if err := remote.TransitionRemoteOwnership(RemoteOwnershipRecovering); err != nil {
			return nil, fmt.Errorf("entity kind %d restore remote ownership: %w", param.Kind, err)
		}
	}
	if e.Base() != nil {
		e.Base().SetLifetime(resolveEntityLifetime(param, bp))
	}
	initEntitySync(e, param, bp)

	// Entity-level post-initialization (after all components are ready)
	if err := e.OnInitFinish(param); err != nil {
		return nil, fmt.Errorf("entity OnInitFinish category %d: %w", param.Category, err)
	}
	return e, nil
}

func normalizeBuilderPolicy(param *EntityBuilderParam) {
	if param == nil {
		return
	}
	if param.Lifetime == EntityLifetimeDefault {
		param.Lifetime = DefaultEntityLifetime(param.NoPersist, param.RemotePolicy)
	}
	if err := ValidateEntityPolicy(param.Kind, param.NoPersist, param.RemotePolicy, param.Lifetime); err != nil {
		panic(err)
	}
}

func (param *EntityBuilderParam) IsRemoteCapable() bool {
	return param != nil && param.RemotePolicy.RemoteCapable()
}

func validateBuiltEntityPolicy(bp *EntityBuilderParam, e IThreadSafeEntity) error {
	if bp == nil || e == nil {
		return nil
	}
	if bp.RemotePolicy.RemoteManaged() {
		if _, ok := e.(IThreadSafeRemoteEntity); !ok {
			return fmt.Errorf("entity kind %d remote=managed but does not implement IThreadSafeRemoteEntity", bp.Kind)
		}
	}
	return nil
}

func resolveEntityLifetime(param *EntityCreateParam, bp *EntityBuilderParam) EntityLifetime {
	if param != nil && param.Lifetime != EntityLifetimeDefault {
		return param.Lifetime
	}
	if bp != nil && bp.Lifetime != EntityLifetimeDefault {
		return bp.Lifetime
	}
	noPersist := false
	remotePolicy := RemotePolicyNone
	if bp != nil {
		noPersist = bp.NoPersist
		remotePolicy = bp.RemotePolicy
	}
	return DefaultEntityLifetime(noPersist, remotePolicy)
}

func initEntitySync(e IThreadSafeEntity, param *EntityCreateParam, bp *EntityBuilderParam) {
	if e == nil || e.Base() == nil {
		return
	}
	if param != nil && param.Sync != nil {
		if !param.Sync.Enabled {
			e.Base().SetSyncState(nil)
			return
		}
		syncParam := *param.Sync
		if syncParam.EntityID == 0 {
			syncParam.EntityID = e.ID()
		}
		if syncParam.Packer == nil && bp != nil && bp.Sync.PackerFactory != nil {
			syncParam.Packer = bp.Sync.PackerFactory(e)
		}
		if syncParam.EntityKind == 0 {
			syncParam.EntityKind = uint32(e.GetEntityKind())
		}
		e.Base().EnableSync(syncParam)
		return
	}
	if bp == nil || !bp.Sync.Enabled {
		return
	}
	syncParam := bp.Sync.toCreateParam(e)
	e.Base().EnableSync(syncParam)
}
