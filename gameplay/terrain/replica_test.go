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
	if err := r.AddChannel(replicaTestCh, p.store); err != nil {
		t.Fatal(err)
	}
	p.r = r
	net.attach(p.sid, r)
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() { defer close(p.done); _ = r.Run(ctx) }()
	r.Want(replicaTestCh, want)
	t.Cleanup(p.stop)
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
	b.r.Want(replicaTestCh, column0[:1])
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

	b.r.Want(replicaTestCh, nil)
	waitSubscribers(t, a, 0)

	b.r.Want(replicaTestCh, column0)
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
	sub := encodeSubscribe(9, -4, keys, []int64{0, -1, 42})
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
	bad := map[string][]byte{
		"empty":        nil,
		"version":      append([]byte{2}, sub[1:]...),
		"type":         {replicaWireVersion, 9, 0, 2},
		"truncated":    chunks[:len(chunks)-1],
		"trailing":     append(slices.Clone(sub), 0),
		"not sorted":   encodeSubscribe(1, 1, []ChunkKey{{X: 0, Y: 1}, {X: 0, Y: 1}}, []int64{0, 0}),
		"too many":     {replicaWireVersion, replicaSubscribe, 0, 2, 0xff, 0xff, 0xff, 0x7f},
		"no revision":  {replicaWireVersion, replicaSubscribe, 0},
		"data too big": encodeChunks(1, 1, 0, []chunkEntry{{key: keys[0], version: 1, data: make([]byte, replicaMaxData+1)}}),
	}
	for name, body := range bad {
		if _, err := decodeReplicaMsg(body); !errors.Is(err, errReplicaMessage) {
			t.Fatalf("%s: decoded (%v)", name, err)
		}
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
