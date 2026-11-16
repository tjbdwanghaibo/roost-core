package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// singletonOpenTimeout 是打开 SingletonStore 时首次读的超时。
const singletonOpenTimeout = 5 * time.Second

// singletonGetBatch 是 Get 一个事务里最多放的读操作数：etcd 服务端 --max-txn-ops 的缺省值 128。
// SingletonLiveMaxSIDs（200）个键要分两个事务读；两个事务之间不是同一版本，Live 只要求每个键各自是最新的。
const singletonGetBatch = 128

// SingletonStore 是 App 单实例锁与业务时间高水位的 etcd 后端（app.SingletonOpener），在 bootstrap 里这样安装：
//
//	app.New(...).Singleton(kitetcd.SingletonStore)
//
// 它在任何 Mod Init 之前被调用，只读同一份配置的 etcd.*（地址与凭据，`_env` 规则与 etcd Mod 相同），自己建两个独立的
// clientv3.Client，不复用 etcd Mod 的连接：一个只给抢锁、续锁、换值、释放（锁操作），一个只给 Live 查询的 Get。
// 二者不共用 gRPC 连接与流：etcd 变慢时并发的 Live 查询可能排满一条连接，续锁若排在它们后面就会超时（Unknown），
// 一直如此会被误判丢锁、进程 fail-stop。
//
// 必须显式写 etcd.endpoints 或 etcd.endpoints_env：不沿用 etcd Mod 声明的 localhost:2379 缺省，否则一个忘了配 etcd 的
// 服务会对着本机的 etcd 加锁。etcd 租约以整秒计，singleton.ttl 不是整秒时直接拒绝，不偷偷取整。
//
// 锁键与发现共用一个 etcd 集群（同一组 etcd.endpoints）；一个工程里所有开锁或读锁的服务必须用同一组 endpoints 与同一个
// singleton.key_prefix（Live、Gate 与 cellworldhost 的 IdentityChecker 要读别的进程的锁键），doctor 的
// singleton:etcd-consistent 检查这一条。etcd 不可用时开锁进程在 ttl − guard 后 fail-stop，生产要求 etcd 三节点或更多。
func SingletonStore(cfg *viper.Viper) (app.SingletonStore, error) {
	if cfg == nil {
		return nil, errors.New("kitetcd: singleton store: nil config")
	}
	var settings config
	present, err := app.LoadConfigWithPresence(cfg, &settings)
	if err != nil {
		return nil, fmt.Errorf("kitetcd: singleton store: %w", err)
	}
	if !present["etcd.endpoints"] && strings.TrimSpace(settings.EndpointsEnv) == "" {
		return nil, errors.New("kitetcd: singleton store: etcd.endpoints (or etcd.endpoints_env) is required: the singleton lock does not fall back to localhost:2379")
	}
	env := mods.ReadEnv()
	endpoints, username, password := settings.endpoint(env)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("kitetcd: singleton store: %w", err)
	}
	if len(endpoints) == 0 {
		return nil, errors.New("kitetcd: singleton store: etcd.endpoints is empty")
	}
	if (username == "") != (password == "") {
		return nil, errors.New("kitetcd: singleton store: config: etcd.username and etcd.password must be set together (directly or through their _env keys)")
	}
	var lock singletonStoreConfig
	if err := app.LoadConfig(cfg, &lock); err != nil {
		return nil, fmt.Errorf("kitetcd: singleton store: %w", err)
	}
	// singleton.ttl 由 App 的声明检查过（带单位的正时长）；这里只加 etcd 自己的限制：租约以整秒计。
	if lock.Singleton.Enabled {
		if err := checkLeaseTTL(lock.Singleton.TTL); err != nil {
			return nil, fmt.Errorf("kitetcd: singleton store: singleton.ttl: %w", err)
		}
	}
	store, err := newSingletonStore(endpoints, username, password)
	if err != nil {
		return nil, fmt.Errorf("kitetcd: singleton store: %w", err)
	}
	// 打开时读一次：地址、凭据或权限写错，在启动时就点名，而不是在第一次抢锁时才超时。读锁前缀下的计数，
	// 开了 RBAC 的集群上这正是锁要用的权限范围。
	probe := strings.TrimSpace(lock.Singleton.KeyPrefix)
	if probe == "" {
		probe = "/"
	}
	ctx, cancel := context.WithTimeout(context.Background(), singletonOpenTimeout)
	defer cancel()
	if _, err := store.kv.Get(ctx, probe, clientv3.WithPrefix(), clientv3.WithCountOnly()); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("kitetcd: singleton store: first read: %w", err)
	}
	return store, nil
}

// singletonStoreConfig 是 SingletonStore 要读的 singleton.*。这几个键的声明与检查归 App（framework/app 的
// singletonConfig），打开之前已检查过；这里为读取再声明一次，enabled 共用 app.SingletonSwitch，ttl 的缺省与 App 相同。
type singletonStoreConfig struct {
	Singleton struct {
		app.SingletonSwitch
		KeyPrefix string        `config:"key_prefix"`
		TTL       time.Duration `config:"ttl" default:"15s" min:"1ns"`
	} `config:"singleton"`
}

// checkLeaseTTL 拒绝 etcd 租约表示不了的时长（负数或不是整秒）。0 表示不挂租约。
func checkLeaseTTL(ttl time.Duration) error {
	if ttl < 0 || ttl%time.Second != 0 {
		return fmt.Errorf("%s is not a whole number of seconds: etcd leases are granted in seconds", ttl)
	}
	return nil
}

// newSingletonStore 建锁操作与 Live 两个客户端（clientv3.New 不阻塞拨号）。
func newSingletonStore(endpoints []string, username, password string) (*singletonStore, error) {
	client := func() (*clientv3.Client, error) {
		return clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second, Username: username, Password: password})
	}
	lock, err := client()
	if err != nil {
		return nil, err
	}
	live, err := client()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &singletonStore{lock: lock, live: live, kv: lock, lease: lock, held: map[string]heldLease{}}, nil
}

// singletonStore 把 app.SingletonStore 的两个比较写入落到 etcd 事务与租约上，语义见各方法。
//
// 键的过期由租约承担：抢锁时申请 ttl 秒的租约，键挂在它上面；续锁续这个租约。held 记着本进程每个锁键当前挂的租约
// 与申请时的 ttl：同一个键只有 App 的单个写者顺序调用（启动获取、续期、最后的 Release），业务时间高水位是另一个键、
// 不挂租约，所以 mu 只保护这张表，不串行化 etcd 调用。
//
// 时间窗口只会更保守：租约由 leader 从收到 Grant / KeepAlive 时开始计时，App 从发出请求之前开始计时，服务端的过期
// 不早于 App 以为的过期；etcd 换 leader 时把全部租约续满一次，只会让旧锁更晚过期，不影响互斥（代价是新进程可能多等一个 ttl）。
type singletonStore struct {
	lock *clientv3.Client // 锁操作
	live *clientv3.Client // Get（Live、IdentityChecker）

	// kv 与 lease 缺省就是 lock；测试替换 lease 来注入续租失败。
	kv    clientv3.KV
	lease clientv3.Lease

	mu   sync.Mutex
	held map[string]heldLease

	closeOnce sync.Once
}

// heldLease 是本进程某个锁键当前挂的租约与申请它时的 ttl。
type heldLease struct {
	id  clientv3.LeaseID
	ttl time.Duration
}

// CompareAndSet 按 expected / next / ttl 分四种情况：
//
//   - ttl = 0（业务时间高水位）：不挂租约。expected 为 nil 比较“键不存在”（CreateRevision == 0），否则比较值。
//   - 抢锁（expected 为 nil）：先 Grant(ttl)，再一个事务 If(CreateRevision == 0) Then(Put 挂新租约) Else(Get)；没写成就撤销新租约。
//     若 Else 读到的正是 next（上一次抢锁已落地、回复丢失），认领它挂的租约，返回 applied=false、current=next，由 App 认领。
//   - 续锁（expected == next，且本进程为这个键持有一个以同一 ttl 申请的租约）：先 KeepAliveOnce，失败即不算续上——
//     租约已不存在时返回 applied=false 与键的当前值，其他错误原样返回（Unknown）；续上后再用一个事务核对
//     Value == expected 且 Lease == 持有的租约，作兜底（令牌唯一，租约续上就说明键还挂在它上面）。
//   - 其他（换值、换 ttl，或没有记录可续）：申请新租约，If(Value == expected) Then(Put 挂新租约) Else(Get)，
//     写成后撤销旧租约（键已挂到新租约上，不受影响）。
//
// 每次调用只用传入的 ctx：clientv3 的内部重试（KeepAliveOnce 在非终止错误上重试、只读请求的重试）都受它限定。
func (s *singletonStore) CompareAndSet(ctx context.Context, key string, expected, next []byte, ttl time.Duration) (bool, []byte, error) {
	if err := checkLeaseTTL(ttl); err != nil {
		return false, nil, fmt.Errorf("kitetcd: singleton store: ttl %w", err)
	}
	if next == nil {
		return false, nil, errors.New("kitetcd: singleton store: next value is nil")
	}
	held, holding := s.heldLease(key)
	switch {
	case ttl == 0:
		return s.putWithoutLease(ctx, key, expected, next, held, holding)
	case expected != nil && bytes.Equal(expected, next) && holding && held.ttl == ttl:
		return s.renew(ctx, key, expected, held)
	default:
		return s.putWithNewLease(ctx, key, expected, next, ttl, held, holding)
	}
}

// renew 续本进程持有的租约，再核对键仍是本进程的值、挂在这个租约上。
func (s *singletonStore) renew(ctx context.Context, key string, expected []byte, held heldLease) (bool, []byte, error) {
	if _, err := s.lease.KeepAliveOnce(ctx, held.id); err != nil {
		if !errors.Is(err, rpctypes.ErrLeaseNotFound) {
			return false, nil, err
		}
		// 租约已过期或被撤销，挂在上面的键随之删除：锁已不是本进程的，交出键现在的值（别人可能已经拿到）。
		s.forget(key, held.id)
		resp, err := s.kv.Get(ctx, key)
		if err != nil {
			return false, nil, err
		}
		return false, firstValue(resp.Kvs), nil
	}
	resp, err := s.kv.Txn(ctx).If(
		clientv3.Compare(clientv3.Value(key), "=", string(expected)),
		clientv3.Compare(clientv3.LeaseValue(key), "=", held.id),
	).Else(clientv3.OpGet(key)).Commit()
	if err != nil {
		return false, nil, err
	}
	if resp.Succeeded {
		return true, nil, nil
	}
	// 租约还在、键却不是本进程的值或不挂在这个租约上：不再续它，尽力撤销（键不在它上面，撤销不影响别人）。
	s.forget(key, held.id)
	s.revoke(ctx, held.id)
	return false, rangeValue(resp, 0), nil
}

// putWithNewLease 是抢锁与换值 / 换 ttl：键挂到新申请的租约上。
func (s *singletonStore) putWithNewLease(ctx context.Context, key string, expected, next []byte, ttl time.Duration, old heldLease, hadOld bool) (bool, []byte, error) {
	grant, err := s.lease.Grant(ctx, int64(ttl/time.Second))
	if err != nil {
		return false, nil, err
	}
	resp, err := s.kv.Txn(ctx).If(compareExpected(key, expected)).
		Then(clientv3.OpPut(key, string(next), clientv3.WithLease(grant.ID))).
		Else(clientv3.OpGet(key)).Commit()
	if err != nil {
		// 结果未知：键可能已经挂到新租约上，所以不撤销它。没挂上的话它在 ttl 后自行过期；挂上了的话 App 下一次
		// 抢锁会在 Else 里读到自己的值并认领（见下）。
		return false, nil, err
	}
	if !resp.Succeeded {
		current := rangeKV(resp, 0)
		if current != nil && bytes.Equal(current.Value, next) && current.Lease != 0 {
			// 上一次写入已落地、回复丢失：键里是本进程的值（令牌唯一），认领它挂的租约。
			s.remember(key, heldLease{id: clientv3.LeaseID(current.Lease), ttl: ttl})
		}
		s.revoke(ctx, grant.ID)
		if current == nil {
			return false, nil, nil
		}
		return false, current.Value, nil
	}
	s.remember(key, heldLease{id: grant.ID, ttl: ttl})
	if hadOld && old.id != grant.ID {
		s.revoke(ctx, old.id)
	}
	return true, nil, nil
}

// putWithoutLease 是 ttl = 0 的写入（业务时间高水位）：键不过期。
func (s *singletonStore) putWithoutLease(ctx context.Context, key string, expected, next []byte, old heldLease, hadOld bool) (bool, []byte, error) {
	resp, err := s.kv.Txn(ctx).If(compareExpected(key, expected)).
		Then(clientv3.OpPut(key, string(next))).
		Else(clientv3.OpGet(key)).Commit()
	if err != nil {
		return false, nil, err
	}
	if !resp.Succeeded {
		return false, rangeValue(resp, 0), nil
	}
	if hadOld {
		// 键原来挂着本进程的租约，现在不挂了；租约留着没有用。
		s.forget(key, old.id)
		s.revoke(ctx, old.id)
	}
	return true, nil, nil
}

// CompareAndDelete 按值删除：If(Value == expected) Then(Delete)，之后尽力撤销本进程为这个键持有的租约。
// 撤销不论删除是否成功都做：没删成（值已是别人的）时键不在这个租约上；删除报错（结果未知）时撤销正好把本进程的键删掉。
func (s *singletonStore) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	if expected == nil {
		return false, errors.New("kitetcd: singleton store: compare-and-delete needs the expected value")
	}
	resp, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.Value(key), "=", string(expected))).
		Then(clientv3.OpDelete(key)).Commit()
	if held, ok := s.heldLease(key); ok {
		s.forget(key, held.id)
		s.revoke(ctx, held.id)
	}
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// Get 在 Live 客户端上读每个键（不存在为 nil），顺序与 keys 相同：一个事务里放多个读，一次往返、同一版本，线性一致。
// 超过 singletonGetBatch 个键分多个事务读。
func (s *singletonStore) Get(ctx context.Context, keys []string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	out := make([][]byte, len(keys))
	for start := 0; start < len(keys); start += singletonGetBatch {
		end := min(start+singletonGetBatch, len(keys))
		ops := make([]clientv3.Op, 0, end-start)
		for _, key := range keys[start:end] {
			ops = append(ops, clientv3.OpGet(key))
		}
		resp, err := s.live.Txn(ctx).Then(ops...).Commit()
		if err != nil {
			return nil, err
		}
		for i := range ops {
			out[start+i] = rangeValue(resp, i)
		}
	}
	return out, nil
}

// Close 幂等：只有第一次真正关闭两个客户端，第一次的错误只报给那一次调用。不撤销持有的租约——App 走到 Close
// 而没有 Release，说明停机不完整，键要留到 ttl 后自然过期（见 framework/app/singleton.go 的 Release）。
func (s *singletonStore) Close() error {
	var err error
	s.closeOnce.Do(func() { err = errors.Join(s.lock.Close(), s.live.Close()) })
	return err
}

func (s *singletonStore) heldLease(key string) (heldLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.held[key]
	return held, ok
}

func (s *singletonStore) remember(key string, held heldLease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[key] = held
}

// forget 只在记录仍是 id 时删除。
func (s *singletonStore) forget(key string, id clientv3.LeaseID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[key].id == id {
		delete(s.held, key)
	}
}

// revoke 尽力撤销租约：失败（ctx 已到期、etcd 不可达）时租约在 ttl 后自行过期。
func (s *singletonStore) revoke(ctx context.Context, id clientv3.LeaseID) {
	_, _ = s.lease.Revoke(ctx, id)
}

// compareExpected 是“键仍是 expected”的比较：expected 为 nil 表示键必须不存在。
// 值比较在键不存在时恒为假（etcd 没法表示“缺失的值”），所以 expected 非 nil 时不存在的键不会被当成匹配。
func compareExpected(key string, expected []byte) clientv3.Cmp {
	if expected == nil {
		return clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	}
	return clientv3.Compare(clientv3.Value(key), "=", string(expected))
}

// rangeKV 是事务第 i 个读结果的第一个键（没有为 nil）。
func rangeKV(resp *clientv3.TxnResponse, i int) *mvccpb.KeyValue {
	if i >= len(resp.Responses) {
		return nil
	}
	rr := resp.Responses[i].GetResponseRange()
	if rr == nil || len(rr.Kvs) == 0 {
		return nil
	}
	return rr.Kvs[0]
}

func rangeValue(resp *clientv3.TxnResponse, i int) []byte {
	if kv := rangeKV(resp, i); kv != nil {
		return kv.Value
	}
	return nil
}

func firstValue(kvs []*mvccpb.KeyValue) []byte {
	if len(kvs) == 0 {
		return nil
	}
	return kvs[0].Value
}

var _ app.SingletonStore = (*singletonStore)(nil)
