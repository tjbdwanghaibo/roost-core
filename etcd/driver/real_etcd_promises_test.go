//go:build integration

package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// startEtcd launches a single-node etcd from PATH on free ports and returns
// its client endpoint. The process is killed with the test.
func startEtcd(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("etcd")
	if err != nil {
		t.Skip("etcd binary not on PATH; the real-etcd guard test is skipped (install etcd to run it)")
	}
	clientURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	peerURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	cmd := exec.Command(binary, "--name", "roost-it", "--data-dir", t.TempDir(),
		"--listen-client-urls", clientURL, "--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL, "--initial-advertise-peer-urls", peerURL,
		"--initial-cluster", "roost-it="+peerURL, "--log-level", "error")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	endpoint := clientURL[len("http://"):]
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", endpoint, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return endpoint
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("etcd did not start listening")
	return ""
}

// U-0153 · C2 · client.go:46（U-0129 留真机）：对一个不存在的键 Get 必须是 ErrKeyNotFound，而不是
// 一个空 KV；写入后再读得到值。`*clientv3.Client` 无法替身，这里对一个真实的单节点 etcd 验证。
func TestRealEtcdGetOfAMissingKeyIsNotFound(t *testing.T) {
	endpoint := startEtcd(t)
	client, err := NewClient(&fetcd.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Get(ctx, "/roost/it/missing"); !errors.Is(err, fetcd.ErrKeyNotFound) {
		t.Fatalf("Get of a missing key = %v, want ErrKeyNotFound", err)
	}
	if err := client.Put(ctx, "/roost/it/present", "v1"); err != nil {
		t.Fatal(err)
	}
	kv, err := client.Get(ctx, "/roost/it/present")
	if err != nil || kv == nil || kv.Value != "v1" {
		t.Fatalf("Get of a written key = (%+v, %v)", kv, err)
	}
}
