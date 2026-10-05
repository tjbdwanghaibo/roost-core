package ai

import "testing"

// RR-20261005-NC-240：Parallel 的结果一旦确定（RequireAll 有一个子节点失败、RequireOne 有一个
// 成功），同一次 Tick 里排在后面的子节点不能再被 Tick。之前循环会继续 Tick 剩下的子节点，
// 再在末尾 Reset 全部：后面的 TaskflowAction 叶子先发起一个没人要的动作，紧接着被打断
// （配了 OnInterrupt 时“启动即取消”，没配时动作成为没人等待的孤儿，一直占着动作组）。

// countingLeaf 是一个发起动作的叶子，记录发起与打断次数。
func countingLeaf(launched, interrupted *int) Node[treeCtx] {
	return &TaskflowAction[treeData]{
		Launch: func(*treeCtx) (int64, error) {
			*launched++
			return int64(*launched), nil
		},
		OnInterrupt: func(int64) { *interrupted++ },
	}
}

func TestParallelStopsTickingChildrenOnceTheOutcomeIsDecided(t *testing.T) {
	cases := []struct {
		name   string
		policy ParallelPolicy
		first  bool // 第一个子节点（条件）的结果
		want   Status
	}{
		{name: "require_all_first_child_fails", policy: ParallelRequireAll, first: false, want: StatusFailure},
		{name: "require_one_first_child_succeeds", policy: ParallelRequireOne, first: true, want: StatusSuccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launched, interrupted := 0, 0
			first := tc.first
			parallel := &Parallel[treeCtx]{Policy: tc.policy, Children: []Node[treeCtx]{
				&Condition[treeCtx]{Check: func(*treeCtx) bool { return first }},
				countingLeaf(&launched, &interrupted),
			}}
			if got := parallel.Tick(&treeCtx{Data: &treeData{}}); got != tc.want {
				t.Fatalf("status = %v, want %v", got, tc.want)
			}
			if launched != 0 || interrupted != 0 {
				t.Fatalf("a child after the deciding one was ticked: launched=%d interrupted=%d, want 0/0", launched, interrupted)
			}
		})
	}
}

// 控制：结果未定时照常 Tick 全部未完成的子节点；之后结果确定时，已在运行的子节点经 Reset
// 收到打断。
func TestParallelStillInterruptsRunningChildrenWhenItCompletes(t *testing.T) {
	launched, interrupted := 0, 0
	fail := false
	parallel := &Parallel[treeCtx]{Policy: ParallelRequireAll, Children: []Node[treeCtx]{
		&FuncNode[treeCtx]{Run: func(*treeCtx) Status {
			if fail {
				return StatusFailure
			}
			return StatusRunning
		}},
		countingLeaf(&launched, &interrupted),
	}}
	ctx := &treeCtx{Data: &treeData{}}
	if got := parallel.Tick(ctx); got != StatusRunning || launched != 1 {
		t.Fatalf("first tick status=%v launched=%d, want running/1", got, launched)
	}
	fail = true
	if got := parallel.Tick(ctx); got != StatusFailure || interrupted != 1 || launched != 1 {
		t.Fatalf("second tick status=%v launched=%d interrupted=%d, want failure/1/1", got, launched, interrupted)
	}
}
