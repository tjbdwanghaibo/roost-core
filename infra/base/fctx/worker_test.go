package fctx

import "testing"

func TestFastWorkerIsLocalExecutionState(t *testing.T) {
	var snapshot ContextSnapshot
	func() {
		_, release := NewContext(WithFastWorker())
		defer release()
		_, nested := NewContext()
		defer nested()
		if !InFastWorker() {
			t.Fatal("nested context lost execution state")
		}
		snapshot = CaptureSnapshot()
	}()
	_, release := NewContext(WithSnapshot(snapshot))
	defer release()
	if InFastWorker() {
		t.Fatal("snapshot moved execution state across boundary")
	}
	if BlockingError("slow") != nil {
		t.Fatal("slow context rejected")
	}
}

func TestLongAndIOWorkerStateDoesNotTravelWithSnapshot(t *testing.T) {
	for _, long := range []bool{true, false} {
		func() {
			option := WithIOWorker()
			if long {
				option = WithLongWorker()
			}
			_, release := NewContext(option)
			defer release()
			_, nested := NewContext()
			if InFastWorker() || InLongWorker() != long || InIOWorker() == long || (BlockingError("test") != nil) != long {
				t.Fatal("wrong nested worker identity")
			}
			snapshot := CaptureSnapshot()
			nested()
			// 新 goroutine 只接收快照，不能继承发起者的执行池身份。
			done := make(chan bool, 1)
			go func() {
				_, reset := NewContext(WithSnapshot(snapshot))
				defer reset()
				done <- InBusinessWorker() || InIOWorker()
			}()
			if <-done {
				t.Fatal("worker identity escaped in snapshot")
			}
		}()
	}
}
