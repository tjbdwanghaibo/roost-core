package driver

import (
	"context"
	"errors"
	"strings"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestIsIndexDefinitionConflict(t *testing.T) {
	for _, err := range []error{
		drivermongo.CommandError{Code: 85},
		drivermongo.CommandError{Code: 86},
		drivermongo.CommandError{Name: "IndexOptionsConflict"},
		drivermongo.CommandError{Name: "IndexKeySpecsConflict"},
	} {
		if !isIndexDefinitionConflict(err) {
			t.Fatalf("expected index definition conflict for %v", err)
		}
	}
	if isIndexDefinitionConflict(drivermongo.CommandError{Code: 27, Name: "IndexNotFound"}) {
		t.Fatalf("index not found must not be treated as definition conflict")
	}
	if isIndexDefinitionConflict(errors.New("ordinary error")) {
		t.Fatalf("ordinary error must not be treated as definition conflict")
	}
}

func TestIsIndexNotFound(t *testing.T) {
	for _, err := range []error{
		drivermongo.CommandError{Code: 27},
		drivermongo.CommandError{Name: "IndexNotFound"},
	} {
		if !isIndexNotFound(err) {
			t.Fatalf("expected index not found for %v", err)
		}
	}
	if isIndexNotFound(drivermongo.CommandError{Code: 86, Name: "IndexKeySpecsConflict"}) {
		t.Fatalf("index definition conflict must not be treated as index not found")
	}
}

func TestIndexConflictPolicyRequiresExplicitAutoRecreate(t *testing.T) {
	idx := fmongo.IndexModel{Name: "idx_player", ConflictPolicy: fmongo.IndexConflictRecreate}
	if shouldRecreateIndexOnConflict(idx, IndexMigrationPolicy{}) {
		t.Fatal("index conflict should not recreate without explicit migration policy")
	}
	if !shouldRecreateIndexOnConflict(idx, IndexMigrationPolicy{AllowRecreate: true}) {
		t.Fatal("index conflict should recreate when model and migration policy both allow it")
	}
}

func TestMongoIndexModelDistinguishesRelativeAndAbsoluteExpiry(t *testing.T) {
	relative := resolveIndexOptions(t, fmongo.IndexModel{TTL: 60})
	if relative.ExpireAfterSeconds == nil || *relative.ExpireAfterSeconds != 60 {
		t.Fatalf("relative ttl=%v, want 60", relative.ExpireAfterSeconds)
	}
	absolute := resolveIndexOptions(t, fmongo.IndexModel{ExpireAt: true})
	if absolute.ExpireAfterSeconds == nil || *absolute.ExpireAfterSeconds != 0 {
		t.Fatalf("absolute ttl=%v, want 0", absolute.ExpireAfterSeconds)
	}
	none := resolveIndexOptions(t, fmongo.IndexModel{})
	if none.ExpireAfterSeconds != nil {
		t.Fatalf("unset ttl=%v, want nil", none.ExpireAfterSeconds)
	}
}

func resolveIndexOptions(t *testing.T, idx fmongo.IndexModel) options.IndexOptions {
	t.Helper()
	var resolved options.IndexOptions
	for _, apply := range mongoIndexModel(idx).Options.List() {
		if err := apply(&resolved); err != nil {
			t.Fatal(err)
		}
	}
	return resolved
}

func TestStringifyIDDoesNotExposeNilSentinel(t *testing.T) {
	if got := stringifyID(nil); got != "" {
		t.Fatalf("nil id=%q, want empty", got)
	}
	if got := stringifyID(42); got != "42" {
		t.Fatalf("id=%q, want 42", got)
	}
}

// FEATURE_LOGIC M8 item 3: an unknown write model type is refused with its
// index before anything reaches the driver — a nil model handed to the driver
// panics inside it. The loop runs before c.coll is touched, so a zero
// collection is enough to prove the refusal. Reverting the default branch made
// no test red before this one existed.
func TestBulkWriteRefusesAnUnknownModelTypeBeforeReachingTheDriver(t *testing.T) {
	c := &collection{}
	_, err := c.BulkWrite(context.Background(), []fmongo.WriteModel{
		{Type: fmongo.WriteModelInsertOne, Document: map[string]any{"a": 1}},
		{Type: fmongo.WriteModelType(99)},
	})
	if err == nil {
		t.Fatal("an unknown write model type was accepted")
	}
	for _, want := range []string{"unsupported", "index 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
}

// 未初始化的客户端不能校验部署；流式查询拒绝 nil 消费者；驱动错误翻译成本包哨兵。副本集 / 逻辑会话与索引定义冲突需要真实 Mongo，
// 由 integration 用例覆盖。
func TestClientAndCollectionGuardsWithoutADeployment(t *testing.T) {
	ctx := context.Background()
	var none *Client
	if err := none.ValidateDeployment(ctx); err == nil || !strings.Contains(err.Error(), "client is not initialized") {
		t.Fatalf("ValidateDeployment on a nil client = %v", err)
	}
	if err := (&Client{}).ValidateDeployment(ctx); err == nil || !strings.Contains(err.Error(), "client is not initialized") {
		t.Fatalf("ValidateDeployment without a connection = %v", err)
	}
	if err := (&collection{}).StreamFind(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "nil stream consumer") {
		t.Fatalf("StreamFind with a nil consumer = %v", err)
	}
	if err := wrapError(nil); err != nil {
		t.Fatalf("wrapError(nil) = %v", err)
	}
	if err := wrapError(mongo.ErrNoDocuments); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("wrapError(ErrNoDocuments) = %v", err)
	}
	duplicate := mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000, Message: "E11000 duplicate key"}}}
	if err := wrapError(duplicate); !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("wrapError(duplicate key) = %v", err)
	}
	other := errors.New("network down")
	if err := wrapError(other); !errors.Is(err, other) {
		t.Fatalf("wrapError(other) = %v, want passthrough", err)
	}
}

// 撞键转换成 fmongo.ErrDuplicateKey 时保留驱动错误链（errors.As 取回 WriteException、错误码与服务端标签）；非撞键错误原样返回。
// 历史：RR-20260926-34。
func TestWrapErrorKeepsTheDriverChainBehindTheSentinel(t *testing.T) {
	duplicate := drivermongo.WriteException{
		WriteErrors: drivermongo.WriteErrors{{Code: 11000, Message: "E11000 duplicate key error"}},
		Labels:      []string{"SomeServerLabel"},
	}
	err := wrapError(duplicate)
	if !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("err=%v, want errors.Is ErrDuplicateKey", err)
	}
	var writeErr drivermongo.WriteException
	if !errors.As(err, &writeErr) || !drivermongo.IsDuplicateKeyError(err) {
		t.Fatalf("err=%v lost the WriteException", err)
	}
	var labeled drivermongo.LabeledError
	if !errors.As(err, &labeled) || !labeled.HasErrorLabel("SomeServerLabel") {
		t.Fatalf("err=%v lost the server labels", err)
	}

	conflict := drivermongo.CommandError{Code: 112, Name: "WriteConflict", Labels: []string{"TransientTransactionError"}}
	if got := wrapError(conflict); errors.Is(got, fmongo.ErrDuplicateKey) || !errors.As(got, &labeled) || !labeled.HasErrorLabel("TransientTransactionError") {
		t.Fatalf("write conflict=%v, want it unchanged and still transient", got)
	}
	if got := wrapError(drivermongo.ErrNoDocuments); got != fmongo.ErrNotFound {
		t.Fatalf("no documents=%v, want the bare ErrNotFound", got)
	}
}
