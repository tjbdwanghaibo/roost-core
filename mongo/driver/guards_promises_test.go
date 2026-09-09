package driver

import (
	"context"
	"errors"
	"strings"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// U-0148 · C2 · gap map core `mongo/driver` 7/7：未初始化的客户端不能校验部署；流式查询拒绝 nil
// 消费者；驱动错误翻译成本包哨兵。`client.go:90 / 93`（副本集 / 逻辑会话）与 `collection.go:262`
// （索引定义冲突）需要真实 Mongo 的 hello / 索引冲突响应，留给真机集成。
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
