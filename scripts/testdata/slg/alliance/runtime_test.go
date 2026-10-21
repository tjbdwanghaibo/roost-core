package alliance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/service/activity"
)

func check(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}

// 固定发现列表仅用于隔离教学进程；不证明 etcd 发现、singleton 或自动 HA。
type fixedDiscovery struct{ etcd.IDiscovery }

func (fixedDiscovery) Discover(context.Context, string) ([]*etcd.ServiceInfo, error) {
	return []*etcd.ServiceInfo{{ServiceType: activity.ServiceType, Sid: 9000}}, nil
}
func redisClient(t *testing.T) fredis.IRedis {
	t.Helper()
	r, e := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	check(t, e)
	t.Cleanup(func() { _ = r.Close() })
	return r
}
func newBus(t *testing.T, prefix, kind string, sid int32) (*bus.Bus, *natsdriver.Client) {
	t.Helper()
	n, e := natsdriver.NewClient(fnats.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")), natsdriver.ClientOptions{})
	check(t, e)
	t.Cleanup(n.Close)
	rpc := natsdriver.NewRPCClient(n, fnats.DefaultRetryPolicy(), 1)
	t.Cleanup(rpc.Stop)
	b := bus.New(n, rpc, nil, bus.Config{Sid: sid, SvcType: kind, Prefix: prefix, WorkerNum: 2, QueueCap: 32})
	t.Cleanup(b.Stop)
	return b, n
}

// coordinatorReservationTTL 是本例协调器的去重保留期；联盟按它停止自动补报（newAlliance 的同名参数）。
const coordinatorReservationTTL = time.Hour

func coordinatorProcess(t *testing.T, prefix string) {
	r := redisClient(t)
	// 存储 TTL 不能短于 ReservationTTL；联盟的补报截止也取同一个值（coordinatorReservationTTL）。
	s, e := activity.NewRedisStores(r, prefix, coordinatorReservationTTL)
	check(t, e)
	groups, e := activity.ParseGroups([]byte("groups:\n  - id: alliance\n    game_sids: [1000, 1001]\n"), "fixture")
	check(t, e)
	svc, e := activity.New(activity.Config{Activities: s.Activities, Participants: s.Participants, Ledger: s.Ledger, Audits: s.Audits, Dispatches: s.Dispatches, Windows: s.Windows, Groups: &groups, ReservationTTL: coordinatorReservationTTL, DispatchBackoff: time.Second})
	check(t, e)
	b, n := newBus(t, prefix, activity.ServiceType, 9000)
	check(t, activity.RegisterHandlers(b, svc))
	check(t, b.Start())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	check(t, n.FlushContext(ctx))
	check(t, os.WriteFile(os.Getenv("SLG_ALLIANCE_READY"), []byte("ready"), 0600))
	// 父进程关闭 stdin 后正常排空；测试超时的清理只终止本次创建的子进程。
	_, e = io.Copy(io.Discard, os.Stdin)
	check(t, e)
}
func startCoordinator(t *testing.T, prefix string) func() {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	log, e := os.Create(filepath.Join(dir, "process.log"))
	check(t, e)
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestSLGAllianceActivity$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "SLG_ALLIANCE_CHILD="+prefix, "SLG_ALLIANCE_READY="+ready)
	cmd.Stdout = log
	cmd.Stderr = log
	stdin, e := cmd.StdinPipe()
	check(t, e)
	check(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = stdin.Close()
			select {
			case e := <-done:
				if e != nil {
					t.Errorf("coordinator stopped: %v", e)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("coordinator failed to drain")
			}
		})
	}
	t.Cleanup(stop)
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if _, e := os.Stat(ready); e == nil {
			return stop
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile(log.Name())
	t.Fatalf("coordinator not ready: %s", raw)
	return stop
}
func client(t *testing.T, prefix string, sid int32) *activity.BusClient {
	b, n := newBus(t, prefix, "game", sid)
	check(t, b.Start())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	check(t, n.FlushContext(ctx))
	c, e := activity.NewBusClient(b, fixedDiscovery{}, "", 3*time.Second)
	check(t, e)
	return c
}

// 唯一公开的地图内容只有据点 id；该切片不发送坐标、敌方实体、成员名单或奖励账本。
// 输出正式编码帧，由 fog_test 的 Receiver 验证关系撤销与重连；内存捕获不冒充 TCP 验收。
type viewSink struct {
	mu     sync.Mutex
	frames map[entitysync.SessionID][][]byte
}

func (s *viewSink) Push(_ context.Context, id entitysync.SessionID, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames[id] = append(s.frames[id], append([]byte(nil), raw...))
	return nil
}
func (s *viewSink) take(id entitysync.SessionID) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.frames[id]
	delete(s.frames, id)
	return out
}

// 视野编号由业务分配：联盟视野按联盟号，侦察视野按玩家。
const (
	allianceView policy.ViewID = 1_000_001
	scoutView    policy.ViewID = 2_000_002
)

type fog struct {
	manager  *entitysync.Manager
	interest *policy.Interest
	sink     *viewSink
}

func newFog(t *testing.T) *fog {
	sink := &viewSink{frames: map[entitysync.SessionID][][]byte{}}
	m, e := entitysync.NewManager(entitysync.ManagerConfig{Transport: sink})
	check(t, e)
	t.Cleanup(func() { check(t, m.Close(context.Background())) })
	in, e := policy.NewInterest(policy.InterestConfig{Manager: m, AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 100, EnterRadius: 100, LeaveRadius: 120}})
	check(t, e)
	t.Cleanup(in.Close)
	// 联盟视野是一份共享的关系视野：成员是联盟共有的目标（据点 99），挂给每个在籍成员；成员变动只改挂接，不复制成员集合。
	// 侦察视野是 bob 私有的关系视野，和联盟视野各自持有同一个据点，最后一个释放才退订。
	check(t, in.DefineRelationView(allianceView, entity.SyncProfile{}))
	check(t, in.SetViewMembers(allianceView, []int64{99}))
	check(t, in.DefineRelationView(scoutView, entity.SyncProfile{}))
	check(t, in.SetViewMembers(scoutView, []int64{99}))
	check(t, m.Register(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 99, Namespace: "alliance-objective", Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.TakeFrozenSyncPayload(1, []byte(`{"objective":99}`)), nil
	}}})))
	for _, id := range []int64{1, 2} {
		check(t, m.OpenSession(entitysync.SessionID(id)))
	}
	return &fog{m, in, sink}
}

// 持久成员状态成功之后才重建关系。调用方串行协调本服视野并暂停会话发布；
// 失败保持 held，不能把陈旧成员缓存当授权。跨服撤权传播延迟不在此切片保证内。
func (f *fog) reconcile(s state) error {
	return f.reconcileFrom(func() (state, error) { return s, nil })
}
func (f *fog) reconcileFrom(load func() (state, error)) error {
	for _, id := range []entitysync.SessionID{1, 2} {
		if e := f.manager.HoldSession(id); e != nil {
			return e
		}
	}
	s, e := load()
	if e != nil {
		return e
	}
	for p, id := range map[string]int64{"alice": 1, "bob": 2} {
		attach := f.interest.DetachView
		if s.Members[p].Active {
			attach = f.interest.AttachView
		}
		if e := attach(id, allianceView); e != nil {
			return e
		}
	}
	if refused := f.interest.Apply(); len(refused) > 0 {
		return fmt.Errorf("view refused: %v", refused)
	}
	for _, id := range []entitysync.SessionID{1, 2} {
		if e := f.manager.ReadySession(id); e != nil {
			return e
		}
	}
	return f.manager.Flush(context.Background())
}
func (f *fog) visible(t *testing.T, id entitysync.SessionID) bool {
	t.Helper()
	if refused := f.interest.Apply(); len(refused) > 0 {
		t.Fatal(refused)
	}
	check(t, f.manager.Flush(context.Background()))
	for _, sid := range f.manager.Subscribers(99) {
		if sid == id {
			return true
		}
	}
	return false
}

// lostProgress 模拟收到成功回复后进程未写本地确认；请求确实经真实 NATS 到达子进程。
// 它不是 NATS 网络故障注入，更不是节点断电。
type lostProgress struct{ activity.Coordinator }

func (c lostProgress) ApplyProgress(ctx context.Context, k activity.Key, p, r string, d activity.ProgressDelta) (activity.Participant, error) {
	v, e := c.Coordinator.ApplyProgress(ctx, k, p, r, d)
	if e != nil {
		return v, e
	}
	return v, errors.New("injected lost application reply")
}
func namespace() string { return fmt.Sprintf("slg_alliance_%d_%d", os.Getpid(), time.Now().UnixNano()) }
func safeNamespace(s string) bool {
	return strings.HasPrefix(s, "slg_alliance_") && !strings.ContainsAny(s, "*.> :/")
}
