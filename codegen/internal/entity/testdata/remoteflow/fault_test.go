package remoteflow

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func remoteNatsURL() string {
	value := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	// 固定连到待停止的第一个节点；正式 driver 默认从 INFO 发现另外两个节点。
	return strings.Split(value, ",")[0]
}

// 故障命令返回时依赖已停止；第二笔正式业务在故障窗口提交，独立恢复任务随后重启节点。
func startRemoteBusinessFault(t *testing.T) func() {
	t.Helper()
	fault := os.Getenv("ROOST_REMOTE_FAULT")
	if fault == "" {
		return func() {}
	}
	script := os.Getenv("ROOST_REMOTE_FAULT_SCRIPT")
	if script == "" {
		t.Fatal("fault script required")
	}
	run := func(action string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "bash", script, action).CombinedOutput()
		t.Logf("fault %s: %s", action, out)
		return err
	}
	var once sync.Once
	heal := func() {
		once.Do(func() {
			if err := run("heal"); err != nil {
				t.Errorf("heal: %v", err)
			}
		})
	}
	t.Cleanup(heal)
	if err := run(fault); err != nil {
		t.Fatalf("inject %s: %v", fault, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		<-timer.C
		heal()
	}()
	return func() { <-done }
}

// 故障读取使用正式权威 backend；计数只观测真实 Mongo 回填，不制造快照。
type observedSnapshotBackend struct {
	entity.IRemoteEntityBackend
	loads atomic.Int64
}

func (b *observedSnapshotBackend) LoadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	b.loads.Add(1)
	return b.IRemoteEntityBackend.LoadRemoteSnapshot(ctx, key, consistency, minVersion)
}
func remoteFaultRefill() bool { return strings.HasPrefix(os.Getenv("ROOST_REMOTE_FAULT"), "nats-") }
