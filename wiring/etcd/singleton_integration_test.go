//go:build integration

package etcd

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// SingletonStore 在真实 etcd 上的语义（docs/maintenance/FEATURE-2026-11-16-singleton-etcd.md §8）：抢锁、续锁、换值、释放、
// ttl = 0、回复丢失后重做、租约过期后续锁失败、续租失败不算续上、Get 多键与缺键、ctx 截止时间。每个用例自己起一个单节点
// etcd（PATH 上的 etcd，用例结束即杀掉），不碰共享环境。
//
//	go test -tags integration -run Singleton ./wiring/etcd/

// lockTTL 是用例的锁 ttl：单节点 etcd 的最小租约约 2s（1.5 × 选举超时向上取整），更短的会被服务端放大。
const lockTTL = 2 * time.Second

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// startSingletonEtcd 在空闲端口上起一个单节点 etcd，返回它的客户端地址与进程（用例可以 SIGSTOP 它）。
func startSingletonEtcd(t *testing.T) (string, *os.Process) {
	t.Helper()
	binary, err := exec.LookPath("etcd")
	if err != nil {
		t.Skip("etcd binary not on PATH; the real-etcd singleton store tests are skipped (install etcd to run them)")
	}
	clientURL := fmt.Sprintf("http://127.0.0.1:%d", freeTCPPort(t))
	peerURL := fmt.Sprintf("http://127.0.0.1:%d", freeTCPPort(t))
	cmd := exec.Command(binary, "--name", "roost-singleton-it", "--data-dir", t.TempDir(),
		"--listen-client-urls", clientURL, "--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL, "--initial-advertise-peer-urls", peerURL,
		"--initial-cluster", "roost-singleton-it="+peerURL, "--log-level", "error")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGCONT)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	endpoint := clientURL[len("http://"):]
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := cli.Get(ctx, "/")
		cancel()
		if err == nil {
			return endpoint, cmd.Process
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("etcd did not start serving")
	return "", nil
}

func singletonConfig(endpoint, prefix string) *viper.Viper {
	cfg := viper.New()
	cfg.Set("etcd.endpoints", []string{endpoint})
	cfg.Set("singleton.enabled", true)
	cfg.Set("singleton.key_prefix", prefix)
	cfg.Set("singleton.ttl", lockTTL.String())
	return cfg
}

// openSingletonStore 经正式的 opener 打开，返回具体类型（用例要看持有的租约、换掉续租）。
func openSingletonStore(t *testing.T, endpoint string) *singletonStore {
	t.Helper()
	store, err := SingletonStore(singletonConfig(endpoint, "/roost-it/singleton"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.(*singletonStore)
}

func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func token() []byte { return []byte(rand.Text() + "|host|1|0") }

// leaseCount 是集群里现有的租约数：撤销没做到时会多出来。
func leaseCount(t *testing.T, s *singletonStore) int {
	t.Helper()
	resp, err := s.lock.Leases(callCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	return len(resp.Leases)
}

func mustCAS(t *testing.T, s app.SingletonStore, key string, expected, next []byte, ttl time.Duration, wantApplied bool, wantCurrent []byte) {
	t.Helper()
	applied, current, err := s.CompareAndSet(callCtx(t), key, expected, next, ttl)
	if err != nil {
		t.Fatalf("CompareAndSet(%s, %q -> %q, %v): %v", key, expected, next, ttl, err)
	}
	if applied != wantApplied || !bytes.Equal(current, wantCurrent) {
		t.Fatalf("CompareAndSet(%s, %q -> %q, %v) = %v, %q; want %v, %q", key, expected, next, ttl, applied, current, wantApplied, wantCurrent)
	}
}

func getOne(t *testing.T, s app.SingletonStore, key string) []byte {
	t.Helper()
	values, err := s.Get(callCtx(t), []string{key})
	if err != nil {
		t.Fatal(err)
	}
	return values[0]
}

func TestSingletonStoreAcquireRenewRelease(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	s, other := openSingletonStore(t, endpoint), openSingletonStore(t, endpoint)
	key := "/roost-it/singleton:game:1"
	mine, theirs := token(), token()

	mustCAS(t, s, key, nil, mine, lockTTL, true, nil)
	mustCAS(t, other, key, nil, theirs, lockTTL, false, mine) // 别人持有：拿到当前值
	mustCAS(t, s, key, mine, mine, lockTTL, true, nil)        // 续锁
	mustCAS(t, other, key, theirs, theirs, lockTTL, false, mine)
	if got := getOne(t, other, key); !bytes.Equal(got, mine) {
		t.Fatalf("Get = %q, want the holder's value %q", got, mine)
	}
	// 续锁真的续了租约：过了一个 ttl 仍在。
	for range 3 {
		time.Sleep(lockTTL / 2)
		mustCAS(t, s, key, mine, mine, lockTTL, true, nil)
	}
	if applied, err := other.CompareAndDelete(callCtx(t), key, theirs); err != nil || applied {
		t.Fatalf("CompareAndDelete with another value = %v, %v; want false, nil", applied, err)
	}
	if applied, err := s.CompareAndDelete(callCtx(t), key, mine); err != nil || !applied {
		t.Fatalf("CompareAndDelete with the holder's value = %v, %v; want true, nil", applied, err)
	}
	if got := getOne(t, s, key); got != nil {
		t.Fatalf("Get after release = %q, want nil", got)
	}
	if n := leaseCount(t, s); n != 0 {
		t.Fatalf("%d lease(s) left after release, want the released lease and the refused ones revoked", n)
	}
	mustCAS(t, other, key, nil, theirs, lockTTL, true, nil) // 释放后别人立刻能拿
}

// 抢锁成功但回复丢失：App 用同一个值重做 CAS(nil, v)，拿到 applied=false、current=v 后认领。store 要认领键挂的租约，
// 之后的续锁续的是它，重做时多申请的租约要撤销。
func TestSingletonStoreAcquireRetriedAfterALostReplyIsClaimed(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	s := openSingletonStore(t, endpoint)
	key := "/roost-it/singleton:game:2"
	mine := token()
	mustCAS(t, s, key, nil, mine, lockTTL, true, nil)
	s.mu.Lock()
	first := s.held[key]
	delete(s.held, key) // 回复丢失：store 没记下这次写入
	s.mu.Unlock()

	mustCAS(t, s, key, nil, mine, lockTTL, false, mine)
	if held, _ := s.heldLease(key); held.id != first.id {
		t.Fatalf("after the retry the store holds lease %x, want the one the key is on (%x)", held.id, first.id)
	}
	if n := leaseCount(t, s); n != 1 {
		t.Fatalf("%d leases after the retry, want 1: the lease granted for the retry must be revoked", n)
	}
	for range 3 {
		time.Sleep(lockTTL / 2)
		mustCAS(t, s, key, mine, mine, lockTTL, true, nil)
	}
}

// 租约过期（进程卡住超过 ttl）后键随之删除：续锁返回 applied=false 与键现在的值，别人随后可以拿到锁。
func TestSingletonStoreRenewAfterTheLeaseExpiredIsNotHeld(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	s, other := openSingletonStore(t, endpoint), openSingletonStore(t, endpoint)
	key := "/roost-it/singleton:game:3"
	mine, theirs := token(), token()
	mustCAS(t, s, key, nil, mine, lockTTL, true, nil)
	time.Sleep(lockTTL + 2*time.Second)
	mustCAS(t, s, key, mine, mine, lockTTL, false, nil)

	mustCAS(t, s, key, nil, mine, lockTTL, true, nil)
	time.Sleep(lockTTL + 2*time.Second)
	mustCAS(t, other, key, nil, theirs, lockTTL, true, nil)
	mustCAS(t, s, key, mine, mine, lockTTL, false, theirs)
	if got := getOne(t, s, key); !bytes.Equal(got, theirs) {
		t.Fatalf("Get = %q, want the new holder's value", got)
	}
}

// failingKeepAlive 让 KeepAliveOnce 返回一个不是“租约不存在”的错误（网络抖动、leader 切换中），其余照常。
type failingKeepAlive struct {
	clientv3.Lease
	err error
}

func (f failingKeepAlive) KeepAliveOnce(context.Context, clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
	return nil, f.err
}

// 续租失败就不算续上：键此刻仍挂在本进程的租约上、核对事务会通过，但租约没有延长，报 applied=true 会让 App 把有效
// 窗口往后推一个 ttl，而服务端的键会在原来的时刻过期。必须原样返回错误（App 按 Unknown 处理）。
// 去掉 renew 里“KeepAliveOnce 失败即返回”的判断，这个用例变红。
func TestSingletonStoreRenewalWhoseKeepAliveFailedIsNotApplied(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	s := openSingletonStore(t, endpoint)
	key := "/roost-it/singleton:game:4"
	mine := token()
	mustCAS(t, s, key, nil, mine, lockTTL, true, nil)
	injected := errors.New("injected keepalive failure")
	s.lease = failingKeepAlive{Lease: s.lock, err: injected}
	applied, current, err := s.CompareAndSet(callCtx(t), key, mine, mine, lockTTL)
	if !errors.Is(err, injected) || applied {
		t.Fatalf("renewal with a failed keep-alive = %v, %q, %v; want the keep-alive error and applied=false", applied, current, err)
	}
	if got := getOne(t, s, key); !bytes.Equal(got, mine) {
		t.Fatalf("Get = %q: the failed renewal must not touch the key", got)
	}
}

// 换值、换 ttl：键挂到新租约上，旧租约撤销；不匹配时不写，返回当前值。
func TestSingletonStoreChangesValueAndTTLOnANewLease(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	s := openSingletonStore(t, endpoint)
	key := "/roost-it/singleton:game:5"
	first, second := token(), token()
	mustCAS(t, s, key, nil, first, lockTTL, true, nil)
	before, _ := s.heldLease(key)
	mustCAS(t, s, key, first, second, lockTTL, true, nil)
	after, _ := s.heldLease(key)
	if after.id == before.id || leaseCount(t, s) != 1 {
		t.Fatalf("change value: lease %x -> %x, %d lease(s); want a new lease and the old one revoked", before.id, after.id, leaseCount(t, s))
	}
	mustCAS(t, s, key, first, token(), lockTTL, false, second)

	mustCAS(t, s, key, second, second, 2*lockTTL, true, nil)
	held, _ := s.heldLease(key)
	ttl, err := s.lock.TimeToLive(callCtx(t), held.id)
	if err != nil || ttl.GrantedTTL != int64(2*lockTTL/time.Second) {
		t.Fatalf("change ttl: granted %v, %v; want %v", ttl, err, 2*lockTTL)
	}
	if n := leaseCount(t, s); n != 1 {
		t.Fatalf("%d leases after the ttl change, want 1", n)
	}
	mustCAS(t, s, key, second, second, 2*lockTTL, true, nil) // 之后按新 ttl 续
}

// ttl = 0（业务时间高水位）：不挂租约，不过期；expected 为 nil 比较“键不存在”，否则比较值。
func TestSingletonStoreZeroTTLWritesAKeyWithoutALease(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	s := openSingletonStore(t, endpoint)
	key := "/roost-it/singleton:business_time"
	a, b, c := []byte("1|a"), []byte("2|b"), []byte("3|c")
	mustCAS(t, s, key, nil, a, 0, true, nil)
	mustCAS(t, s, key, nil, b, 0, false, a)
	mustCAS(t, s, key, a, b, 0, true, nil)
	mustCAS(t, s, key, a, c, 0, false, b)
	resp, err := s.lock.Get(callCtx(t), key)
	if err != nil || len(resp.Kvs) != 1 || resp.Kvs[0].Lease != 0 {
		t.Fatalf("business_time key = %v, %v; want one key without a lease", resp, err)
	}
	if n := leaseCount(t, s); n != 0 {
		t.Fatalf("%d lease(s) granted for a ttl = 0 key", n)
	}
}

// Get 按 keys 的顺序返回，缺的键为 nil；一次最多 SingletonLiveMaxSIDs 个键，超过一个事务的读操作上限时分批。
func TestSingletonStoreGetReadsEveryKeyInOrder(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	s := openSingletonStore(t, endpoint)
	keys := make([]string, app.SingletonLiveMaxSIDs)
	want := make([][]byte, len(keys))
	for i := range keys {
		keys[i] = "/roost-it/singleton:game:" + strconv.Itoa(1000+i)
		if i%3 == 0 {
			want[i] = []byte("holder-" + strconv.Itoa(i))
			mustCAS(t, s, keys[i], nil, want[i], 0, true, nil)
		}
	}
	got, err := s.Get(callCtx(t), keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Fatalf("Get returned %d values for %d keys", len(got), len(keys))
	}
	for i := range keys {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("Get[%d] (%s) = %q, want %q", i, keys[i], got[i], want[i])
		}
	}
	if values, err := s.Get(callCtx(t), nil); err != nil || len(values) != 0 {
		t.Fatalf("Get(nil) = %v, %v", values, err)
	}
}

// 每个方法都遵守 ctx 的截止时间：etcd 卡住（SIGSTOP）时到期即返回错误，不能继续阻塞——App 的“续期失败时 Lost 不晚于
// validUntil 判定”依赖它。
func TestSingletonStoreHonoursTheCallDeadline(t *testing.T) {
	endpoint, process := startSingletonEtcd(t)
	s := openSingletonStore(t, endpoint)
	key := "/roost-it/singleton:game:6"
	mine := token()
	mustCAS(t, s, key, nil, mine, 10*time.Second, true, nil)
	if err := process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer process.Signal(syscall.SIGCONT)
	const deadline = 500 * time.Millisecond
	for name, call := range map[string]func(context.Context) error{
		"renew": func(ctx context.Context) error {
			_, _, err := s.CompareAndSet(ctx, key, mine, mine, 10*time.Second)
			return err
		},
		"acquire": func(ctx context.Context) error {
			_, _, err := s.CompareAndSet(ctx, key+"x", nil, mine, 10*time.Second)
			return err
		},
		"zero":   func(ctx context.Context) error { _, _, err := s.CompareAndSet(ctx, key+"t", nil, mine, 0); return err },
		"get":    func(ctx context.Context) error { _, err := s.Get(ctx, []string{key}); return err },
		"delete": func(ctx context.Context) error { _, err := s.CompareAndDelete(ctx, key, mine); return err },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		start := time.Now()
		err := call(ctx)
		cancel()
		if elapsed := time.Since(start); err == nil || elapsed > deadline+time.Second {
			t.Errorf("%s against a stopped etcd = %v after %v; want an error by the %v deadline", name, err, elapsed, deadline)
		}
	}
}

// 业务时间只许前进在真实 etcd 上：App 经 kitetcd.SingletonStore 读写部署级高水位 <singleton.key_prefix>:business_time
// （不挂租约的键）。先以 +24h 偏移启动一次（写下高水位），再把偏移改回 0 启动：被拒绝，键原样保留。
func TestBusinessTimeHighWaterMarkOnRealEtcd(t *testing.T) {
	endpoint, _ := startSingletonEtcd(t)
	prefix := "/roost-it-business-time/singleton"
	key := prefix + ":business_time"
	if err := runBusinessTimeApp(t, endpoint, prefix, "24h", true); err != nil {
		t.Fatalf("first run with +24h: %v", err)
	}
	s := openSingletonStore(t, endpoint)
	mark := getOne(t, s, key)
	if mark == nil {
		t.Fatal("no high-water mark after the +24h run")
	}
	unixMs, err := strconv.ParseInt(string(mark[:13]), 10, 64)
	if err != nil || time.UnixMilli(unixMs).Before(time.Now().Add(24*time.Hour-time.Minute)) {
		t.Fatalf("high-water mark %q, want real time + 24h", mark)
	}
	if err := runBusinessTimeApp(t, endpoint, prefix, "0s", false); !errors.Is(err, app.ErrBusinessTimeMovedBack) {
		t.Fatalf("second run with the offset back at 0: %v, want ErrBusinessTimeMovedBack", err)
	}
	if after := getOne(t, s, key); !bytes.Equal(after, mark) {
		t.Fatalf("high-water mark after the refusal = %q; want %q untouched", after, mark)
	}
}

type businessTimeProbe struct{ served chan struct{} }

func (p *businessTimeProbe) Name() app.ServiceName    { return "game" }
func (p *businessTimeProbe) Init(*app.Registry) error { return nil }
func (p *businessTimeProbe) Serve(ctx context.Context) error {
	close(p.served)
	<-ctx.Done()
	return nil
}
func (p *businessTimeProbe) Shutdown(context.Context) error { return nil }

func runBusinessTimeApp(t *testing.T, endpoint, prefix, offset string, stopAfterServe bool) error {
	t.Helper()
	config := fmt.Sprintf(`sid: 1
log:
  file: false
  stdout: false
etcd:
  endpoints: [%s]
singleton:
  enabled: true
  key_prefix: %s
time:
  logic_offset: %s
`, endpoint, prefix, offset)
	path := filepath.Join(t.TempDir(), "config.game.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := &businessTimeProbe{served: make(chan struct{})}
	a := app.New("roost-test", "0.0.0").Singleton(SingletonStore)
	a.RegisterServer("game", probe)
	a.RootCmd().SetArgs([]string{"game", "--config", path})
	result := make(chan error, 1)
	go func() { result <- a.Execute() }()
	select {
	case <-probe.served:
		if !stopAfterServe {
			t.Fatal("the service started serving")
		}
		process, _ := os.FindProcess(os.Getpid())
		_ = process.Signal(os.Interrupt)
		return <-result
	case err := <-result:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("run neither served nor returned")
		return nil
	}
}
