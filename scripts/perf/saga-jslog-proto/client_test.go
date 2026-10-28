package jslogproto

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// 发起方（client.go）的两条承诺：
//   - 启动尝试身份（MsgID）全局唯一：被拒之后换一个 Client（重建、另一个进程）用同一个 saga ID 再发起，意图必须真正入流；
//     同一次 StartIntent 里结果未知的重发仍用同一个身份，不重复追加。
//   - 同一个 ID 的每一位等待者都会被下一个终态唤醒；旧一轮的拒绝回执不会结束新一轮。

func (e *env) newClient() *Client {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := NewClient(ctx, e.js, e.l)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(c.Close)
	return c
}

// rejectingCoordinator 起一个开着准入的协调器，并把积压信号顶到高水位之上：新启动全部被拒（终态 Rejected）。
func (e *env) rejectingCoordinator() *Coordinator {
	e.t.Helper()
	coord := e.coordinator("adm", func(o *CoordinatorOptions) {
		o.Admission = &AdmissionOptions{HighSagas: 10, LowSagas: 2, StaleAfter: time.Minute}
	})
	e.takeAll(coord, false)
	coord.adm.set(partLoadKey("test"), PartLoad{Records: 100, At: time.Now().UnixMilli()})
	return coord
}

// startAndWait 写启动意图并等这次发起的终态；超时时报告启动主题上的最后序号，区分“没入流”和“入流了没处理”。
func (e *env) startAndWait(c *Client, id string) *Saga {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := e.lastStartSeq(id)
	ch, err := c.StartIntent(ctx, id, e.transfer(0, false))
	if err != nil {
		e.t.Fatal(err)
	}
	select {
	case <-ch:
	case <-ctx.Done():
		e.t.Fatalf("start of %s acknowledged but no terminal state within 10s; last start-intent seq %d before the call, %d after (unchanged = the intent was deduplicated, never appended)",
			id, before, e.lastStartSeq(id))
	}
	return c.Latest()[id]
}

func (e *env) lastStartSeq(id string) uint64 {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k := e.l.PartitionOf(id)
	stream, err := e.js.Stream(ctx, e.l.StreamOf(k))
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := stream.GetLastMsgForSubject(ctx, e.l.StartSubject(k))
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return 0
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return m.Sequence
}

func (e *env) startIntentCount(id string) uint64 {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k := e.l.PartitionOf(id)
	stream, err := e.js.Stream(ctx, e.l.StreamOf(k))
	if err != nil {
		e.t.Fatal(err)
	}
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(e.l.StartSubject(k)))
	if err != nil {
		e.t.Fatal(err)
	}
	return info.State.Subjects[e.l.StartSubject(k)]
}

// 被拒之后换一个 Client 用同一个 saga ID 再发起：意图必须入流并得到新的终态（版本往上走）。
// 两种来源：发起方进程重建（旧 Client 关掉、新 Client 回放到旧的 Rejected）；另一个仍在运行的 Client。
func TestRejectedStartRetriedByAnotherClientIsAppended(t *testing.T) {
	for _, restart := range []bool{true, false} {
		name := "another client"
		if restart {
			name = "client restarted"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 1)
			e.rejectingCoordinator()
			first := e.startAndWait(e.client, "same")
			if first.Status != StatusRejected || first.Version != 1 {
				t.Fatalf("first start: %s v%d, want rejected v1", first.Status, first.Version)
			}
			if restart {
				e.client.Close()
			}
			other := e.newClient()
			waitUntil(t, 5*time.Second, "new client replayed the rejection", func() bool { return other.Latest()["same"] != nil })
			second := e.startAndWait(other, "same")
			if second.Status != StatusRejected || second.Version != 2 {
				t.Fatalf("retry by another client: %s v%d, want a new rejection v2", second.Status, second.Version)
			}
			if !restart {
				// 原来的 Client 再发起一次：同样入流（它自己的第二次尝试）。
				if third := e.startAndWait(e.client, "same"); third.Version != 3 {
					t.Fatalf("retry by the original client: %s v%d, want v3", third.Status, third.Version)
				}
			}
		})
	}
}

// 同一次 StartIntent 里结果未知（已落库、回复丢了）的重发仍用同一个身份：流按 MsgID 去重，只追加一条；重发拿到的 Duplicate 是正常结果。
func TestStartResendAfterUnknownOutcomeIsNotAppendedTwice(t *testing.T) {
	e := newEnv(t, 1)
	var drops atomic.Int32
	e.client.dropAck = func() bool { return drops.Add(1) <= 2 }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := e.client.StartIntent(ctx, "unknown", e.transfer(0, false)); err != nil {
		t.Fatalf("resend after unknown outcomes: %v", err)
	}
	if n := e.startIntentCount("unknown"); n != 1 {
		t.Fatalf("start intents appended: %d, want 1 (the resends must reuse the attempt's MsgID)", n)
	}
	if e.client.Pubs.Load() != 3 {
		t.Fatalf("publish attempts: %d, want 3 (two with lost replies + one acknowledged as duplicate)", e.client.Pubs.Load())
	}
}

// 第一次发布就被判成重复：身份撞了，意图没入流——不能当成功返回，等待者也要撤掉。
func TestStartIdentityCollisionIsAnError(t *testing.T) {
	e := newEnv(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := e.client.StartIntent(ctx, "x", e.transfer(0, false)); err != nil {
		t.Fatal(err)
	}
	other := e.newClient()
	other.inst = e.client.inst // 人为让两个实例的身份相同
	if _, err := other.StartIntent(ctx, "x", e.transfer(0, false)); !errors.Is(err, ErrStartIdentityReused) {
		t.Fatalf("first publish acknowledged as a duplicate: err=%v, want ErrStartIdentityReused", err)
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if n := len(other.wait["x"]); n != 0 {
		t.Fatalf("%d waiters left behind after the failed start", n)
	}
}

// publishTerminal 直接往分区事件流写一条终态事件（没有协调器，只测发起方的等待者）。
func (e *env) publishTerminal(id string, version uint64, status Status) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _ := json.Marshal(Event{V: WireVersion, Saga: &Saga{ID: id, Version: version, Status: status}})
	if _, err := e.js.Publish(ctx, e.l.EvSubject(e.l.PartitionOf(id), "done"), raw); err != nil {
		e.t.Fatal(err)
	}
}

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestStartIntentWaiters(t *testing.T) {
	startErr := func(e *env, id string) (<-chan struct{}, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return e.client.StartIntent(ctx, id, e.transfer(0, false))
	}
	start := func(t *testing.T, e *env, id string) <-chan struct{} {
		t.Helper()
		ch, err := startErr(e, id)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}

	// 先后两次发起（第二次时第一次还没到终态）：两位都被同一个终态唤醒。
	t.Run("sequential calls", func(t *testing.T) {
		e := newEnv(t, 1)
		first := start(t, e, "same")
		second := start(t, e, "same")
		e.publishTerminal("same", 1, StatusCompleted)
		if !closedWithin(second, 5*time.Second) {
			t.Fatal("second caller not woken by the terminal state")
		}
		if !closedWithin(first, time.Second) {
			t.Fatal("first caller stranded: the second StartIntent replaced its waiter")
		}
	})

	// 同时发起：每一位都被唤醒。
	t.Run("concurrent calls", func(t *testing.T) {
		e := newEnv(t, 1)
		const n = 16
		chans := make([]<-chan struct{}, n)
		var wg sync.WaitGroup
		for i := range chans {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ch, err := startErr(e, "same")
				if err != nil {
					t.Error(err)
					return
				}
				chans[i] = ch
			}()
		}
		wg.Wait()
		if t.Failed() {
			return
		}
		e.publishTerminal("same", 1, StatusCompleted)
		for i, ch := range chans {
			if !closedWithin(ch, 5*time.Second) {
				t.Fatalf("caller %d of %d stranded", i, n)
			}
		}
	})

	// 终态与发起竞争：终态先到的，发起时直接拿到已关闭的通道；后到的，被终态唤醒。没有人卡住。
	t.Run("terminal races calls", func(t *testing.T) {
		e := newEnv(t, 1)
		for round := 0; round < 20; round++ {
			id := ids("race", 20)[round]
			const n = 8
			chans := make([]<-chan struct{}, n)
			var wg sync.WaitGroup
			for i := range chans {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if i == n/2 {
						raw, _ := json.Marshal(Event{V: WireVersion, Saga: &Saga{ID: id, Version: 1, Status: StatusCompleted}})
						if _, err := e.js.Publish(context.Background(), e.l.EvSubject(e.l.PartitionOf(id), "done"), raw); err != nil {
							t.Error(err)
						}
					}
					ch, err := startErr(e, id)
					if err != nil {
						t.Error(err)
						return
					}
					chans[i] = ch
				}()
			}
			wg.Wait()
			if t.Failed() {
				return
			}
			for i, ch := range chans {
				if !closedWithin(ch, 5*time.Second) {
					t.Fatalf("round %d: caller %d stranded", round, i)
				}
			}
		}
	})

	// 被拒之后发起新一轮：旧一轮的拒绝回执（这里重放一条同版本的旧回执）不能结束新一轮，只有更新的终态才算。
	t.Run("new round after rejection", func(t *testing.T) {
		e := newEnv(t, 1)
		first := start(t, e, "same")
		e.publishTerminal("same", 1, StatusRejected)
		if !closedWithin(first, 5*time.Second) {
			t.Fatal("first round not woken by its rejection")
		}
		second := start(t, e, "same")
		if closedWithin(second, 200*time.Millisecond) {
			t.Fatal("new round ended at once by the already-seen rejection")
		}
		e.publishTerminal("same", 1, StatusRejected) // 旧回执再出现一次
		if closedWithin(second, 500*time.Millisecond) {
			t.Fatal("new round ended by the previous round's rejection receipt (v1)")
		}
		e.publishTerminal("same", 2, StatusCompleted)
		if !closedWithin(second, 5*time.Second) {
			t.Fatal("new round not woken by its own terminal state (v2)")
		}
		if s := e.client.Latest()["same"]; s.Version != 2 || s.Status != StatusCompleted {
			t.Fatalf("latest: %s v%d, want completed v2", s.Status, s.Version)
		}
	})
}
