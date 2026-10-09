package attribute

import (
	"testing"
	"time"
)

// RR-20261005-NC-60：Container.Snapshot 的注释承诺“副本在容器锁下取得，快照永远不会读到本容器
// 自身修改方法（Apply / ClearDirty）的半截写入”。旧实现取到 profile 指针后先 RUnlock，再在锁外
// CloneProfile：Apply 可以在复制进行中拿到写锁并改写同一个 profile，快照读到撕裂状态，-race 下
// 也是数据竞争。这里用钩子把 Snapshot 停在复制中途，再发起 Apply，有界等待它是否进入 LoadValues。

// gatedProfile 在 CloneProfile 和 LoadValues 入口各发一个信号，CloneProfile 停在 release 上。
type gatedProfile struct {
	stubProfile
	cloning     chan struct{}
	release     chan struct{}
	applyEnters chan struct{}
}

func (p *gatedProfile) CloneProfile() Profile {
	close(p.cloning)
	<-p.release
	return p.stubProfile.CloneProfile()
}

func (p *gatedProfile) LoadValues(values map[AttrID]AttrValue) uint64 {
	close(p.applyEnters)
	return p.stubProfile.LoadValues(values)
}

func TestSnapshotCopiesUnderTheContainerLock(t *testing.T) {
	container := NewContainer()
	live := &gatedProfile{
		stubProfile: stubProfile{values: map[AttrID]AttrValue{1: 10, 2: 20}},
		cloning:     make(chan struct{}),
		release:     make(chan struct{}),
		applyEnters: make(chan struct{}),
	}
	container.Install(Base, live)

	snapshotDone := make(chan Snapshot, 1)
	go func() { snapshotDone <- container.Snapshot(Base) }()
	select {
	case <-live.cloning:
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot never started copying the layer")
	}

	applyDone := make(chan uint64, 1)
	go func() { applyDone <- container.Apply(Base, map[AttrID]AttrValue{1: 11, 2: 21}) }()

	// Apply must wait for the copy: while CloneProfile is running the
	// container lock is held, so LoadValues cannot start. A bounded wait is
	// the assertion — with the lock held it can only time out.
	select {
	case <-live.applyEnters:
		close(live.release)
		t.Fatal("Apply entered LoadValues while Snapshot was still copying the same profile: the copy is not taken under the container lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(live.release)

	snapshot := <-snapshotDone
	select {
	case <-applyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Apply never finished after the snapshot released the lock")
	}
	for id, want := range map[AttrID]AttrValue{1: 10, 2: 20} {
		if got, _ := snapshot.Profile.GetAttr(id); got != want {
			t.Fatalf("snapshot attr %d = %d, want the pre-Apply %d", id, got, want)
		}
	}
	if got, _ := live.GetAttr(1); got != 11 {
		t.Fatalf("Apply did not land after the snapshot: attr 1 = %d", got)
	}
}
