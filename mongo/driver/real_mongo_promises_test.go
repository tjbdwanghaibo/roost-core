//go:build integration

package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func replicaSetURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; run through scripts/integration/dataengine-env.sh")
	}
	return uri
}

func connect(t *testing.T, uri string, requireReplicaSet bool, policy IndexMigrationPolicy) *Client {
	t.Helper()
	cfg := fmongo.DefaultConfig(uri)
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RequireReplicaSet = requireReplicaSet
	client, err := NewClient(cfg, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

// U-0153 · C2 · client.go:90（U-0148 留真机）：production 模式要求副本集或分片；对一个真实的单机
// mongod 必须拒绝，对副本集放行。client.go:93（逻辑会话）在此顺带探测：mongod 3.6+ 的单机也报告
// logicalSessionTimeoutMinutes，所以那条对受支持的服务端不可达，测试只记录探测结果。
func TestRealMongoValidateDeploymentRefusesAStandalone(t *testing.T) {
	binary, err := exec.LookPath("mongod")
	if err != nil {
		t.Skip("mongod binary not on PATH; the standalone deployment guard test is skipped")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	dir := t.TempDir()
	cmd := exec.Command(binary, "--port", fmt.Sprint(port), "--dbpath", dir, "--bind_ip", "127.0.0.1", "--logpath", filepath.Join(dir, "mongod.log"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", endpoint, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	standalone := connect(t, "mongodb://"+endpoint+"/?directConnection=true", true, IndexMigrationPolicy{})
	if err := standalone.ValidateDeployment(ctx); err == nil || !strings.Contains(err.Error(), "requires a replica set") {
		t.Fatalf("ValidateDeployment against a standalone = %v, want the replica-set refusal", err)
	}
	var hello struct {
		LogicalSessionTimeout *int32 `bson:"logicalSessionTimeoutMinutes"`
	}
	if err := standalone.cli.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if hello.LogicalSessionTimeout == nil {
		t.Log("standalone reports no logicalSessionTimeoutMinutes: client.go:93 IS reachable on this server")
	} else {
		t.Logf("standalone reports logicalSessionTimeoutMinutes=%d: client.go:93 is unreachable on this server version", *hello.LogicalSessionTimeout)
	}
	lenient := connect(t, "mongodb://"+endpoint+"/?directConnection=true", false, IndexMigrationPolicy{})
	if err := lenient.ValidateDeployment(ctx); err != nil {
		t.Fatalf("ValidateDeployment without the replica-set requirement = %v", err)
	}
}

// U-0153 · C2 · collection.go:262（U-0148 留真机）：同名索引定义不同时，没有迁移策略要把冲突原样上抛，
// 有策略（客户端允许 + 模型允许）才丢弃重建。
func TestRealMongoIndexConflictIsSurfacedUnlessRecreationIsAllowed(t *testing.T) {
	uri := replicaSetURI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	strict := connect(t, uri, true, IndexMigrationPolicy{})
	if err := strict.ValidateDeployment(ctx); err != nil {
		t.Fatalf("ValidateDeployment against the replica set = %v", err)
	}
	dbName := fmt.Sprintf("roost_it_guards_%d", time.Now().UnixNano())
	db := strict.Database(dbName)
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	coll := db.Collection("indexed")
	if err := coll.EnsureIndexes(ctx, []fmongo.IndexModel{{Name: "idx_key", Keys: bson.D{{Key: "a", Value: 1}}}}); err != nil {
		t.Fatal(err)
	}
	conflicting := fmongo.IndexModel{Name: "idx_key", Keys: bson.D{{Key: "b", Value: 1}}, RecreateOnConflict: true}
	err := coll.EnsureIndexes(ctx, []fmongo.IndexModel{conflicting})
	if err == nil || !isIndexDefinitionConflict(err) {
		t.Fatalf("conflicting index without a migration policy = %v, want the definition conflict surfaced", err)
	}
	migrating := connect(t, uri, true, IndexMigrationPolicy{AllowRecreate: true})
	if err := migrating.Database(dbName).Collection("indexed").EnsureIndexes(ctx, []fmongo.IndexModel{conflicting}); err != nil {
		t.Fatalf("conflicting index with recreation allowed = %v", err)
	}
	if err := coll.EnsureIndexes(ctx, []fmongo.IndexModel{{Name: "idx_key", Keys: bson.D{{Key: "b", Value: 1}}}}); err != nil {
		t.Fatalf("the recreated definition is not in place: %v", err)
	}
}
