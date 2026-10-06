//go:build integration

package syncbus

// OPEN-ITEMS B31：RR-20260926-56 的升级迁移路径在真实 JetStream 上演练。
// 写了非默认 prefix、没写 stream 的旧部署（v1.17.0 及以前）把流建在 ROOST_SYNC 上；升级后流名由 prefix 派生，
// 新流与旧流的 subjects 重叠，启动必须明确失败（不能静默换流、丢掉游标）；配置 `syncbus.stream: ROOST_SYNC`
// 后能启动，durable 游标从旧部署停下的位置继续。
//
// 旧部署用 `syncbus.stream: ROOST_SYNC` 模拟：v1.17.0 在不写 stream 时的流名、subjects（prefix.>）与 durable 名
// （durableSyncName 只取决于 prefix / topic / sid，RR-56 未改）与此完全相同。2026-09-27 另用 v1.17.0 源码
// 编译的同一流程实跑过一次，结果一致（见 RR-20260926-56 修复记录“后续验证”）。
// 用例在临时端口上自己拉起 nats-server（临时存储目录），不碰共享环境里的 ROOST_SYNC。

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	kitnats "github.com/tjbdwanghaibo/roost-core/kit/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

const migrationTopic = "stream_migration"

func startPrivateJetStream(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server binary not on PATH; the JetStream stream migration drill is skipped (install nats-server to run it)")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	cmd := exec.Command(binary, "-a", "127.0.0.1", "-p", fmt.Sprint(port), "-js", "-sd", t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return "nats://" + addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("nats-server did not start listening")
	return ""
}

type migrationNode struct {
	nats *kitnats.NatsMod
	sync *SyncBusMod
	bus  fsyncbus.ISyncBus
}

// startMigrationNode 经正式 kit NatsMod + SyncBusMod 装配一个 JetStream sync bus。
func startMigrationNode(t *testing.T, url string, sid int32, stream string) (*migrationNode, error) {
	t.Helper()
	cfg := viper.New()
	cfg.Set("sid", sid)
	cfg.Set("server_type", "migration")
	cfg.Set("nats.url", url)
	cfg.Set("syncbus.transport", "jetstream")
	cfg.Set("syncbus.prefix", "zzmigrate.sync")
	cfg.Set("syncbus.storage", "file")
	if stream != "" {
		cfg.Set("syncbus.stream", stream)
	}
	node := &migrationNode{nats: kitnats.NewNatsMod(nil), sync: NewSyncBusMod(sid)}
	registry := app.NewRegistry(cfg)
	if err := node.nats.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := node.nats.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := node.nats.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.stop)
	if err := node.sync.Init(cfg); err != nil {
		return nil, err
	}
	if err := node.sync.Provide(registry); err != nil {
		return nil, err
	}
	if err := node.sync.Start(); err != nil {
		return nil, err
	}
	bus, ok := app.Lookup[fsyncbus.ISyncBus](registry, mods.ModSyncBus)
	if !ok {
		t.Fatal("sync bus capability not published")
	}
	node.bus = bus
	return node, nil
}

func (n *migrationNode) stop() {
	_ = n.sync.StopWithContext(context.Background())
	_ = n.nats.StopWithContext(context.Background())
}

type migrationInbox struct {
	mu   sync.Mutex
	keys []int64
	got  chan struct{}
}

func newMigrationInbox() *migrationInbox { return &migrationInbox{got: make(chan struct{}, 64)} }

func (in *migrationInbox) handle(_ context.Context, msg *fsyncbus.SyncMsg) error {
	in.mu.Lock()
	in.keys = append(in.keys, msg.Key)
	in.mu.Unlock()
	in.got <- struct{}{}
	return nil
}

// await 等到 want 条到达，再留一小段时间确认没有多余的重投。
func (in *migrationInbox) await(t *testing.T, want int) []int64 {
	t.Helper()
	for range want {
		select {
		case <-in.got:
		case <-time.After(10 * time.Second):
			in.mu.Lock()
			defer in.mu.Unlock()
			t.Fatalf("received %v, waiting for %d messages", in.keys, want)
		}
	}
	select {
	case <-in.got:
	case <-time.After(300 * time.Millisecond):
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	keys := slices.Clone(in.keys)
	slices.Sort(keys)
	return keys
}

func publishMigration(t *testing.T, bus fsyncbus.ISyncBus, keys ...int64) {
	t.Helper()
	for _, key := range keys {
		if err := bus.Publish(&fsyncbus.SyncMsg{Topic: migrationTopic, Key: key, Version: key, Data: []byte(fmt.Sprint(key))}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNonDefaultPrefixUpgradeFailsOnOverlapAndResumesOnTheLegacyStream(t *testing.T) {
	url := startPrivateJetStream(t)

	// 旧部署：流 ROOST_SYNC、subjects zzmigrate.sync.>。订阅者确认 1..3 后下线，期间再发 4、5。
	legacySubscriber, err := startMigrationNode(t, url, 1, "ROOST_SYNC")
	if err != nil {
		t.Fatal(err)
	}
	legacyInbox := newMigrationInbox()
	if _, err := legacySubscriber.bus.Subscribe(migrationTopic, legacyInbox.handle); err != nil {
		t.Fatal(err)
	}
	publisher, err := startMigrationNode(t, url, 2, "ROOST_SYNC")
	if err != nil {
		t.Fatal(err)
	}
	publishMigration(t, publisher.bus, 1, 2, 3)
	if got := legacyInbox.await(t, 3); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("legacy subscriber received %v, want [1 2 3]", got)
	}
	legacySubscriber.stop()
	publishMigration(t, publisher.bus, 4, 5)
	publisher.stop()

	// 升级、不改配置：派生流 ZZMIGRATE_SYNC 与 ROOST_SYNC 的 subjects 重叠，必须在启动时明确失败。
	if _, err := startMigrationNode(t, url, 1, ""); err == nil || !strings.Contains(err.Error(), "subjects overlap") || !strings.Contains(err.Error(), "ZZMIGRATE_SYNC") {
		t.Fatalf("upgraded node without syncbus.stream: err=%v, want a subjects-overlap failure naming the derived stream ZZMIGRATE_SYNC", err)
	}

	// 升级并配置 stream: ROOST_SYNC：能启动，游标续上（收到离线期间的 4、5，不重投 1..3），新消息照常到达。
	upgraded, err := startMigrationNode(t, url, 1, "ROOST_SYNC")
	if err != nil {
		t.Fatalf("upgraded node with syncbus.stream: ROOST_SYNC: %v", err)
	}
	inbox := newMigrationInbox()
	if _, err := upgraded.bus.Subscribe(migrationTopic, inbox.handle); err != nil {
		t.Fatal(err)
	}
	if got := inbox.await(t, 2); !slices.Equal(got, []int64{4, 5}) {
		t.Fatalf("after the upgrade the durable cursor delivered %v, want exactly the backlog [4 5]", got)
	}
	upgradedPublisher, err := startMigrationNode(t, url, 2, "ROOST_SYNC")
	if err != nil {
		t.Fatal(err)
	}
	publishMigration(t, upgradedPublisher.bus, 6)
	if got := inbox.await(t, 1); !slices.Equal(got, []int64{4, 5, 6}) {
		t.Fatalf("after publishing 6 the subscriber holds %v, want [4 5 6]", got)
	}
}
