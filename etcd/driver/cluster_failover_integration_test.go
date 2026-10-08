//go:build integration && (darwin || linux)

package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

// 本机三个独立etcd进程，杀掉实际Raft leader；不碰共享etcd，不冒称跨主机网络验收。
func TestRealEtcdThreeNodeLeaderLossPreservesWatchDiscoveryAndElection(t *testing.T) {
	binary, err := exec.LookPath("etcd")
	if err != nil {
		t.Skip("etcd binary is required")
	}
	root := t.TempDir()
	var clients, peers, members []string
	ports := make(map[int]bool)
	endpoint := func() string {
		for {
			port := freePort(t)
			if !ports[port] {
				ports[port] = true
				return fmt.Sprintf("http://127.0.0.1:%d", port)
			}
		}
	}
	for i := range 3 {
		clients = append(clients, endpoint())
		peers = append(peers, endpoint())
		members = append(members, fmt.Sprintf("ha%d=%s", i, peers[i]))
	}
	var commands []*exec.Cmd
	stopped := make(map[int]bool)
	stop := func(i int) {
		if !stopped[i] {
			_ = commands[i].Process.Kill()
			_ = commands[i].Wait()
			stopped[i] = true
		}
	}
	t.Cleanup(func() {
		for i := range commands {
			stop(i)
		}
	})
	token := fmt.Sprintf("roost-ha-%d", time.Now().UnixNano())
	for i := range 3 {
		log, err := os.Create(filepath.Join(root, fmt.Sprintf("node%d.log", i)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = log.Close() })
		cmd := exec.Command(binary, "--name", fmt.Sprintf("ha%d", i), "--data-dir", filepath.Join(root, fmt.Sprintf("node%d", i)),
			"--listen-client-urls", clients[i], "--advertise-client-urls", clients[i],
			"--listen-peer-urls", peers[i], "--initial-advertise-peer-urls", peers[i],
			"--initial-cluster", strings.Join(members, ","), "--initial-cluster-token", token, "--log-level", "error")
		cmd.Env = append(os.Environ(), "GOMAXPROCS=2")
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	asm, err := Assemble(&fetcd.Config{Endpoints: clients, DialTimeout: 2 * time.Second, ServicePrefix: "/ha/service/", LeaseTTL: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := asm.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	write := func(value string) {
		t.Helper()
		for {
			op, done := context.WithTimeout(ctx, time.Second)
			err := asm.Client.Put(op, "/ha/probe", value)
			done()
			if err == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("cluster did not accept write %s: %v", value, err)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	write("ready")
	if err := asm.Start(ctx, &fetcd.ServiceInfo{ServiceType: "game", Sid: 7, Addr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	watch := asm.Client.Watch(ctx, "/ha/probe", fetcd.WatchOption{CreatedNotify: true})
	defer watch.Close()
	select {
	case <-watch.(fetcd.IWatcherReady).Ready():
	case <-ctx.Done():
		t.Fatal("watch was not acknowledged")
	}
	if err := watch.(fetcd.IWatcherError).WatchError(); err != nil {
		t.Fatal(err)
	}
	leader := asm.Election.NewElection("/ha/election")
	if err := leader.Campaign(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	before, ok := leader.(fetcd.IFencedElection).Fence()
	if !ok {
		t.Fatal("election did not acquire a fence")
	}
	killed := -1
	for i, endpoint := range clients {
		status, err := asm.Client.Raw().Status(ctx, endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if status.Leader != 0 && status.Header.MemberId == status.Leader {
			killed = i
			break
		}
	}
	if killed < 0 {
		t.Fatal("could not identify the actual Raft leader")
	}
	stop(killed)
	t.Logf("killed actual Raft leader node %d; two members remain", killed)
	write("after-leader-loss")
	for {
		select {
		case event, open := <-watch.EventChan():
			if !open {
				t.Fatal("watch closed during leader failover")
			}
			if event.KV != nil && event.KV.Value == "after-leader-loss" {
				goto observed
			}
		case <-ctx.Done():
			t.Fatal("watch did not resume after failover")
		}
	}
observed:
	// 跨过注册租约的原TTL，避免只证明旧键在短窗口内尚未过期。
	select {
	case <-time.After(6 * time.Second):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := asm.Client.Get(ctx, "/ha/service/game/7"); err != nil {
		t.Fatalf("discovery did not survive past its original TTL: %v", err)
	}
	if err := leader.Resign(ctx); err != nil {
		t.Fatal(err)
	}
	next := asm.Election.NewElection("/ha/election")
	if err := next.Campaign(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	after, ok := next.(fetcd.IFencedElection).Fence()
	if !ok || after <= before {
		t.Fatalf("leadership fence did not advance: before=%d after=%d held=%v", before, after, ok)
	}
	if err := next.Resign(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("watch resumed, discovery survived TTL, election advanced fence and resigned")
}
