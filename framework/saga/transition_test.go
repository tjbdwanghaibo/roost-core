package saga

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// testLifecycle 是单元用例手写记录的生命周期标识（协调器新建的记录用 newLifecycle 随机生成）。
const testLifecycle = "TESTLIFE"

// 状态机的转移规则（transition.go）原样抽自已删除的 Redis 协调器（第 2 批）。第 2、3 批的差分用例把同一串事件交给两种协调器
// 逐步比对；第 4 批删除 Redis 协调器前，用它在同一串事件上逐步记下的记录（状态、方向、步骤、版本、代际、CommandID、迟到步骤、
// 错误文本、Data）、tombstone 关闭方式与运维判定的错误文本存成 testdata/transition_golden.json（生成方法：在 1fbd5d0e 的差分
// 用例里把 Redis 一侧的 view 写出），这里让分区走同一串事件并逐步比对。时间字段不比。
// 历史：TestTransitionRulesAreSharedByBothCoordinators（1fbd5d0e）。

type goldenView struct {
	Status, Phase          string
	Step, Completed, Late  int
	Attempt, Incarnation   uint32
	Version                uint64
	Op, Command, LastError string
	Data, LateData         string
}

type goldenStep struct {
	Step       string            `json:"step"`
	View       *goldenView       `json:"view,omitempty"`
	Tombstones map[string]string `json:"tombstones,omitempty"`
	Verdict    *string           `json:"verdict,omitempty"`
}

func loadTransitionGolden(t *testing.T) map[string][]goldenStep {
	t.Helper()
	raw, err := os.ReadFile("testdata/transition_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var byTest map[string][]goldenStep
	if err := json.Unmarshal(raw, &byTest); err != nil {
		t.Fatal(err)
	}
	out := map[string][]goldenStep{}
	for name, steps := range byTest {
		_, scenario, _ := strings.Cut(name, "/")
		out[scenario] = steps
	}
	return out
}

// sagaRun 驱动同一笔 saga 在分区上，并按录下的步骤逐步比对。
type sagaRun struct {
	t      *testing.T
	u      *unitPartition
	id     string
	at     time.Time
	golden []goldenStep
	next   int
}

func newSagaRun(t *testing.T, definition Definition, golden []goldenStep) *sagaRun {
	t.Helper()
	if len(golden) == 0 {
		t.Fatal("no recorded steps for this scenario")
	}
	run := &sagaRun{t: t, golden: golden}
	run.u = newUnitPartition(t, func(o *Options) {})
	run.u.e.mu.Lock()
	run.u.e.definitions = map[definitionKey]Definition{}
	run.u.e.mu.Unlock()
	if err := run.u.e.Register(definition); err != nil {
		t.Fatal(err)
	}
	run.at = time.Now().UTC().Truncate(time.Millisecond)
	run.u.clock.Set(run.at)
	run.u.p.wheel = newWheel(run.u.e.opts.Tick, run.u.e.opts.WheelSlots, run.at)
	return run
}

func (s *sagaRun) start(bk string, data []byte) {
	s.t.Helper()
	s.id = IDOf("rally", bk)
	s.u.round(s.at, s.u.startInput(bk, data, "a1", ""))
	s.due(0)
}

// due 把时钟推进 d 并判定一轮（到期）。
func (s *sagaRun) due(d time.Duration) {
	s.t.Helper()
	s.at = s.at.Add(d)
	s.u.round(s.at)
	s.compare("due")
}

func (s *sagaRun) complete(c Completion) {
	s.t.Helper()
	s.u.round(s.at, s.u.resultInput(c))
	s.compare("complete " + s.recordedKey(c.CommandID))
}

// recordedKey 把操作键 / CommandID 换成 golden 记录时的写法。golden 早于生命周期标识，那时操作键是 "<ID>:<方向>:<步骤>"；
// 生命周期只是一笔 saga 之内恒定的命名段，不影响转移规则，比对前去掉它（tombstone 的键在 compare 里反过来补上）。
func (s *sagaRun) recordedKey(key string) string {
	r := s.current()
	if rest, ok := strings.CutPrefix(key, r.ID+":"+r.Lifecycle+":"); ok {
		return r.ID + ":" + rest
	}
	return key
}

func (s *sagaRun) resume() {
	s.t.Helper()
	s.u.round(s.at, s.u.opInput(opInput{Kind: opResume, ID: s.id, Resume: &ResumeRequest{ID: s.id, Now: s.at}, Now: s.at}, "reply.resume"))
	s.compareVerdict("resume", "reply.resume")
	s.compare("resume")
}

func (s *sagaRun) compensate() {
	s.t.Helper()
	s.u.round(s.at, s.u.opInput(opInput{Kind: opCompensate, ID: s.id, Reason: "operator", Now: s.at}, "reply.compensate"))
	s.compareVerdict("compensate", "reply.compensate")
	s.compare("compensate")
}

func (s *sagaRun) expect(step string) goldenStep {
	s.t.Helper()
	if s.next >= len(s.golden) {
		s.t.Fatalf("%s: more steps than recorded (%d)", step, len(s.golden))
	}
	g := s.golden[s.next]
	s.next++
	if g.Step != step {
		s.t.Fatalf("step %d is %q, recorded %q", s.next-1, step, g.Step)
	}
	return g
}

func (s *sagaRun) compareVerdict(step, reply string) {
	s.t.Helper()
	g := s.expect(step + " verdict")
	verdicts := s.u.raw.verdicts(reply)
	_, err := verdictResult(verdicts[len(verdicts)-1])
	want := *g.Verdict
	if (want == "") != (err == nil) || (err != nil && err.Error() != want && !strings.Contains(err.Error(), want)) {
		s.t.Fatalf("%s: recorded error %q, partition verdict error %v", step, want, err)
	}
}

func (s *sagaRun) current() Record {
	s.t.Helper()
	return s.u.record(s.id)
}

func (s *sagaRun) compare(step string) {
	s.t.Helper()
	g := s.expect(step)
	r := s.current()
	got := goldenView{Status: r.Status.String(), Phase: r.Phase.String(), Step: r.Step, Completed: r.CompletedSteps, Late: r.LateStep, Attempt: r.Attempt,
		Incarnation: r.Incarnation, Version: r.Version, Op: s.recordedKey(r.OperationKey), Command: s.recordedKey(r.CommandID), LastError: r.LastError, Data: string(r.Data), LateData: string(r.LateData)}
	if got != *g.View {
		s.t.Fatalf("%s: records differ\nrecorded  %+v\npartition %+v", step, *g.View, got)
	}
	ops := s.u.p.sagas[s.id].Ops
	if len(ops) != len(g.Tombstones) {
		s.t.Fatalf("%s: tombstones: recorded %v, partition %+v", step, g.Tombstones, ops)
	}
	for op, closure := range g.Tombstones {
		if got := closureName(ops[r.ID+":"+r.Lifecycle+":"+strings.TrimPrefix(op, r.ID+":")].Closure); got != closure {
			s.t.Fatalf("%s: tombstone of %s: recorded %s, partition %s", step, op, closure, got)
		}
	}
}

func closureName(c OperationClosure) string {
	switch c {
	case OperationClosedWithResult:
		return "with_result"
	case OperationAbandoned:
		return "abandoned"
	default:
		return "unknown"
	}
}

// success 的 Data 为空时用 nil：线上的结果信封是 JSON（data 带 omitempty），空 Data 与 nil 到达协调器时都是 nil。
func success(r Record, data string) Completion {
	c := Completion{CommandID: r.CommandID, IdempotencyKey: r.OperationKey, SagaID: r.ID, Success: true}
	if data != "" {
		c.Data = []byte(data)
	}
	return c
}

func failure(r Record, retryable bool, reason string) Completion {
	return Completion{CommandID: r.CommandID, IdempotencyKey: r.OperationKey, SagaID: r.ID, Retryable: retryable, Error: reason}
}

func TestTransitionRulesMatchTheRecordedRedisCoordinator(t *testing.T) {
	golden := loadTransitionGolden(t)
	definition := testDefinition() // 两步，每步 1s 超时、2 次尝试、退避 1ms..10ms
	timeout := time.Second + time.Millisecond
	backoffDone := 20 * time.Millisecond

	scenarios := map[string]func(s *sagaRun){
		"forward success": func(s *sagaRun) {
			s.start("ok", []byte("in"))
			s.complete(success(s.current(), "step0"))
			s.due(0)
			s.complete(success(s.current(), "step1"))
			if s.current().Status != StatusCompleted {
				t.Fatalf("status %v", s.current().Status)
			}
		},
		"refusal compensates the completed prefix": func(s *sagaRun) {
			s.start("refuse", nil)
			s.complete(success(s.current(), "step0"))
			s.due(0)
			s.complete(failure(s.current(), false, "no funds"))
			s.due(0)
			s.complete(success(s.current(), ""))
			if s.current().Status != StatusCompensated {
				t.Fatalf("status %v", s.current().Status)
			}
		},
		"retryable failures exhaust into compensation, stale attempt ignored": func(s *sagaRun) {
			s.start("retry", nil)
			s.complete(success(s.current(), "step0"))
			s.due(0)
			first := s.current()
			s.complete(failure(first, true, "busy"))
			s.due(backoffDone)
			s.complete(failure(first, true, "busy again")) // 较早一次尝试的可重试失败（方向 ③）
			s.due(timeout)                                 // 第 2 次尝试超时，用尽 → 补偿第 0 步
			s.due(0)
			s.complete(success(s.current(), ""))
		},
		"late success after abandonment is compensated (direction 4)": func(s *sagaRun) {
			s.start("late", nil)
			s.complete(success(s.current(), "step0"))
			s.due(0)
			attempt1 := s.current()
			s.due(timeout) // 尝试 1 超时 → 退避
			s.due(backoffDone)
			s.due(timeout) // 尝试 2 超时，用尽 → 放弃第 1 步、补偿第 0 步
			s.due(0)
			s.complete(success(attempt1, "late-1")) // 第 1 步的尝试 1 其实生效了：迟到成功
			s.complete(success(attempt1, "late-1")) // 重投：重复
			for !s.current().Status.Terminal() {
				if s.current().Status != StatusWaiting {
					s.due(0)
				}
				s.complete(success(s.current(), ""))
			}
			if s.current().Status != StatusCompensated || s.current().LateStep != 0 {
				t.Fatalf("end %+v", s.current())
			}
		},
		"late compensation success only alarms once per incarnation": func(s *sagaRun) {
			s.start("alarm", nil)
			s.complete(success(s.current(), "step0"))
			s.due(0)
			s.complete(failure(s.current(), false, "refused"))
			s.due(0)
			compensation := s.current()
			s.due(timeout)
			s.due(backoffDone)
			s.due(timeout) // 补偿用尽 → ManualRequired
			if s.current().Status != StatusManualRequired {
				t.Fatalf("status %v", s.current().Status)
			}
			s.complete(success(compensation, ""))
			s.complete(success(compensation, ""))
			if s.u.e.Stats().LateAfterAbandon != 1 {
				t.Fatalf("partition alarmed %d times", s.u.e.Stats().LateAfterAbandon)
			}
		},
		"resume opens a new incarnation and old results are stale": func(s *sagaRun) {
			s.start("resume", nil)
			s.complete(success(s.current(), "step0"))
			s.due(0)
			s.complete(failure(s.current(), false, "refused"))
			s.due(0)
			s.due(timeout)
			s.due(backoffDone)
			old := s.current()
			s.due(timeout) // ManualRequired
			s.resume()
			s.due(0)
			s.complete(failure(old, false, "stale refusal")) // 上一生的拒绝：只计数
			s.complete(success(s.current(), ""))
			if s.current().Incarnation != 1 || s.current().Status != StatusCompensated {
				t.Fatalf("end %+v", s.current())
			}
		},
		"manual compensate in compensate phase and on completed": func(s *sagaRun) {
			s.start("manual", nil)
			s.complete(success(s.current(), "step0"))
			s.due(0)
			s.complete(failure(s.current(), false, "refused"))
			s.due(0)
			s.due(timeout)
			s.due(backoffDone)
			s.due(timeout) // ManualRequired 在补偿方向
			s.compensate()
			s.compensate() // 已在补偿：原样返回
			s.due(0)
			s.complete(success(s.current(), ""))
			s.compensate() // 已补偿：原样返回
		},
		"deadline aborts the forward direction": func(s *sagaRun) {
			s.id = IDOf("rally", "deadline")
			deadline := s.at.Add(500 * time.Millisecond)
			request, _ := prepareStart(StartRequest{Type: "rally", BusinessKey: "deadline", DefinitionVersion: 1, DeadlineAt: deadline}, 1<<20)
			digest, _ := startIntentDigest(request)
			msg := s.u.startInput("deadline", nil, "a1", "")
			msg.Data = mustJSON(t, startInput{V: WireVersion, Request: request, Attempt: "a1", Digest: digest})
			s.u.round(s.at, msg)
			s.due(0)
			s.complete(success(s.current(), "step0"))
			s.due(0)
			s.due(timeout) // 超过截止：转补偿
			s.due(0)
			s.complete(success(s.current(), ""))
		},
	}
	for name, scenario := range scenarios {
		t.Run(name, func(t *testing.T) {
			run := newSagaRun(t, definition, golden[strings.ReplaceAll(name, " ", "_")])
			scenario(run)
			if run.next != len(run.golden) {
				t.Fatalf("ran %d of %d recorded steps", run.next, len(run.golden))
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
