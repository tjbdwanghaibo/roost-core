package saga

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// partition.go 是 JetStream 协调器的分区单写者循环（设计 §4.3～§4.7）。
//
// 一个分区一个 goroutine：取一批输入（启动 / 结果 / 运维）与到期定时器 → 在内存副本（overlay，写时复制）上用 transition.go
// 的纯函数判定 → 把这一轮的事件一次发布（1 条普通发布、多条原子批量，超过服务端上限切成几次顺序 fenced 发布）→ PubAck 之后
// 才提交内存（commit）→ 回复判定、回答查询、剪枝、按需快照。分区之间并行，一个分区同时只有一次发布在途。
//
// 单写者隔离：每次发布的首条带“分区事件过滤（<prefix>.<k>.ev.>）的期望序号 = 内存记的 lastEv”。期望序号对不上而过滤上最后一条
// 不是自己写的 → 别的写者推进了这个分区（崩溃重启的短暂重叠或部署错误）→ ErrPartitionFenced，交给 RuntimeFailure 让整个进程
// fail-stop（App 级统一保证）。
//
// 发布结果三类（JetStreamLog 的约定）：nil 已持久；ErrLogOutcomeUnknown 可能已落库，同消息同 fence 重发，按“Duplicate /
// 10201 / 过滤上最后一条是自己的 = 已提交”判定，超过 PublishUnknownBudget → ErrPublishUnknown（fail-stop）；其余确定没写入：
// ErrLogBatchIncomplete（10176，同流同时打开的批超限等）立即重发；ErrLogRejected（流满等）不重试，ErrPartitionRejected
// fail-stop 并告警。

// partition 是一个分区的协调状态。字段除注明者外只在循环 goroutine 里读写（重放在循环启动前完成）。
type partition struct {
	e      *Engine
	k      int
	stream string
	writer string

	sagas   map[string]*sagaState // 已提交状态（值提交后不再修改）
	lastEv  uint64                // 分区事件过滤上最后一条事件的流序号：下一次发布的期望序号
	inHi    uint64                // 已处理到的最大输入序号
	round   uint64
	wheel   *wheel
	pending []fnats.LogMsg // 重放读到、尚未处理的输入
	// followFrom 是接上输入 Follow 的起点：重放读到的最后一条之后。
	followFrom uint64
	// terminals 是按到达终态顺序排队的可剪枝记录（Completed / Compensated / Failed），剪枝从队头取。
	terminals []pruneEntry
	dueBuf    []string

	sinceSnapshot  int
	lastSnapshotAt time.Time
	// snapshotRev / snapshotObject 只由快照 goroutine 读写；同一时刻至多一个快照在途（snapshotSlot）。
	snapshotRev    uint64
	snapshotObject string
	snapshotSlot   chan struct{}
	purgeStalls    map[string]purgeStall

	inputs  chan fnats.LogMsg
	queries chan pendingQuery
	stopCh  chan struct{}
	follow  fnats.LogFollow
	done    chan struct{}
	workers sync.WaitGroup // 快照 goroutine

	failOnce sync.Once
	failErr  error // failOnce 之内写，done 关闭之后可读
	cancel   context.CancelFunc
}

type pruneEntry struct {
	id        string
	updatedAt time.Time
}

type purgeStall struct {
	floor uint64
	since time.Time
	told  bool
}

// pendingQuery 是交给分区循环回答的查询。
type pendingQuery struct {
	request queryRequest
	reply   string
}

// ---- 一轮 ----

// round 是一轮判定的结果：overlay 是本轮改过的记录（新值），events 是要发布的事件（同一 saga 在本轮的几次转移合并成最后一条，
// 中间是终态的保留——它要进 done subject），verdicts 是要回复的判定，after 是提交之后才做的计数与日志。
type round struct {
	p             *partition
	now           time.Time
	maxIn         uint64
	overlay       map[string]*sagaState
	touched       []string
	events        []logEvent
	eventSubjects []string
	eventAt       map[string]int
	verdicts      []pendingVerdict
	after         []func()
	counts        partitionCounts
}

// pendingVerdict 是一次启动 / 运维尝试的判定；accepted / existing 的记录在回复时取已提交的值。
type pendingVerdict struct {
	reply   string
	attempt string
	outcome string
	reason  string
	errText string
	id      string
	record  *Record
}

// partitionCounts 是一轮的计数增量，提交后并进 Engine 的统计。
type partitionCounts struct {
	started, dispatched, completed, compensated, failed, manualRequired uint64
	duplicates, lateAfterAbandon, staleIncarnation, staleAttempt        uint64
	reopened, rejectedInputs, admissionRejected, unknownSaga            uint64
}

func (p *partition) newRound(now time.Time) *round {
	return &round{p: p, now: now, maxIn: p.inHi, overlay: map[string]*sagaState{}, eventAt: map[string]int{}}
}

// get 返回这一轮看到的记录：本轮改过的取新值，否则取已提交的（只读，修改前必须 clone）。
func (r *round) get(id string) *sagaState {
	if st, ok := r.overlay[id]; ok {
		return st
	}
	return r.p.sagas[id]
}

func (r *round) put(st *sagaState) {
	id := st.Record.ID
	if _, ok := r.overlay[id]; !ok {
		r.touched = append(r.touched, id)
	}
	r.overlay[id] = st
}

// apply 把 before → after 定成一次转移（planTransition 决定代际与要关闭的操作），在副本上记回执与 tombstone，放进 overlay。
// 回执重复 / 身份冲突的检查由调用方先做（与存储的 Apply 同一顺序：回执检查在状态推进之前）。
func (r *round) apply(before *sagaState, after Record, t transition) *sagaState {
	written, closeOperation := planTransition(before.Record, after, t)
	next := before.clone()
	next.Record = written
	if t.receipt != nil {
		if next.Receipts == nil {
			next.Receipts = map[string]string{}
		}
		next.Receipts[t.receipt.CommandID] = receiptDigest(*t.receipt)
	}
	if closeOperation != "" {
		if next.Ops == nil {
			next.Ops = map[string]opTombstone{}
		}
		tomb, exists := next.Ops[closeOperation]
		switch {
		case t.receipt != nil && t.receipt.Success:
			tomb.Closure = OperationClosedWithResult
		case !exists:
			tomb.Closure = OperationAbandoned
		}
		next.Ops[closeOperation] = tomb
	}
	r.put(next)
	return next
}

// emit 记一条事件。同一 saga 本轮已有一条非终态、不带命令的事件时用新的替换它（它的全部内容都在新状态里）。
func (r *round) emit(st *sagaState, kind string, command *Command) {
	subject := r.p.e.layout.NoneEventSubject(r.p.k)
	switch {
	case command != nil:
		subject = r.p.e.layout.CommandEventSubject(r.p.k, command.Topic)
	case st.Record.Status.Terminal():
		subject = r.p.e.layout.DoneEventSubject(r.p.k)
	}
	ev := logEvent{Part: r.p.k, Writer: r.p.writer, At: r.now, Kind: kind, Saga: *st, Command: command}
	id := st.Record.ID
	if i, ok := r.eventAt[id]; ok && r.events[i].Command == nil && !r.events[i].Saga.Record.Status.Terminal() {
		r.events[i] = ev
		r.eventSubjects[i] = subject
		return
	}
	r.eventAt[id] = len(r.events)
	r.events = append(r.events, ev)
	r.eventSubjects = append(r.eventSubjects, subject)
}

func (r *round) verdict(reply, attempt, outcome string, configure func(*pendingVerdict)) {
	if reply == "" {
		return
	}
	v := pendingVerdict{reply: reply, attempt: attempt, outcome: outcome}
	if configure != nil {
		configure(&v)
	}
	r.verdicts = append(r.verdicts, v)
}

func (r *round) rejectInput(kind, reason string, err error) {
	r.counts.rejectedInputs++
	k := r.p.k
	r.after = append(r.after, func() {
		metrics.IncCounter("saga.partition.rejected_input_total", metrics.Labels{"kind": kind, "reason": reason}, 1)
		slog.Warn("saga: partition skipped an invalid input", "partition", k, "kind", kind, "reason", reason, "err", err)
	})
}

// decide 判定一轮：先按序处理输入，再处理到期（轮里到期的与本轮改过、已经到期的），所以已在输入里的结果先于同轮的超时被接收。
// 只读写内存，不做 I/O。
func (p *partition) decide(batch []fnats.LogMsg, now time.Time) *round {
	r := p.newRound(now)
	for _, msg := range batch {
		if msg.Seq <= p.inHi {
			continue
		}
		r.maxIn = max(r.maxIn, msg.Seq)
		switch kind := p.e.layout.inputKindOf(p.k, msg.Subject); kind {
		case "start":
			r.start(msg)
		case "res":
			r.result(msg)
		case "op":
			r.operation(msg)
		default:
			r.rejectInput("unknown", "subject", fmt.Errorf("subject %q", msg.Subject))
		}
	}
	due := p.wheel.advance(now, p.dueBuf[:0])
	due = append(due, r.touched...)
	seen := make(map[string]struct{}, len(due))
	for _, id := range due {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		st := r.get(id)
		// “到期没有”必须与时间轮同一把尺子：轮按墙钟（UnixNano）落槽、取出，这里也按墙钟比。用 time.Time 的 After 比较时，
		// 两边都带单调读数就按单调钟比；墙钟比单调钟快几微秒（本机实测约 3.5ppm），而步骤超时是 tick 的整数倍、判定那一轮的
		// 时刻常常正好落在截止上，轮已经取出的条目会被当成“没到期的陈旧条目”丢掉，记录停在 waiting / pending、再也不重试
		// （2026-10-28 参与方停 10s 的真实用例：150 笔卡住 8～17 笔）。
		if st == nil || st.Record.Status.Terminal() || st.Record.NextRunAt.UnixNano() > now.UnixNano() {
			continue // 陈旧的定时条目：状态机按记录当前的 NextRunAt 判定
		}
		r.due(st)
	}
	p.dueBuf = due[:0]
	return r
}

// start 处理一条启动意图（设计 §2.3、§6、§7）：内存里没有这个 ID → 准入通过就新建；有且启动摘要相同 → 已存在；不同 → 身份冲突。
func (r *round) start(msg fnats.LogMsg) {
	reply := msg.Headers[replyHeader]
	in, err := decodeStartInput(msg.Data)
	if err != nil {
		r.rejectInput("start", "decode", err)
		r.verdict(reply, "", verdictInvalid, func(v *pendingVerdict) { v.errText = err.Error() })
		return
	}
	e := r.p.e
	request, err := prepareStart(in.Request, e.opts.MaxPayloadBytes)
	if err != nil {
		r.verdict(reply, in.Attempt, verdictInvalid, func(v *pendingVerdict) { v.errText = err.Error() })
		return
	}
	if _, ok := e.definition(request.Type, request.DefinitionVersion); !ok {
		r.verdict(reply, in.Attempt, verdictDefinitionMissing, func(v *pendingVerdict) {
			v.errText = fmt.Sprintf("%s/%d", request.Type, request.DefinitionVersion)
		})
		return
	}
	id := IDOf(request.Type, request.BusinessKey)
	record, err := newStartRecord(request, id, r.now)
	if err == nil && record.StartDigest != in.Digest {
		err = fmt.Errorf("%w: start digest differs from the initiator's", ErrInvalidRecord)
	}
	if err != nil || e.layout.PartitionOf(id) != r.p.k {
		r.rejectInput("start", "content", err)
		r.verdict(reply, in.Attempt, verdictInvalid, func(v *pendingVerdict) { v.errText = fmt.Sprint(err) })
		return
	}
	if existing := r.get(id); existing != nil {
		outcome := verdictIdentityConflict
		if existing.Record.StartDigest == record.StartDigest {
			outcome = verdictExisting
		}
		r.verdict(reply, in.Attempt, outcome, func(v *pendingVerdict) { v.id = id })
		return
	}
	if state, gated := e.admit(); gated && !state.Open {
		r.counts.admissionRejected++
		r.verdict(reply, in.Attempt, verdictRejected, func(v *pendingVerdict) { v.reason = state.Reason })
		return
	}
	record.StartAttempt = in.Attempt
	st := &sagaState{Record: record}
	r.put(st)
	r.emit(st, "start", nil)
	r.counts.started++
	r.verdict(reply, in.Attempt, verdictAccepted, func(v *pendingVerdict) { v.id = id })
}

// result 处理一份结果：判定只由 judgeCompletion / judgeHistory 决定，回执与 tombstone
// 在分区内存里。结果的时间以协调器为准（CompletedAt = 本轮时刻）。
func (r *round) result(msg fnats.LogMsg) {
	e := r.p.e
	c, err := decodeResultInput(msg.Data, e.opts.MaxPayloadBytes)
	if err != nil {
		r.rejectInput("result", "decode", err)
		return
	}
	c.CompletedAt = r.now
	st := r.get(c.SagaID)
	if st == nil {
		// 记录已过终态保留期被剪掉（或从没在这个分区存在过）：迟到成功也找不到归宿，只计数并记 ERROR（设计 §2.3）。
		r.counts.unknownSaga++
		r.after = append(r.after, func() {
			metrics.IncCounter("saga.completion.unknown_saga_total", nil, 1)
			slog.Error("saga: result for a saga this partition does not hold (past terminal retention or never started)",
				"saga_id", c.SagaID, "command_id", c.CommandID, "operation", c.IdempotencyKey, "success", c.Success)
		})
		return
	}
	if !strings.HasPrefix(c.IdempotencyKey, c.SagaID+":") {
		r.rejectInput("result", "identity", fmt.Errorf("operation %q is not of saga %s", c.IdempotencyKey, c.SagaID))
		return
	}
	record := st.Record
	incarnation := commandIDIncarnation(c.IdempotencyKey, c.CommandID)
	switch judgeCompletion(record, c, incarnation) {
	case verdictStaleIncarnation:
		r.counts.staleIncarnation++
		r.after = append(r.after, func() { reportStaleIncarnation(record, c, incarnation) })
	case verdictStaleAttempt:
		r.counts.staleAttempt++
		r.after = append(r.after, func() { reportStaleAttempt(record, c) })
	case verdictHistory:
		r.history(st, c, incarnation)
	default:
		definition, ok := e.definition(record.Type, record.DefinitionVersion)
		if !ok {
			// 记录的定义版本没注册：到期时 planDue 会把它 fence 到 ManualRequired。结果不接收（输入不能重投），注册定义后
			// Resume 开新一生，参与方按回执回放这次结论。
			r.rejectInput("result", "definition_missing", fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, record.Type, record.DefinitionVersion))
			return
		}
		if duplicate, conflict := receiptSeen(st, c); duplicate || conflict {
			if conflict {
				r.rejectInput("result", "identity", fmt.Errorf("%w: command %s", ErrIdentityConflict, c.CommandID))
			} else {
				r.counts.duplicates++
			}
			return
		}
		after := applyCompletion(record, definition, c)
		next := r.apply(st, after, transition{cause: causeResult, receipt: &c})
		r.emit(next, "result", nil)
		r.counts.addTerminal(next.Record.Status)
		if c.Success && compensatingLateStep(record) {
			r.after = append(r.after, func() {
				slog.Info("saga: compensated the step that took effect after the coordinator abandoned it",
					"saga_id", record.ID, "saga_type", record.Type, "late_step", record.Step, "status", next.Record.Status.String(), "version", next.Record.Version)
			})
		}
	}
}

// receiptSeen 是存储 Apply 的回执检查：这个 CommandID 已有回执时，摘要相同是重复、不同是身份冲突。
func receiptSeen(st *sagaState, c Completion) (duplicate, conflict bool) {
	prior, ok := st.Receipts[c.CommandID]
	if !ok {
		return false, false
	}
	return prior == receiptDigest(c), prior != receiptDigest(c)
}

// stateHistory 是分区内存版的 completionHistory：这个 CommandID 有回执 → 已记录且是回执
// （摘要不同是身份冲突）；否则操作有 tombstone → 已记录，带关闭方式；都没有 → 没有记录。
func stateHistory(st *sagaState, c Completion) (completionHistory, error) {
	if prior, ok := st.Receipts[c.CommandID]; ok {
		if prior != receiptDigest(c) {
			return completionHistory{}, ErrIdentityConflict
		}
		return completionHistory{Recorded: true, Receipt: true}, nil
	}
	if tomb, ok := st.Ops[c.IdempotencyKey]; ok {
		return completionHistory{Recorded: true, Closure: tomb.Closure}, nil
	}
	return completionHistory{}, nil
}

// history 处理 judgeCompletion 判为 verdictHistory 的结果（不在等这个结论：重复、迟到成功、旧尝试 / 旧一生）。
func (r *round) history(st *sagaState, c Completion, incarnation uint32) {
	record := st.Record
	history, err := stateHistory(st, c)
	if err != nil {
		r.rejectInput("result", "identity", fmt.Errorf("%w: command %s", err, c.CommandID))
		return
	}
	verdict, step := judgeHistory(record, c, history)
	switch verdict {
	case historyNotWaiting:
		r.rejectInput("result", "not_waiting", fmt.Errorf("%w: command %s", ErrNotWaiting, c.CommandID))
	case historyLateStep:
		next := r.apply(st, lateStepState(record, c, step), transition{cause: causeLateSuccess, receipt: &c})
		r.emit(next, "late_success", nil)
		r.counts.lateAfterAbandon++
		reopened := record.Status == StatusFailed || record.Status == StatusCompensated
		if reopened {
			r.counts.reopened++
		}
		r.after = append(r.after, func() {
			reportLateCompensation(record, c)
			if reopened {
				reportReopen(record, next.Record, reopenLateSuccess)
			}
		})
	case historyLateAlarm:
		tomb, ok := st.Ops[c.IdempotencyKey]
		if !ok || containsIncarnation(tomb.LateAlarms, incarnation) {
			r.counts.duplicates++ // 没有 tombstone 或这一代际已告警过：与 LateSuccessAlarmStore 返回 false 同样处理
			return
		}
		next := st.clone()
		tomb.LateAlarms = append(slices.Clone(tomb.LateAlarms), incarnation)
		next.Ops[c.IdempotencyKey] = tomb
		r.put(next)
		r.emit(next, "late_alarm", nil)
		r.counts.lateAfterAbandon++
		r.after = append(r.after, func() { reportLateAfterAbandon(record, c) })
	default:
		r.counts.duplicates++
	}
}

func containsIncarnation(list []uint32, incarnation uint32) bool {
	for _, v := range list {
		if v == incarnation {
			return true
		}
	}
	return false
}

// operation 处理一条运维意图（Resume / Compensate），规则见 planResume / planCompensate，另加回执保留期守卫
// （选项 RR-A）：Resume 与补偿方向的 Compensate 会重新执行操作，记录上次更新早于“现在 − 回执保留期 + 最长步骤窗口”时拒绝。
func (r *round) operation(msg fnats.LogMsg) {
	reply := msg.Headers[replyHeader]
	in, err := decodeOpInput(msg.Data)
	if err != nil {
		r.rejectInput("op", "decode", err)
		r.verdict(reply, "", verdictInvalid, func(v *pendingVerdict) { v.errText = err.Error() })
		return
	}
	st := r.get(in.ID)
	if st == nil {
		r.verdict(reply, in.Attempt, verdictNotFound, nil)
		return
	}
	e := r.p.e
	record := st.Record
	definition, defined := e.definition(record.Type, record.DefinitionVersion)
	refuse := func(err error) {
		outcome := verdictRefused
		switch {
		case errors.Is(err, ErrDeadlineExpired):
			outcome = verdictDeadlineExpired
		case errors.Is(err, ErrDefinitionMissing):
			outcome = verdictDefinitionMissing
		case errors.Is(err, ErrReceiptsExpired):
			outcome = verdictReceiptsExpired
		case errors.Is(err, ErrInvalidRecord):
			outcome = verdictInvalid
		}
		r.verdict(reply, in.Attempt, outcome, func(v *pendingVerdict) { v.errText = err.Error() })
	}
	switch in.Kind {
	case opResume:
		request, deadline, err := prepareResume(*in.Resume, in.Now)
		if err != nil {
			refuse(err)
			return
		}
		after, err := planResume(record, defined, request, deadline)
		if err != nil {
			refuse(err)
			return
		}
		if e.receiptsExpired(record, definition, r.now) {
			refuse(ErrReceiptsExpired)
			return
		}
		next := r.apply(st, after, transition{cause: causeResume})
		r.emit(next, "resume", nil)
		r.reopenedByOperator(record, next, reopenResume)
	case opCompensate:
		after, write, err := planCompensate(record, in.Reason, in.Now.UTC())
		if !write {
			switch {
			case err == nil:
				r.verdict(reply, in.Attempt, verdictUnchanged, func(v *pendingVerdict) { v.record = &after })
			case errors.Is(err, ErrSagaCompleted):
				r.verdict(reply, in.Attempt, verdictSagaCompleted, func(v *pendingVerdict) { v.record = &after })
			default:
				refuse(err)
			}
			return
		}
		if record.Phase == PhaseCompensate && e.receiptsExpired(record, definition, r.now) {
			refuse(ErrReceiptsExpired)
			return
		}
		next := r.apply(st, after, transition{cause: causeManualCompensate})
		r.emit(next, "compensate", nil)
		r.reopenedByOperator(record, next, reopenCompensate)
	}
	r.verdict(reply, in.Attempt, verdictAccepted, func(v *pendingVerdict) { v.id = in.ID })
}

func (r *round) reopenedByOperator(before Record, next *sagaState, reason string) {
	if before.LateStep == 0 {
		return
	}
	r.counts.reopened++
	r.after = append(r.after, func() { reportReopen(before, next.Record, reason) })
}

// due 处理一条到期记录（planDue 的全部出口）；派发的命令带分区号，参与方据此把结果发回这个分区。
func (r *round) due(st *sagaState) {
	e := r.p.e
	definition, defined := e.definition(st.Record.Type, st.Record.DefinitionVersion)
	plan := planDue(st.Record, definition, defined, r.now)
	next := r.apply(st, plan.after, transition{cause: plan.cause})
	var command *Command
	if plan.command != nil {
		c := *plan.command
		c.Partition = uint16(r.p.k)
		command = &c
	}
	r.emit(next, eventKindOf(plan.cause), command)
	switch plan.cause {
	case causeDispatch:
		r.counts.dispatched++
	case causeInvalidStep:
		r.counts.failed++
		r.counts.manualRequired++
	default:
		r.counts.addTerminal(next.Record.Status)
	}
}

func (c *partitionCounts) addTerminal(status Status) {
	switch status {
	case StatusCompleted:
		c.completed++
	case StatusCompensated:
		c.compensated++
	case StatusFailed:
		c.failed++
	case StatusManualRequired:
		c.failed++
		c.manualRequired++
	}
}

// ---- 循环 ----

// run 是分区循环，直到 ctx 结束或分区 fail-stop。
func (p *partition) run(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(p.e.opts.Tick)
	defer ticker.Stop()
	for {
		batch := p.pending
		p.pending = nil
		if len(batch) == 0 {
			select {
			case <-ctx.Done():
				return
			case msg := <-p.inputs:
				batch = append(batch, msg)
			case <-ticker.C:
			case q := <-p.queries:
				p.answer(q)
				continue
			}
		}
	drain:
		for len(batch) < p.e.opts.MaxBatch {
			select {
			case msg := <-p.inputs:
				batch = append(batch, msg)
			default:
				break drain
			}
		}
		now := p.e.now()
		r := p.decide(batch, now)
		if err := p.commit(ctx, r); err != nil {
			if ctx.Err() != nil {
				return
			}
			p.fail(err)
			return
		}
		p.answerQueued()
		p.prune(now)
		p.maybeSnapshot(ctx, now)
	}
}

// commit 发布这一轮的事件，PubAck 之后才交给 install 改内存。没有事件的轮只推进输入水位（输入已经判定过、不会改变状态）。
func (p *partition) commit(ctx context.Context, r *round) error {
	last := p.lastEv
	if len(r.events) > 0 {
		var err error
		if last, err = p.publish(ctx, r); err != nil {
			return err
		}
	}
	p.install(r, last)
	return nil
}

// install 是提交的内存部分（事件已持久之后）：overlay 并进已提交状态、推进水位、排定时器与剪枝队列，然后计数、记日志、回复判定、
// 通知本进程的终态等待者。
func (p *partition) install(r *round, lastEv uint64) {
	if len(r.events) > 0 {
		p.lastEv = lastEv
		for _, id := range r.touched {
			st := r.overlay[id]
			p.sagas[id] = st
			switch {
			case !st.Record.Status.Terminal():
				p.wheel.schedule(id, st.Record.NextRunAt)
			case st.Record.Status != StatusManualRequired:
				p.terminals = append(p.terminals, pruneEntry{id: id, updatedAt: st.Record.UpdatedAt})
			}
		}
		p.sinceSnapshot += len(r.events)
		p.e.stats.events.Add(uint64(len(r.events)))
	}
	p.inHi = r.maxIn
	p.e.stats.add(r.counts)
	for _, fn := range r.after {
		fn()
	}
	for _, v := range r.verdicts {
		p.sendVerdict(v)
	}
	if len(r.events) > 0 {
		p.e.watch.observeCommitted(r.events)
	}
}

// publish 把一轮的事件写进日志：按 LogBatchMaxMessages 切块，每块一次 fenced 发布（首条带期望序号 = 上一块的末条序号）。
// 中间块的事件带上一轮的输入水位，只有最后一块带这一轮的：中途崩溃时这一轮的输入会被重新处理（判定都幂等：启动按摘要判“已存在”，
// 结果按回执判重复，运维意图按状态判定）。返回最后一条事件的流序号。
func (p *partition) publish(ctx context.Context, r *round) (uint64, error) {
	p.round++
	// MsgID = <写者>.<分区>.<轮首的 lastEv>.<轮号>.<序号>：同一写者的各分区共用流，分区号必须在里面（否则两个分区第一轮的
	// MsgID 相同，服务端按去重吞掉或报 10158）。
	base := fmt.Sprintf("%s.%d.%d.%d", p.writer, p.k, p.lastEv, p.round)
	last := p.lastEv
	for start := 0; start < len(r.events); start += fnats.LogBatchMaxMessages {
		end := min(start+fnats.LogBatchMaxMessages, len(r.events))
		in := p.inHi
		if end == len(r.events) {
			in = r.maxIn
		}
		msgs := make([]fnats.LogMsg, 0, end-start)
		for i := start; i < end; i++ {
			ev := r.events[i]
			ev.In = in
			data, err := encodeEvent(ev)
			if err != nil {
				return 0, fmt.Errorf("saga: encode event of %s: %w", ev.Saga.Record.ID, err)
			}
			msgs = append(msgs, fnats.LogMsg{Subject: r.eventSubjects[i], Data: data, MsgID: fmt.Sprintf("%s.%d", base, i)})
		}
		seq, err := p.publishChunk(ctx, msgs, fnats.LogFence{Filter: p.e.layout.EventFilter(p.k), LastSeq: last})
		if err != nil {
			return 0, err
		}
		last = seq
	}
	return last, nil
}

// publishChunk 一次提交 msgs 并处理三类结果（见文件注释）。
func (p *partition) publishChunk(ctx context.Context, msgs []fnats.LogMsg, fence fnats.LogFence) (uint64, error) {
	if p.e.hooks.beforePublish != nil {
		p.e.hooks.beforePublish(p.k)
	}
	deadline := time.Now().Add(p.e.opts.PublishUnknownBudget)
	lastID := msgs[len(msgs)-1].MsgID
	maybeStored := false
	for attempt := 0; ; attempt++ {
		publishCtx, cancel := context.WithTimeout(ctx, p.e.opts.PublishTimeout)
		ack, err := p.e.log.PublishFenced(publishCtx, msgs, fence)
		cancel()
		switch {
		case err == nil:
			if ack.Duplicate {
				p.e.stats.publishResolved.Add(1)
			}
			return ack.Seq, nil
		case ctx.Err() != nil:
			return 0, ctx.Err()
		case errors.Is(err, fnats.ErrLogWrongLastSequence), errors.Is(err, fnats.ErrLogDuplicateBatch):
			// 期望序号不符或批已在去重窗口内：看分区事件过滤上的最后一条是不是这一块的最后一条。
			seq, mine, lerr := p.lastIsMine(ctx, lastID)
			if lerr != nil {
				return 0, fmt.Errorf("saga: partition %d: resolve %v: %w", p.k, err, lerr)
			}
			if mine {
				p.e.stats.publishResolved.Add(1)
				return seq, nil
			}
			if errors.Is(err, fnats.ErrLogDuplicateBatch) {
				return 0, fmt.Errorf("%w: partition %d: batch reported duplicate but the last event is not ours (%v)", ErrPartitionFenced, p.k, err)
			}
			return 0, fmt.Errorf("%w: partition %d expected last event %d: %v", ErrPartitionFenced, p.k, fence.LastSeq, err)
		case errors.Is(err, fnats.ErrLogRejected), errors.Is(err, fnats.ErrLogBatchTooLarge):
			return 0, fmt.Errorf("%w: partition %d: %v", ErrPartitionRejected, p.k, err)
		case errors.Is(err, fnats.ErrLogOutcomeUnknown):
			maybeStored = true
			p.e.stats.publishUnknown.Add(1)
		case errors.Is(err, fnats.ErrLogBatchIncomplete):
			// 10176：服务端丢弃了这个批（同流同时打开的批超限、缺号、批超时），确定没写入，立即重发（驱动每次换批号）。
			p.e.stats.batchIncomplete.Add(1)
		default:
			// 其余错误（连接断开中、驱动已关闭等）：这一次确定没写入，在预算内重发。
			p.e.stats.publishRetried.Add(1)
		}
		if time.Now().After(deadline) {
			if maybeStored {
				return 0, fmt.Errorf("%w: partition %d after %d attempts: %v", ErrPublishUnknown, p.k, attempt+1, err)
			}
			return 0, fmt.Errorf("saga: partition %d publish failed for %v: %w", p.k, p.e.opts.PublishUnknownBudget, err)
		}
		if !errors.Is(err, fnats.ErrLogBatchIncomplete) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// lastIsMine 读分区事件过滤上的最后一条（leader 读）：MsgID 是 lastID 就是这一块已经提交。
func (p *partition) lastIsMine(ctx context.Context, lastID string) (uint64, bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, p.e.opts.PublishTimeout)
	defer cancel()
	msg, err := p.e.log.LastMsg(readCtx, p.stream, p.e.layout.EventFilter(p.k))
	if err != nil {
		return 0, false, err
	}
	return msg.Seq, msg.MsgID == lastID, nil
}

// fail 让分区停下（只一次）：记下原因，报告给 RuntimeFailure（整个进程 fail-stop），记 ERROR 与计数。可以从快照 goroutine 调用。
func (p *partition) fail(err error) {
	p.failOnce.Do(func() {
		p.failErr = err
		reason := "error"
		switch {
		case errors.Is(err, ErrPartitionFenced):
			reason = "fenced"
		case errors.Is(err, ErrPublishUnknown):
			reason = "publish_unknown"
		case errors.Is(err, ErrPartitionRejected):
			reason = "rejected"
		}
		metrics.IncCounter("saga.partition.fail_stop_total", metrics.Labels{"reason": reason}, 1)
		slog.Error("saga: partition stops (fail-stop)", "partition", p.k, "reason", reason, "err", err)
		p.e.stats.failStops.Add(1)
		if p.cancel != nil {
			p.cancel()
		}
		p.e.reportFailure(fmt.Errorf("saga partition %d: %w", p.k, err))
	})
}

// sendVerdict 把判定发给这次尝试的回复 subject（核心 NATS，至多一次；丢了由发起方的 Get 补查兜底）。
func (p *partition) sendVerdict(v pendingVerdict) {
	reply := verdictReply{V: WireVersion, Attempt: v.attempt, Outcome: v.outcome, Reason: v.reason, Error: v.errText, Record: v.record}
	if v.id != "" && reply.Record == nil {
		if st := p.sagas[v.id]; st != nil {
			record := st.Record.Clone()
			reply.Record = &record
		}
	}
	p.e.publishReply(v.reply, reply)
}

// ---- 查询 ----

// answerQueued 回答排队的查询（不阻塞）。
func (p *partition) answerQueued() {
	for {
		select {
		case q := <-p.queries:
			p.answer(q)
		default:
			return
		}
	}
}

// answer 从已提交状态回答一次查询（Get / List）。
func (p *partition) answer(q pendingQuery) {
	reply := queryReply{V: WireVersion, Part: p.k}
	switch q.request.Kind {
	case "get":
		if st := p.sagas[q.request.ID]; st != nil {
			record := st.Record.Clone()
			reply.Found, reply.Record = true, &record
		}
	case "list":
		reply.Found = true
		reply.Records = listRecords(p.sagas, *q.request.Query)
	default:
		reply.Error = "unknown query"
	}
	p.e.publishReply(q.reply, reply)
}

// listRecords 按 Query 过滤一个分区的记录（最多 Limit 条，按更新时间、ID 排序后截取）。
func listRecords(sagas map[string]*sagaState, q Query) []Record {
	out := make([]Record, 0)
	for _, st := range sagas {
		r := st.Record
		if q.Type != "" && r.Type != q.Type {
			continue
		}
		if q.DefinitionVersion != 0 && r.DefinitionVersion != q.DefinitionVersion {
			continue
		}
		if len(q.Statuses) > 0 && !containsStatus(q.Statuses, r.Status) {
			continue
		}
		if !q.UpdatedBefore.IsZero() && !r.UpdatedAt.Before(q.UpdatedBefore) {
			continue
		}
		out = append(out, r.Clone())
	}
	sortRecords(out)
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

func containsStatus(list []Status, s Status) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- 剪枝 ----

// pruneBatch 是每轮最多剪掉的记录数，避免一次长停顿。
const pruneBatch = 1024

// prune 剪掉终态（Completed / Compensated / Failed）且更新时间早于 now − TerminalRetention 的记录（设计 §4.5）。ManualRequired
// 与非终态常驻。剪掉之后同一业务键可以再开一笔（唯一性窗口 = 终态保留期，选项 W-A）。
func (p *partition) prune(now time.Time) {
	cutoff := now.Add(-p.e.opts.TerminalRetention)
	n := 0
	for len(p.terminals) > 0 && n < pruneBatch {
		head := p.terminals[0]
		if !head.updatedAt.Before(cutoff) {
			break
		}
		p.terminals[0] = pruneEntry{}
		p.terminals = p.terminals[1:]
		n++
		st := p.sagas[head.id]
		if st == nil || !st.Record.UpdatedAt.Equal(head.updatedAt) || !prunable(st.Record.Status) {
			continue // 之后又改过（重开、Resume）：以后面那条为准
		}
		delete(p.sagas, head.id)
		p.e.stats.pruned.Add(1)
	}
}

func prunable(status Status) bool {
	return status == StatusCompleted || status == StatusCompensated || status == StatusFailed
}
