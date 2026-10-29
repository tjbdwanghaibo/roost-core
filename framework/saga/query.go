package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// query.go 是 JetStream 协调器在核心 NATS 上的请求 / 回复（不进流）：
//
//   - 所有者一侧：每个拥有的分区订阅 <prefix>.q.<k>（Get），进程订阅 <prefix>.q.list（List 散播，每个拥有的分区各回一条）；
//     查询交给分区循环、从已提交状态回答（单写者状态不加锁）。
//   - 发起方一侧：启动 / 运维意图带回复头 Roost-Saga-Reply（本 Engine 的回复前缀 + 尝试标识），所有者在提交之后（不写事件的
//     判定在本轮结束时）只回这一次尝试（选项 R-A）。判定回复丢了（所有者在提交后崩溃、回复 subject 没人听）时发起方按
//     StartVerdictTimeout 之后每 200ms 问一次所有者（Get）：启动摘要相同 → 已接受；不同 → 身份冲突；没有 → 继续等（意图还在流里）。
//
// 启动尝试标识 = <Engine 实例随机标识（crypto/rand.Text，128 位）>.<实例内序号>，意图 MsgID = start.<id>.<标识>，全局唯一
// （RV-20261028-01）：MsgID 撞上去重窗口里的另一次尝试，意图会被吞掉、PubAck 照样成功，等待者永远等不到。同一次调用里结果未知的
// 网络重发沿用同一标识（靠流去重只追加一条）；第一次发布就得到 Duplicate → ErrStartIdentityReused。

// replyRouter 把核心 NATS 上的判定回复按尝试标识交给等它的调用。
type replyRouter struct {
	mu      sync.Mutex
	waiting map[string]chan verdictReply
}

func (r *replyRouter) await(attempt string) (<-chan verdictReply, func()) {
	ch := make(chan verdictReply, 1)
	r.mu.Lock()
	r.waiting[attempt] = ch
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.waiting, attempt)
		r.mu.Unlock()
	}
}

func (r *replyRouter) deliver(data []byte) {
	var reply verdictReply
	if json.Unmarshal(data, &reply) != nil || reply.V != WireVersion {
		return
	}
	r.mu.Lock()
	ch := r.waiting[reply.Attempt]
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- reply:
		default: // 同一尝试的重复判定（输入被重新处理）：第一条为准
		}
	}
}

// nextAttempt 是本实例下一次启动 / 运维尝试的标识。
func (e *Engine) nextAttempt() string {
	return e.instance + "." + strconv.FormatUint(e.attemptSeq.Add(1), 10)
}

// publishInput 把一条输入追加到分区（不带期望序号：输入有多个写者，去重靠 MsgID）。结果未知时同 MsgID 重发；
// intent=true 时第一次发布就得到 Duplicate 说明尝试标识被复用（ErrStartIdentityReused）。ctx 结束时，若之前某次可能已落库，
// 返回 ErrStartUnconfirmed（同时满足 ctx 错误）。
func (e *Engine) publishInput(ctx context.Context, msg fnats.LogMsg, intent bool) error {
	maybeStored := false
	for {
		publishCtx, cancel := context.WithTimeout(ctx, e.opts.PublishTimeout)
		ack, err := e.log.PublishFenced(publishCtx, []fnats.LogMsg{msg}, fnats.LogFence{})
		cancel()
		switch {
		case err == nil:
			if ack.Duplicate && intent && !maybeStored {
				return fmt.Errorf("%w: %s", ErrStartIdentityReused, msg.MsgID)
			}
			return nil
		case errors.Is(err, fnats.ErrLogOutcomeUnknown):
			maybeStored = true
		case ctx.Err() == nil:
			return fmt.Errorf("saga: append %s: %w", msg.Subject, err)
		}
		if ctx.Err() != nil {
			if maybeStored {
				return fmt.Errorf("%w: %w", ErrStartUnconfirmed, ctx.Err())
			}
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// submitIntent 追加一条带回复头的意图并等所有者的判定。startDigest 非空（启动）时，超时后按 Get 补查。
func (e *Engine) submitIntent(ctx context.Context, subject, msgID, attempt string, data []byte, id, startDigest string) (Record, error) {
	if !e.ready.Load() {
		return Record{}, ErrClosed
	}
	ch, done := e.replies.await(attempt)
	defer done()
	msg := fnats.LogMsg{Subject: subject, Data: data, MsgID: msgID, Headers: map[string]string{replyHeader: e.replyPrefix + "." + attempt}}
	if err := e.publishInput(ctx, msg, true); err != nil {
		return Record{}, err
	}
	timer := time.NewTimer(e.opts.StartVerdictTimeout)
	defer timer.Stop()
	var poll <-chan time.Time
	for {
		select {
		case reply := <-ch:
			return verdictResult(reply)
		case <-timer.C:
			if startDigest != "" {
				ticker := time.NewTicker(200 * time.Millisecond)
				defer ticker.Stop()
				poll = ticker.C
			}
		case <-poll:
			record, err := e.Get(ctx, id)
			if err == nil {
				if record.StartDigest == startDigest {
					return record, nil
				}
				return Record{}, ErrIdentityConflict
			}
		case <-ctx.Done():
			return Record{}, fmt.Errorf("%w: %w", ErrStartUnconfirmed, ctx.Err())
		}
	}
}

// verdictResult 把判定回复换成调用方看到的结果与错误。
func verdictResult(reply verdictReply) (Record, error) {
	record := func() Record {
		if reply.Record == nil {
			return Record{}
		}
		return *reply.Record
	}
	switch reply.Outcome {
	case verdictAccepted, verdictExisting, verdictUnchanged:
		if reply.Record == nil {
			return Record{}, fmt.Errorf("saga: %s verdict without a record", reply.Outcome)
		}
		return record(), nil
	case verdictIdentityConflict:
		return Record{}, ErrIdentityConflict
	case verdictRejected:
		return Record{}, errAdmissionClosed(reply.Reason)
	case verdictDefinitionMissing:
		return Record{}, fmt.Errorf("%w: %s", ErrDefinitionMissing, reply.Error)
	case verdictNotFound:
		return Record{}, ErrNotFound
	case verdictSagaCompleted:
		return record(), ErrSagaCompleted
	case verdictDeadlineExpired:
		return Record{}, ErrDeadlineExpired
	case verdictReceiptsExpired:
		return Record{}, ErrReceiptsExpired
	case verdictInvalid:
		return Record{}, fmt.Errorf("%w: %s", ErrInvalidRecord, reply.Error)
	case verdictRefused:
		return Record{}, errors.New(reply.Error)
	}
	return Record{}, fmt.Errorf("saga: unknown verdict %q", reply.Outcome)
}

// ask 向分区 k 的所有者问一次（Get）。所有者不在或超时返回 ErrPartitionUnavailable。
func (e *Engine) ask(ctx context.Context, k int, request queryRequest) (queryReply, error) {
	request.V = WireVersion
	data, _ := json.Marshal(request)
	askCtx, cancel := context.WithTimeout(ctx, e.opts.QueryTimeout)
	defer cancel()
	raw, err := e.raw.RequestContext(askCtx, e.layout.QuerySubject(k), data)
	if err != nil {
		if ctx.Err() != nil {
			return queryReply{}, ctx.Err()
		}
		return queryReply{}, fmt.Errorf("%w: partition %d: %v", ErrPartitionUnavailable, k, err)
	}
	var reply queryReply
	if err := json.Unmarshal(raw, &reply); err != nil || reply.V != WireVersion || reply.Part != k {
		return queryReply{}, fmt.Errorf("%w: partition %d answered an undecodable reply", ErrPartitionUnavailable, k)
	}
	if reply.Error != "" {
		return queryReply{}, fmt.Errorf("saga: partition %d: %s", k, reply.Error)
	}
	return reply, nil
}

// askAll 向全部分区所有者散播一次查询，每个分区收一条回复；超时还有分区没回答时返回 ErrPartitionUnavailable 并点名，
// 不返回残缺结果。
func (e *Engine) askAll(ctx context.Context, request queryRequest) ([]queryReply, error) {
	request.V = WireVersion
	data, _ := json.Marshal(request)
	askCtx, cancel := context.WithTimeout(ctx, e.opts.QueryTimeout)
	defer cancel()
	got := make(map[int]queryReply, e.layout.Partitions)
	err := e.log.RequestMany(askCtx, e.layout.ListSubject(), data, func(raw []byte) bool {
		var reply queryReply
		if json.Unmarshal(raw, &reply) == nil && reply.V == WireVersion && reply.Part >= 0 && reply.Part < e.layout.Partitions {
			got[reply.Part] = reply
		}
		return len(got) < e.layout.Partitions
	})
	if len(got) == e.layout.Partitions {
		replies := make([]queryReply, 0, len(got))
		for k := range e.layout.Partitions {
			if got[k].Error != "" {
				return nil, fmt.Errorf("saga: partition %d: %s", k, got[k].Error)
			}
			replies = append(replies, got[k])
		}
		return replies, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var missing []string
	for k := range e.layout.Partitions {
		if _, ok := got[k]; !ok {
			missing = append(missing, strconv.Itoa(k))
		}
	}
	return nil, fmt.Errorf("%w: partitions [%s] did not answer (%v)", ErrPartitionUnavailable, strings.Join(missing, " "), err)
}

// sortRecords 按更新时间、ID 排序（List 的结果顺序）。
func sortRecords(records []Record) {
	slices.SortFunc(records, func(a, b Record) int {
		if c := a.UpdatedAt.Compare(b.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// ---- 所有者一侧 ----

// serveQueries 订阅拥有分区的 Get subject 与列表散播 subject，把查询交给分区循环。订阅回调只做解码与非阻塞投递：
// 分区的查询队列满时丢弃这次查询（发起方超时），不拖住 NATS 回调。
func (e *Engine) serveQueries() error {
	limits := fnats.PendingLimits{Messages: 8192, Bytes: 64 << 20, OnError: func(err error) {
		metrics.IncCounter("saga.query.subscription_errors_total", nil, 1)
		slog.Error("saga: query subscription error", "err", err)
	}}
	route := func(p *partition, request queryRequest, reply string) {
		select {
		case p.queries <- pendingQuery{request: request, reply: reply}:
		default:
			metrics.IncCounter("saga.query.dropped_total", nil, 1)
		}
	}
	for _, p := range e.partitions {
		p := p
		sub, err := e.raw.SubscribeBounded(e.layout.QuerySubject(p.k), limits, func(msg *fnats.Msg) {
			var request queryRequest
			if msg.Reply == "" || json.Unmarshal(msg.Data, &request) != nil || request.V != WireVersion || request.Kind != "get" {
				return
			}
			route(p, request, msg.Reply)
		})
		if err != nil {
			return fmt.Errorf("saga: subscribe queries of partition %d: %w", p.k, err)
		}
		e.subscriptions = append(e.subscriptions, sub)
	}
	if len(e.partitions) == 0 {
		return nil
	}
	sub, err := e.raw.SubscribeBounded(e.layout.ListSubject(), limits, func(msg *fnats.Msg) {
		var request queryRequest
		if msg.Reply == "" || json.Unmarshal(msg.Data, &request) != nil || request.V != WireVersion || request.Kind != "list" || request.Query == nil {
			return
		}
		for _, p := range e.partitions {
			route(p, request, msg.Reply)
		}
	})
	if err != nil {
		return fmt.Errorf("saga: subscribe list queries: %w", err)
	}
	e.subscriptions = append(e.subscriptions, sub)
	return nil
}

// publishReply 经核心 NATS 发一条回复（判定或查询结果），至多一次；失败只计数（发起方按超时与补查处理）。
func (e *Engine) publishReply(subject string, reply any) {
	if subject == "" {
		return
	}
	data, err := json.Marshal(reply)
	if err == nil {
		err = e.raw.PublishOnce(subject, data)
	}
	if err != nil {
		metrics.IncCounter("saga.query.reply_failures_total", nil, 1)
	}
}
