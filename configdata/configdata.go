package configdata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	fctx "github.com/tjbdwanghaibo/cube-core/ctx"
	"github.com/tjbdwanghaibo/cube-core/lifecycle"
)

// Name is the stable identifier for a business configuration object.
type Name string

var (
	ErrSnapshotNotFound = errors.New("configdata: snapshot not found")
	ErrTableNotFound    = errors.New("configdata: table not found")
	ErrObjectNotFound   = errors.New("configdata: object not found")
	ErrCustomNotFound   = errors.New("configdata: custom data not found")
)

// Snapshot is an immutable, point-in-time view of all business configuration.
// A running handler should keep reading the same snapshot through the bound request Context even if
// the global Store is hot-reloaded while the handler is executing.
type Snapshot struct {
	Version  uint64
	LoadedAt time.Time
	Hash     string

	tables  map[Name]any
	objects map[Name]any
	custom  map[Name]any
	// fingerprints holds explicit content digests set via SetFingerprint;
	// they take precedence over json.Marshal in the snapshot hash, so
	// aggregates with unexported state (external table sets) still make
	// content drift visible.
	fingerprints map[Name]string
}

func newSnapshot(version uint64) *Snapshot {
	return &Snapshot{
		Version:      version,
		LoadedAt:     time.Now(),
		tables:       make(map[Name]any),
		objects:      make(map[Name]any),
		custom:       make(map[Name]any),
		fingerprints: make(map[Name]string),
	}
}

// SetFingerprint records an explicit content digest for one snapshot member,
// overriding json.Marshal in the hash computation. Build callbacks whose
// value would serialize opaquely (unexported fields, external generated
// aggregates) must call this or their content stays invisible to Hash.
// SetFingerprint is only valid inside a build callback, before the snapshot
// is finalized; on a published snapshot it is a no-op (the Hash is already
// sealed and must stay immutable).
func (s *Snapshot) SetFingerprint(name Name, digest string) {
	if s == nil || name == "" || s.Hash != "" {
		return
	}
	if s.fingerprints == nil {
		s.fingerprints = make(map[Name]string)
	}
	s.fingerprints[name] = digest
}

func (s *Snapshot) table(name Name) (any, bool) {
	if s == nil {
		return nil, false
	}
	v, ok := s.tables[name]
	return v, ok
}

func (s *Snapshot) object(name Name) (any, bool) {
	if s == nil {
		return nil, false
	}
	v, ok := s.objects[name]
	return v, ok
}

func (s *Snapshot) customData(name Name) (any, bool) {
	if s == nil {
		return nil, false
	}
	v, ok := s.custom[name]
	return v, ok
}

func (s *Snapshot) finalize() error {
	if s == nil {
		return nil
	}
	hasher := sha256.New()
	writePart := func(kind, name string, payload []byte) {
		// Length-prefixed segments: no delimiter collision even when a name
		// contains the separator characters; the payload is streamed, never
		// materialized into an intermediate string.
		_, _ = fmt.Fprintf(hasher, "%d:%s%d:%s%d:", len(kind), kind, len(name), name, len(payload))
		_, _ = hasher.Write(payload)
	}
	consumed := make(map[Name]bool, len(s.fingerprints))
	appendPart := func(kind string, values map[Name]any) error {
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, string(name))
		}
		sort.Strings(names)
		for _, name := range names {
			if digest, ok := s.fingerprints[Name(name)]; ok {
				consumed[Name(name)] = true
				writePart(kind, name, []byte(digest))
				continue
			}
			value := values[Name(name)]
			raw, err := json.Marshal(value)
			if err != nil {
				// A swallowed error here would freeze the hash into a
				// constant and silently disable drift detection.
				return fmt.Errorf("configdata: hash %s %s: %w (provide Snapshot.SetFingerprint for unmarshalable values)", kind, name, err)
			}
			// A struct whose fields are all unexported marshals to "{}" —
			// the member's content would be invisible to the hash. Demand an
			// explicit fingerprint instead of silently degrading.
			if string(raw) == "{}" && marshalsOpaquely(value) {
				return fmt.Errorf("configdata: hash %s %s: value serializes opaquely (no exported fields) — call Snapshot.SetFingerprint in the build callback", kind, name)
			}
			writePart(kind, name, raw)
		}
		return nil
	}
	if err := appendPart("table", s.tables); err != nil {
		return err
	}
	if err := appendPart("object", s.objects); err != nil {
		return err
	}
	if err := appendPart("custom", s.custom); err != nil {
		return err
	}
	for name := range s.fingerprints {
		if !consumed[name] {
			return fmt.Errorf("configdata: fingerprint set for unknown member %s (typo?)", name)
		}
	}
	s.Hash = hex.EncodeToString(hasher.Sum(nil))
	return nil
}

// marshalsOpaquely reports whether v is a struct (or pointer to one) with
// fields but none of them exported — json.Marshal yields "{}" for it.
func marshalsOpaquely(v any) bool {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct || t.NumField() == 0 {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			return false
		}
	}
	return true
}

// Table is an immutable keyed business configuration table.
type Table[K comparable, V any] struct {
	name    Name
	rows    []V
	byKey   map[K]int
	indexes map[string]map[string][]int
}

func newTable[K comparable, V any](def TableDef[K, V], rows []V) (*Table[K, V], error) {
	t := &Table[K, V]{
		name:    def.Name,
		rows:    append([]V(nil), rows...),
		byKey:   make(map[K]int, len(rows)),
		indexes: make(map[string]map[string][]int, len(def.Indexes)),
	}
	for i, row := range t.rows {
		key := def.Key(row)
		if _, exists := t.byKey[key]; exists {
			return nil, fmt.Errorf("configdata: table %s duplicate key %v", def.Name, key)
		}
		t.byKey[key] = i
		for _, idx := range def.Indexes {
			if idx.Name == "" || idx.Key == nil {
				continue
			}
			value := idx.Key(row)
			if value == "" && idx.SkipEmpty {
				continue
			}
			m := t.indexes[idx.Name]
			if m == nil {
				m = make(map[string][]int)
				t.indexes[idx.Name] = m
			}
			m[value] = append(m[value], i)
		}
	}
	return t, nil
}

// MarshalJSON serializes the table name and rows (in file order). Without
// it the snapshot hash would see an empty object for every table (all fields
// are unexported) and config drift in table content would go undetected.
func (t *Table[K, V]) MarshalJSON() ([]byte, error) {
	if t == nil {
		return []byte("null"), nil
	}
	return json.Marshal(struct {
		Name Name `json:"name"`
		Rows []V  `json:"rows"`
	}{Name: t.name, Rows: t.rows})
}

func (t *Table[K, V]) Name() Name {
	if t == nil {
		return ""
	}
	return t.name
}

func (t *Table[K, V]) Len() int {
	if t == nil {
		return 0
	}
	return len(t.rows)
}

func (t *Table[K, V]) Get(key K) (V, bool) {
	var zero V
	if t == nil {
		return zero, false
	}
	idx, ok := t.byKey[key]
	if !ok {
		return zero, false
	}
	return t.rows[idx], true
}

func (t *Table[K, V]) MustGet(key K) V {
	v, ok := t.Get(key)
	if !ok {
		panic(fmt.Errorf("configdata: table %s key %v: %w", t.Name(), key, ErrTableNotFound))
	}
	return v
}

func (t *Table[K, V]) Rows() []V {
	if t == nil {
		return nil
	}
	return append([]V(nil), t.rows...)
}

func (t *Table[K, V]) GetByIndex(indexName string, value string) []V {
	if t == nil {
		return nil
	}
	idx := t.indexes[indexName]
	if len(idx) == 0 {
		return nil
	}
	rowIndexes := idx[value]
	if len(rowIndexes) == 0 {
		return nil
	}
	ret := make([]V, 0, len(rowIndexes))
	for _, rowIndex := range rowIndexes {
		ret = append(ret, t.rows[rowIndex])
	}
	return ret
}

// BuildContext exposes already-loaded config while building and validating a
// new snapshot. It is never shared with live handlers.
type BuildContext struct {
	Dir      string
	Snapshot *Snapshot
	// StrictJSON rejects unknown fields when decoding table/object files
	// (Store.SetStrictJSON).
	StrictJSON bool
}

func TableFrom[K comparable, V any](snap *Snapshot, name Name) (*Table[K, V], bool) {
	raw, ok := snap.table(name)
	if !ok {
		return nil, false
	}
	table, ok := raw.(*Table[K, V])
	return table, ok
}

func MustTableFrom[K comparable, V any](snap *Snapshot, name Name) *Table[K, V] {
	table, ok := TableFrom[K, V](snap, name)
	if !ok || table == nil {
		panic(fmt.Errorf("configdata: table %s: %w", name, ErrTableNotFound))
	}
	return table
}

func ObjectFrom[V any](snap *Snapshot, name Name) (V, bool) {
	var zero V
	raw, ok := snap.object(name)
	if !ok {
		return zero, false
	}
	obj, ok := raw.(V)
	if !ok {
		return zero, false
	}
	return obj, true
}

func MustObjectFrom[V any](snap *Snapshot, name Name) V {
	obj, ok := ObjectFrom[V](snap, name)
	if !ok {
		panic(fmt.Errorf("configdata: object %s: %w", name, ErrObjectNotFound))
	}
	return obj
}

func CustomFrom[V any](snap *Snapshot, name Name) (V, bool) {
	var zero V
	raw, ok := snap.customData(name)
	if !ok {
		return zero, false
	}
	obj, ok := raw.(V)
	if !ok {
		return zero, false
	}
	return obj, true
}

func MustCustomFrom[V any](snap *Snapshot, name Name) V {
	obj, ok := CustomFrom[V](snap, name)
	if !ok {
		panic(fmt.Errorf("configdata: custom %s: %w", name, ErrCustomNotFound))
	}
	return obj
}

type tableDef interface {
	name() Name
	file() string
	load(*BuildContext) (any, error)
	validate(*BuildContext, any) error
}

type objectDef interface {
	name() Name
	file() string
	load(*BuildContext) (any, error)
	validate(*BuildContext, any) error
}

type customDef interface {
	name() Name
	build(*BuildContext) (any, error)
	validate(*BuildContext, any) error
}

// IndexDef describes a string-keyed secondary index. Generated config getters
// can wrap the string key with typed helpers.
type IndexDef[V any] struct {
	Name      string
	Key       func(V) string
	SkipEmpty bool
}

// TableDef describes a JSON-backed keyed table.
type TableDef[K comparable, V any] struct {
	Name    Name
	File    string
	Key     func(V) K
	Indexes []IndexDef[V]
	// Validate runs once per row after every table and object is loaded.
	Validate func(*BuildContext, V) error
	// ValidateTable runs once per table before the row loop — the place for
	// whole-table invariants (reference target existence, row-count bounds)
	// that must fire even when the table is empty.
	ValidateTable func(*BuildContext, *Table[K, V]) error
}

func (d TableDef[K, V]) name() Name   { return d.Name }
func (d TableDef[K, V]) file() string { return d.File }

func (d TableDef[K, V]) load(ctx *BuildContext) (any, error) {
	if d.Name == "" {
		return nil, errors.New("configdata: table name is empty")
	}
	if d.File == "" {
		return nil, fmt.Errorf("configdata: table %s file is empty", d.Name)
	}
	if d.Key == nil {
		return nil, fmt.Errorf("configdata: table %s key func is nil", d.Name)
	}
	var rows []V
	if err := readJSON(filepath.Join(ctx.Dir, d.File), &rows, ctx.StrictJSON); err != nil {
		return nil, fmt.Errorf("configdata: load table %s: %w", d.Name, err)
	}
	return newTable(d, rows)
}

func (d TableDef[K, V]) validate(ctx *BuildContext, raw any) error {
	if d.Validate == nil && d.ValidateTable == nil {
		return nil
	}
	table, ok := raw.(*Table[K, V])
	if !ok {
		return fmt.Errorf("configdata: table %s type mismatch", d.Name)
	}
	if d.ValidateTable != nil {
		if err := d.ValidateTable(ctx, table); err != nil {
			return fmt.Errorf("configdata: validate table %s: %w", d.Name, err)
		}
	}
	if d.Validate == nil {
		return nil
	}
	for _, row := range table.rows {
		if err := d.Validate(ctx, row); err != nil {
			return fmt.Errorf("configdata: validate table %s: %w", d.Name, err)
		}
	}
	return nil
}

// ObjectDef describes a JSON object config.
type ObjectDef[V any] struct {
	Name     Name
	File     string
	Validate func(*BuildContext, V) error
}

func (d ObjectDef[V]) name() Name   { return d.Name }
func (d ObjectDef[V]) file() string { return d.File }

func (d ObjectDef[V]) load(ctx *BuildContext) (any, error) {
	if d.Name == "" {
		return nil, errors.New("configdata: object name is empty")
	}
	if d.File == "" {
		return nil, fmt.Errorf("configdata: object %s file is empty", d.Name)
	}
	var obj V
	if err := readJSON(filepath.Join(ctx.Dir, d.File), &obj, ctx.StrictJSON); err != nil {
		return nil, fmt.Errorf("configdata: load object %s: %w", d.Name, err)
	}
	return obj, nil
}

func (d ObjectDef[V]) validate(ctx *BuildContext, raw any) error {
	if d.Validate == nil {
		return nil
	}
	obj, ok := raw.(V)
	if !ok {
		return fmt.Errorf("configdata: object %s type mismatch", d.Name)
	}
	if err := d.Validate(ctx, obj); err != nil {
		return fmt.Errorf("configdata: validate object %s: %w", d.Name, err)
	}
	return nil
}

// CustomDef builds runtime-only config from already-loaded table/object data.
type CustomDef[V any] struct {
	Name     Name
	Build    func(*BuildContext) (V, error)
	Validate func(*BuildContext, V) error
}

func (d CustomDef[V]) name() Name { return d.Name }

func (d CustomDef[V]) build(ctx *BuildContext) (any, error) {
	if d.Name == "" {
		return nil, errors.New("configdata: custom name is empty")
	}
	if d.Build == nil {
		return nil, fmt.Errorf("configdata: custom %s build func is nil", d.Name)
	}
	v, err := d.Build(ctx)
	if err != nil {
		return nil, fmt.Errorf("configdata: build custom %s: %w", d.Name, err)
	}
	return v, nil
}

func (d CustomDef[V]) validate(ctx *BuildContext, raw any) error {
	if d.Validate == nil {
		return nil
	}
	obj, ok := raw.(V)
	if !ok {
		return fmt.Errorf("configdata: custom %s type mismatch", d.Name)
	}
	if err := d.Validate(ctx, obj); err != nil {
		return fmt.Errorf("configdata: validate custom %s: %w", d.Name, err)
	}
	return nil
}

// Registry stores schema definitions. It is normally populated during game
// bootstrap, before Store.Load or Store.Reload is called.
type Registry struct {
	mu      sync.RWMutex
	tables  []tableDef
	objects []objectDef
	custom  []customDef
	names   map[Name]string
}

func NewRegistry() *Registry {
	return &Registry{names: make(map[Name]string)}
}

func (r *Registry) RegisterTable(def tableDef) error {
	if def == nil {
		return errors.New("configdata: nil table def")
	}
	return r.register(def.name(), "table", func() {
		r.tables = append(r.tables, def)
	})
}

func (r *Registry) RegisterObject(def objectDef) error {
	if def == nil {
		return errors.New("configdata: nil object def")
	}
	return r.register(def.name(), "object", func() {
		r.objects = append(r.objects, def)
	})
}

func (r *Registry) RegisterCustom(def customDef) error {
	if def == nil {
		return errors.New("configdata: nil custom def")
	}
	return r.register(def.name(), "custom", func() {
		r.custom = append(r.custom, def)
	})
}

func (r *Registry) register(name Name, kind string, appendDef func()) error {
	if name == "" {
		return fmt.Errorf("configdata: %s name is empty", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if oldKind, exists := r.names[name]; exists {
		// A same-kind duplicate used to be a silent no-op ("idempotent"),
		// which also silently discarded a *different* definition that
		// happened to collide on the name (easy with inferred auto-table
		// names) — the second table then never loaded. Defs contain
		// closures and cannot be compared, so every duplicate is an error.
		return fmt.Errorf("configdata: config name %s already registered (old=%s new=%s)", name, oldKind, kind)
	}
	r.names[name] = kind
	appendDef()
	return nil
}

func (r *Registry) defs() ([]tableDef, []objectDef, []customDef) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]tableDef(nil), r.tables...), append([]objectDef(nil), r.objects...), append([]customDef(nil), r.custom...)
}

func RegisterTable[K comparable, V any](r *Registry, def TableDef[K, V]) error {
	if r == nil {
		return errors.New("configdata: registry is nil")
	}
	return r.RegisterTable(def)
}

func MustRegisterTable[K comparable, V any](r *Registry, def TableDef[K, V]) {
	if err := RegisterTable(r, def); err != nil {
		panic(err)
	}
}

func RegisterObject[V any](r *Registry, def ObjectDef[V]) error {
	if r == nil {
		return errors.New("configdata: registry is nil")
	}
	return r.RegisterObject(def)
}

func MustRegisterObject[V any](r *Registry, def ObjectDef[V]) {
	if err := RegisterObject(r, def); err != nil {
		panic(err)
	}
}

func RegisterCustom[V any](r *Registry, def CustomDef[V]) error {
	if r == nil {
		return errors.New("configdata: registry is nil")
	}
	return r.RegisterCustom(def)
}

func MustRegisterCustom[V any](r *Registry, def CustomDef[V]) {
	if err := RegisterCustom(r, def); err != nil {
		panic(err)
	}
}

type ReloadEvent struct {
	Reason    string
	Old       *Snapshot
	New       *Snapshot
	StartedAt time.Time
	AppliedAt time.Time
}

type ReloadListener interface {
	Name() string
	ValidateReload(context.Context, ReloadEvent) error
	BeforeApplyReload(context.Context, ReloadEvent) error
	AfterApplyReload(context.Context, ReloadEvent) error
	RollbackReload(context.Context, ReloadEvent, error)
}

type ReloadHook struct {
	HookName    string
	Validate    func(context.Context, ReloadEvent) error
	BeforeApply func(context.Context, ReloadEvent) error
	AfterApply  func(context.Context, ReloadEvent) error
	Rollback    func(context.Context, ReloadEvent, error)
}

func (h ReloadHook) Name() string {
	if h.HookName == "" {
		return "anonymous"
	}
	return h.HookName
}

func (h ReloadHook) ValidateReload(ctx context.Context, event ReloadEvent) error {
	if h.Validate == nil {
		return nil
	}
	return h.Validate(ctx, event)
}

func (h ReloadHook) BeforeApplyReload(ctx context.Context, event ReloadEvent) error {
	if h.BeforeApply == nil {
		return nil
	}
	return h.BeforeApply(ctx, event)
}

func (h ReloadHook) AfterApplyReload(ctx context.Context, event ReloadEvent) error {
	if h.AfterApply == nil {
		return nil
	}
	return h.AfterApply(ctx, event)
}

func (h ReloadHook) RollbackReload(ctx context.Context, event ReloadEvent, cause error) {
	if h.Rollback != nil {
		h.Rollback(ctx, event, cause)
	}
}

// Store owns the current immutable config snapshot and supports hot reload by
// atomically publishing a newly built snapshot.
type Store struct {
	registry  *Registry
	dir       string
	dirMu     sync.RWMutex
	strict    atomic.Bool
	lifecycle *lifecycle.Registry
	current   atomic.Pointer[Snapshot]
	previous  atomic.Pointer[Snapshot]
	version   atomic.Uint64
	mu        sync.Mutex
	// valMu keeps ReloadListener.ValidateReload single-threaded across
	// Reload and the lock-free DryRun (listeners commonly reuse scratch
	// state in Validate); it is held only for the validate phase, so a
	// long DryRun build still never blocks an emergency Rollback.
	valMu    sync.Mutex
	listMu   sync.RWMutex
	listener []reloadListenerEntry
	nextID   uint64
}

type reloadListenerEntry struct {
	id       uint64
	listener ReloadListener
}

func NewStore(registry *Registry, dir string) *Store {
	if registry == nil {
		registry = NewRegistry()
	}
	return &Store{registry: registry, dir: dir}
}

func (s *Store) Registry() *Registry {
	if s == nil {
		return nil
	}
	return s.registry
}

func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	s.dirMu.RLock()
	defer s.dirMu.RUnlock()
	return s.dir
}

func (s *Store) SetLifecycleRegistry(reg *lifecycle.Registry) {
	if s == nil {
		return
	}
	s.listMu.Lock()
	s.lifecycle = reg
	s.listMu.Unlock()
}

func (s *Store) SetDir(dir string) {
	if s == nil {
		return
	}
	s.dirMu.Lock()
	s.dir = dir
	s.dirMu.Unlock()
}

// SetStrictJSON toggles strict decoding for table and object files: unknown
// JSON fields are rejected instead of silently ignored. Off by default (a
// renamed column otherwise zeroes silently — with reference checks then
// short-circuited by the zero values, the drift is invisible).
func (s *Store) SetStrictJSON(strict bool) {
	if s != nil {
		s.strict.Store(strict)
	}
}

func (s *Store) Current() *Snapshot {
	if s == nil {
		return nil
	}
	return s.current.Load()
}

func (s *Store) Load(ctx context.Context) (*Snapshot, error) {
	return s.Reload(ctx)
}

func (s *Store) Reload(ctx context.Context) (*Snapshot, error) {
	return s.ReloadWithReason(ctx, "manual")
}

func (s *Store) ReloadWithReason(ctx context.Context, reason string) (*Snapshot, error) {
	if s == nil {
		return nil, errors.New("configdata: store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.current.Load()
	started := time.Now()
	// Version numbers are allocated from a monotonic counter and never
	// reverted: a failed or rolled-back generation burns its number, so a
	// number handed to listeners (or to the metrics gauge) is never reused
	// for different content.
	snap, err := s.build(ctx, s.version.Add(1))
	if err != nil {
		return nil, err
	}
	if err := s.commit(ctx, commitRequest{
		reason: reason, emitName: "configdata",
		old: old, target: snap, started: started,
	}); err != nil {
		return nil, err
	}
	return snap, nil
}

// commitRequest carries one snapshot publication (reload or rollback).
type commitRequest struct {
	reason   string
	emitName string
	old      *Snapshot
	target   *Snapshot
	started  time.Time
	extra    map[string]any
	// clearPrevious consumes the one-level undo slot instead of writing it:
	// a rollback must not leave the just-rejected snapshot reachable as
	// "previous" (a second rollback would republish the bad generation).
	clearPrevious bool
}

// publishMu serializes transitions of the process-global slots (defaultStore
// and the fctx runtime-config) across Stores: two stores committing
// concurrently must not lose each other's updates on revert.
var publishMu sync.Mutex

// commit publishes req.target through the full listener protocol. Contract:
//   - Every listener callback (and the lifecycle emit) is panic-contained: a
//     panic becomes an error and takes the failure path instead of leaving
//     the store with a half-published generation.
//   - RollbackReload pairs with BeforeApplyReload: it fires, in reverse
//     order, for exactly the listeners whose BeforeApplyReload succeeded —
//     regardless of whether their AfterApplyReload ran.
//   - When req.old is nil (first load) there is no previous generation to
//     restore, so rollback callbacks are skipped entirely: listeners must
//     never interpret a nil Old as "revert to defaults".
//   - The store state (current/version/defaultStore/fctx) is applied and
//     reverted as one unit under s.mu; no listener runs between the
//     individual stores.
func (s *Store) commit(ctx context.Context, req commitRequest) error {
	event := ReloadEvent{Reason: req.reason, Old: req.old, New: req.target, StartedAt: req.started}
	listeners := s.reloadListeners()
	s.valMu.Lock()
	err := runReloadValidate(ctx, listeners, event)
	s.valMu.Unlock()
	if err != nil {
		return err
	}
	prepared, err := runReloadBeforeApply(ctx, listeners, event)
	if err != nil {
		runReloadRollback(ctx, listeners[:prepared], event, err)
		return err
	}
	event.AppliedAt = time.Now()
	publishMu.Lock()
	prevDefault := DefaultStore()
	prevConfig := fctx.RuntimeConfig()
	s.current.Store(req.target)
	SetDefaultStore(s)
	fctx.SetRuntimeConfig(req.target)
	publishMu.Unlock()
	revert := func() {
		// Restore the saved values, not values derived from req.old: the
		// global slots may have belonged to a different Store.
		publishMu.Lock()
		s.current.Store(req.old)
		SetDefaultStore(prevDefault)
		fctx.SetRuntimeConfig(prevConfig)
		publishMu.Unlock()
	}
	data := map[string]any{
		"reason":  req.reason,
		"version": req.target.Version,
		"hash":    req.target.Hash,
	}
	for k, v := range req.extra {
		data[k] = v
	}
	if err := s.emitConfigReload(ctx, lifecycle.Event{
		Phase: lifecycle.PhaseConfigReload,
		Name:  req.emitName,
		Data:  data,
	}); err != nil {
		revert()
		runReloadRollback(ctx, listeners[:prepared], event, err)
		return err
	}
	if err := runReloadAfterApply(ctx, listeners, event); err != nil {
		revert()
		runReloadRollback(ctx, listeners[:prepared], event, err)
		return err
	}
	if req.clearPrevious {
		s.previous.Store(nil)
	} else if req.old != nil {
		s.previous.Store(req.old)
	}
	return nil
}

// DryRun builds and validates a candidate snapshot without publishing it.
// It deliberately does not take the store mutex: a long dry-run over a large
// dataset must never block an emergency Rollback.
func (s *Store) DryRun(ctx context.Context, reason string) (*Snapshot, error) {
	if s == nil {
		return nil, errors.New("configdata: store is nil")
	}
	old := s.current.Load()
	started := time.Now()
	snap, err := s.build(ctx, s.version.Add(1)) // burns a number: candidates never collide with published versions
	if err != nil {
		return nil, err
	}
	event := ReloadEvent{Reason: reason, Old: old, New: snap, StartedAt: started}
	s.valMu.Lock()
	err = runReloadValidate(ctx, s.reloadListeners(), event)
	s.valMu.Unlock()
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *Store) Rollback(ctx context.Context, reason string) (*Snapshot, error) {
	if s == nil {
		return nil, errors.New("configdata: store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.previous.Load()
	if prev == nil {
		return nil, errors.New("configdata: previous snapshot not found")
	}
	old := s.current.Load()
	extra := map[string]any{}
	if old != nil {
		extra["from_version"] = old.Version
	}
	if err := s.commit(ctx, commitRequest{
		reason: reason, emitName: "configdata.rollback",
		old: old, target: prev, started: time.Now(), extra: extra,
		clearPrevious: true,
	}); err != nil {
		return nil, err
	}
	return prev, nil
}

func (s *Store) emitConfigReload(ctx context.Context, event lifecycle.Event) error {
	if s == nil {
		return nil
	}
	s.listMu.RLock()
	reg := s.lifecycle
	s.listMu.RUnlock()
	if reg == nil {
		return nil
	}
	return safeReloadCall("lifecycle emit", string(event.Phase), func() error {
		return reg.Emit(ctx, event)
	})
}

func (s *Store) AddReloadListener(listener ReloadListener) func() {
	if s == nil || listener == nil {
		return func() {}
	}
	s.listMu.Lock()
	s.nextID++
	id := s.nextID
	s.listener = append(s.listener, reloadListenerEntry{id: id, listener: listener})
	s.listMu.Unlock()
	return func() {
		s.listMu.Lock()
		defer s.listMu.Unlock()
		for i, item := range s.listener {
			if item.id == id {
				s.listener = append(s.listener[:i], s.listener[i+1:]...)
				return
			}
		}
	}
}

func (s *Store) reloadListeners() []ReloadListener {
	if s == nil {
		return nil
	}
	s.listMu.RLock()
	defer s.listMu.RUnlock()
	listeners := make([]ReloadListener, 0, len(s.listener))
	for _, item := range s.listener {
		listeners = append(listeners, item.listener)
	}
	return listeners
}

// safeReloadCall runs one listener callback with the panic containment every
// other callback registry in this repository provides (lifecycle.emitHook,
// etcd.WatchCallback, misc.SafeFunc): a panicking listener must fail the
// reload cleanly, never abandon the store between two of its state stores.
// Callbacks must not call runtime.Goexit (t.Fatal included): Goexit unwinds
// past recover and skips the revert.
func safeReloadCall(phase, name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = wrapPanic(fmt.Sprintf("configdata: reload %s %s", phase, name), r)
		}
	}()
	if err := fn(); err != nil {
		return fmt.Errorf("configdata: reload %s %s: %w", phase, name, err)
	}
	return nil
}

// wrapPanic keeps the errors.Is chain when the panic value is an error, and
// attaches the stack — a panicking generated loader is undiagnosable
// without it.
func wrapPanic(prefix string, r any) error {
	stack := debug.Stack()
	if e, ok := r.(error); ok {
		return fmt.Errorf("%s: panic: %w\n%s", prefix, e, stack)
	}
	return fmt.Errorf("%s: panic: %v\n%s", prefix, r, stack)
}

// safeListenerName resolves a listener's name without letting a broken
// (typed-nil) implementation panic outside the containment.
func safeListenerName(listener ReloadListener) (name string) {
	defer func() {
		if recover() != nil {
			name = "<unnamed>"
		}
	}()
	return listener.Name()
}

func runReloadValidate(ctx context.Context, listeners []ReloadListener, event ReloadEvent) error {
	for _, listener := range listeners {
		if listener == nil {
			continue
		}
		if err := safeReloadCall("validate", safeListenerName(listener), func() error {
			return listener.ValidateReload(ctx, event)
		}); err != nil {
			return err
		}
	}
	return nil
}

// runReloadBeforeApply returns how many listeners prepared successfully so
// the failure paths can roll back exactly the prepared ones.
func runReloadBeforeApply(ctx context.Context, listeners []ReloadListener, event ReloadEvent) (int, error) {
	for i, listener := range listeners {
		if listener == nil {
			continue
		}
		if err := safeReloadCall("before apply", safeListenerName(listener), func() error {
			return listener.BeforeApplyReload(ctx, event)
		}); err != nil {
			return i, err
		}
	}
	return len(listeners), nil
}

func runReloadAfterApply(ctx context.Context, listeners []ReloadListener, event ReloadEvent) error {
	for _, listener := range listeners {
		if listener == nil {
			continue
		}
		if err := safeReloadCall("after apply", safeListenerName(listener), func() error {
			return listener.AfterApplyReload(ctx, event)
		}); err != nil {
			return err
		}
	}
	return nil
}

// runReloadRollback notifies the given prepared listeners in reverse order.
// A nil event.Old means there is no previous generation to restore — the
// callbacks are skipped so no listener ever "rolls back" into defaults.
func runReloadRollback(ctx context.Context, prepared []ReloadListener, event ReloadEvent, cause error) {
	if event.Old == nil {
		return
	}
	for i := len(prepared) - 1; i >= 0; i-- {
		listener := prepared[i]
		if listener == nil {
			continue
		}
		_ = safeReloadCall("rollback", safeListenerName(listener), func() error {
			listener.RollbackReload(ctx, event, cause)
			return nil
		})
	}
}

func (s *Store) build(ctx context.Context, version uint64) (*Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// One consistent dir for the whole build (SetDir is concurrent-safe but
	// must not change the directory between the stat check and the reads).
	dir := s.Dir()
	if dir == "" {
		return nil, errors.New("configdata: data dir is empty")
	}
	if stat, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("configdata: stat dir %s: %w", dir, err)
	} else if !stat.IsDir() {
		return nil, fmt.Errorf("configdata: data dir %s is not a directory", dir)
	}
	tables, objects, custom := s.registry.defs()
	snap := newSnapshot(version)
	buildCtx := &BuildContext{Dir: dir, Snapshot: snap, StrictJSON: s.strict.Load()}
	// A panicking def (business Build/Validate code, or generated loaders
	// choking on malformed data) must fail the build, not the process.
	safeDef := func(kind string, name Name, fn func() error) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = wrapPanic(fmt.Sprintf("configdata: %s %s", kind, name), r)
			}
		}()
		return fn()
	}
	for _, def := range tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := safeDef("load table", def.name(), func() error {
			raw, err := def.load(buildCtx)
			if err != nil {
				return err
			}
			snap.tables[def.name()] = raw
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for _, def := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := safeDef("load object", def.name(), func() error {
			raw, err := def.load(buildCtx)
			if err != nil {
				return err
			}
			snap.objects[def.name()] = raw
			return nil
		}); err != nil {
			return nil, err
		}
	}
	// Validate tables and objects BEFORE building customs: custom builders
	// consume table data assuming referential integrity, and a dangling
	// reference should surface as the precise validate error, not as a
	// builder crash.
	for _, def := range tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := safeDef("validate table", def.name(), func() error {
			return def.validate(buildCtx, snap.tables[def.name()])
		}); err != nil {
			return nil, err
		}
	}
	for _, def := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := safeDef("validate object", def.name(), func() error {
			return def.validate(buildCtx, snap.objects[def.name()])
		}); err != nil {
			return nil, err
		}
	}
	for _, def := range custom {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := safeDef("build custom", def.name(), func() error {
			raw, err := def.build(buildCtx)
			if err != nil {
				return err
			}
			snap.custom[def.name()] = raw
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for _, def := range custom {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := safeDef("validate custom", def.name(), func() error {
			return def.validate(buildCtx, snap.custom[def.name()])
		}); err != nil {
			return nil, err
		}
	}
	if err := snap.finalize(); err != nil {
		return nil, err
	}
	return snap, nil
}

var (
	defaultStore    atomic.Pointer[Store]
	defaultRegistry = NewRegistry()
)

func DefaultRegistry() *Registry {
	return defaultRegistry
}

func SetDefaultStore(store *Store) {
	defaultStore.Store(store)
}

func DefaultStore() *Store {
	return defaultStore.Load()
}

func Current() *Snapshot {
	if store := DefaultStore(); store != nil {
		return store.Current()
	}
	return nil
}

func ActiveSnapshot() *Snapshot {
	if c := fctx.CurrentContext(); c != nil {
		if snap, ok := c.Config.(*Snapshot); ok && snap != nil {
			return snap
		}
	}
	return Current()
}

func readJSON(path string, out any, strict bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(raw)
	// A file truncated to "null" (or emptied) decodes into a zero table with
	// no error — a whole dataset silently vanishing. Refuse it.
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return fmt.Errorf("configdata: %s: empty or null document", path)
	}
	decode := func(data []byte) error {
		if !strict {
			return json.Unmarshal(data, out)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(out); err != nil {
			return err
		}
		// json.Unmarshal rejects trailing content; strict mode must not be
		// more lenient than lenient mode.
		if decoder.More() {
			return fmt.Errorf("trailing content after top-level JSON value")
		}
		return nil
	}
	// Wrapper detection is explicit, not "first decode failed": with an
	// object target the lenient decode of {"data":{...}} would silently
	// succeed as an all-zero value and the wrapper branch would never run.
	if trimmed[0] == '{' {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err == nil {
			wrapperKeys := 0
			var candidate json.RawMessage
			for _, key := range []string{"rows", "records", "data"} {
				value, ok := probe[key]
				if !ok {
					continue
				}
				if string(bytes.TrimSpace(value)) == "null" {
					// An explicit null wrapper must not sneak past the
					// null-document guard above.
					return fmt.Errorf("configdata: %s: wrapper key %q is null", path, key)
				}
				wrapperKeys++
				if candidate == nil {
					candidate = value
				}
			}
			if wrapperKeys > 1 {
				return fmt.Errorf("configdata: %s: multiple wrapper keys (rows/records/data) present — ambiguous document", path)
			}
			// Treat as a wrapper only when the document is exactly one
			// wrapper key and nothing else; any other shape is the real
			// document.
			if wrapperKeys == 1 && len(probe) == 1 {
				if err := decode(candidate); err != nil {
					return fmt.Errorf("configdata: %s: %w", path, err)
				}
				return nil
			}
		}
	}
	if err := decode(raw); err != nil {
		return fmt.Errorf("configdata: %s: %w", path, err)
	}
	return nil
}
