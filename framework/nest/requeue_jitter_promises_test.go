package nest

import (
	"testing"
	"time"
)

// U-0279：对称交叉创建（RR-20260926-48）靠“整条回滚 + 重新准入”解开，前提是冲突的两侧下一轮不再同时开始。
// 之前重排延迟固定 5ms：同一时刻因同一冲突失败的两条消息总在同一时刻重新准入，而延迟队列是单个定时器，
// 醒来晚了就把所有已到期的消息背靠背放出，两侧之间残留的一点错开也被抹平——没有任何打破对称的机制，
// 只能等调度噪声偶然把两侧错开到超过冲突窗口（v1.20.0 / v1.19.2 生成工程实测正常负载下 35%～53% 的运行耗尽 400 次上限）。
//
// 这里不跑定时器、不等时间：Dispatcher 不启动延迟循环，直接看 requeueNestDispatch 写进延迟堆的到期时刻。
// 同一原因、同一重排次数的一批消息在同一轮里重排，要求：
//   - 每条延迟都不短于下限 entityGroupDispatchRequeueDelay（400 次上限对应的最短重排窗口不缩短）；
//   - 延迟彼此错开，至少有一对相差超过半个下限（对称的两侧下一轮不再同时开始）。
//
// 用 [调用前, 调用后] 夹住 time.Now，判定与本机调度快慢无关：固定延迟时 max(下界) − min(上界) ≤ 0，必红。
func TestSymmetricTransientRequeuesAreNotReadmittedInLockstep(t *testing.T) {
	const batch = 64
	d := &Dispatcher{delayed: make(map[*delayedMsg]struct{})}
	mgr := &NestMgr{dispatcher: d}
	before := make([]time.Time, batch)
	after := make([]time.Time, batch)
	for i := range batch {
		msg := &Msg{Tid: int64(1000 + i), Type: MsgTypeSingle, PendingRequeues: 7}
		before[i] = time.Now()
		if !requeueNestDispatch(mgr, msg, "lock_timeout") {
			t.Fatalf("message %d was not requeued", i)
		}
		after[i] = time.Now()
	}
	if len(d.delayedHeap) != batch {
		t.Fatalf("delayed messages=%d, want %d", len(d.delayedHeap), batch)
	}
	due := make(map[int64]time.Time, batch)
	for _, dm := range d.delayedHeap {
		if dm.msg.PendingRequeues != 8 {
			t.Fatalf("requeued message %d carries PendingRequeues=%d, want 8", dm.msg.Tid, dm.msg.PendingRequeues)
		}
		due[dm.msg.Tid] = dm.due
	}
	var longestFloor, shortestCeil time.Duration
	for i := range batch {
		at, ok := due[int64(1000+i)]
		if !ok {
			t.Fatalf("message %d is missing from the delay queue", i)
		}
		floor, ceil := at.Sub(after[i]), at.Sub(before[i]) // 本条实际延迟落在 [floor, ceil]
		if ceil < entityGroupDispatchRequeueDelay {
			t.Fatalf("message %d was requeued after at most %v, shorter than the %v floor", i, ceil, entityGroupDispatchRequeueDelay)
		}
		if i == 0 || floor > longestFloor {
			longestFloor = floor
		}
		if i == 0 || ceil < shortestCeil {
			shortestCeil = ceil
		}
	}
	if spread := longestFloor - shortestCeil; spread < entityGroupDispatchRequeueDelay/2 {
		t.Fatalf("%d messages that hit the same conflict in the same round are re-admitted in lockstep: delays spread at most %v (shortest <= %v, longest >= %v), want > %v so symmetric conflicts break up",
			batch, max(spread, 0), shortestCeil, longestFloor, entityGroupDispatchRequeueDelay/2)
	}
}
