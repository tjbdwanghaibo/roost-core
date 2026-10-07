package actionflow

import "testing"

// F02-7 是结构性守卫：刻意用未初始化的内部状态让 apply panic，验证最外层入口
// 仍释放 executing。正常业务回调已有 recover；本例不代表发现可达的业务 panic。
func TestOuterExecutionResetsAfterInternalPanic(t *testing.T) {
	r := &ActionRunner{}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("uninitialized internal state did not panic")
			}
		}()
		_ = r.submit(runnerCommand{op: opStart, group: 1})
	}()
	if r.Deferring() || r.applyingOuter || r.runaway || len(r.deferred) != 0 {
		t.Fatal("outer execution flags retained after internal panic")
	}
}
