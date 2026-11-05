package worker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func goStart(body func()) error { go body(); return nil }

// 同一个键按接纳顺序逐个执行、不同键互不等待；在跑与排队合计到上限时拒绝；panic 只结束这一个任务。
// 总线 RPC 的按顺序键派发与 effect 消费者的按实体派发都建在它上面（各自包里有经正式入口的回归）。
func TestKeyedDispatcherOrdersOneKeyAndBoundsPending(t *testing.T) {
	d := NewKeyedDispatcher(3)
	release := make(chan struct{})
	started := make(chan string, 8)
	var mu sync.Mutex
	var order []string
	task := func(name string, block bool) func() {
		return func() {
			started <- name
			if block {
				<-release
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		}
	}
	if err := d.Dispatch("a", goStart, task("a-1", true)); err != nil {
		t.Fatal(err)
	}
	if err := d.Dispatch("a", goStart, task("a-2", false)); err != nil {
		t.Fatal(err)
	}
	if got := <-started; got != "a-1" {
		t.Fatalf("first started %s", got)
	}
	if err := d.Dispatch("b", goStart, task("b-1", false)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != "b-1" {
			t.Fatalf("%s started while a-1 of the same key was running", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("b-1 did not run while key a was busy")
	}
	// a-1 在跑、a-2 排队，再接纳两个就超过上限 3（b-1 可能已结束，所以先补满）。
	for d.Pending() < 3 {
		if err := d.Dispatch("a", goStart, task("a-x", false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Dispatch("c", goStart, task("c-1", false)); !errors.Is(err, ErrKeyedDispatcherFull) {
		t.Fatalf("dispatch over the limit = %v, want ErrKeyedDispatcherFull", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for d.Pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var keyA []string
	for _, name := range order {
		if name != "b-1" {
			keyA = append(keyA, name)
		}
	}
	if len(keyA) < 2 || keyA[0] != "a-1" || keyA[1] != "a-2" {
		t.Fatalf("key a ran in order %v, want a-1 then a-2", keyA)
	}

	// panic 只结束这一个任务，同键后面的照常执行。
	done := make(chan struct{})
	if err := d.Dispatch("p", goStart, func() { panic("boom") }); err != nil {
		t.Fatal(err)
	}
	if err := d.Dispatch("p", goStart, func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the task after a panic on the same key never ran")
	}
}
