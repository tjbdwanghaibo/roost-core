// Package mongotest is a filter-evaluating in-memory MongoDB for tests.
//
// It is exported — rather than internal to the kit — because a test double
// that does not evaluate its filters is worse than none: it makes every
// version CAS, lease predicate, unique-index conflict and tombstone guard
// pass unconditionally, so the semantics those constructs exist to enforce go
// untested. Business repositories and roost-service need the same double the
// kit's own tests use, and hand-rolled copies reliably get it wrong (comparing
// with fmt.Sprint, encoding documents with encoding/json instead of bson, or
// returning nil from Pipeline so the production read path never executes).
//
// The contract that makes it trustworthy: an unsupported construct returns
// ErrUnsupported instead of silently matching, scalars round-trip through the
// real bson codec rather than hand-written widening rules, and
// WithTransaction snapshots every collection so an abort really rolls back.
package mongotest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// ErrUnsupported marks a query or update construct the fake does not model.
// It is returned instead of guessing, so a test can never pass because the
// fake quietly ignored something.
var ErrUnsupported = errors.New("mongofake: unsupported construct")

// Client implements fmongo.IMongo.
type Client struct {
	mu  sync.Mutex
	dbs map[string]*Database

	// TransientRetries makes WithTransaction re-invoke the callback this many
	// extra times before the final attempt, reproducing the documented
	// "automatic retry" of ISession.WithTransaction (the mongo driver retries
	// on TransientTransactionError / UnknownTransactionCommitResult). Callback
	// bodies must be idempotent; this switch proves it.
	TransientRetries int

	// StartSessionErr, when set, fails StartSession.
	StartSessionErr error

	sessions int
	attempts int
}

func NewClient() *Client { return &Client{dbs: make(map[string]*Database)} }

func (c *Client) Database(name string) fmongo.IDatabase {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dbs == nil {
		c.dbs = make(map[string]*Database)
	}
	db := c.dbs[name]
	if db == nil {
		db = &Database{name: name, collections: make(map[string]*Collection)}
		c.dbs[name] = db
	}
	return db
}

// DatabaseForSid mirrors the production naming so scope-aware code under test
// resolves to a distinct database, not silently to the same one.
func (c *Client) DatabaseForSid(prefix string, sid int32) fmongo.IDatabase {
	return c.Database(fmt.Sprintf("%s_%d", prefix, sid))
}

func (c *Client) StartSession(context.Context) (fmongo.ISession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.StartSessionErr != nil {
		return nil, c.StartSessionErr
	}
	c.sessions++
	return &session{client: c}, nil
}

func (c *Client) Ping(context.Context) error  { return nil }
func (c *Client) Close(context.Context) error { return nil }

// Sessions counts StartSession calls (the "one session per transaction"
// assertion several packages make).
func (c *Client) Sessions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions
}

// Attempts counts callback invocations across all transactions.
func (c *Client) Attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

// Collection is a typed shortcut for Database(db).Collection(name).
func (c *Client) Collection(db, name string) *Collection {
	return c.Database(db).Collection(name).(*Collection)
}

type session struct{ client *Client }

// WithTransaction models the two properties production code depends on:
// all-or-nothing application, and the driver's documented automatic retry.
// Each attempt starts from the pre-transaction state, so a callback that
// accumulates across attempts — or a batch that claims atomicity but leaves a
// partial write behind — fails here instead of in production.
func (s *session) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	s.client.mu.Lock()
	retries := s.client.TransientRetries
	s.client.mu.Unlock()
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		snapshot := s.client.snapshot()
		s.client.mu.Lock()
		s.client.attempts++
		s.client.mu.Unlock()
		err = fn(ctx)
		if err != nil {
			// Abort: the callback's writes never became visible.
			s.client.restore(snapshot)
			return err
		}
		if attempt < retries {
			// The commit outcome was unknown, so the driver re-runs the whole
			// callback against the original state.
			s.client.restore(snapshot)
		}
	}
	return err
}

func (s *session) EndSession(context.Context) {}

// collectionSnapshot is one collection's documents at a point in time.
type collectionSnapshot struct {
	coll  *Collection
	docs  map[string]bson.M
	order []string
}

// snapshot captures every collection so an aborted transaction can be undone.
func (c *Client) snapshot() []collectionSnapshot {
	var out []collectionSnapshot
	for _, coll := range c.collections() {
		coll.mu.Lock()
		docs := make(map[string]bson.M, len(coll.docs))
		for key, doc := range coll.docs {
			docs[key] = cloneDoc(doc)
		}
		out = append(out, collectionSnapshot{coll: coll, docs: docs, order: append([]string(nil), coll.order...)})
		coll.mu.Unlock()
	}
	return out
}

func (c *Client) restore(snapshots []collectionSnapshot) {
	restored := make(map[*Collection]bool, len(snapshots))
	for _, snap := range snapshots {
		snap.coll.mu.Lock()
		snap.coll.docs = snap.docs
		snap.coll.order = snap.order
		snap.coll.mu.Unlock()
		restored[snap.coll] = true
	}
	// A collection created inside the aborted transaction never existed.
	for _, coll := range c.collections() {
		if restored[coll] {
			continue
		}
		coll.mu.Lock()
		coll.docs = make(map[string]bson.M)
		coll.order = nil
		coll.mu.Unlock()
	}
}

func (c *Client) collections() []*Collection {
	c.mu.Lock()
	databases := make([]*Database, 0, len(c.dbs))
	for _, db := range c.dbs {
		databases = append(databases, db)
	}
	c.mu.Unlock()
	var out []*Collection
	for _, db := range databases {
		db.mu.Lock()
		for _, coll := range db.collections {
			out = append(out, coll)
		}
		db.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Database implements fmongo.IDatabase.
type Database struct {
	mu          sync.Mutex
	name        string
	collections map[string]*Collection
}

func (d *Database) Name() string { return d.name }

func (d *Database) Collection(name string) fmongo.ICollection {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.collections == nil {
		d.collections = make(map[string]*Collection)
	}
	coll := d.collections[name]
	if coll == nil {
		coll = &Collection{name: name, docs: make(map[string]bson.M), Errors: make(map[string]error), Calls: make(map[string]int)}
		d.collections[name] = coll
	}
	return coll
}

func (d *Database) Drop(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.collections = make(map[string]*Collection)
	return nil
}

// Collection is an in-memory collection with real query semantics.
type Collection struct {
	mu    sync.Mutex
	name  string
	docs  map[string]bson.M
	order []string

	uniqueIndexes map[string][]string
	Indexes       []fmongo.IndexModel

	// Errors injects a failure for a method name ("FindOne", "UpdateOne",
	// "BulkWrite", ...), replacing the old per-fake error fields.
	Errors map[string]error
	// Calls counts invocations per method name.
	Calls map[string]int
	// LastFilter/LastUpdate keep the old assertions expressible.
	LastFilter any
	LastUpdate any
}

func (c *Collection) fail(method string) error {
	c.Calls[method]++
	return c.Errors[method]
}

// Seed inserts a document directly, bypassing duplicate checks and call
// counters — the test fixture entry point.
func (c *Collection) Seed(doc any) error {
	normalized, err := normalizeDoc(doc)
	if err != nil {
		return err
	}
	key, err := docKey(normalized)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.docs[key]; !exists {
		c.order = append(c.order, key)
	}
	c.docs[key] = normalized
	return nil
}

// Documents returns every stored document in insertion order.
func (c *Collection) Documents() []bson.M {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]bson.M, 0, len(c.docs))
	for _, key := range c.order {
		if doc, ok := c.docs[key]; ok {
			out = append(out, cloneDoc(doc))
		}
	}
	return out
}

// Len is the stored document count.
func (c *Collection) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.docs)
}

// Lookup returns one document by its _id.
func (c *Collection) Lookup(id any) (bson.M, bool) {
	key, err := idKey(id)
	if err != nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	doc, ok := c.docs[key]
	if !ok {
		return nil, false
	}
	return cloneDoc(doc), true
}

func (c *Collection) InsertOne(_ context.Context, doc any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail("InsertOne"); err != nil {
		return "", err
	}
	return c.insertLocked(doc)
}

func (c *Collection) insertLocked(doc any) (string, error) {
	normalized, err := normalizeDoc(doc)
	if err != nil {
		return "", err
	}
	key, err := docKey(normalized)
	if err != nil {
		return "", err
	}
	if _, exists := c.docs[key]; exists {
		return "", fmongo.ErrDuplicateKey
	}
	if err := c.checkUniqueLocked(normalized, key); err != nil {
		return "", err
	}
	c.docs[key] = normalized
	c.order = append(c.order, key)
	return key, nil
}

func (c *Collection) InsertMany(ctx context.Context, docs []any) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail("InsertMany"); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(docs))
	for _, doc := range docs {
		id, err := c.insertLocked(doc)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (c *Collection) FindOne(_ context.Context, filter any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter = filter
	if err := c.fail("FindOne"); err != nil {
		return err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		return fmongo.ErrNotFound
	}
	return decodeInto(c.docs[matched[0]], result)
}

func (c *Collection) Find(_ context.Context, filter any, results any, opts ...fmongo.FindOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter = filter
	if err := c.fail("Find"); err != nil {
		return err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return err
	}
	if len(opts) > 0 {
		if matched, err = c.sortLocked(matched, opts[0].Sort); err != nil {
			return err
		}
		if opts[0].Skip > 0 && int(opts[0].Skip) < len(matched) {
			matched = matched[opts[0].Skip:]
		}
		if opts[0].Limit > 0 && int(opts[0].Limit) < len(matched) {
			matched = matched[:opts[0].Limit]
		}
	}
	docs := make([]bson.M, 0, len(matched))
	for _, key := range matched {
		docs = append(docs, c.docs[key])
	}
	return decodeInto(docs, results)
}

// StreamFind implements fmongo.IStreamingCollection so loaders exercise their
// production cursor path rather than silently falling back to Find.
func (c *Collection) StreamFind(_ context.Context, filter any, consume func([]byte) error, opts ...fmongo.FindOption) error {
	c.mu.Lock()
	c.LastFilter = filter
	if err := c.fail("StreamFind"); err != nil {
		c.mu.Unlock()
		return err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if len(opts) > 0 {
		if matched, err = c.sortLocked(matched, opts[0].Sort); err != nil {
			c.mu.Unlock()
			return err
		}
		if opts[0].Limit > 0 && int(opts[0].Limit) < len(matched) {
			matched = matched[:opts[0].Limit]
		}
	}
	raws := make([][]byte, 0, len(matched))
	for _, key := range matched {
		raw, err := bson.Marshal(c.docs[key])
		if err != nil {
			c.mu.Unlock()
			return err
		}
		raws = append(raws, raw)
	}
	c.mu.Unlock()
	for _, raw := range raws {
		if err := consume(raw); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collection) UpdateOne(_ context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter, c.LastUpdate = filter, update
	if err := c.fail("UpdateOne"); err != nil {
		return nil, err
	}
	return c.updateLocked(filter, update, false, false)
}

func (c *Collection) UpdateMany(_ context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter, c.LastUpdate = filter, update
	if err := c.fail("UpdateMany"); err != nil {
		return nil, err
	}
	return c.updateLocked(filter, update, false, true)
}

func (c *Collection) ReplaceOne(_ context.Context, filter any, replacement any) (*fmongo.UpdateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter, c.LastUpdate = filter, replacement
	if err := c.fail("ReplaceOne"); err != nil {
		return nil, err
	}
	return c.replaceLocked(filter, replacement, false)
}

func (c *Collection) DeleteOne(_ context.Context, filter any) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter = filter
	if err := c.fail("DeleteOne"); err != nil {
		return 0, err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return 0, err
	}
	if len(matched) == 0 {
		return 0, nil
	}
	c.removeLocked(matched[0])
	return 1, nil
}

func (c *Collection) DeleteMany(_ context.Context, filter any) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter = filter
	if err := c.fail("DeleteMany"); err != nil {
		return 0, err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return 0, err
	}
	for _, key := range matched {
		c.removeLocked(key)
	}
	return int64(len(matched)), nil
}

func (c *Collection) FindOneAndUpdate(_ context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter, c.LastUpdate = filter, update
	if err := c.fail("FindOneAndUpdate"); err != nil {
		return err
	}
	option := fmongo.FindOneAndUpdateOption{}
	if len(opts) > 0 {
		option = opts[0]
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return err
	}
	if len(matched) == 0 && !option.Upsert {
		return fmongo.ErrNotFound
	}
	var before bson.M
	if len(matched) > 0 {
		before = cloneDoc(c.docs[matched[0]])
	}
	if _, err := c.updateLocked(filter, update, option.Upsert, false); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if !option.ReturnAfter {
		if before == nil {
			return fmongo.ErrNotFound
		}
		return decodeInto(before, result)
	}
	after, err := c.matchLocked(filter)
	if err != nil {
		return err
	}
	if len(after) == 0 {
		// The update moved the document out of its own filter (a version CAS
		// bump does exactly this); return it by identity instead.
		if before != nil {
			if key, keyErr := docKey(before); keyErr == nil {
				if doc, ok := c.docs[key]; ok {
					return decodeInto(doc, result)
				}
			}
		}
		return fmongo.ErrNotFound
	}
	return decodeInto(c.docs[after[0]], result)
}

func (c *Collection) FindOneAndDelete(_ context.Context, filter any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter = filter
	if err := c.fail("FindOneAndDelete"); err != nil {
		return err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		return fmongo.ErrNotFound
	}
	doc := cloneDoc(c.docs[matched[0]])
	c.removeLocked(matched[0])
	if result == nil {
		return nil
	}
	return decodeInto(doc, result)
}

func (c *Collection) FindOneAndReplace(_ context.Context, filter any, replacement any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter, c.LastUpdate = filter, replacement
	if err := c.fail("FindOneAndReplace"); err != nil {
		return err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		return fmongo.ErrNotFound
	}
	before := cloneDoc(c.docs[matched[0]])
	if _, err := c.replaceLocked(filter, replacement, false); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return decodeInto(before, result)
}

func (c *Collection) CountDocuments(_ context.Context, filter any) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastFilter = filter
	if err := c.fail("CountDocuments"); err != nil {
		return 0, err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		return 0, err
	}
	return int64(len(matched)), nil
}

// Aggregate is intentionally unsupported: no kit production path uses it, and
// returning an empty result would be indistinguishable from a passing query.
func (c *Collection) Aggregate(_ context.Context, _ any, _ any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail("Aggregate"); err != nil {
		return err
	}
	return fmt.Errorf("%w: Aggregate", ErrUnsupported)
}

func (c *Collection) BulkWrite(_ context.Context, models []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail("BulkWrite"); err != nil {
		return nil, err
	}
	result := &fmongo.BulkWriteResult{}
	for i := range models {
		model := models[i]
		switch model.Type {
		case fmongo.WriteModelInsertOne:
			if _, err := c.insertLocked(model.Document); err != nil {
				return result, err
			}
			result.InsertedCount++
		case fmongo.WriteModelUpdateOne:
			c.LastFilter, c.LastUpdate = model.Filter, model.Update
			partial, err := c.updateLocked(model.Filter, model.Update, model.Upsert, false)
			if err != nil {
				return result, err
			}
			result.MatchedCount += partial.MatchedCount
			result.ModifiedCount += partial.ModifiedCount
			result.UpsertedCount += partial.UpsertedCount
		case fmongo.WriteModelReplaceOne:
			c.LastFilter, c.LastUpdate = model.Filter, model.Document
			partial, err := c.replaceLocked(model.Filter, model.Document, model.Upsert)
			if err != nil {
				return result, err
			}
			result.MatchedCount += partial.MatchedCount
			result.ModifiedCount += partial.ModifiedCount
			result.UpsertedCount += partial.UpsertedCount
		case fmongo.WriteModelDeleteOne:
			c.LastFilter = model.Filter
			matched, err := c.matchLocked(model.Filter)
			if err != nil {
				return result, err
			}
			if len(matched) > 0 {
				c.removeLocked(matched[0])
				result.DeletedCount++
			}
		default:
			return result, fmt.Errorf("%w: bulk model type %d", ErrUnsupported, model.Type)
		}
	}
	return result, nil
}

func (c *Collection) EnsureIndexes(_ context.Context, indexes []fmongo.IndexModel) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail("EnsureIndexes"); err != nil {
		return err
	}
	c.Indexes = append(c.Indexes, indexes...)
	for _, index := range indexes {
		if !index.Unique {
			continue
		}
		fields, err := indexFields(index.Keys)
		if err != nil {
			return err
		}
		if c.uniqueIndexes == nil {
			c.uniqueIndexes = make(map[string][]string)
		}
		name := index.Name
		if name == "" {
			name = strings.Join(fields, "_")
		}
		c.uniqueIndexes[name] = fields
	}
	return nil
}

// HasIndex reports whether an index covering exactly these key fields (in
// order) was ensured — lets tests assert the index a hot query depends on.
func (c *Collection) HasIndex(fields ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, index := range c.Indexes {
		got, err := indexFields(index.Keys)
		if err != nil || len(got) != len(fields) {
			continue
		}
		same := true
		for i := range fields {
			if got[i] != fields[i] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

func (c *Collection) checkUniqueLocked(doc bson.M, selfKey string) error {
	for _, fields := range c.uniqueIndexes {
		values := make([]any, 0, len(fields))
		for _, field := range fields {
			value, ok := lookupPath(doc, field)
			if !ok {
				values = nil
				break
			}
			values = append(values, value)
		}
		if values == nil {
			continue
		}
		for key, existing := range c.docs {
			if key == selfKey {
				continue
			}
			same := true
			for i, field := range fields {
				other, ok := lookupPath(existing, field)
				if !ok {
					same = false
					break
				}
				if eq, err := valuesEqual(other, values[i]); err != nil || !eq {
					same = false
					break
				}
			}
			if same {
				return fmongo.ErrDuplicateKey
			}
		}
	}
	return nil
}

func (c *Collection) removeLocked(key string) {
	delete(c.docs, key)
	for i, existing := range c.order {
		if existing == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *Collection) matchLocked(filter any) ([]string, error) {
	normalized, err := normalizeFilter(filter)
	if err != nil {
		return nil, err
	}
	// Real Mongo answers an _id equality from the always-present _id index
	// rather than scanning. Modelling that keeps the fake O(1) on the hot
	// point-lookup shape, so benchmarks that project through it measure the
	// production code instead of the fake's scan.
	if candidates, ok := c.idCandidatesLocked(normalized); ok {
		var matched []string
		for _, key := range candidates {
			doc, exists := c.docs[key]
			if !exists {
				continue
			}
			hit, err := matchDoc(doc, normalized)
			if err != nil {
				return nil, err
			}
			if hit {
				matched = append(matched, key)
			}
		}
		return matched, nil
	}
	var matched []string
	for _, key := range c.order {
		doc, ok := c.docs[key]
		if !ok {
			continue
		}
		hit, err := matchDoc(doc, normalized)
		if err != nil {
			return nil, err
		}
		if hit {
			matched = append(matched, key)
		}
	}
	return matched, nil
}

// idCandidatesLocked returns the _id keys a filter can possibly match when it
// pins _id by equality or $in, plus whether that shortcut applies at all.
func (c *Collection) idCandidatesLocked(filter bson.M) ([]string, bool) {
	spec, present := filter["_id"]
	if !present {
		return nil, false
	}
	if operators, isOperator := operatorSpec(spec); isOperator {
		operand, hasIn := operators["$in"]
		if !hasIn || len(operators) != 1 {
			return nil, false
		}
		values, err := valueList(operand)
		if err != nil {
			return nil, false
		}
		keys := make([]string, 0, len(values))
		for _, value := range values {
			key, err := idKey(value)
			if err != nil {
				return nil, false
			}
			keys = append(keys, key)
		}
		return keys, true
	}
	key, err := idKey(spec)
	if err != nil {
		return nil, false
	}
	return []string{key}, true
}

func (c *Collection) sortLocked(keys []string, spec any) ([]string, error) {
	if spec == nil {
		return keys, nil
	}
	fields, err := sortFields(spec)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), keys...)
	var sortErr error
	sort.SliceStable(out, func(i, j int) bool {
		for _, field := range fields {
			left, _ := lookupPath(c.docs[out[i]], field.name)
			right, _ := lookupPath(c.docs[out[j]], field.name)
			cmp, err := compareValues(left, right)
			if err != nil {
				sortErr = err
				return false
			}
			if cmp == 0 {
				continue
			}
			if field.descending {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
	return out, sortErr
}

func (c *Collection) updateLocked(filter any, update any, upsert bool, many bool) (*fmongo.UpdateResult, error) {
	matched, err := c.matchLocked(filter)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		if !upsert {
			return &fmongo.UpdateResult{}, nil
		}
		seed, err := seedFromFilter(filter)
		if err != nil {
			return nil, err
		}
		updated, err := applyUpdate(seed, update)
		if err != nil {
			return nil, err
		}
		key, err := docKey(updated)
		if err != nil {
			return nil, err
		}
		if _, exists := c.docs[key]; exists {
			return nil, fmongo.ErrDuplicateKey
		}
		if err := c.checkUniqueLocked(updated, key); err != nil {
			return nil, err
		}
		c.docs[key] = updated
		c.order = append(c.order, key)
		return &fmongo.UpdateResult{UpsertedCount: 1, UpsertedID: key}, nil
	}
	if !many {
		matched = matched[:1]
	}
	result := &fmongo.UpdateResult{}
	for _, key := range matched {
		updated, err := applyUpdate(cloneDoc(c.docs[key]), update)
		if err != nil {
			return nil, err
		}
		newKey, err := docKey(updated)
		if err != nil {
			return nil, err
		}
		if newKey != key {
			return nil, fmt.Errorf("%w: update changed _id", ErrUnsupported)
		}
		if err := c.checkUniqueLocked(updated, key); err != nil {
			return nil, err
		}
		c.docs[key] = updated
		result.MatchedCount++
		result.ModifiedCount++
	}
	return result, nil
}

func (c *Collection) replaceLocked(filter any, replacement any, upsert bool) (*fmongo.UpdateResult, error) {
	matched, err := c.matchLocked(filter)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeDoc(replacement)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		if !upsert {
			return &fmongo.UpdateResult{}, nil
		}
		if _, ok := normalized["_id"]; !ok {
			seed, err := seedFromFilter(filter)
			if err != nil {
				return nil, err
			}
			if id, ok := seed["_id"]; ok {
				normalized["_id"] = id
			}
		}
		key, err := docKey(normalized)
		if err != nil {
			return nil, err
		}
		if _, exists := c.docs[key]; exists {
			return nil, fmongo.ErrDuplicateKey
		}
		if err := c.checkUniqueLocked(normalized, key); err != nil {
			return nil, err
		}
		c.docs[key] = normalized
		c.order = append(c.order, key)
		return &fmongo.UpdateResult{UpsertedCount: 1, UpsertedID: key}, nil
	}
	key := matched[0]
	if _, ok := normalized["_id"]; !ok {
		normalized["_id"] = c.docs[key]["_id"]
	}
	newKey, err := docKey(normalized)
	if err != nil {
		return nil, err
	}
	if newKey != key {
		return nil, fmt.Errorf("%w: replacement changed _id", ErrUnsupported)
	}
	if err := c.checkUniqueLocked(normalized, key); err != nil {
		return nil, err
	}
	c.docs[key] = normalized
	return &fmongo.UpdateResult{MatchedCount: 1, ModifiedCount: 1}, nil
}

var (
	_ fmongo.IMongo               = (*Client)(nil)
	_ fmongo.IDatabase            = (*Database)(nil)
	_ fmongo.ICollection          = (*Collection)(nil)
	_ fmongo.IStreamingCollection = (*Collection)(nil)
	_ fmongo.ISession             = (*session)(nil)
)

// --- document / filter / update evaluation ---

func normalizeDoc(doc any) (bson.M, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: nil document", ErrUnsupported)
	}
	raw, err := bson.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var out bson.M
	if err := bson.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func cloneDoc(doc bson.M) bson.M {
	out := make(bson.M, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	return out
}

func decodeInto(value any, result any) error {
	if docs, ok := value.([]bson.M); ok {
		return decodeSlice(docs, result)
	}
	raw, err := bson.Marshal(value)
	if err != nil {
		return err
	}
	return bson.Unmarshal(raw, result)
}

func decodeSlice(docs []bson.M, result any) error {
	switch out := result.(type) {
	case *[]bson.Raw:
		list := make([]bson.Raw, 0, len(docs))
		for _, doc := range docs {
			raw, err := bson.Marshal(doc)
			if err != nil {
				return err
			}
			list = append(list, bson.Raw(raw))
		}
		*out = list
		return nil
	case *[]bson.M:
		list := make([]bson.M, 0, len(docs))
		for _, doc := range docs {
			list = append(list, cloneDoc(doc))
		}
		*out = list
		return nil
	}
	// Typed slice: wrap the array in a document, then let the driver decode
	// the array value straight into the caller's slice.
	raw, err := bson.Marshal(bson.M{"v": docs})
	if err != nil {
		return err
	}
	target := struct {
		V bson.RawValue `bson:"v"`
	}{}
	if err := bson.Unmarshal(raw, &target); err != nil {
		return err
	}
	return target.V.Unmarshal(result)
}

func docKey(doc bson.M) (string, error) {
	id, ok := doc["_id"]
	if !ok {
		return "", fmt.Errorf("%w: document without _id", ErrUnsupported)
	}
	return idKey(id)
}

func idKey(id any) (string, error) {
	if text, ok := id.(string); ok {
		return "s:" + text, nil
	}
	if number, ok := asInt64(id); ok && id != nil {
		return fmt.Sprintf("n:%d", number), nil
	}
	return "", fmt.Errorf("%w: _id type %T", ErrUnsupported, id)
}

func normalizeFilter(filter any) (bson.M, error) {
	switch value := filter.(type) {
	case nil:
		return bson.M{}, nil
	case bson.M:
		return value, nil
	case map[string]any:
		return bson.M(value), nil
	case bson.D:
		out := make(bson.M, len(value))
		for _, element := range value {
			out[element.Key] = element.Value
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: filter type %T", ErrUnsupported, filter)
	}
}

// seedFromFilter builds the document an upsert inserts when nothing matched:
// Mongo seeds it from the filter's equality fields only, so a CAS predicate
// like {_version: 4} deliberately does NOT leak into the inserted document.
func seedFromFilter(filter any) (bson.M, error) {
	normalized, err := normalizeFilter(filter)
	if err != nil {
		return nil, err
	}
	seed := bson.M{}
	for key, spec := range normalized {
		if strings.HasPrefix(key, "$") {
			continue
		}
		if _, isOperator := operatorSpec(spec); isOperator {
			continue
		}
		seed[key] = normalizeScalar(spec)
	}
	return seed, nil
}

func matchDoc(doc bson.M, filter bson.M) (bool, error) {
	for key, spec := range filter {
		switch key {
		case "$and":
			branches, err := filterList(spec)
			if err != nil {
				return false, err
			}
			for _, branch := range branches {
				hit, err := matchDoc(doc, branch)
				if err != nil || !hit {
					return false, err
				}
			}
		case "$or":
			branches, err := filterList(spec)
			if err != nil {
				return false, err
			}
			any := false
			for _, branch := range branches {
				hit, err := matchDoc(doc, branch)
				if err != nil {
					return false, err
				}
				if hit {
					any = true
					break
				}
			}
			if !any {
				return false, nil
			}
		default:
			hit, err := matchField(doc, key, spec)
			if err != nil || !hit {
				return false, err
			}
		}
	}
	return true, nil
}

func filterList(spec any) ([]bson.M, error) {
	switch value := spec.(type) {
	case bson.A:
		out := make([]bson.M, 0, len(value))
		for _, entry := range value {
			normalized, err := normalizeFilter(entry)
			if err != nil {
				return nil, err
			}
			out = append(out, normalized)
		}
		return out, nil
	case []any:
		return filterList(bson.A(value))
	case []bson.M:
		return value, nil
	default:
		return nil, fmt.Errorf("%w: logical operator operand %T", ErrUnsupported, spec)
	}
}

func matchField(doc bson.M, field string, spec any) (bool, error) {
	value, present := lookupPath(doc, field)
	operators, ok := operatorSpec(spec)
	if !ok {
		if !present {
			return false, nil
		}
		return valuesEqual(value, spec)
	}
	for operator, operand := range operators {
		switch operator {
		case "$exists":
			want, ok := operand.(bool)
			if !ok {
				return false, fmt.Errorf("%w: $exists operand %T", ErrUnsupported, operand)
			}
			if present != want {
				return false, nil
			}
		case "$eq":
			if !present {
				return false, nil
			}
			eq, err := valuesEqual(value, operand)
			if err != nil || !eq {
				return false, err
			}
		case "$ne":
			if present {
				eq, err := valuesEqual(value, operand)
				if err != nil {
					return false, err
				}
				if eq {
					return false, nil
				}
			}
		case "$gt", "$gte", "$lt", "$lte":
			if !present {
				return false, nil
			}
			cmp, err := compareValues(value, operand)
			if err != nil {
				return false, err
			}
			switch operator {
			case "$gt":
				if cmp <= 0 {
					return false, nil
				}
			case "$gte":
				if cmp < 0 {
					return false, nil
				}
			case "$lt":
				if cmp >= 0 {
					return false, nil
				}
			case "$lte":
				if cmp > 0 {
					return false, nil
				}
			}
		case "$in":
			if !present {
				return false, nil
			}
			candidates, err := valueList(operand)
			if err != nil {
				return false, err
			}
			found := false
			for _, candidate := range candidates {
				eq, err := valuesEqual(value, candidate)
				if err != nil {
					return false, err
				}
				if eq {
					found = true
					break
				}
			}
			if !found {
				return false, nil
			}
		default:
			return false, fmt.Errorf("%w: query operator %s", ErrUnsupported, operator)
		}
	}
	return true, nil
}

func operatorSpec(spec any) (bson.M, bool) {
	normalized, err := normalizeFilter(spec)
	if err != nil {
		return nil, false
	}
	if len(normalized) == 0 {
		return nil, false
	}
	for key := range normalized {
		if !strings.HasPrefix(key, "$") {
			return nil, false
		}
	}
	return normalized, true
}

func valueList(operand any) ([]any, error) {
	switch value := operand.(type) {
	case bson.A:
		return []any(value), nil
	case []any:
		return value, nil
	case []string:
		out := make([]any, 0, len(value))
		for _, entry := range value {
			out = append(out, entry)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: $in operand %T", ErrUnsupported, operand)
	}
}

func lookupPath(doc bson.M, path string) (any, bool) {
	if doc == nil {
		return nil, false
	}
	parts := strings.Split(path, ".")
	var current any = doc
	for _, part := range parts {
		container, ok := current.(bson.M)
		if !ok {
			if plain, isMap := current.(map[string]any); isMap {
				container = bson.M(plain)
			} else {
				return nil, false
			}
		}
		value, exists := container[part]
		if !exists {
			return nil, false
		}
		current = value
	}
	return current, true
}

func setPath(doc bson.M, path string, value any) {
	parts := strings.Split(path, ".")
	current := doc
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(bson.M)
		if !ok {
			next = bson.M{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}

func unsetPath(doc bson.M, path string) {
	parts := strings.Split(path, ".")
	current := doc
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(bson.M)
		if !ok {
			return
		}
		current = next
	}
	delete(current, parts[len(parts)-1])
}

func applyUpdate(doc bson.M, update any) (bson.M, error) {
	if pipeline, ok := update.(bson.A); ok {
		return applyPipeline(doc, pipeline)
	}
	normalized, err := normalizeFilter(update)
	if err != nil {
		return nil, err
	}
	operators := false
	for key := range normalized {
		if strings.HasPrefix(key, "$") {
			operators = true
			break
		}
	}
	if !operators {
		// Whole-document replacement keeps the existing _id when omitted.
		replacement, err := normalizeDoc(update)
		if err != nil {
			return nil, err
		}
		if _, ok := replacement["_id"]; !ok {
			replacement["_id"] = doc["_id"]
		}
		return replacement, nil
	}
	for operator, operand := range normalized {
		fields, err := normalizeFilter(operand)
		if err != nil {
			return nil, err
		}
		switch operator {
		case "$set":
			for field, value := range fields {
				setPath(doc, field, normalizeScalar(value))
			}
		case "$unset":
			for field := range fields {
				unsetPath(doc, field)
			}
		case "$inc":
			for field, delta := range fields {
				current, _ := lookupPath(doc, field)
				sum, err := addNumbers(current, delta)
				if err != nil {
					return nil, err
				}
				setPath(doc, field, sum)
			}
		default:
			return nil, fmt.Errorf("%w: update operator %s", ErrUnsupported, operator)
		}
	}
	return doc, nil
}

func applyPipeline(doc bson.M, pipeline bson.A) (bson.M, error) {
	current := doc
	for _, stage := range pipeline {
		normalized, err := normalizeFilter(stage)
		if err != nil {
			return nil, err
		}
		replacement, ok := normalized["$replaceWith"]
		if !ok || len(normalized) != 1 {
			return nil, fmt.Errorf("%w: aggregation stage %v", ErrUnsupported, normalized)
		}
		next, err := normalizeDoc(replacement)
		if err != nil {
			return nil, err
		}
		if _, ok := next["_id"]; !ok {
			next["_id"] = current["_id"]
		}
		current = next
	}
	return current, nil
}

// normalizeScalar stores a value the way the BSON codec would, by actually
// round-tripping it. Hand-written widening rules would drift from the driver
// (which encoding each Go integer width maps to is the driver's business), and
// a fake that stores a different type than the server is a trap, not a test.
func normalizeScalar(value any) any {
	raw, err := bson.Marshal(bson.M{"v": value})
	if err != nil {
		return value
	}
	var holder bson.M
	if err := bson.Unmarshal(raw, &holder); err != nil {
		return value
	}
	return holder["v"]
}

func addNumbers(current any, delta any) (any, error) {
	left, leftOK := asInt64(current)
	right, rightOK := asInt64(delta)
	if (current == nil || leftOK) && rightOK {
		return left + right, nil
	}
	return nil, fmt.Errorf("%w: $inc on %T by %T", ErrUnsupported, current, delta)
}

func asInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, true
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	default:
		return 0, false
	}
}

func valuesEqual(left any, right any) (bool, error) {
	if left == nil || right == nil {
		return left == nil && right == nil, nil
	}
	if leftBytes, ok := asBytes(left); ok {
		rightBytes, ok := asBytes(right)
		if !ok {
			return false, nil
		}
		return string(leftBytes) == string(rightBytes), nil
	}
	if leftNum, ok := asFloat(left); ok {
		rightNum, ok := asFloat(right)
		if !ok {
			return false, nil
		}
		return leftNum == rightNum, nil
	}
	switch typed := left.(type) {
	case string:
		other, ok := right.(string)
		return ok && typed == other, nil
	case bool:
		other, ok := right.(bool)
		return ok && typed == other, nil
	case time.Time:
		other, ok := asTime(right)
		return ok && typed.UnixMilli() == other.UnixMilli(), nil
	}
	if leftTime, ok := asTime(left); ok {
		rightTime, ok := asTime(right)
		return ok && leftTime.UnixMilli() == rightTime.UnixMilli(), nil
	}
	return fmt.Sprint(left) == fmt.Sprint(right), nil
}

func compareValues(left any, right any) (int, error) {
	if left == nil && right == nil {
		return 0, nil
	}
	if left == nil {
		return -1, nil
	}
	if right == nil {
		return 1, nil
	}
	if leftNum, ok := asFloat(left); ok {
		rightNum, ok := asFloat(right)
		if !ok {
			return 0, fmt.Errorf("%w: compare %T with %T", ErrUnsupported, left, right)
		}
		switch {
		case leftNum < rightNum:
			return -1, nil
		case leftNum > rightNum:
			return 1, nil
		default:
			return 0, nil
		}
	}
	if leftTime, ok := asTime(left); ok {
		rightTime, ok := asTime(right)
		if !ok {
			return 0, fmt.Errorf("%w: compare time with %T", ErrUnsupported, right)
		}
		return leftTime.Compare(rightTime), nil
	}
	leftText, leftOK := left.(string)
	rightText, rightOK := right.(string)
	if leftOK && rightOK {
		return strings.Compare(leftText, rightText), nil
	}
	return 0, fmt.Errorf("%w: compare %T with %T", ErrUnsupported, left, right)
}

func asFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	default:
		return 0, false
	}
}

func asTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed, true
	case bson.DateTime:
		return typed.Time(), true
	default:
		return time.Time{}, false
	}
}

func asBytes(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case []byte:
		return typed, true
	case bson.Binary:
		return typed.Data, true
	default:
		return nil, false
	}
}

type sortField struct {
	name       string
	descending bool
}

func sortFields(spec any) ([]sortField, error) {
	switch value := spec.(type) {
	case bson.D:
		out := make([]sortField, 0, len(value))
		for _, element := range value {
			direction, ok := asInt64(element.Value)
			if !ok {
				return nil, fmt.Errorf("%w: sort direction %T", ErrUnsupported, element.Value)
			}
			out = append(out, sortField{name: element.Key, descending: direction < 0})
		}
		return out, nil
	case bson.M:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]sortField, 0, len(keys))
		for _, key := range keys {
			direction, _ := asInt64(value[key])
			out = append(out, sortField{name: key, descending: direction < 0})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: sort spec %T", ErrUnsupported, spec)
	}
}

func indexFields(keys any) ([]string, error) {
	switch value := keys.(type) {
	case bson.D:
		out := make([]string, 0, len(value))
		for _, element := range value {
			out = append(out, element.Key)
		}
		return out, nil
	case bson.M:
		out := make([]string, 0, len(value))
		for key := range value {
			out = append(out, key)
		}
		sort.Strings(out)
		return out, nil
	default:
		return nil, fmt.Errorf("%w: index keys %T", ErrUnsupported, keys)
	}
}
