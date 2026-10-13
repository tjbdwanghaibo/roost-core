package attribute

import (
	"testing"
	"time"
)

// attribute 容器的框架承诺：快照交出去的是副本（读者改不动容器里的那份）、空层礼貌回答而不是
// panic、未命名的 Selector 就是 Base、Apply / Dirty / ClearDirty 按层隔离。
// 历史：U-0230 / RR-20260917-06

// stubProfile is the minimum a generated profile does: hold values, track a
// dirty mask, and clone deeply.
type stubProfile struct {
	values    map[AttrID]AttrValue
	dirtyMask uint64
}

func newStub() *stubProfile { return &stubProfile{values: map[AttrID]AttrValue{}} }

func (p *stubProfile) Reset() { p.values = map[AttrID]AttrValue{}; p.dirtyMask = 0 }
func (p *stubProfile) CloneProfile() Profile {
	clone := newStub()
	for id, value := range p.values {
		clone.values[id] = value
	}
	clone.dirtyMask = p.dirtyMask
	return clone
}
func (p *stubProfile) DirtyMask() uint64 { return p.dirtyMask }
func (p *stubProfile) ClearDirty()       { p.dirtyMask = 0 }
func (p *stubProfile) SetAttr(id AttrID, value AttrValue) bool {
	if p.values[id] == value {
		return false
	}
	p.values[id] = value
	p.dirtyMask |= 1 << uint(id)
	return true
}
func (p *stubProfile) SetDirectAttr(id AttrID, value AttrValue) bool { return p.SetAttr(id, value) }
func (p *stubProfile) GetAttr(id AttrID) (AttrValue, bool) {
	value, ok := p.values[id]
	return value, ok
}
func (p *stubProfile) LoadValues(values map[AttrID]AttrValue) uint64 {
	before := p.dirtyMask
	for id, value := range values {
		p.SetDirectAttr(id, value)
	}
	return p.dirtyMask &^ before
}
func (p *stubProfile) ExportValues(dst map[AttrID]AttrValue) {
	for id, value := range p.values {
		dst[id] = value
	}
}
func (p *stubProfile) Update() bool                   { return false }
func (p *stubProfile) MetaByID(AttrID) (Meta, bool)   { return Meta{}, false }
func (p *stubProfile) MetaByName(string) (Meta, bool) { return Meta{}, false }

func TestSnapshotHandsOutACopy(t *testing.T) {
	container := NewContainer()
	live := newStub()
	live.SetAttr(1, 10)
	container.Install(Base, live)

	snapshot := container.Snapshot(Base)
	if snapshot.Profile == nil {
		t.Fatal("snapshot of an installed layer has no profile")
	}
	if value, _ := snapshot.Profile.GetAttr(1); value != 10 {
		t.Fatalf("snapshot value = %d", value)
	}
	snapshot.Profile.SetAttr(1, 99)
	if value, _ := live.GetAttr(1); value != 10 {
		t.Fatalf("mutating the snapshot reached the container's profile: %d", value)
	}
}

func TestEmptyLayerAnswersInsteadOfPanicking(t *testing.T) {
	container := NewContainer()
	snapshot := container.Snapshot(Selector{Layer: "missing"})
	if snapshot.Profile != nil {
		t.Fatal("an empty layer produced a profile")
	}
	if snapshot.Selector.Layer != "missing" {
		t.Fatalf("the snapshot forgot which layer was asked for: %q", snapshot.Selector.Layer)
	}
	if mask := container.Dirty(Selector{Layer: "missing"}); mask != 0 {
		t.Fatalf("empty layer reported dirty bits: %d", mask)
	}
	if applied := container.Apply(Selector{Layer: "missing"}, map[AttrID]AttrValue{1: 1}); applied != 0 {
		t.Fatalf("applying to an empty layer reported %d", applied)
	}
	container.ClearDirty(Selector{Layer: "missing"}) // must not panic
	// A nil container is the zero value a game may hold before it builds one.
	var nilContainer *Container
	if snapshot := nilContainer.Snapshot(Base); snapshot.Profile != nil {
		t.Fatal("a nil container produced a profile")
	}
	if _, ok := nilContainer.Live(Base); ok {
		t.Fatal("a nil container reported a live profile")
	}
}

// An unnamed selector is Base: a game that only ever has one layer should not
// have to spell it.
func TestZeroSelectorIsBase(t *testing.T) {
	container := NewContainer()
	live := newStub()
	live.SetAttr(2, 5)
	container.Install(Selector{}, live)
	if _, ok := container.Live(Base); !ok {
		t.Fatal("a profile installed under the zero selector is not at Base")
	}
	if Base.String() != "base" || (Selector{}).String() != "base" {
		t.Fatalf("selector names: %q %q", Base.String(), (Selector{}).String())
	}
}

func TestLayersAreIsolated(t *testing.T) {
	container := NewContainer()
	base, final := newStub(), newStub()
	container.Install(Base, base)
	container.Install(Final, final)

	if applied := container.Apply(Base, map[AttrID]AttrValue{3: 7}); applied == 0 {
		t.Fatal("applying to the base layer reported no change")
	}
	if mask := container.Dirty(Final); mask != 0 {
		t.Fatalf("the final layer got dirty from a base write: %d", mask)
	}
	container.ClearDirty(Base)
	if mask := container.Dirty(Base); mask != 0 {
		t.Fatalf("ClearDirty left %d", mask)
	}
	if got := len(container.Layers()); got != 2 {
		t.Fatalf("container holds %d layers, want 2", got)
	}
	if !container.Remove(Final) || container.Remove(Final) {
		t.Fatal("Remove did not report exactly one removal")
	}
}

// Container.Snapshot 在容器锁下复制：快照永远读不到本容器 Apply / ClearDirty 的半截写入。
// 这里用钩子把 Snapshot 停在复制中途，再发起 Apply，有界等待它是否进入 LoadValues。
// 历史：RR-20261005-NC-60

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
