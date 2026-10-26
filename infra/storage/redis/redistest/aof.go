package redistest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// AOFAddrEnv 指向一个满足权威 Redis 部署契约（单主、appendonly yes、appendfsync always、noeviction）的实例，通常由
// scripts/it-redis-aof.sh start 导出。设置了它，AOFAddr 直接用它；没设置时为每个用例在临时目录里起一个私有 redis-server。
const AOFAddrEnv = "ROOST_SAGA_IT_REDIS_ADDR"

// AOFAddr 返回一个可用作权威 Redis 的地址：优先取 ROOST_SAGA_IT_REDIS_ADDR；否则本机有 redis-server 时起一个私有实例
// （t 结束时停掉并删除目录）；两者都没有时 Skip。共用的实例上，用例要用唯一的键前缀并登记清理（Prefix / Cleanup）。
func AOFAddr(t testing.TB) string {
	t.Helper()
	if addr := os.Getenv(AOFAddrEnv); addr != "" {
		return addr
	}
	return StartAOF(t).Addr
}

// AOFServer 是测试私有的 redis-server 进程，故障注入用例（kill -9、改配置）只在它上面做。
type AOFServer struct {
	Addr string
	dir  string
	port int
	cmd  *exec.Cmd
	t    testing.TB
}

// StartAOF 在临时目录起一个私有 redis-server：AOF always、noeviction、不写 RDB；本机没有 redis-server 时 Skip。
// t 结束时停进程（数据目录随 t.TempDir 删除）。
func StartAOF(t testing.TB) *AOFServer {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skipf("%s is unset and redis-server is not installed; run scripts/it-redis-aof.sh start", AOFAddrEnv)
	}
	server := &AOFServer{dir: t.TempDir(), t: t}
	server.port = freePort(t)
	server.Addr = "127.0.0.1:" + strconv.Itoa(server.port)
	server.start(binary)
	t.Cleanup(server.stop)
	return server
}

func (s *AOFServer) start(binary string) {
	s.t.Helper()
	s.cmd = exec.Command(binary, "--port", strconv.Itoa(s.port), "--bind", "127.0.0.1", "--dir", s.dir,
		"--appendonly", "yes", "--appendfsync", "always", "--no-appendfsync-on-rewrite", "no",
		"--maxmemory", "512mb", "--maxmemory-policy", "noeviction", "--save", "", "--logfile", filepath.Join(s.dir, "redis.log"))
	if err := s.cmd.Start(); err != nil {
		s.t.Fatalf("redistest: start redis-server: %v", err)
	}
	client := goredis.NewClient(&goredis.Options{Addr: s.Addr, MaxRetries: -1})
	defer client.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := client.Ping(ctx).Err()
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(s.dir, "redis.log"))
			s.t.Fatalf("redistest: redis-server on %s did not answer: %v\n%s", s.Addr, err, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Kill9 用 SIGKILL 杀掉进程并等它退出：模拟 Redis 崩溃，AOF 里已 fsync 的写保留，未回复的脚本由加载时的截断规则处理。
func (s *AOFServer) Kill9() {
	s.t.Helper()
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGKILL)
	_ = s.cmd.Wait()
	s.cmd = nil
}

// Restart 在同一端口、同一数据目录上重新起进程（加载 AOF），等它能 PING 通。
func (s *AOFServer) Restart() {
	s.t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		s.t.Fatal(err)
	}
	s.start(binary)
}

func (s *AOFServer) stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
}

func freePort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(fmt.Errorf("redistest: pick a port: %w", err))
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
