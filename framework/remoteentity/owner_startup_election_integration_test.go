//go:build integration

package remoteentity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// O-M6-5（维护者第十二轮决定）：owner 启动时建 Remote 存储索引（EnsureRemoteStorage → EnsureIndexes）
// 撞上 Mongo 选举，做有界重试而不是启动失败。私有副本集上一边让主节点 stepDown，一边连续执行 owner
// 启动的存储初始化（每次一个新库，都要真正建索引）：选举期间的每一次都必须成功。修前在开发期观察到
// InterruptedDueToReplStateChange 让 owner 子进程启动失败（MIRROR-STEP-6-LOCAL §3.5）。
func TestMirrorLocalOwnerStorageInitSurvivesAMongoElection(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || os.Getenv("ROOST_MIRROR_LOCAL") != "1" {
		t.Skip("run scripts/mirror-local.sh test-core (private dependency processes)")
	}
	script := os.Getenv("ROOST_MIRROR_LOCAL_SCRIPT")
	if script == "" {
		t.Fatal("the private environment does not export ROOST_MIRROR_LOCAL_SCRIPT")
	}
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oldMetrics := metrics.DefaultRegistry()
	registry := metrics.NewRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(oldMetrics) })
	stamp := time.Now().UnixNano()
	var databases []string
	t.Cleanup(func() {
		settle := exec.Command(script, "fault", "mongo-settle")
		if out, err := settle.CombinedOutput(); err != nil {
			t.Errorf("mongo-settle: %v\n%s", err, out)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range databases {
			_ = client.Database(name).Drop(cleanupCtx)
		}
		_ = client.Close(cleanupCtx)
	})

	var stepDownDone atomic.Bool
	stepDownOut := make(chan string, 1)
	go func() {
		out, err := exec.Command(script, "fault", "mongo-stepdown").CombinedOutput()
		if err != nil {
			stepDownOut <- fmt.Sprintf("error: %v: %s", err, out)
		} else {
			stepDownOut <- strings.TrimSpace(string(out))
		}
		stepDownDone.Store(true)
	}()

	// stepDown 开始前后、选举期间、新主选出之后 2s 都在初始化，覆盖“在途被打断”和“打到旧主”两种。
	var calls, slowest int
	var slowestTook time.Duration
	var tail time.Time
	for {
		if stepDownDone.Load() {
			if tail.IsZero() {
				tail = time.Now().Add(2 * time.Second)
			} else if time.Now().After(tail) {
				break
			}
		}
		calls++
		database := fmt.Sprintf("m6o5_%d_%d", stamp, calls)
		databases = append(databases, database)
		started := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := NewMongoCommitter(client, database, 1, time.Hour).EnsureRemoteStorage(callCtx)
		cancel()
		if took := time.Since(started); took > slowestTook {
			slowest, slowestTook = calls, took
		}
		if err != nil {
			t.Fatalf("owner storage init #%d failed during the election: %v", calls, err)
		}
		if calls > 5000 {
			t.Fatal("stepDown never finished")
		}
	}
	var retries int64
	for _, metric := range registry.Snapshot() {
		if metric.Name == "mongo.ensure_index.election_retries.total" {
			retries = metric.Value
		}
	}
	// 选举是否正好打断某次初始化取决于时序：retries = 0 说明这一轮没撞上（通过但不构成证据，再跑一轮）。
	t.Logf("stepDown: %s; %d owner storage inits all succeeded; slowest #%d took %s; election retries %d",
		<-stepDownOut, calls, slowest, slowestTook, retries)
}
