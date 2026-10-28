package saga

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 两种协调器共用 transition.go 的判定：同一串事件（启动、到期、结果、Resume、Compensate）分别交给 Redis 引擎（内存 Store、
// 手动领取）与 JetStream 分区（decide / install），逐步比对记录的状态字段与 tombstone。时间字段不比（Redis 引擎的 Complete 用真实
// 时钟），版本、代际、CommandID、步骤、迟到步骤、错误文本都要一致。

// sagaPair 驱动同一笔 saga 在两种协调器上。
type sagaPair struct {
	t      *testing.T
	ctx    context.Context
	redis  *Engine
	store  *memoryStore
	u      *unitPartition
	id     string
	at     time.Time
	redisC []Command // Redis 引擎派发的命令（发布函数收到的）
}

func newSagaPair(t *testing.T, definition Definition) *sagaPair {
	t.Helper()
	pair := &sagaPair{t: t, ctx: context.Background(), store: newMemoryStore()}
	options := DefaultOptions()
	engine, err := NewEngine(pair.store, PublishFunc(func(_ context.Context, c Command) error { return nil }), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(definition); err != nil {
		t.Fatal(err)
	}
	pair.redis = engine
	pair.u = newUnitPartition(t, func(o *JetStreamOptions) {})
	pair.u.e.mu.Lock()
	pair.u.e.definitions = map[definitionKey]Definition{}
	pair.u.e.mu.Unlock()
	if err := pair.u.e.Register(definition); err != nil {
		t.Fatal(err)
	}
	pair.at = time.Now().UTC().Truncate(time.Millisecond)
	pair.u.clock.Set(pair.at)
	pair.u.p.wheel = newWheel(pair.u.e.opts.Tick, pair.u.e.opts.WheelSlots, pair.at)
	return pair
}

func (s *sagaPair) start(bk string, data []byte) {
	s.t.Helper()
	s.id = IDOf("rally", bk)
	if _, err := s.redis.StartSaga(s.ctx, StartRequest{ID: s.id, Type: "rally", BusinessKey: bk, DefinitionVersion: 1, Data: data, Now: s.at}); err != nil {
		s.t.Fatal(err)
	}
	s.u.round(s.at, s.u.startInput(bk, data, "a1", ""))
	// 分区在同一轮里派发；Redis 引擎领取一次才派发。
	s.due(0)
}

// due 把时钟推进 d 并处理到期：Redis 引擎按 ClaimDue（NextRunAt ≤ now）领取后 processClaimed；分区一轮 decide。
func (s *sagaPair) due(d time.Duration) {
	s.t.Helper()
	// Redis 引擎的 Complete 用真实时钟排下一次到期，所以虚拟时钟不能落后于真实时钟。
	s.at = maxTime(s.at, time.Now().UTC()).Add(d)
	claimed, err := s.store.ClaimDue(s.ctx, ClaimRequest{Owner: "pair", Now: s.at, LeaseDuration: time.Minute, Limit: 10})
	if err != nil {
		s.t.Fatal(err)
	}
	for _, record := range claimed {
		if err := s.redis.processClaimed(s.ctx, record, s.at); err != nil {
			s.t.Fatal(err)
		}
	}
	s.u.round(s.at)
	s.compare("due")
}

// redisSettle 补上分区在同一轮里做的那次派发：分区处理完输入后立即处理本轮改过、已经到期的记录，而 Redis 引擎要等下一次领取。
// 转移之后立即到期的只有“新步骤”（Attempt == 0，NextRunAt = 转移时刻）；重试退避中的（Attempt > 0）要等退避，两边都不在本轮派发。
func (s *sagaPair) redisSettle() {
	s.t.Helper()
	record := s.current()
	if record.Status.Terminal() || record.Status == StatusWaiting || record.Attempt != 0 {
		return
	}
	if err := s.redis.processClaimed(s.ctx, record, s.at); err != nil {
		s.t.Fatal(err)
	}
}

// complete 把同一份结果交给两边。Redis 引擎的 Complete 用真实时钟，分区用 s.at：只比较非时间字段。
func (s *sagaPair) complete(c Completion) {
	s.t.Helper()
	_, err := s.redis.Complete(s.ctx, c)
	if err != nil && !errors.Is(err, ErrNotWaiting) && !errors.Is(err, ErrIdentityConflict) {
		s.t.Fatalf("redis complete: %v", err)
	}
	s.redisSettle()
	s.u.round(s.at, s.u.resultInput(c))
	s.compare("complete " + c.CommandID)
}

func (s *sagaPair) resume() {
	s.t.Helper()
	_, redisErr := s.redis.Resume(s.ctx, ResumeRequest{ID: s.id, Now: s.at})
	s.redisSettle()
	s.u.round(s.at, s.u.opInput(opInput{Kind: opResume, ID: s.id, Resume: &ResumeRequest{ID: s.id, Now: s.at}, Now: s.at}, "reply.resume"))
	s.compareVerdict("resume", redisErr, "reply.resume")
	s.compare("resume")
}

func (s *sagaPair) compensate() {
	s.t.Helper()
	_, redisErr := s.redis.Compensate(s.ctx, s.id, "operator", s.at)
	s.redisSettle()
	s.u.round(s.at, s.u.opInput(opInput{Kind: opCompensate, ID: s.id, Reason: "operator", Now: s.at}, "reply.compensate"))
	s.compareVerdict("compensate", redisErr, "reply.compensate")
	s.compare("compensate")
}

func (s *sagaPair) compareVerdict(step string, redisErr error, reply string) {
	s.t.Helper()
	verdicts := s.u.raw.verdicts(reply)
	_, err := verdictResult(verdicts[len(verdicts)-1])
	if (redisErr == nil) != (err == nil) || (redisErr != nil && redisErr.Error() != err.Error() && !errors.Is(err, redisErr)) {
		s.t.Fatalf("%s: redis error %v, partition verdict error %v", step, redisErr, err)
	}
}

// current 是 Redis 引擎上的当前记录。
func (s *sagaPair) current() Record {
	s.t.Helper()
	record, err := s.store.Get(s.ctx, s.id)
	if err != nil {
		s.t.Fatal(err)
	}
	return record
}

func (s *sagaPair) compare(step string) {
	s.t.Helper()
	a := s.current()
	b := s.u.record(s.id)
	type view struct {
		Status                 Status
		Phase                  Phase
		Step, Completed, Late  int
		Attempt, Incarnation   uint32
		Version                uint64
		Op, Command, LastError string
		Data, LateData         string
	}
	of := func(r Record) view {
		return view{r.Status, r.Phase, r.Step, r.CompletedSteps, r.LateStep, r.Attempt, r.Incarnation, r.Version, r.OperationKey, r.CommandID, r.LastError, string(r.Data), string(r.LateData)}
	}
	if of(a) != of(b) {
		s.t.Fatalf("%s: records differ\nredis     %+v\npartition %+v", step, of(a), of(b))
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for op, closure := range s.store.closures {
		if s.store.closed[op] != s.id {
			continue
		}
		if got := s.u.p.sagas[s.id].Ops[op].Closure; got != closure {
			s.t.Fatalf("%s: tombstone of %s: redis %v, partition %v", step, op, closure, got)
		}
	}
	if len(s.u.p.sagas[s.id].Ops) != countClosed(s.store, s.id) {
		s.t.Fatalf("%s: tombstones: redis %d, partition %+v", step, countClosed(s.store, s.id), s.u.p.sagas[s.id].Ops)
	}
}

func countClosed(store *memoryStore, id string) int {
	n := 0
	for _, owner := range store.closed {
		if owner == id {
			n++
		}
	}
	return n
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

func TestTransitionRulesAreSharedByBothCoordinators(t *testing.T) {
	definition := testDefinition() // 两步，每步 1s 超时、2 次尝试、退避 1ms..10ms
	timeout := time.Second + time.Millisecond
	backoffDone := 20 * time.Millisecond

	scenarios := map[string]func(s *sagaPair){
		"forward success": func(s *sagaPair) {
			s.start("ok", []byte("in"))
			s.complete(success(s.current(), "step0"))
			s.due(0)
			s.complete(success(s.current(), "step1"))
			if s.current().Status != StatusCompleted {
				t.Fatalf("status %v", s.current().Status)
			}
		},
		"refusal compensates the completed prefix": func(s *sagaPair) {
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
		"retryable failures exhaust into compensation, stale attempt ignored": func(s *sagaPair) {
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
		"late success after abandonment is compensated (direction 4)": func(s *sagaPair) {
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
		"late compensation success only alarms once per incarnation": func(s *sagaPair) {
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
		"resume opens a new incarnation and old results are stale": func(s *sagaPair) {
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
		"manual compensate in compensate phase and on completed": func(s *sagaPair) {
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
		"deadline aborts the forward direction": func(s *sagaPair) {
			s.id = IDOf("rally", "deadline")
			deadline := s.at.Add(500 * time.Millisecond)
			if _, err := s.redis.StartSaga(s.ctx, StartRequest{ID: s.id, Type: "rally", BusinessKey: "deadline", DefinitionVersion: 1, DeadlineAt: deadline, Now: s.at}); err != nil {
				t.Fatal(err)
			}
			request, _ := prepareStart(StartRequest{Type: "rally", BusinessKey: "deadline", DefinitionVersion: 1, DeadlineAt: deadline}, 1<<20)
			digest, _ := startIntentDigest(request)
			msg := s.u.startInput("deadline", nil, "a1", "")
			msg.Data = mustJSON(t, startInput{V: LogWireVersion, Request: request, Attempt: "a1", Digest: digest})
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
			scenario(newSagaPair(t, definition))
		})
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
