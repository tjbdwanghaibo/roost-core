package cellworld

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

// 真实 NATS：测试自己在私有端口起一个单节点 nats-server（core NATS，不开 JetStream，不写数据目录之外的文件），
// 用完杀掉；PATH 里没有 nats-server 或 -short 时跳过。不连共享隔离环境。

func startNATSServer(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("real nats-server suite skipped in -short")
	}
	bin, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server executable unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	_ = listener.Close()
	dir := t.TempDir()
	cmd := exec.Command(bin, "-a", "127.0.0.1", "-p", fmt.Sprint(addr.Port), "-l", filepath.Join(dir, "nats.log"), "-P", filepath.Join(dir, "nats.pid"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(dir, "nats.log"))
			t.Fatalf("nats-server did not start: %v\n%s", err, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "nats://" + addr.String()
}

func dialNATS(t testing.TB, url string) *driver.Client {
	t.Helper()
	client, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{IgnoreDiscoveredServers: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

// natsSpec：每个 Cell 与 Base 各用一条独立的 NATS 连接（与多进程部署一致，只是共享一个 OS 进程）。
func natsSpec(t testing.TB, cols, rows int32) worldSpec {
	url := startNATSServer(t)
	return worldSpec{grid: worldGrid(t, cols, rows), transport: func(t testing.TB, _ string) Transport {
		return NATSTransport(dialNATS(t, url))
	}}
}

// 真实 NATS 上的视野约定比进程内替身多留一拍。
const (
	natsMaxMiss = 2
	natsMaxGap  = 4*worldPeriod + 50*time.Millisecond
)

func TestNATSWorldStraightCrossing(t *testing.T) {
	scenarioStraightCrossing(t, startWorld(t, natsSpec(t, 2, 1)), natsMaxMiss, natsMaxGap)
}

func TestNATSWorldJitterAtBoundary(t *testing.T) {
	scenarioJitter(t, startWorld(t, natsSpec(t, 2, 1)), natsMaxMiss, natsMaxGap)
}

func TestNATSWorldDiagonalThroughFourCellCorner(t *testing.T) {
	scenarioDiagonal(t, startWorld(t, natsSpec(t, 2, 2)), natsMaxMiss, natsMaxGap)
}

func TestNATSWorldAttackAcrossBoundary(t *testing.T) {
	scenarioAttackAcrossAndDuringMigration(t, startWorld(t, natsSpec(t, 2, 1)), false)
}
