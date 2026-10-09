package skillsync

import (
	"errors"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

// 固定 acquire 已登记引用、尚未拿到 view 锁时发生关闭的交错。
func TestAcquireRechecksObserverAfterWaiting(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	view := observerKey{observer: observer, key: 1}
	entry := &viewLockEntry{}
	entry.mutex.Lock()
	c.viewLocks[view] = entry
	done := make(chan error, 1)
	go func() {
		release, err := c.acquireView(view)
		if err == nil {
			release()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		c.mutex.Lock()
		refs := entry.refs
		c.mutex.Unlock()
		if refs == 1 {
			break
		}
		if time.Now().After(deadline) {
			entry.mutex.Unlock()
			t.Fatal("acquire did not register view")
		}
		goruntime.Gosched()
	}
	c.mutex.Lock()
	delete(c.activeObservers, observer) // CloseObserver 的拒绝新请求状态。
	c.mutex.Unlock()
	entry.mutex.Unlock()
	if err := <-done; !errors.Is(err, ErrObserverClosed) {
		t.Fatalf("waiting acquire entered closed view: %v", err)
	}
}

func TestCoordinatorExplicitLifecycleCapacityAndManifestIdentity(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	publisher := &recordingPublisher{}
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}, MaxObservers: 1, MaxPrograms: 1})
	if err != nil {
		t.Fatal(err)
	}
	one, two := syncstream.Observer{ID: 1}, syncstream.Observer{ID: 2}
	if err := c.PublishSnapshot(one, 1); !errors.Is(err, ErrObserverClosed) {
		t.Fatalf("implicit observer opened: %v", err)
	}
	if err := c.OpenObserver(one); err != nil {
		t.Fatal(err)
	}
	if err := c.OpenObserver(two); !errors.Is(err, ErrCoordinatorCapacity) {
		t.Fatalf("observer limit=%v", err)
	}
	if err := c.RegisterProgram(1, program); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterProgram(1, program); err != nil {
		t.Fatalf("idempotent registration=%v", err)
	}
	if err := c.PublishManifest(one, 1); err != nil {
		t.Fatal(err)
	}
	if len(publisher.packets) != 1 || publisher.packets[0].Stream.Topic != TopicManifest {
		t.Fatal("registered manifest not published")
	}
	definition, err := skill.Parse([]byte(strings.ReplaceAll(visibleWaitSkill, "visible_wait", "other_wait")))
	if err != nil {
		t.Fatal(err)
	}
	other, diagnostics := skill.Compile(definition, skill.DefaultCompileEnvironment())
	for _, d := range diagnostics {
		if d.Severity == skill.DiagnosticError {
			t.Fatal(d)
		}
	}
	if err := c.RegisterProgram(1, other); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("manifest overwritten: %v", err)
	}
	if err := c.RegisterProgram(2, other); !errors.Is(err, ErrCoordinatorCapacity) {
		t.Fatalf("manifest capacity=%v", err)
	}
	c.UnregisterProgram(1)
	if err := c.RegisterProgram(2, other); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseObserver(one); err != nil {
		t.Fatal(err)
	}
	if err := c.OpenObserver(two); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishSnapshot(one, 1); !errors.Is(err, ErrObserverClosed) {
		t.Fatalf("old observer reopened: %v", err)
	}
}

func TestCoordinatorCloseMustFinishBeforeReopen(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	publisher := &perObserverGate{entered: make(chan int64, 1), releases: map[int64]chan struct{}{1: make(chan struct{})}}
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.PublishSnapshot(observer, 1) }()
	select {
	case <-publisher.entered:
	case <-time.After(time.Second):
		close(publisher.releases[1])
		t.Fatal("publisher did not enter")
	}
	closeErr := c.CloseObserver(observer)
	openErr := c.OpenObserver(observer)
	close(publisher.releases[1])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(closeErr, ErrApplyInProgress) || !errors.Is(openErr, ErrApplyInProgress) {
		t.Fatalf("close=%v reopen=%v", closeErr, openErr)
	}
	if err := c.CloseObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := c.RetryPending(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c.outbox.Metrics().Pending != 0 {
		t.Fatal("closed observer retained pending packets")
	}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
}

func TestClosedObserverIdentitiesDoNotAccumulate(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 2000; id++ {
		observer := syncstream.Observer{ID: id}
		if err := c.OpenObserver(observer); err != nil {
			t.Fatal(err)
		}
		if err := c.CloseObserver(observer); err != nil {
			t.Fatal(err)
		}
	}
	// 检查 Coordinator 自己持有的索引；不把外部 History/Runtime 的状态混进来。
	value := reflect.ValueOf(c).Elem()
	for index := range value.NumField() {
		field := value.Field(index)
		if field.Kind() == reflect.Map && field.Len() != 0 {
			t.Errorf("closed observer retained in %s: %d entries", value.Type().Field(index).Name, field.Len())
		}
	}
}

func TestRegisteredManifestPlansAreBounded(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	for key := int64(1); key <= 2048; key++ {
		c.RegisterProgram(key, program)
	}
	if len(c.plans) > 1024 {
		t.Fatalf("manifest registry retained %d plans", len(c.plans))
	}
}
