// 远端只读对象（mirror）：不是本进程 Nest 实体的对象作为 subject 与本地实体同处一个 Manager、同一会话流：
// 全量 / 增量 / 删除，按 (epoch, seq) 丢旧，墓碑挡住迟到的旧版本，过期清理，删除后同 ID 重新出现走 remove-before-create。

package policy

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// mirrorContent 是测试里 mirror 的内容：位置与一个标签（打进载荷，客户端据此判断收到的是哪一版）。
type mirrorContent struct {
	At    spatial.Point
	Label string
}

// streamClient 是一个按会话解帧的客户端：ref → subject、subject → 最后一份载荷，以及按交付顺序的操作记录。
type streamClient struct {
	mu       sync.Mutex
	refs     map[entitysync.SessionID]map[frame.ObjectRef]int64
	objects  map[entitysync.SessionID]map[int64]string
	ops      map[entitysync.SessionID][]string
	decoders map[entitysync.SessionID]*entitysync.Decoder
}

func (c *streamClient) Push(_ context.Context, session entitysync.SessionID, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs == nil {
		c.refs = make(map[entitysync.SessionID]map[frame.ObjectRef]int64)
		c.objects = make(map[entitysync.SessionID]map[int64]string)
		c.ops = make(map[entitysync.SessionID][]string)
		c.decoders = make(map[entitysync.SessionID]*entitysync.Decoder)
	}
	if c.decoders[session] == nil {
		c.decoders[session] = entitysync.NewDecoder(frame.DefaultLimits())
	}
	decoded, err := c.decoders[session].Decode(data)
	if err != nil {
		return err
	}
	if c.refs[session] == nil {
		c.refs[session] = make(map[frame.ObjectRef]int64)
		c.objects[session] = make(map[int64]string)
	}
	refs, objects := c.refs[session], c.objects[session]
	for _, object := range decoded.Objects {
		subject := refs[object.Ref]
		payload := ""
		for _, component := range object.Components {
			update := component.Update
			subject = update.SubjectID
			kind := "delta"
			if update.Full {
				kind = "full"
			}
			payload = kind + ":" + string(update.Payload.BytesCopy())
		}
		switch object.Operation {
		case frame.ObjectCreate:
			refs[object.Ref] = subject
			objects[subject] = payload
			c.ops[session] = append(c.ops[session], fmt.Sprintf("create:%d:%s", subject, payload))
		case frame.ObjectUpdate:
			objects[subject] = payload
			c.ops[session] = append(c.ops[session], fmt.Sprintf("update:%d:%s", subject, payload))
		case frame.ObjectRemove:
			delete(refs, object.Ref)
			delete(objects, subject)
			c.ops[session] = append(c.ops[session], fmt.Sprintf("remove:%d", subject))
		}
	}
	return nil
}

// take 取走并清空会话的操作记录。
func (c *streamClient) take(session entitysync.SessionID) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.ops[session]
	delete(c.ops, session)
	return out
}

// holds 返回会话当前持有的 subject 与载荷。
func (c *streamClient) holds(session entitysync.SessionID) map[int64]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[int64]string, len(c.objects[session]))
	for id, payload := range c.objects[session] {
		out[id] = payload
	}
	return out
}

type mirrorHarness struct {
	t        *testing.T
	manager  *entitysync.Manager
	interest *Interest
	client   *streamClient
	mirrors  *Mirrors[mirrorContent]
}

// newMirrorHarness：一个 Manager、一个 Interest（与 newInterest 相同的 AOI）、一个解帧客户端；
// 玩家 1 是本地实体 subject（subjectState，载荷 "snapshot"/"delta"），在 (100,100) 进入。
func newMirrorHarness(t *testing.T, withDelta bool) *mirrorHarness {
	t.Helper()
	client := &streamClient{}
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	interest := newInterest(t, manager)
	config := MirrorConfig[mirrorContent]{
		Manager: manager, Interest: interest,
		Position: func(c mirrorContent) spatial.Point { return c.At },
		Snapshot: func(c mirrorContent, _ entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return entity.CopyFrozenSyncPayload(7, []byte(c.Label)), nil
		},
		TTL: time.Second,
	}
	if withDelta {
		config.Delta = func(c mirrorContent, _ entity.SyncProfile, mask uint64) (entity.FrozenSyncPayload, error) {
			return entity.CopyFrozenSyncPayload(7, []byte(fmt.Sprintf("%s/m%d", c.Label, mask))), nil
		}
	}
	mirrors, err := NewMirrors(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mirrors.Close)
	player(t, manager, 1)
	mustEnter(t, interest, 1, spatial.Point{X: 100, Y: 100})
	h := &mirrorHarness{t: t, manager: manager, interest: interest, client: client, mirrors: mirrors}
	h.flush()
	if got := client.take(1); !slices.Equal(got, []string{"create:1:full:snapshot"}) {
		t.Fatalf("setup: player 1 sees %v", got)
	}
	return h
}

// flush 走两次 Flush：第一次的政策阶段把 Show / Move / Hide 说给 Manager，第二次把新订阅的全量与变化发出。
func (h *mirrorHarness) flush() {
	h.t.Helper()
	for range 2 {
		flushInterest(h.t, h.interest, h.manager)
	}
}

func (h *mirrorHarness) apply(id int64, ver MirrorVersion, at spatial.Point, label string, mask uint64, now time.Time) MirrorOutcome {
	h.t.Helper()
	outcome, err := h.mirrors.Apply(MirrorItem[mirrorContent]{
		ID: id, Namespace: "monster", Kind: 4, Version: ver, Content: mirrorContent{At: at, Label: label}, Mask: mask,
	}, now)
	if err != nil {
		h.t.Fatalf("apply %d %v: %v", id, ver, err)
	}
	return outcome
}

func (h *mirrorHarness) expectOps(want ...string) {
	h.t.Helper()
	h.flush()
	if got := h.client.take(1); !slices.Equal(got, want) {
		h.t.Fatalf("player 1 operations = %v, want %v", got, want)
	}
}

var mirrorNear = spatial.Point{X: 120, Y: 100}

// 远端对象与本地实体在同一个会话流里：mirror 作为 subject 进入视野，客户端收到它的全量；同一会话仍持有本地实体。
func TestMirrorJoinsTheSameSessionAsALocalEntity(t *testing.T) {
	h := newMirrorHarness(t, false)
	now := time.Unix(1000, 0)
	if got := h.apply(500, MirrorVersion{Epoch: 1, Seq: 1}, mirrorNear, "a1", 0, now); got != MirrorCreated {
		t.Fatalf("first apply = %v, want created", got)
	}
	h.expectOps("create:500:full:a1")
	if got := h.client.holds(1); got[1] != "full:snapshot" || got[500] != "full:a1" {
		t.Fatalf("player 1 holds %v, want its own entity and the mirror", got)
	}
	if content, ver, ok := h.mirrors.Get(500); !ok || content.Label != "a1" || ver != (MirrorVersion{Epoch: 1, Seq: 1}) {
		t.Fatalf("Get = %+v %v %v", content, ver, ok)
	}
}

// 按 (epoch, seq) 丢旧：同 epoch 更小的 seq、相同版本都被丢弃且不产生帧；更大的 epoch 即使 seq 更小也是新的。
func TestMirrorDropsVersionsThatAreNotNewer(t *testing.T) {
	h := newMirrorHarness(t, false)
	now := time.Unix(1000, 0)
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 5}, mirrorNear, "e1s5", 0, now)
	h.expectOps("create:500:full:e1s5")
	for _, stale := range []MirrorVersion{{Epoch: 1, Seq: 4}, {Epoch: 1, Seq: 5}, {Epoch: 0, Seq: 99}} {
		if got := h.apply(500, stale, mirrorNear, "stale", 0, now); got != MirrorStale {
			t.Fatalf("apply %v = %v, want stale", stale, got)
		}
	}
	h.expectOps()
	if got := h.apply(500, MirrorVersion{Epoch: 2, Seq: 1}, mirrorNear, "e2s1", 0, now); got != MirrorUpdated {
		t.Fatalf("newer epoch = %v, want updated", got)
	}
	h.expectOps("update:500:full:e2s1")
}

// 增量：给了 Delta 且 Mask 非零时客户端收到增量（packer 拿到的是 mask），Mask 为 0 时收到全量。
func TestMirrorSendsDeltaForMaskAndFullForZeroMask(t *testing.T) {
	h := newMirrorHarness(t, true)
	now := time.Unix(1000, 0)
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 1}, mirrorNear, "a", 0, now)
	h.expectOps("create:500:full:a")
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 2}, mirrorNear, "b", 0b10, now)
	h.expectOps("update:500:delta:b/m2")
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 3}, mirrorNear, "c", 0, now)
	h.expectOps("update:500:full:c")
}

// 显式删除：客户端收到 remove；墓碑挡住不新于删除版本的迟到更新；更新的版本让对象重新出现（remove 先于 create）。
// 重新出现时上一次的退役可能还没发完 remove（这里在同一次 Flush 之前就重新 Apply），也不能丢。
func TestMirrorRemoveTombstonesAndRecreates(t *testing.T) {
	h := newMirrorHarness(t, false)
	now := time.Unix(1000, 0)
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 3}, mirrorNear, "a", 0, now)
	h.expectOps("create:500:full:a")
	if !h.mirrors.Remove(500, MirrorVersion{Epoch: 1, Seq: 4}, now) {
		t.Fatal("remove of a known mirror returned false")
	}
	h.expectOps("remove:500")
	for _, late := range []MirrorVersion{{Epoch: 1, Seq: 3}, {Epoch: 1, Seq: 4}} {
		if got := h.apply(500, late, mirrorNear, "late", 0, now); got != MirrorTombstoned {
			t.Fatalf("late apply %v = %v, want tombstoned", late, got)
		}
	}
	h.expectOps()
	if got := h.mirrors.Len(); got != 0 {
		t.Fatalf("Len after remove = %d", got)
	}
	// 同一拍里删除又重新出现。
	h.apply(500, MirrorVersion{Epoch: 2, Seq: 1}, mirrorNear, "b", 0, now)
	h.mirrors.Remove(500, MirrorVersion{Epoch: 2, Seq: 2}, now)
	h.apply(500, MirrorVersion{Epoch: 3, Seq: 1}, mirrorNear, "c", 0, now)
	h.flush()
	h.flush()
	ops := h.client.take(1)
	if len(ops) == 0 || ops[len(ops)-1] != "create:500:full:c" {
		t.Fatalf("player 1 operations after remove and recreate = %v, want the last to create c", ops)
	}
	if got := h.client.holds(1)[500]; got != "full:c" {
		t.Fatalf("player 1 holds %q for 500, want full:c", got)
	}
}

// 过期：TTL 内没有 Apply 的 mirror 被 Expire 删除，客户端收到 remove；TTL 内更新过的不动。
func TestMirrorExpiresObjectsNotHeardFrom(t *testing.T) {
	h := newMirrorHarness(t, false)
	start := time.Unix(1000, 0)
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 1}, mirrorNear, "quiet", 0, start)
	h.apply(501, MirrorVersion{Epoch: 1, Seq: 1}, spatial.Point{X: 110, Y: 110}, "busy", 0, start)
	h.flush()
	h.client.take(1)
	h.apply(501, MirrorVersion{Epoch: 1, Seq: 2}, spatial.Point{X: 110, Y: 110}, "busy2", 0, start.Add(800*time.Millisecond))
	if got := h.mirrors.Expire(start.Add(1500 * time.Millisecond)); got != 1 {
		t.Fatalf("Expire removed %d, want 1", got)
	}
	h.flush()
	// 同一帧里对象的先后由 Manager 决定，这里只比集合。
	got := h.client.take(1)
	slices.Sort(got)
	if want := []string{"remove:500", "update:501:full:busy2"}; !slices.Equal(got, want) {
		t.Fatalf("player 1 operations = %v, want %v", got, want)
	}
	if _, _, ok := h.mirrors.Get(500); ok {
		t.Fatal("expired mirror still known")
	}
}

// 位置：mirror 走出观察者的视野半径时客户端收到 remove，走回来时重新 create（AOI 按 mirror 的位置判定）。
func TestMirrorPositionDrivesVisibility(t *testing.T) {
	h := newMirrorHarness(t, false)
	now := time.Unix(1000, 0)
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 1}, mirrorNear, "here", 0, now)
	h.expectOps("create:500:full:here")
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 2}, spatial.Point{X: 900, Y: 900}, "far", 0, now)
	h.expectOps("remove:500")
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 3}, mirrorNear, "back", 0, now)
	h.expectOps("create:500:full:back")
}

// 并发：多个 goroutine 交错 Apply 同一批对象（不同来源、乱序版本），最终每个对象的内容都是最大版本。
func TestMirrorConcurrentAppliesConvergeOnTheNewestVersion(t *testing.T) {
	h := newMirrorHarness(t, false)
	now := time.Unix(1000, 0)
	const objects, versions, writers = 8, 50, 4
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := range versions {
				seq := uint64((v*7+w*13)%versions + 1)
				for id := range objects {
					_, _ = h.mirrors.Apply(MirrorItem[mirrorContent]{
						ID: int64(600 + id), Namespace: "monster", Kind: 4,
						Version: MirrorVersion{Epoch: 1, Seq: seq},
						Content: mirrorContent{At: mirrorNear, Label: fmt.Sprintf("s%d", seq)},
					}, now)
				}
			}
		}()
	}
	wg.Wait()
	h.flush()
	holds := h.client.holds(1)
	for id := range objects {
		content, ver, ok := h.mirrors.Get(int64(600 + id))
		if !ok || ver.Seq != versions || content.Label != fmt.Sprintf("s%d", versions) {
			t.Fatalf("mirror %d = %+v %v %v, want seq %d", 600+id, content, ver, ok, versions)
		}
		if got := holds[int64(600+id)]; got != fmt.Sprintf("full:s%d", versions) {
			t.Fatalf("player 1 holds %q for %d", got, 600+id)
		}
	}
}

// Close 删除全部 mirror（订阅者收到 remove），之后的 Apply 返回 ErrMirrorsClosed。
func TestMirrorCloseRemovesEverything(t *testing.T) {
	h := newMirrorHarness(t, false)
	now := time.Unix(1000, 0)
	h.apply(500, MirrorVersion{Epoch: 1, Seq: 1}, mirrorNear, "a", 0, now)
	h.expectOps("create:500:full:a")
	h.mirrors.Close()
	h.expectOps("remove:500")
	if _, err := h.mirrors.Apply(MirrorItem[mirrorContent]{ID: 501, Version: MirrorVersion{Epoch: 1, Seq: 1}, Content: mirrorContent{At: mirrorNear}}, now); err == nil {
		t.Fatal("apply after Close succeeded")
	}
}

// 保留时间按来源：Hold 给出的对象按它过期（队伍快照每秒一份、保留 3s），没给的按 TTL（witness 每拍一份、保留 1s）；
// 同一对象的保留时间跟着最近一次被接受的那份走：长保留的对象被一份短保留的更新覆盖后，按短的过期。
func TestMirrorHoldOverridesTTLPerItem(t *testing.T) {
	h := newMirrorHarness(t, false)
	start := time.Unix(1000, 0)
	hold := func(id int64, ver MirrorVersion, label string, d time.Duration, now time.Time) {
		t.Helper()
		if _, err := h.mirrors.Apply(MirrorItem[mirrorContent]{
			ID: id, Namespace: "monster", Kind: 4, Version: ver, Content: mirrorContent{At: mirrorNear, Label: label}, Hold: d,
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	hold(500, MirrorVersion{Epoch: 1, Seq: 1}, "team", 3*time.Second, start)
	h.apply(501, MirrorVersion{Epoch: 1, Seq: 1}, mirrorNear, "witness", 0, start)
	hold(502, MirrorVersion{Epoch: 1, Seq: 1}, "team", 3*time.Second, start)
	h.apply(502, MirrorVersion{Epoch: 1, Seq: 2}, mirrorNear, "witness", 0, start.Add(100*time.Millisecond))
	h.flush()
	h.client.take(1)
	if got := h.mirrors.Expire(start.Add(1500 * time.Millisecond)); got != 2 {
		t.Fatalf("at 1.5s Expire removed %d, want 2 (the TTL-held 501 and 502, whose last accepted item had no hold)", got)
	}
	if _, _, ok := h.mirrors.Get(500); !ok {
		t.Fatal("the mirror held for 3s expired at 1.5s")
	}
	if got := h.mirrors.Expire(start.Add(2900 * time.Millisecond)); got != 0 {
		t.Fatalf("at 2.9s Expire removed %d, want 0", got)
	}
	if got := h.mirrors.Expire(start.Add(3100 * time.Millisecond)); got != 1 {
		t.Fatalf("at 3.1s Expire removed %d, want 1 (the 3s hold ran out)", got)
	}
	// 墓碑至少保留 2 × 保留时间：6s 时清理过期墓碑之后，迟到的旧快照仍不能让它复活。
	h.mirrors.Expire(start.Add(6 * time.Second))
	if outcome := h.apply(500, MirrorVersion{Epoch: 1, Seq: 1}, mirrorNear, "late", 0, start.Add(7*time.Second)); outcome != MirrorTombstoned {
		t.Fatalf("a late copy of the expired 3s-held mirror came back as %v, want tombstoned", outcome)
	}
}
