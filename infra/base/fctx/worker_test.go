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
