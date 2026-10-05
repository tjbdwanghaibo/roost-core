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
// WithTransaction keeps private collection snapshots; abort discards only its own writes.
package mongotest

import (
	"context"
	"errors"
	"fmt"
	"math/big"
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
	// callback retry of ISession.WithTransaction (TransientTransactionError).
	// This test knob does not model UnknownTransactionCommitResult. Callback
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
		db = &Database{name: name, client: c, collections: make(map[string]*Collection)}
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

// TransientTransactionError is the error label the real server attaches to a
// transaction that can be retried from the start (NoSuchTransaction after an
// abort, WriteConflict). The driver's WithTransaction re-runs the whole
// callback while the callback's error carries it; errors returned by this fake
// expose it through HasErrorLabel, the same method the driver's
// mongo.LabeledError interface uses.
const TransientTransactionError = "TransientTransactionError"

// ErrNoSuchTransaction is what every operation in a transaction returns once
// an earlier write in the same transaction failed. The real server aborts the
// transaction on any write error (a duplicate key included) and answers later
// statements with NoSuchTransaction (code 251) labelled
// TransientTransactionError; code that "reads back after a duplicate key"
// inside the transaction therefore never sees the document, and the driver
// retries the callback until transaction_timeout (RR-20260926-34).
var ErrNoSuchTransaction = errors.New("mongofake: NoSuchTransaction: transaction has been aborted")

// transientAttemptLimit bounds how often WithTransaction re-runs a callback
// whose error is labelled TransientTransactionError. The real driver keeps
// retrying until its 120s budget (or the caller's deadline) runs out; a test
// cannot wait that long, so the fake stops after this many attempts and
// returns the last error, which still carries the label.
const transientAttemptLimit = 16

// labeledError carries server error labels the way the driver's errors do.
type labeledError struct {
	err    error
	labels []string
}

func (e *labeledError) Error() string { return e.err.Error() }
func (e *labeledError) Unwrap() error { return e.err }
func (e *labeledError) HasErrorLabel(label string) bool {
	for _, own := range e.labels {
		if own == label {
			return true
		}
	}
	return false
}

// duplicateKeyError mirrors what mongo/driver returns for E11000:
// fmongo.ErrDuplicateKey in front of a server error that carries labels (none
// of them TransientTransactionError). The driver's retry check stops at the
// first labelled error in a chain, so a duplicate key joined in front of a
// transient error is not retried there — and must not be here either.
func duplicateKeyError() error {
	return &labeledError{err: fmongo.ErrDuplicateKey}
}

func hasErrorLabel(err error, label string) bool {
	var labeled interface{ HasErrorLabel(string) bool }
	return errors.As(err, &labeled) && labeled.HasErrorLabel(label)
}

type transactionKey struct{}

// transaction is the server-side state of one WithTransaction attempt. It is
// carried in the callback's context, so only operations issued with that
// context (or a context derived from it) belong to the transaction.
type transaction struct {
	mu       sync.Mutex
	cause    error
	client   *Client
	views    map[*Collection]*collectionSnapshot
	finished bool
}

func transactionFrom(ctx context.Context) *transaction {
	if ctx == nil {
		return nil
	}
	tx, _ := ctx.Value(transactionKey{}).(*transaction)
	return tx
}

// refuse returns the NoSuchTransaction error once the transaction aborted.
func (tx *transaction) refuse() error {
	if tx == nil {
		return nil
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.finished {
		return ErrNoSuchTransaction
	}
	if tx.cause == nil {
		return nil
	}
	return &labeledError{
		err:    fmt.Errorf("%w (aborted by: %v)", ErrNoSuchTransaction, tx.cause),
		labels: []string{TransientTransactionError},
	}
}

// writeFailed records a write error. Like the server, any write failure —
// except "no document matched", which is not a server error — aborts the
// transaction. The error itself is returned unchanged.
func (tx *transaction) writeFailed(err error) error {
	if tx == nil || err == nil || errors.Is(err, fmongo.ErrNotFound) {
		return err
	}
	tx.mu.Lock()
	if tx.cause == nil {
		tx.cause = err
	}
	tx.mu.Unlock()
	return err
}

// WithTransaction models the properties production code depends on:
// all-or-nothing application, the server aborting a transaction on its first
// write error, and the driver's documented automatic retry. Each attempt
// starts from the pre-transaction state, so a callback that accumulates across
// attempts — or a batch that claims atomicity but leaves a partial write
// behind — fails here instead of in production.
//
// Retry follows the driver: a callback error labelled
// TransientTransactionError re-runs the callback (bounded by
// transientAttemptLimit and ctx), and a callback that returns nil after its
// transaction was aborted fails to commit with NoSuchTransaction, which is
// transient too. Any other error aborts and is returned as is.
func (s *session) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.client.mu.Lock()
	retries := s.client.TransientRetries
	s.client.mu.Unlock()
	forced, transient := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx := &transaction{client: s.client, views: make(map[*Collection]*collectionSnapshot)}
		for _, snap := range s.client.snapshot() {
			tx.views[snap.coll] = snap
		}
		s.client.mu.Lock()
		s.client.attempts++
		s.client.mu.Unlock()
		err := func() (err error) {
			completed := false
			defer func() {
				if !completed {
					tx.finish()
				}
			}()
			err = fn(context.WithValue(ctx, transactionKey{}, tx))
			completed = true
			return err
		}()
		if err == nil {
			// Committing an aborted transaction is refused by the server.
			err = tx.refuse()
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && forced < retries {
			// 测试强制重跑尚未发布的 attempt，不恢复共享库，也不模拟未知提交。
			forced++
			tx.finish()
			continue
		}
		if err == nil {
			err = tx.commit()
		}
		tx.finish()
		if err != nil {
			// RR-20261004-NC-29：abort 只丢弃私有状态，不撤回他人已提交写。
			if !hasErrorLabel(err, TransientTransactionError) {
				return err
			}
			transient++
			if transient >= transientAttemptLimit {
				return fmt.Errorf("mongofake: transaction still transient after %d attempts: %w", transient, err)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return errors.Join(ctxErr, err)
			}
			continue
		}
		return nil
	}
}

func (s *session) EndSession(context.Context) {}

// collectionSnapshot is one collection's documents at a point in time.
type collectionSnapshot struct {
	coll         *Collection
	docs         map[string]bson.M
	order        []string
	baseRevision uint64
	revision     uint64
}

// snapshot captures committed data under the same lock order used by commit.
func (c *Client) snapshot() []*collectionSnapshot {
	collections := c.collections()
	for _, coll := range collections {
		coll.mu.Lock()
	}
	defer func() {
		for i := len(collections) - 1; i >= 0; i-- {
			collections[i].mu.Unlock()
		}
	}()
	out := make([]*collectionSnapshot, 0, len(collections))
	for _, coll := range collections {
		docs := make(map[string]bson.M, len(coll.docs))
		for key, doc := range coll.docs {
			docs[key] = cloneDoc(doc)
		}
		out = append(out, &collectionSnapshot{coll: coll, docs: docs, order: append([]string(nil), coll.order...), baseRevision: coll.revision, revision: coll.revision})
	}
	return out
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
	sort.Slice(out, func(i, j int) bool { return collectionLess(out[i], out[j]) })
	return out
}

// Database implements fmongo.IDatabase.
type Database struct {
	mu          sync.Mutex
	name        string
	client      *Client
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
		coll = &Collection{name: name, database: d.name, client: d.client, docs: make(map[string]bson.M), Errors: make(map[string]error), Calls: make(map[string]int)}
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
	mu       sync.Mutex
	name     string
	database string
	client   *Client
	revision uint64
	docs     map[string]bson.M
	order    []string

	uniqueIndexes map[string]uniqueIndex
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

// enter counts the call and refuses it the way the server would: an injected
// failure, or a transaction that an earlier write error already aborted.
func (c *Collection) enter(tx *transaction, method string) error {
	if err := c.fail(method); err != nil {
		return err
	}
	if tx != nil && tx.client != c.client {
		return fmt.Errorf("%w: transaction belongs to another client", ErrUnsupported)
	}
	return tx.refuse()
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
	c.revision++
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

func (c *Collection) InsertOne(ctx context.Context, doc any) (id string, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	if err := c.enter(tx, "InsertOne"); err != nil {
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
		return "", duplicateKeyError()
	}
	if err := c.checkUniqueLocked(normalized, key); err != nil {
		return "", err
	}
	c.docs[key] = normalized
	c.revision++
	c.order = append(c.order, key)
	return key, nil
}

func (c *Collection) InsertMany(ctx context.Context, docs []any) (inserted []string, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	if err := c.enter(tx, "InsertMany"); err != nil {
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

func (c *Collection) FindOne(ctx context.Context, filter any, result any) error {
	tx := transactionFrom(ctx)
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter = filter
	if err := c.enter(tx, "FindOne"); err != nil {
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

func (c *Collection) Find(ctx context.Context, filter any, results any, opts ...fmongo.FindOption) error {
	tx := transactionFrom(ctx)
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter = filter
	if err := c.enter(tx, "Find"); err != nil {
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
		matched = paginateMatches(matched, opts[0])
	}
	docs := make([]bson.M, 0, len(matched))
	for _, key := range matched {
		docs = append(docs, c.docs[key])
	}
	return decodeInto(docs, results)
}

// StreamFind implements fmongo.IStreamingCollection so loaders exercise their
// production cursor path rather than silently falling back to Find.
func (c *Collection) StreamFind(ctx context.Context, filter any, consume func([]byte) error, opts ...fmongo.FindOption) error {
	tx := transactionFrom(ctx)
	unlock := c.lockFor(tx)
	c.LastFilter = filter
	if err := c.enter(tx, "StreamFind"); err != nil {
		unlock()
		return err
	}
	matched, err := c.matchLocked(filter)
	if err != nil {
		unlock()
		return err
	}
	if len(opts) > 0 {
		if matched, err = c.sortLocked(matched, opts[0].Sort); err != nil {
			unlock()
			return err
		}
		matched = paginateMatches(matched, opts[0])
	}
	raws := make([][]byte, 0, len(matched))
	for _, key := range matched {
		raw, err := bson.Marshal(c.docs[key])
		if err != nil {
			unlock()
			return err
		}
		raws = append(raws, raw)
	}
	unlock()
	for _, raw := range raws {
		if err := consume(raw); err != nil {
			return err
		}
	}
	return nil
}

// RR-20261004-NC-20：排序后先skip再limit，页外为空；比较完成后再缩窄int64。
func paginateMatches(matched []string, option fmongo.FindOption) []string {
	if option.Skip > 0 {
		if option.Skip >= int64(len(matched)) {
			return matched[:0]
		}
		matched = matched[option.Skip:]
	}
	if option.Limit > 0 && option.Limit < int64(len(matched)) {
		matched = matched[:option.Limit]
	}
	return matched
}

func (c *Collection) UpdateOne(ctx context.Context, filter any, update any) (result *fmongo.UpdateResult, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter, c.LastUpdate = filter, update
	if err := c.enter(tx, "UpdateOne"); err != nil {
		return nil, err
	}
	return c.updateLocked(filter, update, false, false)
}

func (c *Collection) UpdateMany(ctx context.Context, filter any, update any) (result *fmongo.UpdateResult, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter, c.LastUpdate = filter, update
	if err := c.enter(tx, "UpdateMany"); err != nil {
		return nil, err
	}
	return c.updateLocked(filter, update, false, true)
}

func (c *Collection) ReplaceOne(ctx context.Context, filter any, replacement any) (result *fmongo.UpdateResult, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter, c.LastUpdate = filter, replacement
	if err := c.enter(tx, "ReplaceOne"); err != nil {
		return nil, err
	}
	return c.replaceLocked(filter, replacement, false)
}

func (c *Collection) DeleteOne(ctx context.Context, filter any) (deleted int64, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter = filter
	if err := c.enter(tx, "DeleteOne"); err != nil {
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

func (c *Collection) DeleteMany(ctx context.Context, filter any) (deleted int64, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter = filter
	if err := c.enter(tx, "DeleteMany"); err != nil {
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

func (c *Collection) FindOneAndUpdate(ctx context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) (err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter, c.LastUpdate = filter, update
	if err := c.enter(tx, "FindOneAndUpdate"); err != nil {
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
	var selectedKey string
	if len(matched) > 0 {
		selectedKey = matched[0]
		before = cloneDoc(c.docs[selectedKey])
	}
	updated, err := c.updateLocked(filter, update, option.Upsert, false)
	if err != nil {
		return err
	}
	if updated.UpsertedCount > 0 {
		selectedKey = updated.UpsertedID
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
	// RR-20261004-NC-25：post-image 属于被更新的身份，不能用旧谓词另选文档。
	doc, ok := c.docs[selectedKey]
	if !ok {
		return fmongo.ErrNotFound
	}
	return decodeInto(doc, result)
}

func (c *Collection) FindOneAndDelete(ctx context.Context, filter any, result any) (err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter = filter
	if err := c.enter(tx, "FindOneAndDelete"); err != nil {
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

func (c *Collection) FindOneAndReplace(ctx context.Context, filter any, replacement any, result any) (err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter, c.LastUpdate = filter, replacement
	if err := c.enter(tx, "FindOneAndReplace"); err != nil {
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

func (c *Collection) CountDocuments(ctx context.Context, filter any) (int64, error) {
	tx := transactionFrom(ctx)
	unlock := c.lockFor(tx)
	defer unlock()
	c.LastFilter = filter
	if err := c.enter(tx, "CountDocuments"); err != nil {
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
func (c *Collection) Aggregate(ctx context.Context, _ any, _ any) error {
	tx := transactionFrom(ctx)
	unlock := c.lockFor(tx)
	defer unlock()
	if err := c.enter(tx, "Aggregate"); err != nil {
		return err
	}
	return fmt.Errorf("%w: Aggregate", ErrUnsupported)
}

func (c *Collection) BulkWrite(ctx context.Context, models []fmongo.WriteModel) (bulk *fmongo.BulkWriteResult, err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	unlock := c.lockFor(tx)
	defer unlock()
	if err := c.enter(tx, "BulkWrite"); err != nil {
		return nil, err
	}
	// RR-20261004-NC-28：正式 driver 在提交任何模型前先验证全部 Type。
	for i, model := range models {
		switch model.Type {
		case fmongo.WriteModelInsertOne, fmongo.WriteModelUpdateOne, fmongo.WriteModelReplaceOne, fmongo.WriteModelDeleteOne:
		default:
			return nil, fmt.Errorf("%w: bulk model type %d at index %d", ErrUnsupported, model.Type, i)
		}
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

func (c *Collection) EnsureIndexes(ctx context.Context, indexes []fmongo.IndexModel) (err error) {
	tx := transactionFrom(ctx)
	defer func() { err = tx.writeFailed(err) }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enter(tx, "EnsureIndexes"); err != nil {
		return err
	}
	if tx != nil {
		return fmt.Errorf("%w: transactional EnsureIndexes", ErrUnsupported)
	}
	for _, index := range indexes {
		fields, err := indexFields(index.Keys)
		if err != nil {
			return err
		}
		// RR-20261004-NC-27：先验证存量文档，再发布本个索引；前面成功的索引保留。
		if index.Unique {
			unique := uniqueIndex{fields: fields, sparse: index.Sparse}
			for key, doc := range c.docs {
				if err := c.checkUniqueFieldsLocked(doc, key, unique); err != nil {
					return err
				}
			}
			if c.uniqueIndexes == nil {
				c.uniqueIndexes = make(map[string]uniqueIndex)
			}
			name := index.Name
			if name == "" {
				name = strings.Join(fields, "_")
			}
			c.uniqueIndexes[name] = unique
		}
		c.Indexes = append(c.Indexes, index)
		c.revision++
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

// uniqueIndex is the part of a unique IndexModel the duplicate check needs.
// The fake has no partial indexes: fmongo.IndexModel carries no
// PartialFilterExpression, so Unique + Sparse is the whole definition.
type uniqueIndex struct {
	fields []string
	sparse bool
}

func (c *Collection) checkUniqueLocked(doc bson.M, selfKey string) error {
	for _, index := range c.uniqueIndexes {
		if err := c.checkUniqueFieldsLocked(doc, selfKey, index); err != nil {
			return err
		}
	}
	return nil
}

// uniqueKey returns the index key a document is stored under, or ok=false
// when the index holds no entry for it.
//
// RR-20261004-05: Mongo keys a missing field (including a dotted path that
// runs through a scalar or absent parent) as BSON null, equal to an explicit
// null. A sparse index leaves a document out only when every indexed field is
// missing; one present field — even an explicit null — indexes the document
// and the missing ones still count as null. Checked on a real replica set
// (8.0.28). The fake used to skip a document whenever any field was missing
// and ignored Sparse, so it accepted duplicate null keys real Mongo rejects.
func uniqueKey(doc bson.M, index uniqueIndex) (values []any, ok bool, err error) {
	values = make([]any, len(index.fields))
	present := false
	for i, field := range index.fields {
		value, found, err := uniqueFieldValue(doc, field)
		if err != nil {
			return nil, false, err
		}
		if found {
			values[i] = value
			present = true
		}
	}
	if index.sparse && !present {
		return nil, false, nil
	}
	return values, true, nil
}

// uniqueFieldValue reads one indexed path, refusing arrays.
//
// RR-20261005-NC-102: real Mongo builds a multikey entry per array element —
// {tags:[1,2]} collides with {tags:[2,3]} and with {tags:2}, and a path
// through an array of documents ("m.n" over m:[{n:1}]) indexes each element's
// n. The fake used to compare the whole array as one value and treat a path
// through an array as missing (null), so it accepted duplicates real Mongo
// rejects and reported duplicates real Mongo accepts. It does not model
// multikey indexes, so it says so instead of guessing.
func uniqueFieldValue(doc bson.M, path string) (any, bool, error) {
	parts := strings.Split(path, ".")
	for depth := 1; depth <= len(parts); depth++ {
		value, found := lookupPath(doc, strings.Join(parts[:depth], "."))
		if !found {
			return nil, false, nil
		}
		switch value.(type) {
		case bson.A, []any:
			return nil, false, fmt.Errorf("%w: unique index path %q reaches an array (multikey index)", ErrUnsupported, path)
		}
		if depth == len(parts) {
			return value, true, nil
		}
	}
	return nil, false, nil
}

func (c *Collection) checkUniqueFieldsLocked(doc bson.M, selfKey string, index uniqueIndex) error {
	values, ok, err := uniqueKey(doc, index)
	if err != nil || !ok {
		return err
	}
	for key, existing := range c.docs {
		if key == selfKey {
			continue
		}
		// Same rule as uniqueKey, inline so the scan does not allocate per
		// stored document: a missing field compares as null, and a sparse
		// index skips a stored document none of whose fields exist. Stored
		// documents passed the same array refusal on their own write.
		same, present := true, false
		for i, field := range index.fields {
			other, found, err := uniqueFieldValue(existing, field)
			if err != nil {
				return err
			}
			present = present || found
			eq, err := valuesEqual(other, values[i])
			if err != nil {
				return err
			}
			if !eq {
				same = false
				break
			}
		}
		if same && (present || !index.sparse) {
			return duplicateKeyError()
		}
	}
	return nil
}

func (c *Collection) removeLocked(key string) {
	delete(c.docs, key)
	c.revision++
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
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			key, err := idKey(value)
			if err != nil {
				return nil, false
			}
			// RR-20261004-NC-23：按规范化物理键去重，保留首次出现顺序。
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
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
			return nil, duplicateKeyError()
		}
		if err := c.checkUniqueLocked(updated, key); err != nil {
			return nil, err
		}
		c.docs[key] = updated
		c.revision++
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
		c.revision++
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
			return nil, duplicateKeyError()
		}
		if err := c.checkUniqueLocked(normalized, key); err != nil {
			return nil, err
		}
		c.docs[key] = normalized
		c.revision++
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
	c.revision++
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
		out[k] = cloneBSONValue(v)
	}
	return out
}

// RR-20261004-NC-22：复制 BSON 容器并保持 M/D 形状，避免快照和读结果共享数据。
// 存储值经真实 BSON codec 归一；其他 BSON 标量是值类型，无可变子对象。
func cloneBSONValue(value any) any {
	switch v := value.(type) {
	case bson.M:
		return cloneDoc(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, entry := range v {
			out[key] = cloneBSONValue(entry)
		}
		return out
	case bson.D:
		out := make(bson.D, len(v))
		for i, entry := range v {
			out[i] = bson.E{Key: entry.Key, Value: cloneBSONValue(entry.Value)}
		}
		return out
	case bson.A:
		out := make(bson.A, len(v))
		for i, entry := range v {
			out[i] = cloneBSONValue(entry)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, entry := range v {
			out[i] = cloneBSONValue(entry)
		}
		return out
	case bson.Binary:
		v.Data = append([]byte(nil), v.Data...)
		return v
	case bson.CodeWithScope:
		v.Scope = cloneBSONValue(v.Scope)
		return v
	case bson.Raw:
		return append(bson.Raw(nil), v...)
	case bson.RawValue:
		v.Value = append([]byte(nil), v.Value...)
		return v
	case []byte:
		return append([]byte(nil), v...)
	default:
		return value
	}
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
		var value any
		var exists bool
		switch container := current.(type) {
		case bson.M:
			value, exists = container[part]
		case map[string]any:
			value, exists = container[part]
		case bson.D:
			for _, entry := range container {
				if entry.Key == part {
					value, exists = entry.Value, true
					break
				}
			}
		default:
			return nil, false
		}
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
		next, ok := pathMap(current[part])
		if !ok {
			next = bson.M{}
		}
		current[part] = next
		current = next
	}
	current[parts[len(parts)-1]] = value
}

func unsetPath(doc bson.M, path string) {
	parts := strings.Split(path, ".")
	current := doc
	for _, part := range parts[:len(parts)-1] {
		next, ok := pathMap(current[part])
		if !ok {
			return
		}
		current[part] = next
		current = next
	}
	delete(current, parts[len(parts)-1])
}

// RR-20261004-NC-26：BSON 解码产生 D；修改所经容器时保留同级字段。
// 数组下标及位置运算符不在替身的已支持路径语法中。
func pathMap(value any) (bson.M, bool) {
	switch value := value.(type) {
	case bson.M:
		return value, true
	case map[string]any:
		return bson.M(value), true
	case bson.D:
		out := make(bson.M, len(value))
		for _, entry := range value {
			out[entry.Key] = entry.Value
		}
		return out, true
	default:
		return nil, false
	}
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
	if _, ok := asFloat(left); ok {
		_, ok := asFloat(right)
		if !ok {
			return false, nil
		}
		cmp, err := compareNumbers(left, right)
		return cmp == 0 && err == nil, err
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
	if _, ok := asFloat(left); ok {
		_, ok := asFloat(right)
		if !ok {
			return 0, fmt.Errorf("%w: compare %T with %T", ErrUnsupported, left, right)
		}
		return compareNumbers(left, right)
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

// RR-20261004-NC-24：整数和有限浮点按精确值比较，不先舍入到 float64。
// 非有限浮点显式拒绝，避免 NaN 在排序中冒充相等。
func compareNumbers(left, right any) (int, error) {
	a, err := exactNumber(left)
	if err != nil {
		return 0, err
	}
	b, err := exactNumber(right)
	if err != nil {
		return 0, err
	}
	return a.Cmp(b), nil
}

func exactNumber(value any) (*big.Rat, error) {
	n := new(big.Rat)
	switch v := value.(type) {
	case uint:
		return n.SetInt(new(big.Int).SetUint64(uint64(v))), nil
	case uint8:
		return n.SetInt64(int64(v)), nil
	case uint16:
		return n.SetInt64(int64(v)), nil
	case uint32:
		return n.SetInt64(int64(v)), nil
	case uint64:
		return n.SetInt(new(big.Int).SetUint64(v)), nil
	case float32:
		if n.SetFloat64(float64(v)) != nil {
			return n, nil
		}
	case float64:
		if n.SetFloat64(v) != nil {
			return n, nil
		}
	default:
		if v, ok := asInt64(value); ok {
			return n.SetInt64(v), nil
		}
	}
	return nil, fmt.Errorf("%w: numeric value %v (%T)", ErrUnsupported, value, value)
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
