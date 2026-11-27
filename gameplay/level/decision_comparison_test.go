package level

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/ai/decision"
)

// 两份原生定义由各自生产编译器加载，不翻译规则。只投影可比的观察量：
// 当前阶段、这一步新提出的动作（含顺序、全部参数）；外部动作执行不是观察量。
type decisionComparison struct {
	h *harness
	p *decision.Program
	s decision.State
}

type comparisonCall struct {
	name string
	arg  int64
}

func newDecisionComparison(t *testing.T) *decisionComparison {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile("testdata/decision_comparison/" + name + ".json")
		must(t, err)
		return raw
	}
	p, err := decision.Decode(read("decision"), decision.Registry{
		Events: []string{"sight", "probe", "order", "arm"}, Fields: []string{"score"}, Intents: []string{"signal", "flash"},
	})
	must(t, err)
	d, err := DecodeDefinition(read("level"))
	must(t, err)
	r := Registry{Events: map[string]map[string]ValueKind{}, Actions: map[string]ActionSpec{}}
	for _, kind := range []string{"sight", "probe", "order", "arm"} {
		r.Events[kind] = map[string]ValueKind{"score": Integer}
	}
	for _, name := range []string{"signal", "flash"} {
		r.Actions[name] = ActionSpec{Version: 1, Args: map[string]ValueKind{"channel": Integer}}
	}
	h := newHarness(t, compiled(t, d, r), nest.RollbackUndo, false, nil)
	must(t, h.run(initStart))
	c := &decisionComparison{h: h, p: p, s: p.NewState()}
	c.checkStage(t, 0, "watch")
	if len(h.c.PendingActions()) != 0 {
		t.Fatal("unexpected action on initialization")
	}
	return c
}

func comparisonEvent(kind string, seq uint64, score int64) Event {
	e := hit(seq) // 真实实例/epoch，单一权威来源 server
	e.Kind, e.Fields = kind, map[string]Value{"score": Int(score)}
	return e
}

func (c *decisionComparison) checkStage(t *testing.T, now int64, want string) {
	t.Helper()
	v := c.h.c.Project()
	if c.s.Stage != want || v.Phase != Running || !slices.Equal(v.Stages, []StageView{{ID: want, Generation: 1}}) {
		t.Fatalf("tick %d: decision stage=%q, level=%s/%v, want %q", now, c.s.Stage, v.Phase, v.Stages, want)
	}
}

// event 为空时才推进时钟；投递事件不偷偷追加 Tick。把同一个权威字段给两侧；
// instance/epoch/source/sequence 是 level 信封，不能虚构成 decision 的内建能力。
func (c *decisionComparison) step(t *testing.T, now int64, ev Event, stage string, wantErr error, wantAI, wantLevel []comparisonCall) []decision.Intent {
	t.Helper()
	in := decision.Input{Now: now, Event: ev.Kind}
	if ev.Kind != "" {
		in.Fields = map[string]int64{"score": ev.Fields["score"].Int}
	}
	next, intents, err := c.p.Step(c.s, in)
	must(t, err)
	c.s = next
	if c.s.Now != now {
		t.Fatalf("decision clock=%d, want tick %d", c.s.Now, now)
	}
	var aiCalls []comparisonCall
	for _, intent := range intents {
		aiCalls = append(aiCalls, comparisonCall{intent.Name, intent.Arg})
	}

	before := c.h.c.Snapshot()
	err = c.h.run(func(l *LevelComponent) error {
		if ev.Kind == "" {
			return l.AdvanceTick(uint64(now))
		}
		return l.Handle(ev, now)
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("tick %d %q: level error=%v, want %v", now, ev.Kind, err, wantErr)
	}
	after := c.h.c.Snapshot()
	if err != nil && !reflect.DeepEqual(before, after) {
		t.Fatalf("tick %d %q: rejected input changed level state", now, ev.Kind)
	}
	var levelCalls []comparisonCall
	for _, e := range c.h.c.PendingActions() {
		if e.Sequence <= before.NextEffect {
			continue // 账本中的旧 pending 不是这一步再次发出的动作。
		}
		v, ok := e.Args["channel"]
		if !ok || len(e.Args) != 1 || v.Kind != Integer || e.Status != "pending" || after.Now != now {
			t.Fatalf("tick %d %q: unexpected new effect %+v at %d", now, ev.Kind, e, after.Now)
		}
		levelCalls = append(levelCalls, comparisonCall{e.Name, v.Int})
	}
	if after.NextEffect-before.NextEffect != uint64(len(levelCalls)) {
		t.Fatalf("tick %d %q: new effects missing from pending", now, ev.Kind)
	}
	c.checkStage(t, now, stage)
	if !slices.Equal(aiCalls, wantAI) || !slices.Equal(levelCalls, wantLevel) {
		t.Fatalf("tick %d %q: decision=%v want=%v; level=%v want=%v", now, ev.Kind, aiCalls, wantAI, levelCalls, wantLevel)
	}
	return intents
}

func TestDecisionComparison(t *testing.T) {
	signal := []comparisonCall{{"signal", 7}}
	flash := []comparisonCall{{"flash", 9}}
	t.Run("shared_trace", func(t *testing.T) {
		c := newDecisionComparison(t)
		// 交集的两侧逐步用同一个 golden，不是只检查最后的阶段。
		for _, row := range []struct {
			now   int64
			event Event
			stage string
			calls []comparisonCall
		}{
			{1, comparisonEvent("sight", 1, 1), "watch", nil},
			{2, comparisonEvent("sight", 2, 2), "alert", nil},
			{3, comparisonEvent("order", 3, 2), "alert", signal},
			{4, comparisonEvent("arm", 4, 0), "alert", nil},
			{5, Event{}, "alert", nil},
			{6, Event{}, "alert", flash},
			{9, Event{}, "alert", nil},
		} {
			c.step(t, row.now, row.event, row.stage, nil, row.calls, row.calls)
			if row.now == 4 || row.now == 5 {
				due, ok := c.h.c.NextDeadline()
				if !ok || due != 6 || len(c.s.Pending) != 1 || c.s.Pending[0].Due != 6 {
					t.Fatalf("tick %d: level deadline=%d/%t, decision pending=%+v, want due=6", row.now, due, ok, c.s.Pending)
				}
			}
		}
	})
	t.Run("once_is_not_max_fires", func(t *testing.T) {
		c := newDecisionComparison(t)
		for i, score := range []int64{1, 2, 3} {
			now := int64(i + 1)
			var wantLevel []comparisonCall
			if i == 1 {
				wantLevel = signal
			}
			c.step(t, now, comparisonEvent("probe", uint64(now), score), "watch", nil, nil, wantLevel)
			// watch 按配置顺序有 sight、probe 两个监听，只有 probe 应关闭。
			if !slices.Equal(c.s.Active, []bool{true, false}) {
				t.Fatalf("tick %d: unexpected active listeners: %v", now, c.s.Active)
			}
			want := FireState{}
			if i >= 1 {
				want = FireState{Count: 1, LastAt: 2}
			}
			if got := c.h.c.Snapshot().Fires["watch/probe"]; got != want {
				t.Fatalf("tick %d: level fires=%+v want=%+v", now, got, want)
			}
		}
		// 正对照：监听器最初确实开启，并非一直不工作。
		fresh := newDecisionComparison(t)
		fresh.step(t, 1, comparisonEvent("probe", 1, 2), "watch", nil, signal, signal)
		fresh.step(t, 2, comparisonEvent("probe", 2, 3), "watch", nil, nil, nil)
	})
	t.Run("replay_and_receipt", func(t *testing.T) {
		c := newDecisionComparison(t)
		c.step(t, 1, comparisonEvent("sight", 1, 2), "alert", nil, nil, nil)
		e := comparisonEvent("order", 2, 2)
		first := c.step(t, 3, e, "alert", nil, signal, signal)[0]
		effect := c.h.c.PendingActions()[0]
		pending := c.h.c.Snapshot()
		if effect.ID == "" || effect.Epoch != e.Epoch || effect.Instance != e.Instance || effect.Stage != "alert" || effect.Generation != 1 || pending.Sources[e.Source].Sequence != e.Sequence || pending.Variables["global/acks"].Int != 0 {
			t.Fatalf("missing source/generation/pending metadata or premature acknowledgement: %+v", pending)
		}
		replay := c.step(t, 3, e, "alert", nil, signal, nil)[0]
		if replay != first || !reflect.DeepEqual(c.h.c.Snapshot(), pending) {
			t.Fatal("identical input: expected same AI node/intent again, level ledger unchanged")
		}
		// 对 decision 而言这些仍是本阶段 score>=2 的事件，不能替宿主鉴权/去重。
		c.step(t, 4, comparisonEvent("order", 2, 3), "alert", ErrConflict, signal, nil)
		c.step(t, 5, comparisonEvent("order", 1, 2), "alert", ErrStale, signal, nil)
		stale := comparisonEvent("order", 3, 2)
		stale.Epoch--
		c.step(t, 6, stale, "alert", ErrStale, signal, nil)

		// 只给拥有账本的一侧送回执；没有凭空给 AI 添一个 Complete。
		receipt := Receipt{Instance: effect.Instance, Epoch: effect.Epoch, ID: effect.ID, Success: true}
		complete := func(r Receipt, now int64) error {
			return c.h.run(func(l *LevelComponent) error { return l.Complete(r, now) })
		}
		wrongEpoch := receipt
		wrongEpoch.Epoch--
		if err := complete(wrongEpoch, 7); !errors.Is(err, ErrStale) || !reflect.DeepEqual(c.h.c.Snapshot(), pending) {
			t.Fatalf("wrong-epoch receipt must leave request pending: %v", err)
		}
		must(t, complete(receipt, 8))
		done := c.h.c.Snapshot()
		if done.Now != 8 || done.Effects[effect.ID].Status != "done" || !done.Effects[effect.ID].Success || done.Variables["global/acks"] != Int(1) || len(c.h.c.PendingActions()) != 0 {
			t.Fatalf("successful receipt must raise action.done exactly once: %+v", done)
		}
		must(t, complete(receipt, 9))
		receipt.Success = false
		if err := complete(receipt, 10); !errors.Is(err, ErrConflict) || !reflect.DeepEqual(c.h.c.Snapshot(), done) {
			t.Fatalf("duplicate/conflicting receipts changed state: %v", err)
		}
		c.checkStage(t, 10, "alert")
		// 新流水的同内容是新事实；请求 ID 变，decision 节点 ID 不变。
		later := c.step(t, 11, comparisonEvent("order", 3, 2), "alert", nil, signal, signal)[0]
		newEffect := c.h.c.PendingActions()[0]
		if later != first || newEffect.ID == effect.ID || newEffect.Sequence != effect.Sequence+1 || c.h.c.Snapshot().Sources[e.Source].Sequence != 3 {
			t.Fatalf("new source sequence: intent=%+v effects=%+v/%+v", later, effect, newEffect)
		}
	})
}
