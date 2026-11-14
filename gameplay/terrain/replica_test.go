package terrain

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// 块副本（replica.go）的进程间行为：每个“进程”是一个 Replicator + 自己的视图，进程之间是一张进程内的网（replicaNet），
// 可以对每条消息丢弃、重复、延迟（延迟用 AfterFunc，所以会乱序），也能把录下的旧消息重放给某个进程。
// 分区：3 × 2 块的地图按列分给进程——第 0 列归 sid 1（A），第 1、2 列归 sid 2（B）；B 订阅第 0 列（它的 Ghost 带）。

const (
	sidA int32 = 1
	sidB int32 = 2
	// replicaTestCh 是测试用的那一路（格的 Ghost 带）。
	replicaTestCh ReplicaChannel = 1
)

func ownerByColumn(key ChunkKey) (int32, bool) {
	switch {
	case key.X == 0:
		return sidA, true
	case key.X == 1 || key.X == 2:
		return sidB, true
	}
	return 0, false
}

// replicaNet 是进程内的网：Send 投给目标进程的 Deliver（直接或经 AfterFunc 延迟），fault 决定每条消息的命运；sent
// 录下每条块消息（重放用）。
type replicaNet struct {
	mu    sync.Mutex
	nodes map[int32]*Replicator
	fault func(from, to int32, body []byte) (drop, dup bool, delay time.Duration)
	sent  []recorded
}

type recorded struct {
	from, to int32
	body     []byte
}

func newReplicaNet() *replicaNet { return &replicaNet{nodes: map[int32]*Replicator{}} }

func (n *replicaNet) attach(sid int32, r *Replicator) {
	n.mu.Lock()
	n.nodes[sid] = r
	n.mu.Unlock()
}

func (n *replicaNet) detach(sid int32) {
	n.mu.Lock()
	delete(n.nodes, sid)
	n.mu.Unlock()
}

func (n *replicaNet) setFault(f func(from, to int32, body []byte) (bool, bool, time.Duration)) {
	n.mu.Lock()
	n.fault = f
	n.mu.Unlock()
}

func (n *replicaNet) sender(from int32) func(int32, []byte) bool {
	return func(to int32, body []byte) bool {
		n.mu.Lock()
		fault := n.fault
		if len(body) > 1 && body[1] == replicaChunks {
			n.sent = append(n.sent, recorded{from: from, to: to, body: append([]byte(nil), body...)})
		}
		n.mu.Unlock()
		copies, delay := 1, time.Duration(0)
		if fault != nil {
			drop, dup, d := fault(from, to, body)
			if drop {
				return true // 传输“发出去了”，路上丢了
			}
			if dup {
				copies = 2
			}
			delay = d
		}
		for range copies {
			deliver := func() {
				n.mu.Lock()
				target := n.nodes[to]
				n.mu.Unlock()
				if target != nil {
					target.Deliver(from, body)
				}
			}
			if delay > 0 {
				time.AfterFunc(delay, deliver)
			} else {
				deliver()
			}
		}
		return true
	}
}

// recordedTo 是到目前为止发给 to 的块消息（按发出顺序）。
func (n *replicaNet) recordedTo(to int32) []recorded {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []recorded
	for _, m := range n.sent {
		if m.to == to {
			out = append(out, m)
		}
	}
	return out
}

// viewStore 是副本一侧的存放处：一个视图加“显示不了”的块（与 game-demo 的 Map.ApplyReplica 同一规则）。
type viewStore struct {
	mu         sync.Mutex
	view       *View
	unloadable map[ChunkKey]bool
	forgotten  []ChunkKey
}

func (s *viewStore) Held(key ChunkKey) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unloadable[key] {
		return -1
	}
	version, loaded := s.view.Version(key)
	if !loaded {
		return -1
	}
	return version
}

func (s *viewStore) Apply(key ChunkKey, o Overlay) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.Version >= 1 {
		if err := s.view.SetOverlay(key, o); err != nil {
			return err
		}
		delete(s.unloadable, key)
		return nil
	}
	if version, loaded := s.view.Version(key); loaded && version >= 1 {
		return fmt.Errorf("%w: the owner says base, this view holds version %d", ErrStaleOverlay, version)
	}
	if err := s.view.LoadBase(key); err != nil {
		return err
	}
	delete(s.unloadable, key)
	return nil
}

func (s *viewStore) Forget(key ChunkKey) {
	s.mu.Lock()
	s.forgotten = append(s.forgotten, key)
	s.mu.Unlock()
}

// replicaProc 是一个“进程”：视图、Replicator（拥有者一侧以自己的视图为源）与它的副本存放处。
type replicaProc struct {
	sid    int32
	view   *View
	store  *viewStore
	r      *Replicator
	cancel context.CancelFunc
	done   chan struct{}
}

const (
	testRenew = 40 * time.Millisecond
	testLease = 150 * time.Millisecond
)

// startReplicaProc 起一个进程：视图装满整张图（不在 unloadable 里的块），按列归属，副本一路想要 want。
func startReplicaProc(t *testing.T, net *replicaNet, sid int32, pack *Pack, unloadable []ChunkKey, want []ChunkKey, mod func(*ReplicatorConfig)) *replicaProc {
	t.Helper()
	p := &replicaProc{sid: sid, view: NewView(pack)}
	p.store = &viewStore{view: p.view, unloadable: map[ChunkKey]bool{}}
	for key := range ChunksIn(pack.Meta().Bounds()) {
		if slices.Contains(unloadable, key) {
			p.store.unloadable[key] = true
			continue
		}
		if err := p.view.LoadBase(key); err != nil {
			t.Fatal(err)
		}
	}
	p.startReplicator(t, net, want, mod)
	return p
}

func (p *replicaProc) startReplicator(t *testing.T, net *replicaNet, want []ChunkKey, mod func(*ReplicatorConfig)) {
	t.Helper()
	p.startReplicatorStore(t, net, want, mod, p.store)
}

// startReplicatorWith 是 startReplicator，副本那一路换成 store（例如要占位记录的存放处）。
func (p *replicaProc) startReplicatorWith(t *testing.T, net *replicaNet, want []ChunkKey, store ReplicaStore) {
	t.Helper()
	p.startReplicatorStore(t, net, want, nil, store)
}

func (p *replicaProc) startReplicatorStore(t *testing.T, net *replicaNet, want []ChunkKey, mod func(*ReplicatorConfig), store ReplicaStore) {
	t.Helper()
	cfg := ReplicatorConfig{
		Self: p.sid, Meta: p.view.Meta(), Send: net.sender(p.sid), OwnerOf: ownerByColumn,
		Source: p.view, Owns: func(key ChunkKey) bool { owner, _ := ownerByColumn(key); return owner == p.sid },
		Renew: testRenew, Lease: testLease, Tick: 5 * time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	r, err := NewReplicator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddChannel(replicaTestCh, store); err != nil {
		t.Fatal(err)
	}
	p.r = r
	net.attach(p.sid, r)
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() { defer close(p.done); _ = r.Run(ctx) }()
	if err := r.Want(replicaTestCh, want); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
}

// want 换掉这一路想要的块；用例里的订阅都远小于上限，被拒就是失败。
func (p *replicaProc) want(t *testing.T, keys []ChunkKey) {
	t.Helper()
	if err := p.r.Want(replicaTestCh, keys); err != nil {
		t.Fatal(err)
	}
}

// stop 停下 Replicator（视图留着：重启时可以拿它当“上次的库”）。可重复调用。
func (p *replicaProc) stop() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
		p.cancel = nil
	}
}

// commit 是拥有者一次提交：新版本进视图，然后 Changed（与 game-demo 的提交后回调同一顺序）。
func (p *replicaProc) commit(t *testing.T, key ChunkKey, version int64, seed uint64) Chunk {
	t.Helper()
	c := randomChunk(seed)
	if err := p.view.SetOverlay(key, Overlay{BaseRevision: p.view.Meta().BaseRevision, Version: version, Heights: c.Heights, Materials: c.Materials}); err != nil {
		t.Fatal(err)
	}
	p.r.Changed(key)
	return c
}

func randomChunk(seed uint64) Chunk {
	rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
	c := NewChunk()
	base := uint16(rng.IntN(20000))
	for k := range c.Heights {
		c.Heights[k] = base + uint16(rng.IntN(40)) // 平缓起伏：与真实的改块一样可压缩
		c.Materials[k] = byte(rng.IntN(5))
	}
	return c
}

// waitVersion 等 p 的视图里 key 到 version（有界：2 秒）。
func waitVersion(t *testing.T, p *replicaProc, key ChunkKey, version int64, what string) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(2 * time.Second)
	for {
		if got := p.store.Held(key); got == version {
			return time.Since(start)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: chunk %v held %d after 2s, want %d (stats %+v)", what, key, p.store.Held(key), version, p.r.Stats())
		}
		time.Sleep(time.Millisecond)
	}
}

// sameData 断言两个视图里 key 的数据、版本一致，且块内与跨块的查询逐点相同。
func sameData(t *testing.T, a, b *View, key ChunkKey) {
	t.Helper()
	ca, va, la := a.Chunk(key)
	cb, vb, lb := b.Chunk(key)
	if !la || !lb || va != vb || !equalChunk(ca, cb) {
		t.Fatalf("chunk %v: owner version %d (loaded %v), replica version %d (loaded %v), data equal %v", key, va, la, vb, lb, equalChunk(ca, cb))
	}
	bounds := key.Bounds()
	for x := bounds.Min.X; x < bounds.Max.X; x += 37 {
		for y := bounds.Min.Y; y < bounds.Max.Y; y += 37 {
			ga, ea := a.Probe(x, y)
			gb, eb := b.Probe(x, y)
			if (ea == nil) != (eb == nil) || ga != gb {
				t.Fatalf("probe (%d, %d): owner %+v %v, replica %+v %v", x, y, ga, ea, gb, eb)
			}
		}
	}
}

var column0 = []ChunkKey{{X: 0, Y: 0}, {X: 0, Y: 1}}

func waitSubscribers(t *testing.T, p *replicaProc, n uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.r.Stats().Subscribers != n {
		if time.Now().After(deadline) {
			t.Fatalf("sid %d has %d subscriptions, want %d", p.sid, p.r.Stats().Subscribers, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// 推送：拥有者提交之后，订阅了这块的副本在有界时间内持有同一版本、同一数据，查询逐点相同；连续几次提交，副本停在
// 最新的版本；订阅里不归拥有者写的块不答；不想要的块不装。
func TestReplicaPushesCommitsToTheSubscriber(t *testing.T) {
	pack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	net := newReplicaNet()
	a := startReplicaProc(t, net, sidA, pack, nil, nil, nil)
	b := startReplicaProc(t, net, sidB, pack, nil, append(slices.Clone(column0), ChunkKey{X: 1, Y: 0}), nil)
	waitSubscribers(t, a, 1)

	a.commit(t, column0[0], 1, 1)
	waitVersion(t, b, column0[0], 1, "first push")
	sameData(t, a.view, b.view, column0[0])
	for v := int64(2); v <= 5; v++ {
		a.commit(t, column0[0], v, uint64(v))
	}
	a.commit(t, column0[1], 1, 9)
	waitVersion(t, b, column0[0], 5, "latest of several commits")
	waitVersion(t, b, column0[1], 1, "the other chunk")
	sameData(t, a.view, b.view, column0[0])
	sameData(t, a.view, b.view, column0[1])
	if s := a.r.Stats(); s.PushedOut == 0 || s.ChunkBytesOut == 0 {
		t.Fatalf("owner stats %+v: no push went out", s)
	}
	if s := b.r.Stats(); s.WrongOwner != 0 || s.DecodeErrors != 0 || s.ApplyErrors != 0 {
		t.Fatalf("replica stats %+v", s)
	}
	// B 想要的 (1,0) 是 B 自己写的块：不订阅（视图里就是权威数据），谁的订阅表里都没有 B 的订阅。
	if s := b.r.Stats(); s.Subscribers != 0 || s.SubscribesIn != 0 {
		t.Fatalf("replica stats %+v: B subscribed its own chunk", s)
	}
}

// 丢失、重复、乱序：推送全部丢掉时，下一次订阅续期补齐；之后打开重复与随机延迟（乱序），副本仍停在最新版本；把录下的
// 全部旧消息按逆序重放给副本，版本与数据都不回退（只计 Stale）。
func TestReplicaSurvivesLossDuplicatesAndReordering(t *testing.T) {
	pack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	net := newReplicaNet()
	a := startReplicaProc(t, net, sidA, pack, nil, nil, nil)
	b := startReplicaProc(t, net, sidB, pack, nil, column0, nil)
	waitSubscribers(t, a, 1)

	net.setFault(func(from, to int32, body []byte) (bool, bool, time.Duration) {
		return from == sidA && body[1] == replicaChunks && to == sidB, false, 0
	})
	a.commit(t, column0[0], 1, 1)
	a.commit(t, column0[0], 2, 2)
	time.Sleep(3 * testRenew)
	if got := b.store.Held(column0[0]); got != 0 {
		t.Fatalf("with every chunk message dropped the replica holds %d", got)
	}
	net.setFault(nil)
	elapsed := waitVersion(t, b, column0[0], 2, "catch-up after loss")
	if elapsed > 3*testRenew {
		t.Fatalf("catch-up took %v, more than about one renew (%v)", elapsed, testRenew)
	}
	sameData(t, a.view, b.view, column0[0])

	rng := rand.New(rand.NewPCG(7, 7))
	var rngMu sync.Mutex
	net.setFault(func(int32, int32, []byte) (bool, bool, time.Duration) {
		rngMu.Lock()
		defer rngMu.Unlock()
		return false, rng.IntN(2) == 0, time.Duration(rng.IntN(30)) * time.Millisecond
	})
	for v := int64(3); v <= 12; v++ {
		a.commit(t, column0[0], v, uint64(v))
		time.Sleep(time.Duration(v%5) * time.Millisecond)
	}
	waitVersion(t, b, column0[0], 12, "duplicates and reordering")
	time.Sleep(60 * time.Millisecond) // 延迟的旧消息都到了
	net.setFault(nil)
	if got := b.store.Held(column0[0]); got != 12 {
		t.Fatalf("after the delayed copies arrived the replica holds %d, want 12", got)
	}
	staleBefore := b.r.Stats().Stale
	old := net.recordedTo(sidB)
	for i := len(old) - 1; i >= 0; i-- {
		b.r.Deliver(old[i].from, old[i].body)
	}
	time.Sleep(20 * time.Millisecond)
	if got := b.store.Held(column0[0]); got != 12 {
		t.Fatalf("replaying %d old messages moved the replica to %d", len(old), got)
	}
	sameData(t, a.view, b.view, column0[0])
	if b.r.Stats().Stale == staleBefore {
		t.Fatal("the replayed messages were not counted stale")
	}
}

// 重启：副本进程重启后按“上次的库”（旧版本）装入，订阅一到就补齐到拥有者的最新；拥有者重启（订阅表丢了）后，
// 副本的续期在一个 Renew 内重新订上，之后的提交照常推到。
func TestReplicaCatchesUpAfterEitherSideRestarts(t *testing.T) {
	pack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	net := newReplicaNet()
	a := startReplicaProc(t, net, sidA, pack, nil, nil, nil)
	b := startReplicaProc(t, net, sidB, pack, nil, column0, nil)
	waitSubscribers(t, a, 1)
	first := a.commit(t, column0[0], 1, 1)
	waitVersion(t, b, column0[0], 1, "before the restarts")

	// B 停下；A 接着改；B 重启时的“库”只有版本 1（读库之后 A 又提交了两次）。
	b.stop()
	net.detach(sidB)
	a.commit(t, column0[0], 2, 2)
	a.commit(t, column0[0], 3, 3)
	b2 := startReplicaProc(t, net, sidB, pack, nil, nil, nil)
	b2.stop()
	if err := b2.view.SetOverlay(column0[0], Overlay{BaseRevision: testMeta.BaseRevision, Version: 1, Heights: first.Heights, Materials: first.Materials}); err != nil {
		t.Fatal(err)
	}
	b2.startReplicator(t, net, column0, nil)
	waitVersion(t, b2, column0[0], 3, "replica restart")
	sameData(t, a.view, b2.view, column0[0])

	// A 重启：新的 Replicator（订阅表空），视图是“库”（版本 3）。B2 续期之后 A2 又有订阅，提交照常推到。
	a.stop()
	net.detach(sidA)
	a.startReplicator(t, net, nil, nil)
	waitSubscribers(t, a, 1)
	a.commit(t, column0[0], 4, 4)
	waitVersion(t, b2, column0[0], 4, "owner restart")
	sameData(t, a.view, b2.view, column0[0])
}

// 原始地形换版（D13）：A、B 都换成 BaseRevision 4 的包；B 的这块记录基于旧版本、显示不了（-1），A 已把它丢弃成原始块
// （Version 0）。B 订阅后得到“原始块”、按新包装入；换版之前的旧消息（BaseRevision 3 的覆盖块，版本再大）被丢弃，
// 不会把丢弃掉的覆盖块装回来；之后 A 在新包上改出版本 1，迟到的“原始块”消息按旧版本忽略。
func TestReplicaFollowsADiscardOnANewBaseRevision(t *testing.T) {
	oldPack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	newMeta := testMeta
	newMeta.BaseRevision = testMeta.BaseRevision + 1
	newPack, _ := writeTestPack(t, newMeta, Generated(newMeta, testSeed+1))

	// 换版之前：B 持有 A 的覆盖块，录下这条（BaseRevision 3、版本 7）。
	net := newReplicaNet()
	a := startReplicaProc(t, net, sidA, oldPack, nil, nil, nil)
	b := startReplicaProc(t, net, sidB, oldPack, nil, column0, nil)
	waitSubscribers(t, a, 1)
	a.commit(t, column0[0], 7, 7)
	waitVersion(t, b, column0[0], 7, "before the new base revision")
	stale := net.recordedTo(sidB)
	a.stop()
	b.stop()
	net.detach(sidA)
	net.detach(sidB)

	// 换版之后：B2 先起（这块显示不了），A2 后起（这块已丢弃成原始块）。
	b2 := startReplicaProc(t, net, sidB, newPack, []ChunkKey{column0[0]}, column0, nil)
	if got := b2.store.Held(column0[0]); got != -1 {
		t.Fatalf("held %d before the owner is up, want -1", got)
	}
	// 拥有者还是旧包上的进程（换版只换了 B）：它不答 B2 的订阅（不同包版本），B2 也不收它的任何块，这块仍显示不了。
	aOld := startReplicaProc(t, net, sidA, oldPack, nil, nil, nil)
	aOld.commit(t, column0[0], 8, 8)
	deadline := time.Now().Add(2 * time.Second)
	for aOld.r.Stats().BaseRevisionDrops < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the old owner took a subscription from another base revision: %+v", aOld.r.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	if s := aOld.r.Stats(); s.Subscribers != 0 || s.ChunksOut != 0 {
		t.Fatalf("the old owner kept or answered a subscription from another base revision: %+v", s)
	}
	if got := b2.store.Held(column0[0]); got != -1 || b2.r.Stats().Applied != 0 {
		t.Fatalf("a replica on the new pack took the old owner's state: held %d, stats %+v", got, b2.r.Stats())
	}
	aOld.stop()
	net.detach(sidA)
	a2 := startReplicaProc(t, net, sidA, newPack, nil, nil, nil)
	waitVersion(t, b2, column0[0], 0, "the owner's discard")
	sameData(t, a2.view, b2.view, column0[0])

	for _, m := range stale {
		b2.r.Deliver(m.from, m.body)
	}
	deadline = time.Now().Add(2 * time.Second)
	for b2.r.Stats().BaseRevisionDrops == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the old revision's messages were not dropped: %+v", b2.r.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	if got := b2.store.Held(column0[0]); got != 0 {
		t.Fatalf("an old revision's push brought the discarded overlay back: held %d", got)
	}

	a2.commit(t, column0[0], 1, 11)
	waitVersion(t, b2, column0[0], 1, "first edit on the new pack")
	late := encodeChunks(replicaTestCh, newMeta.BaseRevision, time.Now().UnixNano(), []chunkEntry{{key: column0[0], version: 0}})
	b2.r.Deliver(sidA, late)
	time.Sleep(20 * time.Millisecond)
	if got := b2.store.Held(column0[0]); got != 1 {
		t.Fatalf("a late base message moved the replica from version 1 to %d", got)
	}
	sameData(t, a2.view, b2.view, column0[0])
}

// 拥有者自己装不进的块（原始块坏了、记录装不进来）按原始块答（可编辑地形方案 §31.9 N4，维护者 2026-11-14 定 A）：
// 副本拿到 Version 0，有了说法，之后的续期不再要它；拥有者计 Unloaded。修前拥有者对这样的块不答，副本 Held 一直是 -1，
// 每次续期都再问一遍，game-demo 的客户端永远等不到这块的确认。
func TestReplicaAnswersAChunkTheOwnerCannotLoadAsBase(t *testing.T) {
	pack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	net := newReplicaNet()
	bad := column0[0]
	a := startReplicaProc(t, net, sidA, pack, []ChunkKey{bad}, nil, nil)
	b := startReplicaProc(t, net, sidB, pack, []ChunkKey{bad}, column0, nil)
	waitSubscribers(t, a, 1)
	waitVersion(t, b, bad, 0, "the owner's answer for a chunk it cannot load")
	if got := b.store.Held(bad); got != 0 {
		t.Fatalf("held %d after the owner's answer, want 0", got)
	}
	if s := a.r.Stats(); s.Unloaded == 0 {
		t.Fatalf("the owner did not count the chunk it cannot load: %+v", s)
	}
	// 有了说法之后的续期：拥有者不再为这块发任何东西。
	settled := a.r.Stats().ChunksOut
	time.Sleep(4 * testRenew)
	if s := a.r.Stats(); s.ChunksOut != settled || s.SubscribesIn < 2 {
		t.Fatalf("after the answer the renewals still fetched chunks: %d chunk states out (was %d), %d subscriptions in", s.ChunksOut, settled, s.SubscribesIn)
	}
}

// 只认拥有者；不想要的块不装；撤销订阅时 store 收到 Forget、拥有者的订阅表清掉；副本停了不续期，拥有者在租约之后删掉订阅。
func TestReplicaTrustsOnlyTheOwnerAndLetsGo(t *testing.T) {
	pack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	net := newReplicaNet()
	a := startReplicaProc(t, net, sidA, pack, nil, nil, nil)
	b := startReplicaProc(t, net, sidB, pack, nil, column0, nil)
	waitSubscribers(t, a, 1)

	c := randomChunk(5)
	data, err := encodeChunk(c)
	if err != nil {
		t.Fatal(err)
	}
	forged := encodeChunks(replicaTestCh, testMeta.BaseRevision, time.Now().UnixNano(), []chunkEntry{{key: column0[0], version: 9, data: data}})
	b.r.Deliver(3, forged) // 不是拥有者
	unwanted := encodeChunks(replicaTestCh, testMeta.BaseRevision, time.Now().UnixNano(), []chunkEntry{{key: ChunkKey{X: 0, Y: 1}, version: 9, data: data}})
	b.want(t, column0[:1])
	time.Sleep(20 * time.Millisecond)
	b.r.Deliver(sidA, unwanted) // 拥有者发的，但 B 已经不要 (0,1) 了
	deadline := time.Now().Add(2 * time.Second)
	for s := b.r.Stats(); s.WrongOwner == 0 || s.Unwanted == 0; s = b.r.Stats() {
		if time.Now().After(deadline) {
			t.Fatalf("stats %+v: the forged and the unwanted message were not counted", s)
		}
		time.Sleep(time.Millisecond)
	}
	if b.store.Held(column0[0]) != 0 || b.store.Held(ChunkKey{X: 0, Y: 1}) != 0 {
		t.Fatal("a message from a non-owner or for an unwanted chunk was applied")
	}
	b.store.mu.Lock()
	forgot := slices.Clone(b.store.forgotten)
	b.store.mu.Unlock()
	if !slices.Equal(forgot, []ChunkKey{{X: 0, Y: 1}}) {
		t.Fatalf("forgotten %v, want (0,1)", forgot)
	}

	b.want(t, nil)
	waitSubscribers(t, a, 0)

	b.want(t, column0)
	waitSubscribers(t, a, 1)
	b.stop()
	start := time.Now()
	waitSubscribers(t, a, 0)
	if elapsed := time.Since(start); elapsed < testLease-testRenew {
		t.Fatalf("the subscription went after %v, before its lease %v", elapsed, testLease)
	}
}

// 线上格式：往返、版本不同、截断、尾部多余字节、键不升序、计数越界都按坏消息拒绝。
func TestReplicaWireFormat(t *testing.T) {
	keys := []ChunkKey{{X: 0, Y: 0}, {X: 0, Y: 5}, {X: 3, Y: 1}}
	sub := encodeSubscribe(9, -4, keys, []int64{0, -1, 42}, nil)
	m, err := decodeReplicaMsg(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.(subscribeMsg); got.channel != 9 || got.rev != -4 || !slices.Equal(got.keys, keys) || !slices.Equal(got.held, []int64{0, -1, 42}) {
		t.Fatalf("subscribe round trip: %+v", got)
	}
	chunks := encodeChunks(2, 3, 123, []chunkEntry{{key: keys[2], version: 0}, {key: keys[0], version: 8, data: []byte{1, 2, 3}}})
	m, err = decodeReplicaMsg(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.(chunksMsg); got.channel != 2 || got.rev != 3 || got.sentAt != 123 || len(got.entries) != 2 || got.entries[1].version != 8 || string(got.entries[1].data) != "\x01\x02\x03" || got.entries[0].data != nil {
		t.Fatalf("chunks round trip: %+v", got)
	}
	withLots := encodeSubscribe(9, 1, keys, []int64{0, 0, 3}, []int64{-1, 0, 7})
	m, err = decodeReplicaMsg(withLots)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.(subscribeMsg); !slices.Equal(got.heldLots, []int64{-1, 0, 7}) {
		t.Fatalf("subscribe with lots round trip: %+v", got)
	}
	lotEntries := []chunkEntry{
		{key: keys[0], version: 4, noData: true, hasLots: true, lotRev: 9, lots: []int64{-5, 1 << 40}},
		{key: keys[1], version: 0, hasLots: true, lotRev: 0},
		{key: keys[2], version: 2, data: []byte{7}, hasLots: true, lotRev: 1, lots: []int64{3}},
	}
	m, err = decodeReplicaMsg(encodeChunks(1, 1, 0, lotEntries))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.(chunksMsg).entries; !slices.EqualFunc(got, lotEntries, func(a, b chunkEntry) bool {
		return a.key == b.key && a.version == b.version && a.noData == b.noData && a.hasLots == b.hasLots && a.lotRev == b.lotRev &&
			slices.Equal(a.lots, b.lots) && slices.Equal(a.data, b.data)
	}) {
		t.Fatalf("chunks with lots round trip: %+v", got)
	}
	badFlags := encodeChunks(1, 1, 0, []chunkEntry{{key: keys[0], version: 0}})
	badFlags[len(badFlags)-1] = 0x80
	tooManyLots := encodeChunks(1, 1, 0, []chunkEntry{{key: keys[0], version: 0, hasLots: true, lotRev: 1, lots: make([]int64, replicaMaxLots+1)}})
	badLotsFlag := slices.Clone(sub)
	badLotsFlag[len(badLotsFlag)-1] = 2
	bad := map[string][]byte{
		"empty":         nil,
		"version":       append([]byte{1}, sub[1:]...),
		"entry flags":   badFlags,
		"too many lots": tooManyLots,
		"lots flag":     badLotsFlag,
		"type":          {replicaWireVersion, 9, 0, 2},
		"truncated":     chunks[:len(chunks)-1],
		"trailing":      append(slices.Clone(sub), 0),
		"not sorted":    encodeSubscribe(1, 1, []ChunkKey{{X: 0, Y: 1}, {X: 0, Y: 1}}, []int64{0, 0}, nil),
		"too many":      {replicaWireVersion, replicaSubscribe, 0, 2, 0xff, 0xff, 0xff, 0x7f},
		"no revision":   {replicaWireVersion, replicaSubscribe, 0},
		"data too big":  encodeChunks(1, 1, 0, []chunkEntry{{key: keys[0], version: 1, data: make([]byte, replicaMaxData+1)}}),
	}
	for name, body := range bad {
		if _, err := decodeReplicaMsg(body); !errors.Is(err, errReplicaMessage) {
			t.Fatalf("%s: decoded (%v)", name, err)
		}
	}
}

// heldNothing 是没有任何数据的副本存放处：订阅上限的用例只看发出去的订阅。
type heldNothing struct{}

func (heldNothing) Held(ChunkKey) int64           { return 0 }
func (heldNothing) Apply(ChunkKey, Overlay) error { return nil }
func (heldNothing) Forget(ChunkKey)               {}

// subscriberRig 是只有副本一侧的 Replicator：发出的消息录在 sent 里（按目标 sid），时钟手动拨，不起 Run——用例在自己的
// goroutine 上直接调 takeInput / tick（与 Run 的循环是同两步）。
type subscriberRig struct {
	r    *Replicator
	now  time.Time
	sent map[int32][][]byte
}

func newSubscriberRig(t *testing.T, meta Meta, ownerOf func(ChunkKey) (int32, bool)) *subscriberRig {
	t.Helper()
	rig := &subscriberRig{now: time.Unix(1_700_000_000, 0), sent: map[int32][][]byte{}}
	r, err := NewReplicator(ReplicatorConfig{
		Self: sidB, Meta: meta, OwnerOf: ownerOf, Renew: testRenew, Lease: testLease,
		Now:  func() time.Time { return rig.now },
		Send: func(to int32, body []byte) bool { rig.sent[to] = append(rig.sent[to], body); return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddChannel(replicaTestCh, heldNothing{}); err != nil {
		t.Fatal(err)
	}
	rig.r = r
	return rig
}

// renewed 处理 Want 交来的输入，再把时钟拨过一个续期周期并 tick：返回这段时间发给 owner 的订阅（真实解码器解出来的块，
// 每条一份）。发出去的任何一条消息解不出来都算失败——接收端按坏消息丢弃，这份订阅永远建不起来。
func (rig *subscriberRig) renewed(t *testing.T, owner int32) [][]ChunkKey {
	t.Helper()
	clear(rig.sent)
	rig.r.takeInput()
	rig.now = rig.now.Add(testRenew)
	rig.r.tick()
	var subs [][]ChunkKey
	for to, bodies := range rig.sent {
		for _, body := range bodies {
			m, err := decodeReplicaMsg(body)
			if err != nil {
				t.Errorf("a subscription sent to sid %d cannot be decoded by the receiver: %v", to, err)
				continue
			}
			if sub, ok := m.(subscribeMsg); ok && to == owner {
				subs = append(subs, sub.keys)
			}
		}
	}
	return subs
}

// 一路副本对同一个拥有者的一份订阅至多 MaxSubscriptionChunks 块（RV-20261110-01）：订阅是一条消息，接收端的解码上限
// 就是这个数。超过时 Want 整份拒绝并返回 ErrSubscriptionTooLarge——不发出接收端必然解不了的消息；之前接受的那份订阅
// 照常续期。上限按拥有者分别算，只算真正要订阅的块（重复的、地图外的、本进程自己写的、没有拥有者的不算）。
func TestReplicaSubscriptionLimitPerOwner(t *testing.T) {
	meta := Meta{BaseRevision: 1, ChunksX: 256, ChunksY: 256}
	oneOwner := func(ChunkKey) (int32, bool) { return sidA, true }
	// first 返回地图里按 ID 升序的前 n 块（订阅里的次序）。
	first := func(n int) []ChunkKey {
		keys := make([]ChunkKey, n)
		for i := range keys {
			keys[i] = ChunkKey{X: int32(i / 256), Y: int32(i % 256)}
		}
		return keys
	}
	same := func(t *testing.T, what string, subs [][]ChunkKey, want ...[]ChunkKey) {
		t.Helper()
		if !slices.EqualFunc(subs, want, func(a, b []ChunkKey) bool { return slices.Equal(a, b) }) {
			sizes := make([]int, len(subs))
			for i, sub := range subs {
				sizes[i] = len(sub)
			}
			t.Errorf("%s: %d subscriptions of %v chunks, want %d", what, len(subs), sizes, len(want))
		}
	}

	t.Run("at the limit", func(t *testing.T) {
		rig, keys := newSubscriberRig(t, meta, oneOwner), first(MaxSubscriptionChunks)
		if err := rig.r.Want(replicaTestCh, keys); err != nil {
			t.Fatalf("Want of %d chunks: %v", len(keys), err)
		}
		// 一次是 Want 之后立即发的，一次是续期：两条都解得出来、都是整份。
		same(t, "subscribed and renewed", rig.renewed(t, sidA), keys, keys)
	})

	t.Run("one over the limit", func(t *testing.T) {
		rig := newSubscriberRig(t, meta, oneOwner)
		err := rig.r.Want(replicaTestCh, first(MaxSubscriptionChunks+1))
		same(t, "after a rejected Want", rig.renewed(t, sidA)) // 什么都不发：不留一份永远解不了的订阅在重试
		if !errors.Is(err, ErrSubscriptionTooLarge) {
			t.Fatalf("Want of %d chunks from one owner: %v, want ErrSubscriptionTooLarge", MaxSubscriptionChunks+1, err)
		}
		if got := rig.r.Stats(); got.WantsRejected != 1 || got.SubscribesOut != 0 {
			t.Fatalf("stats %+v: want one rejected Want and no subscription sent", got)
		}
	})

	t.Run("a rejected Want keeps the accepted subscription", func(t *testing.T) {
		rig, small := newSubscriberRig(t, meta, oneOwner), first(3)
		if err := rig.r.Want(replicaTestCh, small); err != nil {
			t.Fatal(err)
		}
		same(t, "the small subscription", rig.renewed(t, sidA), small, small)
		err := rig.r.Want(replicaTestCh, first(MaxSubscriptionChunks+1))
		same(t, "renewal after a rejected Want", rig.renewed(t, sidA), small) // 只有续期，还是原来那 3 块
		if !errors.Is(err, ErrSubscriptionTooLarge) {
			t.Fatalf("growing past the limit: %v, want ErrSubscriptionTooLarge", err)
		}
		// 之后合法的 Want 照常生效：换成另一份、撤销。
		if err := rig.r.Want(replicaTestCh, first(5)); err != nil {
			t.Fatal(err)
		}
		same(t, "the next accepted Want", rig.renewed(t, sidA), first(5), first(5))
		if err := rig.r.Want(replicaTestCh, nil); err != nil {
			t.Fatal(err)
		}
		same(t, "revoked", rig.renewed(t, sidA), nil)
	})

	t.Run("the limit is per owner", func(t *testing.T) {
		// 左半张图归 sid 1、右半张归 sid 3：各 128 × 256 = 32768 块，整张图 65536 块一次订阅，每个拥有者恰好到上限。
		split := int32(128)
		rig := newSubscriberRig(t, meta, func(key ChunkKey) (int32, bool) {
			if key.X < split {
				return sidA, true
			}
			return 3, true
		})
		all := first(256 * 256)
		if err := rig.r.Want(replicaTestCh, all); err != nil {
			t.Fatalf("65536 chunks, 32768 from each of two owners: %v", err)
		}
		left := all[:MaxSubscriptionChunks]
		same(t, "the left owner", rig.renewed(t, sidA), left, left)
		// 分界右移一列：左边的拥有者 129 × 256 块，超了；整份 Want 拒绝，两个拥有者的订阅都保持原样。
		split = 129
		err := rig.r.Want(replicaTestCh, all)
		same(t, "the left owner after a rejected Want", rig.renewed(t, sidA), left)
		if !errors.Is(err, ErrSubscriptionTooLarge) {
			t.Fatalf("33024 chunks from one owner: %v, want ErrSubscriptionTooLarge", err)
		}
	})

	t.Run("only chunks that are subscribed count", func(t *testing.T) {
		// 第 255 列归本进程自己写、第 254 列没有拥有者：都不订阅。
		rig := newSubscriberRig(t, meta, func(key ChunkKey) (int32, bool) {
			switch key.X {
			case 255:
				return sidB, true
			case 254:
				return 0, false
			}
			return sidA, true
		})
		keys := first(MaxSubscriptionChunks)
		extra := slices.Concat(keys, keys[:100], []ChunkKey{{X: 256, Y: 0}, {X: -1, Y: 3}, {X: 255, Y: 7}, {X: 254, Y: 7}})
		if err := rig.r.Want(replicaTestCh, extra); err != nil {
			t.Fatalf("%d subscribable chunks plus duplicates and chunks that are never subscribed: %v", len(keys), err)
		}
		same(t, "subscribed", rig.renewed(t, sidA), keys, keys)
	})
}

// lotStore 是想要占位记录的存放处（ReplicaLotStore）：块数据照 viewStore，占位记录按修订号只进不退（与 game-demo 的
// Map.ApplyReplicaLots 同一规则）。
type lotStore struct {
	*viewStore
	lmu  sync.Mutex
	lots map[ChunkKey]heldLots
}

type heldLots struct {
	rev  int64
	lots []int64
}

func (s *lotStore) HeldLots(key ChunkKey) int64 {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return s.lots[key].rev
}

func (s *lotStore) ApplyLots(key ChunkKey, rev int64, lots []int64) error {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	if rev <= s.lots[key].rev {
		return fmt.Errorf("%w: lots revision %d, holding %d", ErrStaleOverlay, rev, s.lots[key].rev)
	}
	s.lots[key] = heldLots{rev: rev, lots: slices.Clone(lots)}
	return nil
}

func (s *lotStore) held(key ChunkKey) heldLots {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return s.lots[key]
}

// ownerLots 是拥有者一侧的占位记录（ReplicatorConfig.Lots）：测试直接改、再 Changed。
type ownerLots struct {
	mu   sync.Mutex
	lots map[ChunkKey]heldLots
}

func (o *ownerLots) get(key ChunkKey) (int64, []int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	l := o.lots[key]
	return l.rev, l.lots
}

func (o *ownerLots) set(key ChunkKey, rev int64, lots ...int64) {
	o.mu.Lock()
	o.lots[key] = heldLots{rev: rev, lots: lots}
	o.mu.Unlock()
}

func waitLots(t *testing.T, p *replicaProc, store *lotStore, key ChunkKey, rev int64, want []int64, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := store.held(key); got.rev == rev && slices.Equal(got.lots, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: chunk %v holds lots %+v after 2s, want revision %d %v (stats %+v)", what, key, store.held(key), rev, want, p.r.Stats())
		}
		time.Sleep(time.Millisecond)
	}
}

// 占位记录（P5：分区场景邻格的建筑占地进 Ghost 带）：拥有者改了占位（块数据不变）之后推到想要它的副本；推送全丢时
// 续期的答复补齐，且只差占位时答复不带块数据；录下的旧消息倒序重放不回退；之后的块提交照常到、不动占位；不要占位记录
// 的一路（存放处没实现 ReplicaLotStore）订阅里没有修订号，收到的块状态也不带占位记录。
func TestReplicaCarriesTheOwnersLots(t *testing.T) {
	pack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	net := newReplicaNet()
	owner := &ownerLots{lots: map[ChunkKey]heldLots{}}
	a := startReplicaProc(t, net, sidA, pack, nil, nil, func(cfg *ReplicatorConfig) { cfg.Lots = owner.get })
	b := &replicaProc{sid: sidB, view: NewView(pack)}
	lots := &lotStore{viewStore: &viewStore{view: b.view, unloadable: map[ChunkKey]bool{}}, lots: map[ChunkKey]heldLots{}}
	b.store = lots.viewStore
	for key := range ChunksIn(pack.Meta().Bounds()) {
		if err := b.view.LoadBase(key); err != nil {
			t.Fatal(err)
		}
	}
	b.startReplicatorWith(t, net, column0, lots)
	const sidC int32 = 3
	c := startReplicaProc(t, net, sidC, pack, nil, column0, nil) // 不要占位记录的一路
	waitSubscribers(t, a, 2)

	owner.set(column0[0], 1, 11, -22)
	a.r.Changed(column0[0])
	waitLots(t, b, lots, column0[0], 1, []int64{11, -22}, "first push")

	// 推送全丢：续期的答复补上（块数据没变，答复不带它）。
	net.setFault(func(from, to int32, body []byte) (bool, bool, time.Duration) {
		return from == sidA && to == sidB && body[1] == replicaChunks, false, 0
	})
	owner.set(column0[0], 2, 33)
	a.r.Changed(column0[0])
	time.Sleep(testRenew / 2)
	net.setFault(nil)
	waitLots(t, b, lots, column0[0], 2, []int64{33}, "renewal after a lost push")
	toB := net.recordedTo(sidB)
	last, err := decodeReplicaMsg(toB[len(toB)-1].body)
	if err != nil {
		t.Fatal(err)
	}
	if e := last.(chunksMsg).entries[0]; !e.hasLots || !e.noData || e.data != nil {
		t.Fatalf("the renewal's answer %+v: want lots only, no chunk data", e)
	}

	// 倒序重放录下的旧消息：占位不回退。
	for i := len(toB) - 1; i >= 0; i-- {
		b.r.Deliver(toB[i].from, toB[i].body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.r.Stats().LotsStale == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("replayed lots never reached the store: %+v", b.r.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	if got := lots.held(column0[0]); got.rev != 2 {
		t.Fatalf("replayed old messages moved the lots back to %+v", got)
	}

	// 块提交照常推到，占位记录不变。
	a.commit(t, column0[0], 1, 5)
	waitVersion(t, b, column0[0], 1, "a commit after the lots")
	sameData(t, a.view, b.view, column0[0])
	if got := lots.held(column0[0]); got.rev != 2 || !slices.Equal(got.lots, []int64{33}) {
		t.Fatalf("lots after a data commit: %+v", got)
	}

	// 不要占位记录的一路：块数据照常，收到的块状态没有一条带占位记录。
	waitVersion(t, c, column0[0], 1, "the replica without lots")
	for _, m := range net.recordedTo(sidC) {
		decoded, err := decodeReplicaMsg(m.body)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range decoded.(chunksMsg).entries {
			if e.hasLots {
				t.Fatalf("a replica that wants no lots got them: %+v", e)
			}
		}
	}
	if s := b.r.Stats(); s.DecodeErrors != 0 || s.ApplyErrors != 0 || s.LotsApplied < 2 {
		t.Fatalf("replica stats %+v", s)
	}
}

// 测量（ROOST_TERRAIN_REPLICA_PERF=1）：进程内网上，一次提交（视图换版 + Changed）到副本应用完的延迟（含拥有者读块、
// 编码、副本解码、装入），与一块推送的字节。数据是原始块上抬高一个 r=5m 的圆（与玩家小笔刷相同的改法）。真实 NATS
// 上的数见 cellworld 的 TestTerrainReplicaOverNodes。
func TestReplicaMeasure(t *testing.T) {
	if os.Getenv("ROOST_TERRAIN_REPLICA_PERF") != "1" {
		t.Skip("set ROOST_TERRAIN_REPLICA_PERF=1")
	}
	pack, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	net := newReplicaNet()
	applied := make(chan int64, 16)
	a := startReplicaProc(t, net, sidA, pack, nil, nil, nil)
	startReplicaProc(t, net, sidB, pack, nil, column0, func(cfg *ReplicatorConfig) {
		cfg.Applied = func(_ ReplicaChannel, _ ChunkKey, version int64, _ time.Duration) { applied <- version }
	})
	waitSubscribers(t, a, 1)
	base, err := pack.ReadChunk(column0[0])
	if err != nil {
		t.Fatal(err)
	}
	const n = 300
	latencies := make([]time.Duration, 0, n)
	before := a.r.Stats()
	for v := int64(1); v <= n; v++ {
		c := base.Clone()
		cx, cy := int(v*7)%54+5, int(v*13)%54+5
		for j := cy - 5; j <= cy+5; j++ {
			for i := cx - 5; i <= cx+5; i++ {
				if (i-cx)*(i-cx)+(j-cy)*(j-cy) <= 25 {
					c.Heights[j*ChunkSamples+i] += uint16(v % 50)
				}
			}
		}
		start := time.Now()
		if err := a.view.SetOverlay(column0[0], Overlay{BaseRevision: testMeta.BaseRevision, Version: v, Heights: c.Heights, Materials: c.Materials}); err != nil {
			t.Fatal(err)
		}
		a.r.Changed(column0[0])
		for got := <-applied; got != v; got = <-applied {
		}
		latencies = append(latencies, time.Since(start))
	}
	after := a.r.Stats()
	slices.Sort(latencies)
	t.Logf("commit → replica applied over the in-process net: n=%d p50=%v p90=%v p99=%v max=%v; push bytes per chunk %.0f (heights + materials raw 12288)",
		n, latencies[n/2], latencies[n*9/10], latencies[n*99/100], latencies[n-1],
		float64(after.ChunkBytesOut-before.ChunkBytesOut)/float64(after.ChunksOut-before.ChunksOut))
}
