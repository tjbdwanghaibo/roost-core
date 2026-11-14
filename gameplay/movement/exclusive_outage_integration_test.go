//go:build integration

package movement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
	officialmongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// KNOWN-LIMITS“独占模式 Remote 心跳的真实依赖故障注入”：独占模式（不经共享）直接转移所有权时，旧 owner 与权威之间的
// Mongo 出故障。正式装配（Mongo 所有权权威、Redis、NATS），旧 owner A 的 Mongo 连接经本用例自建的 toxiproxy 代理（直连
// 副本集主节点），新 owner B 直连。独占稳定持有时写准入不碰 Mongo，所以连接断开本身不影响 A 的心跳；故障落在转移上，
// 三种时刻：
//   - down_before：转移前断开。转移先读权威对齐（refresh ownership）就失败，什么也没改，A 仍是独占 owner、照常推进；
//     B 写不进去。恢复后正常转给 B。
//   - down_at_cas：读权威之后、所有权 CAS 发出时断开（casFaultMongo 在 CAS 那一刻关代理，网络错误是真实的）。CAS 没到
//     权威，复核也读不到，A 的实例冻结在 Recovering（结果未知，不准入任何写）；权威仍是 A，B 写不进去。恢复后 A 的下一次
//     写准入读到权威、解冻并写入，再正常转给 B。
//   - reply_lost：CAS 发出时给 A 的回程加 10s 延迟（大于 A 的 OpTimeout 2s）。CAS 已在权威生效、A 收不到回复，复核同样
//     超时，A 冻结在 Recovering；权威已是 B，B 立刻能写。恢复后 A 的写准入读到权威是 B，答 ErrRemoteFenced。
//
// 不双推进：B 拥有之后，A 加载过的任何实例都不再有周期推进（心跳的拍经写准入，被拒后不执行回调）；后两种从转移开始
// A 就不推进。Redis 在独占模式不参与（分布式锁只给共享模式），所以只注入 Mongo 故障。
//
// 运行：source 隔离环境的 env.sh（要 toxiproxy）后
//
//	GOWORK=off go test -race -tags=integration -run '^TestRealRemoteExclusiveTransferDuringAMongoFault$' -count=1 ./gameplay/movement
func TestRealRemoteExclusiveTransferDuringAMongoFault(t *testing.T) {
	for i, fault := range []string{"down_before", "down_at_cas", "reply_lost"} {
		t.Run(fault, func(t *testing.T) {
			testExclusiveTransferDuringAMongoFault(t, fault, int64(21+i), heartbeat.DeliveryBatch)
		})
	}
}

func testExclusiveTransferDuringAMongoFault(t *testing.T, fault string, unique int64, delivery heartbeat.Delivery) {
	proxy := newMongoPrimaryProxy(t)
	faulty := &casFaultMongo{IMongo: proxy.client}
	const sidA, sidB = int32(1001), int32(1002)
	managers := realRemoteManagersWith(t, func(sid int32, cfg *remoteentity.Config) fmongo.IMongo {
		if sid != sidA {
			return nil
		}
		cfg.OpTimeout = 2 * time.Second // 结果未知的判定与复核不等 30s
		return faulty
	})
	ctx := context.Background()
	log := &npcLog{}
	a := newNPCProc(t, "A", sidA, log, managers, 4, delivery)
	b := newNPCProc(t, "B", sidB, log, managers, 4, delivery)
	b.setOnLoad(npcRestore)
	id := npcID(t, unique)
	a.mustRun(id, npcStart) // 首写认领：A 独占
	ea := a.instance(id)
	expectRunning(t, ea, "A owns the entity exclusively")
	if lease, found, err := b.remote.GetRemoteOwnership(ctx, id); err != nil || !found || lease.OwnerSid != sidA || lease.Shared {
		t.Fatalf("authority before the fault: %+v found=%v err=%v, want exclusive owner %d", lease, found, err, sidA)
	}

	switch fault {
	case "down_before":
		proxy.setEnabled(t, false)
	case "down_at_cas":
		faulty.arm(func() { proxy.setEnabled(t, false) })
	case "reply_lost":
		faulty.arm(func() { proxy.addToxic(t, "reply_lost", "latency", "downstream", map[string]any{"latency": 10000}) })
	}
	transferStart := log.mark()
	_, transferErr := a.remote.TransferRemoteOwnership(ctx, id, sidB)
	t.Logf("%s: transfer returned %v; A's instance is %s", fault, transferErr, ea.RemoteOwnershipState())
	if transferErr == nil {
		t.Fatalf("%s: the transfer reported success although A could not get an answer from the authority", fault)
	}
	if fault != "down_before" && !faulty.fired() {
		t.Fatalf("%s: the transfer never reached the ownership CAS", fault)
	}
	wantState, wantOwner := entity.RemoteOwnershipRecovering, sidA
	switch fault {
	case "down_before":
		wantState = entity.RemoteOwnershipLocalOwned // 读权威就失败：确定没有改动
	case "reply_lost":
		wantOwner = sidB
	}
	if s := ea.RemoteOwnershipState(); s != wantState {
		t.Fatalf("%s: A's instance is %s after the failed transfer, want %s", fault, s, wantState)
	}
	waitFor(t, fmt.Sprintf("the authority names sid %d", wantOwner), func() bool {
		lease, found, err := b.remote.GetRemoteOwnership(ctx, id)
		return err == nil && found && lease.OwnerSid == wantOwner
	})
	bErr := b.run(id, func(*remoteNPC) error { return nil })
	if (wantOwner == sidA) != (bErr != nil) {
		t.Fatalf("%s: B's write with the authority naming sid %d: %v", fault, wantOwner, bErr)
	}
	aErr := a.run(id, func(*remoteNPC) error { return nil })
	if (wantState == entity.RemoteOwnershipLocalOwned) != (aErr == nil) {
		t.Fatalf("%s: A's write with its instance %s: %v", fault, wantState, aErr)
	}
	time.Sleep(npcQuiet)
	if n := log.ticks(ea.who, transferStart, 0); wantState == entity.RemoteOwnershipRecovering && n != 0 {
		t.Fatalf("%s: A's instance advanced %d times while its ownership was unknown: %v", fault, n, log.since(transferStart, ea.who))
	}

	// 恢复连接：A 的下一次写准入读到权威。
	proxy.heal(t)
	if wantOwner == sidA {
		waitFor(t, "A writes again once the authority is reachable", func() bool { return a.run(id, func(*remoteNPC) error { return nil }) == nil })
		if _, err := a.remote.TransferRemoteOwnership(ctx, id, sidB); err != nil {
			t.Fatalf("%s: transfer after the fault healed: %v", fault, err)
		}
		if err := b.run(id, func(*remoteNPC) error { return nil }); err != nil {
			t.Fatalf("%s: B's first write after the transfer: %v", fault, err)
		}
	}
	waitFor(t, "A learns that B owns the entity", func() bool { return errorsIsFenced(a.run(id, func(*remoteNPC) error { return nil })) })
	settled := log.mark()
	eb := b.instance(id)
	if eb == nil {
		t.Fatalf("%s: B has no instance after owning the entity", fault)
	}
	b.mustRun(id, npcStart) // 业务在新 owner 上让它跑起来（动作与任务不持久，不随加载恢复）
	expectRunning(t, eb, "B owns the entity")
	time.Sleep(npcQuiet)
	from := settled
	if wantOwner == sidB { // reply_lost：转移那一刻起 A 就不再拥有（down_* 恢复后到正常转移之前 A 仍是 owner，可以推进）
		from = transferStart
	}
	for _, ev := range log.since(from, "") {
		if (ev.what == "ai tick" || ev.what == "work tick") && ev.who != "" && ev.who[0] == 'A' {
			t.Fatalf("%s: double advance: A's instance %s advanced after it stopped owning the entity: %v", fault, ev.who, log.since(from, ev.who))
		}
	}
}

// casFaultMongo 是 A 的 Mongo 客户端：armed 时，下一次所有权 CAS（_remote_entity_meta 上的 FindOneAndUpdate）发出之前
// 先执行一次故障动作（关代理 / 加回程延迟），再照常经代理发出，错误是真实的网络错误。
type casFaultMongo struct {
	fmongo.IMongo
	mu     sync.Mutex
	action func()
	done   bool
}

func (m *casFaultMongo) arm(action func()) {
	m.mu.Lock()
	m.action, m.done = action, false
	m.mu.Unlock()
}

func (m *casFaultMongo) fired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.done
}

func (m *casFaultMongo) trigger() {
	m.mu.Lock()
	action := m.action
	m.action = nil
	if action != nil {
		m.done = true
	}
	m.mu.Unlock()
	if action != nil {
		action()
	}
}

func (m *casFaultMongo) Database(name string) fmongo.IDatabase {
	return casFaultDatabase{IDatabase: m.IMongo.Database(name), owner: m}
}

type casFaultDatabase struct {
	fmongo.IDatabase
	owner *casFaultMongo
}

func (d casFaultDatabase) Collection(name string) fmongo.ICollection {
	collection := d.IDatabase.Collection(name)
	if name != "_remote_entity_meta" {
		return collection
	}
	return casFaultCollection{ICollection: collection, owner: d.owner}
}

type casFaultCollection struct {
	fmongo.ICollection
	owner *casFaultMongo
}

func (c casFaultCollection) FindOneAndUpdate(ctx context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	c.owner.trigger()
	return c.ICollection.FindOneAndUpdate(ctx, filter, update, result, opts...)
}

func errorsIsFenced(err error) bool { return err != nil && errors.Is(err, entity.ErrRemoteFenced) }

// mongoPrimaryProxy 是本用例在环境 toxiproxy 上自建的代理（唯一命名、临时端口），上游是副本集当前主节点；client 以
// directConnection 经它连接。只在这个代理上加删 toxic / 启停，结束时删除；不碰别的代理，不调 /reset。
type mongoPrimaryProxy struct {
	api, name string
	client    fmongo.IMongo
}

func newMongoPrimaryProxy(t *testing.T) *mongoPrimaryProxy {
	t.Helper()
	api, uri := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"), os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || api == "" || uri == "" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with the isolated environment and its toxiproxy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	direct, err := officialmongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		Primary string `bson:"primary"`
	}
	err = direct.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
	_ = direct.Disconnect(context.Background())
	if err != nil || hello.Primary == "" {
		t.Fatalf("find the replica-set primary: %+v %v", hello, err)
	}
	p := &mongoPrimaryProxy{api: api, name: fmt.Sprintf("movement-mongo-fault-%d-%d", os.Getpid(), time.Now().UnixNano())}
	var created struct {
		Listen string `json:"listen"`
	}
	body := p.call(t, http.MethodPost, "/proxies", map[string]any{"name": p.name, "listen": "127.0.0.1:0", "upstream": hello.Primary, "enabled": true})
	if err := json.Unmarshal(body, &created); err != nil || created.Listen == "" {
		t.Fatalf("toxiproxy create %s: %v %s", p.name, err, body)
	}
	t.Cleanup(func() { p.call(t, http.MethodDelete, "/proxies/"+p.name, nil) })
	p.client, err = mongodriver.NewClient(fmongo.DefaultConfig("mongodb://"+created.Listen+"/?directConnection=true"), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.client.Close(context.Background()) })
	return p
}

func (p *mongoPrimaryProxy) setEnabled(t *testing.T, enabled bool) {
	t.Helper()
	p.call(t, http.MethodPost, "/proxies/"+p.name, map[string]any{"enabled": enabled})
}

func (p *mongoPrimaryProxy) addToxic(t *testing.T, name, kind, stream string, attributes map[string]any) {
	t.Helper()
	p.call(t, http.MethodPost, "/proxies/"+p.name+"/toxics", map[string]any{"name": name, "type": kind, "stream": stream, "toxicity": 1.0, "attributes": attributes})
}

// heal 启用代理并删掉它上面的全部 toxic。
func (p *mongoPrimaryProxy) heal(t *testing.T) {
	t.Helper()
	p.setEnabled(t, true)
	var toxics []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/proxies/"+p.name+"/toxics", nil), &toxics); err != nil {
		t.Fatal(err)
	}
	for _, toxic := range toxics {
		p.call(t, http.MethodDelete, "/proxies/"+p.name+"/toxics/"+toxic.Name, nil)
	}
}

func (p *mongoPrimaryProxy) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, p.api+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 && !(method == http.MethodDelete && response.StatusCode == http.StatusNotFound) {
		t.Fatalf("toxiproxy %s %s: %s %s", method, path, response.Status, raw)
	}
	return raw
}
