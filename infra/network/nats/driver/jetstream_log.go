package driver

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// JetStreamLogClient 实现 fnats.JetStreamLog：与 JetStreamClient 同一个连接、同一个 nats.go JetStream 上下文。
// 已关闭判据在 client 上（见 Client 的类型注释）：每个导出方法先经 admit，nats.go 的失败经 client.wrapError 翻译，
// Close 之后一律 fnats.ErrClosed（PublishFenced 发送中途被关闭时同时是 fnats.ErrLogOutcomeUnknown）。
//
// 原子批量按 ADR-50 手写头：Nats-Batch-Id / Nats-Batch-Sequence（从 1 起）/ 末条 Nats-Batch-Commit: 1，非末条用核心
// publish、末条用 request 取服务端的批确认——nats.go v1.53.1 没有原子批量的高层 API。语义依据（私有三节点 R3 实测）见
// docs/maintenance/PROPOSAL-2026-10-27-saga-jetstream-log.md §2 与本包 jetstream_log_real_integration_test.go。
type JetStreamLogClient struct {
	js     gojs.JetStream
	client *Client
}

// NewJetStreamLog 在 JetStreamClient 的连接与上下文上建日志原语，不做网络往返。
func NewJetStreamLog(js *JetStreamClient) (*JetStreamLogClient, error) {
	if js == nil || js.js == nil || js.client == nil {
		return nil, errors.New("nats log: jetstream client is nil")
	}
	return &JetStreamLogClient{js: js.js, client: js.client}, nil
}

func (c *JetStreamLogClient) admit() error {
	if c == nil || c.js == nil || c.client == nil {
		return errors.New("nats log: not initialized")
	}
	return c.client.admit()
}

// 服务端 JetStream API 错误码（nats-server server/jetstream_errors_generated.go）。
const (
	jsErrWrongLastSequence         = 10071
	jsErrWrongLastSequenceConstant = 10164
	jsErrBatchIncomplete           = 10176
	jsErrBatchTooLarge             = 10199
	jsErrBatchDuplicateMsgID       = 10201
	jsErrMessageNotFound           = 10037
	// jsErrDuplicateInProcess：同一 MsgID 的另一次提交还在服务端处理（R>1 的提案在途）。日志的调用方 MsgID 唯一时，只可能是
	// “结果未知后同 MsgID 重发”撞上了前一次还没落定的发送，前一次可能提交，所以按结果未知归类，而不是确定没写入（第 2 批加）。
	jsErrDuplicateInProcess = 10158
)

const (
	headerMsgID             = "Nats-Msg-Id"
	headerExpectedSubject   = "Nats-Expected-Last-Subject-Sequence-Subject"
	headerExpectedSequence  = "Nats-Expected-Last-Subject-Sequence"
	headerBatchID           = "Nats-Batch-Id"
	headerBatchSequence     = "Nats-Batch-Sequence"
	headerBatchCommit       = "Nats-Batch-Commit"
	reservedHeaderPrefix    = "nats-"
	jsAPIMessageGetTemplate = "$JS.API.STREAM.MSG.GET.%s"
)

func (c *JetStreamLogClient) EnsureLogStream(ctx context.Context, config fnats.LogStreamConfig) (fnats.LogStreamInfo, error) {
	if err := c.admit(); err != nil {
		return fnats.LogStreamInfo{}, err
	}
	if strings.TrimSpace(config.Name) == "" || len(config.Subjects) == 0 || config.Replicas < 1 || config.Duplicates < 0 || config.MaxBytes < 0 {
		return fnats.LogStreamInfo{}, fmt.Errorf("nats log: invalid log stream config %+v", config)
	}
	if version := c.client.conn.ConnectedServerVersion(); !serverVersionAtLeast(version, fnats.MinLogServerVersion) {
		return fnats.LogStreamInfo{}, c.client.wrapError(fmt.Errorf("%w: connected nats-server %q, need >= %s (atomic batches with Nats-Msg-Id)",
			fnats.ErrLogUnsupportedServer, version, fnats.MinLogServerVersion))
	}
	maxBytes := config.MaxBytes
	if maxBytes == 0 {
		maxBytes = -1
	}
	stream, err := c.js.CreateOrUpdateStream(ctx, gojs.StreamConfig{
		Name:               config.Name,
		Subjects:           append([]string(nil), config.Subjects...),
		Retention:          gojs.LimitsPolicy,
		Storage:            gojs.FileStorage,
		Replicas:           config.Replicas,
		MaxBytes:           maxBytes,
		MaxMsgs:            -1,
		MaxAge:             0,
		Discard:            gojs.DiscardNew,
		Duplicates:         config.Duplicates,
		AllowAtomicPublish: true,
		AllowDirect:        true,
	})
	if err != nil {
		return fnats.LogStreamInfo{}, fmt.Errorf("nats log: ensure stream %q: %w", config.Name, c.client.wrapError(err))
	}
	info := stream.CachedInfo()
	if info == nil {
		return fnats.LogStreamInfo{}, c.client.wrapError(fmt.Errorf("nats log: ensure stream %q: no stream info", config.Name))
	}
	// 老服务端会忽略不认识的字段（AllowAtomicPublish），所以按服务端回报的生效配置核对，不信请求本身。
	got := info.Config
	var mismatch []string
	if !got.AllowAtomicPublish {
		mismatch = append(mismatch, "allow_atomic=false")
	}
	if !got.AllowDirect {
		mismatch = append(mismatch, "allow_direct=false")
	}
	if got.Discard != gojs.DiscardNew {
		mismatch = append(mismatch, "discard="+got.Discard.String())
	}
	if got.MaxAge != 0 {
		mismatch = append(mismatch, "max_age="+got.MaxAge.String())
	}
	if got.Storage != gojs.FileStorage {
		mismatch = append(mismatch, "storage="+got.Storage.String())
	}
	if got.Replicas != config.Replicas {
		mismatch = append(mismatch, fmt.Sprintf("replicas=%d", got.Replicas))
	}
	if config.Duplicates > 0 && got.Duplicates != config.Duplicates {
		mismatch = append(mismatch, "duplicate_window="+got.Duplicates.String())
	}
	if len(mismatch) > 0 {
		return fnats.LogStreamInfo{}, c.client.wrapError(fmt.Errorf("%w: stream %q effective config: %s", fnats.ErrLogUnsupportedServer, config.Name, strings.Join(mismatch, ", ")))
	}
	out := fnats.LogStreamInfo{
		Name:     got.Name,
		Subjects: append([]string(nil), got.Subjects...),
		Replicas: got.Replicas,
		Msgs:     info.State.Msgs,
		Bytes:    info.State.Bytes,
		FirstSeq: info.State.FirstSeq,
		LastSeq:  info.State.LastSeq,
	}
	if info.Cluster != nil {
		out.Leader = info.Cluster.Leader
	}
	return out, nil
}

func (c *JetStreamLogClient) PublishFenced(ctx context.Context, msgs []fnats.LogMsg, fence fnats.LogFence) (fnats.LogPublishAck, error) {
	if err := c.admit(); err != nil {
		return fnats.LogPublishAck{}, err
	}
	if ctx == nil {
		return fnats.LogPublishAck{}, errors.New("nats log: publish context is required")
	}
	// 批号每次调用新生成：同一组消息的重发换一个批号，不会接到服务端可能还留着的上一次半批上（MsgID 不变，去重靠它）。
	batchID := ""
	if len(msgs) > 1 {
		batchID = rand.Text()
	}
	built, err := buildLogMessages(msgs, fence, batchID, c.client.conn.MaxPayload())
	if err != nil {
		return fnats.LogPublishAck{}, c.client.wrapError(err)
	}
	if len(built) == 1 {
		return c.publishSingle(ctx, built[0])
	}
	return c.publishBatch(ctx, built)
}

func (c *JetStreamLogClient) publishSingle(ctx context.Context, msg *gonats.Msg) (fnats.LogPublishAck, error) {
	// 不让 nats.go 自己重试：重发与判定由调用方按结果未知的规则做。
	ack, err := c.js.PublishMsg(ctx, msg, gojs.WithRetryAttempts(0))
	if err != nil {
		var apiErr *gojs.APIError
		if errors.As(err, &apiErr) && !c.client.closed() {
			return fnats.LogPublishAck{}, logAPIError(int(apiErr.ErrorCode), apiErr.Description)
		}
		return fnats.LogPublishAck{}, fmt.Errorf("%w: publish %s: %w", fnats.ErrLogOutcomeUnknown, msg.Subject, c.client.wrapError(err))
	}
	return fnats.LogPublishAck{Stream: ack.Stream, Seq: ack.Sequence, Count: 1, Duplicate: ack.Duplicate}, nil
}

// batchAck 是服务端对原子批提交消息的回复（PubAck 加 batch / count）。
type batchAck struct {
	Error  *gojs.APIError `json:"error,omitempty"`
	Stream string         `json:"stream"`
	Seq    uint64         `json:"seq"`
	Batch  string         `json:"batch"`
	Count  int            `json:"count"`
}

func (c *JetStreamLogClient) publishBatch(ctx context.Context, msgs []*gonats.Msg) (fnats.LogPublishAck, error) {
	last := len(msgs) - 1
	for _, msg := range msgs[:last] {
		if err := c.client.conn.PublishMsg(msg); err != nil {
			// 提交消息还没发：服务端最多留着一个未提交的半批，超时后丢弃，这次确定没有写入。
			return fnats.LogPublishAck{}, fmt.Errorf("nats log: atomic batch abandoned before commit: %w", c.client.wrapError(err))
		}
	}
	reply, err := c.client.conn.RequestMsgWithContext(ctx, msgs[last])
	if err != nil {
		return fnats.LogPublishAck{}, fmt.Errorf("%w: atomic batch commit: %w", fnats.ErrLogOutcomeUnknown, c.client.wrapError(err))
	}
	return parseBatchAck(reply.Data, len(msgs))
}

// parseBatchAck 把提交回复翻译成结果：服务端错误按错误码归类（这次确定没写入），成功时核对条数。
func parseBatchAck(data []byte, want int) (fnats.LogPublishAck, error) {
	var ack batchAck
	if err := json.Unmarshal(data, &ack); err != nil {
		return fnats.LogPublishAck{}, fmt.Errorf("%w: undecodable atomic batch reply %q: %v", fnats.ErrLogOutcomeUnknown, data, err)
	}
	if ack.Error != nil {
		return fnats.LogPublishAck{}, logAPIError(int(ack.Error.ErrorCode), ack.Error.Description)
	}
	if ack.Count != want || ack.Seq == 0 {
		// 服务端说成功却对不上条数：不能确定写了什么，按结果未知交给调用方重发判定（重发会得到 10201 或成功）。
		return fnats.LogPublishAck{}, fmt.Errorf("%w: atomic batch reply count=%d seq=%d, want count %d", fnats.ErrLogOutcomeUnknown, ack.Count, ack.Seq, want)
	}
	return fnats.LogPublishAck{Stream: ack.Stream, Seq: ack.Seq, Count: ack.Count}, nil
}

// logAPIError 把服务端明确的拒绝映射成哨兵错误；文本保留错误码与描述。唯一的例外是 10158（同一 MsgID 的前一次提交还在处理），
// 归为结果未知。
func logAPIError(code int, description string) error {
	sentinel := fnats.ErrLogRejected
	switch code {
	case jsErrDuplicateInProcess:
		return fmt.Errorf("%w (err_code=%d: %s)", fnats.ErrLogOutcomeUnknown, code, description)
	case jsErrWrongLastSequence, jsErrWrongLastSequenceConstant:
		sentinel = fnats.ErrLogWrongLastSequence
	case jsErrBatchDuplicateMsgID:
		sentinel = fnats.ErrLogDuplicateBatch
	case jsErrBatchIncomplete:
		sentinel = fnats.ErrLogBatchIncomplete
	case jsErrBatchTooLarge:
		sentinel = fnats.ErrLogBatchTooLarge
	}
	return fmt.Errorf("%w (err_code=%d: %s)", sentinel, code, description)
}

// buildLogMessages 校验并组装要发布的消息：MsgID 必填、业务头不得用保留前缀、首条带期望序号、批量时带批头；
// 每条的数据加头不超过连接的 max_payload（nats.go 的同一判据），超出的一条也不发。
func buildLogMessages(msgs []fnats.LogMsg, fence fnats.LogFence, batchID string, maxPayload int64) ([]*gonats.Msg, error) {
	switch {
	case len(msgs) == 0:
		return nil, errors.New("nats log: nothing to publish")
	case len(msgs) > fnats.LogBatchMaxMessages:
		return nil, fmt.Errorf("%w: %d messages, at most %d per atomic batch", fnats.ErrLogBatchTooLarge, len(msgs), fnats.LogBatchMaxMessages)
	}
	if fence.Filter != "" && !validSubject(fence.Filter, true) {
		return nil, fmt.Errorf("nats log: invalid fence filter %q", fence.Filter)
	}
	out := make([]*gonats.Msg, len(msgs))
	for i, in := range msgs {
		if !validSubject(in.Subject, false) {
			return nil, fmt.Errorf("nats log: message %d: invalid subject %q", i, in.Subject)
		}
		if in.MsgID == "" || strings.ContainsAny(in.MsgID, " \t\r\n") {
			return nil, fmt.Errorf("nats log: message %d: a non-empty MsgID without whitespace is required", i)
		}
		msg := gonats.NewMsg(in.Subject)
		msg.Data = in.Data
		if err := setBusinessHeaders(msg.Header, in.Headers); err != nil {
			return nil, fmt.Errorf("nats log: message %d: %w", i, err)
		}
		msg.Header.Set(headerMsgID, in.MsgID)
		if i == 0 && fence.Filter != "" {
			msg.Header.Set(headerExpectedSubject, fence.Filter)
			msg.Header.Set(headerExpectedSequence, strconv.FormatUint(fence.LastSeq, 10))
		}
		if batchID != "" {
			msg.Header.Set(headerBatchID, batchID)
			msg.Header.Set(headerBatchSequence, strconv.Itoa(i+1))
			if i == len(msgs)-1 {
				msg.Header.Set(headerBatchCommit, "1")
			}
		}
		if size := int64(len(msg.Data) + encodedHeaderSize(msg.Header)); maxPayload > 0 && size > maxPayload {
			return nil, fmt.Errorf("%w: message %d is %d bytes with headers, max_payload is %d", fnats.ErrLogRejected, i, size, maxPayload)
		}
		out[i] = msg
	}
	return out, nil
}

// encodedHeaderSize 是 nats.go 编码头块的字节数（"NATS/1.0\r\n"、每个值一行 "k: v\r\n"、结尾 "\r\n"）。
func encodedHeaderSize(h gonats.Header) int {
	size := len("NATS/1.0\r\n") + len("\r\n")
	for key, values := range h {
		for _, value := range values {
			size += len(key) + len(": ") + len(value) + len("\r\n")
		}
	}
	return size
}

// setBusinessHeaders 写业务头：键非空、不含空白与冒号、不以 "Nats-" 开头（保留给驱动与服务端）；值不含换行。
func setBusinessHeaders(dst gonats.Header, headers map[string]string) error {
	for key, value := range headers {
		if key == "" || strings.ContainsAny(key, " \t\r\n:") {
			return fmt.Errorf("nats: invalid header key %q", key)
		}
		if strings.HasPrefix(strings.ToLower(key), reservedHeaderPrefix) {
			return fmt.Errorf("nats: header %q uses the reserved Nats- prefix", key)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("nats: header %q value contains a line break", key)
		}
		dst.Set(key, value)
	}
	return nil
}

// businessHeaders 取出业务头（每个键第一个值），略过 "Nats-" 开头的保留头；没有业务头时返回 nil。
func businessHeaders(h gonats.Header) map[string]string {
	var out map[string]string
	for key, values := range h {
		if len(values) == 0 || strings.HasPrefix(strings.ToLower(key), reservedHeaderPrefix) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(h))
		}
		out[key] = values[0]
	}
	return out
}

// validSubject 检查发布 subject（不许通配）或过滤（allowWildcards 时允许 '*' 与末尾 '>'）。
func validSubject(subject string, allowWildcards bool) bool {
	if subject == "" || strings.ContainsAny(subject, " \t\r\n") {
		return false
	}
	tokens := strings.Split(subject, ".")
	for i, token := range tokens {
		switch {
		case token == "":
			return false
		case token == "*" || token == ">":
			if !allowWildcards || (token == ">" && i != len(tokens)-1) {
				return false
			}
		case strings.ContainsAny(token, "*>"):
			return false
		}
	}
	return true
}

func toLogMsg(subject string, header gonats.Header, data []byte, seq uint64) fnats.LogMsg {
	return fnats.LogMsg{
		Subject: subject,
		Data:    append([]byte(nil), data...),
		MsgID:   header.Get(headerMsgID),
		Headers: businessHeaders(header),
		Seq:     seq,
	}
}

// LastMsg 用 $JS.API.STREAM.MSG.GET（由流 leader 回答）而不是 direct get：日志流开着 AllowDirect，nats.go 的
// GetLastMsgForSubject 会走 direct get，可能由落后的副本回答一条旧消息——重放终点与结果未知的判定都不能用旧值。
func (c *JetStreamLogClient) LastMsg(ctx context.Context, stream, filter string) (fnats.LogMsg, error) {
	if err := c.admit(); err != nil {
		return fnats.LogMsg{}, err
	}
	if ctx == nil || strings.TrimSpace(stream) == "" || !validSubject(filter, true) {
		return fnats.LogMsg{}, fmt.Errorf("nats log: last message: invalid stream %q or filter %q", stream, filter)
	}
	request, err := json.Marshal(struct {
		LastFor string `json:"last_by_subj"`
	}{LastFor: filter})
	if err != nil {
		return fnats.LogMsg{}, err
	}
	reply, err := c.client.conn.RequestWithContext(ctx, fmt.Sprintf(jsAPIMessageGetTemplate, stream), request)
	if err != nil {
		return fnats.LogMsg{}, fmt.Errorf("nats log: last message of %q on %s: %w", filter, stream, c.client.wrapError(err))
	}
	var response struct {
		Error   *gojs.APIError `json:"error,omitempty"`
		Message *struct {
			Subject string `json:"subject"`
			Seq     uint64 `json:"seq"`
			Header  []byte `json:"hdrs,omitempty"`
			Data    []byte `json:"data,omitempty"`
		} `json:"message,omitempty"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return fnats.LogMsg{}, fmt.Errorf("nats log: last message of %q on %s: undecodable reply: %w", filter, stream, err)
	}
	switch {
	case response.Error != nil && response.Error.ErrorCode == jsErrMessageNotFound:
		return fnats.LogMsg{}, fmt.Errorf("%w: no message matches %q on %s", fnats.ErrLogNotFound, filter, stream)
	case response.Error != nil:
		return fnats.LogMsg{}, fmt.Errorf("nats log: last message of %q on %s: %w", filter, stream, response.Error)
	case response.Message == nil:
		return fnats.LogMsg{}, fmt.Errorf("nats log: last message of %q on %s: empty reply", filter, stream)
	}
	var header gonats.Header
	if len(response.Message.Header) > 0 {
		if header, err = gonats.DecodeHeadersMsg(response.Message.Header); err != nil {
			return fnats.LogMsg{}, fmt.Errorf("nats log: last message of %q on %s: headers: %w", filter, stream, err)
		}
	}
	return toLogMsg(response.Message.Subject, header, response.Message.Data, response.Message.Seq), nil
}

// orderedMessages 建一个有序消费者并开始拉取：from == 0 只要新消息，否则从序号 from 起。
func (c *JetStreamLogClient) orderedMessages(ctx context.Context, stream string, filters []string, from uint64) (gojs.Consumer, gojs.MessagesContext, error) {
	for _, filter := range filters {
		if !validSubject(filter, true) {
			return nil, nil, fmt.Errorf("nats log: invalid filter %q", filter)
		}
	}
	config := gojs.OrderedConsumerConfig{FilterSubjects: append([]string(nil), filters...)}
	if from == 0 {
		config.DeliverPolicy = gojs.DeliverNewPolicy
	} else {
		config.DeliverPolicy = gojs.DeliverByStartSequencePolicy
		config.OptStartSeq = from
	}
	consumer, err := c.js.OrderedConsumer(ctx, stream, config)
	if err != nil {
		return nil, nil, fmt.Errorf("nats log: ordered consumer on %s: %w", stream, c.client.wrapError(err))
	}
	messages, err := consumer.Messages(gojs.PullMaxMessages(1024))
	if err != nil {
		return nil, nil, fmt.Errorf("nats log: ordered consumer on %s: %w", stream, c.client.wrapError(err))
	}
	return consumer, messages, nil
}

func (c *JetStreamLogClient) ReadRange(ctx context.Context, stream string, filters []string, from, to uint64, fn func(fnats.LogMsg) error) error {
	if err := c.admit(); err != nil {
		return err
	}
	if ctx == nil || fn == nil {
		return errors.New("nats log: read range needs a context and a callback")
	}
	if from == 0 {
		from = 1
	}
	if to > 0 && from > to {
		return nil
	}
	consumer, messages, err := c.orderedMessages(ctx, stream, filters, from)
	if err != nil {
		return err
	}
	defer messages.Stop()
	if info := consumer.CachedInfo(); to == 0 && info != nil && info.NumPending == 0 {
		return nil
	}
	for {
		msg, err := messages.Next(gojs.NextContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("nats log: read %s: %w", stream, c.client.wrapError(err))
		}
		meta, err := msg.Metadata()
		if err != nil {
			return fmt.Errorf("nats log: read %s: %w", stream, err)
		}
		if err := fn(toLogMsg(msg.Subject(), msg.Headers(), msg.Data(), meta.Sequence.Stream)); err != nil {
			return err
		}
		if (to > 0 && meta.Sequence.Stream >= to) || (to == 0 && meta.NumPending == 0) {
			return nil
		}
	}
}

func (c *JetStreamLogClient) Follow(ctx context.Context, stream string, filters []string, from uint64, fn func(fnats.LogMsg)) (fnats.LogFollow, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	if ctx == nil || fn == nil {
		return nil, errors.New("nats log: follow needs a context and a callback")
	}
	_, messages, err := c.orderedMessages(ctx, stream, filters, from)
	if err != nil {
		return nil, err
	}
	follow := newLogFollow(messages.Stop)
	go follow.run(func() bool {
		msg, err := messages.Next()
		if err != nil {
			if errors.Is(err, gojs.ErrMsgIteratorClosed) || c.client.closed() || follow.stopping() {
				return false
			}
			// 有序消费者在 Next 里自己重建（心跳丢失、断线、选主）；这里只剩重建本身失败，稍等再试。
			slog.Warn("nats log: follow read failed; retrying", "stream", stream, "err", err)
			return follow.sleep(200 * time.Millisecond)
		}
		meta, err := msg.Metadata()
		if err != nil {
			slog.Warn("nats log: follow message without metadata; skipped", "stream", stream, "subject", msg.Subject(), "err", err)
			return true
		}
		// 有序消费者保证不跳号、不重复；这里再守一道单调，重建边界上万一重复交付也不会让回调看到回退的序号。
		if !follow.advance(meta.Sequence.Stream) {
			return true
		}
		fn(toLogMsg(msg.Subject(), msg.Headers(), msg.Data(), meta.Sequence.Stream))
		return true
	})
	return follow, nil
}

// logFollow 是一个后台读取循环的句柄：run 在自己的 goroutine 里反复调用 step，step 返回 false 结束。回调就在这个
// goroutine 里执行，所以 done 关闭即“没有在途回调、也不会再有”。
type logFollow struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	release  func() // 停止底层读取（让阻塞的 Next / Updates 返回）
	lastSeq  uint64 // 只在 run 的 goroutine 里读写
}

func newLogFollow(release func()) *logFollow {
	return &logFollow{stopCh: make(chan struct{}), done: make(chan struct{}), release: release}
}

func (f *logFollow) run(step func() bool) {
	defer close(f.done)
	for !f.stopping() && step() {
	}
}

func (f *logFollow) stopping() bool {
	select {
	case <-f.stopCh:
		return true
	default:
		return false
	}
}

// sleep 等 d 或停止；返回是否继续。
func (f *logFollow) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-f.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func (f *logFollow) advance(seq uint64) bool {
	if seq <= f.lastSeq {
		return false
	}
	f.lastSeq = seq
	return true
}

// Stop 关闭读取并在 ctx 内等循环（含正在执行的回调）结束；超时返回 ctx 错误，可再调用继续等。
func (f *logFollow) Stop(ctx context.Context) error {
	f.stopOnce.Do(func() {
		close(f.stopCh)
		if f.release != nil {
			f.release()
		}
	})
	select {
	case <-f.done:
		return nil
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("nats log: follow still running: %w", ctx.Err())
	}
}

func (f *logFollow) Done() <-chan struct{} { return f.done }

func (c *JetStreamLogClient) Purge(ctx context.Context, stream, filter string, below uint64) error {
	if err := c.admit(); err != nil {
		return err
	}
	if below <= 1 {
		return nil
	}
	if filter != "" && !validSubject(filter, true) {
		return fmt.Errorf("nats log: invalid purge filter %q", filter)
	}
	handle, err := c.js.Stream(ctx, stream)
	if err != nil {
		return fmt.Errorf("nats log: purge %s: %w", stream, c.client.wrapError(err))
	}
	opts := []gojs.StreamPurgeOpt{gojs.WithPurgeSequence(below)}
	if filter != "" {
		opts = append(opts, gojs.WithPurgeSubject(filter))
	}
	if err := handle.Purge(ctx, opts...); err != nil {
		return fmt.Errorf("nats log: purge %s filter %q below %d: %w", stream, filter, below, c.client.wrapError(err))
	}
	return nil
}

func (c *JetStreamLogClient) DurableAckFloors(ctx context.Context, stream string) (map[string]uint64, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	handle, err := c.js.Stream(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("nats log: consumers of %s: %w", stream, c.client.wrapError(err))
	}
	// 先读流的末序号，再列消费者：一个消费者在列出时没有待投递（NumPending）也没有待确认（NumAckPending）的消息，说明
	// 过滤匹配、序号不超过这个末序号的消息它都已确认，它的有效 floor 至少是这个末序号。服务端的 AckFloor.Stream 只在
	// 匹配过滤的消息被确认时前进：过滤只匹配自己主题的消费者空闲时停在 0（或它最后一条匹配消息），会让按 floor 清理的
	// 调用方（saga 分区）永远清不掉别的主题的消息（2026-10-28 私有三节点实测，设计 §16 第 1 条）。
	streamInfo, err := handle.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("nats log: consumers of %s: %w", stream, c.client.wrapError(err))
	}
	lastSeq := streamInfo.State.LastSeq
	lister := handle.ListConsumers(ctx)
	floors := map[string]uint64{}
	for info := range lister.Info() {
		if info == nil || info.Config.Durable == "" {
			continue
		}
		floor := info.AckFloor.Stream
		if info.NumPending == 0 && info.NumAckPending == 0 {
			floor = max(floor, lastSeq)
		}
		floors[info.Config.Durable] = floor
	}
	if err := lister.Err(); err != nil {
		return nil, fmt.Errorf("nats log: consumers of %s: %w", stream, c.client.wrapError(err))
	}
	return floors, nil
}

func (c *JetStreamLogClient) KeyValue(ctx context.Context, config fnats.LogKVConfig) (fnats.LogKV, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	history := config.History
	if history == 0 {
		history = 1
	}
	kv, err := c.js.CreateOrUpdateKeyValue(ctx, gojs.KeyValueConfig{
		Bucket:         config.Bucket,
		Replicas:       config.Replicas,
		History:        history,
		TTL:            config.MaxAge,
		LimitMarkerTTL: config.LimitMarkerTTL,
		MaxValueSize:   config.MaxValueSize,
		MaxBytes:       config.MaxBytes,
		Storage:        gojs.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("nats log: key value bucket %q: %w", config.Bucket, c.client.wrapError(err))
	}
	return &logKV{kv: kv, client: c.client}, nil
}

// logKV 是 fnats.LogKV 的实现；已关闭判据在 client 上。
type logKV struct {
	kv     gojs.KeyValue
	client *Client
}

// kvError 先看驱动的已关闭状态，再把 nats.go 的 KV 错误归成哨兵：不存在 / 已删除 → ErrLogNotFound；键已存在、修订号不符
// （R1 为 10071，R>1 为 10164）→ ErrLogKVConflict。
func (k *logKV) kvError(op, key string, err error) error {
	if k.client.closed() {
		return fnats.ErrClosed
	}
	var apiErr *gojs.APIError
	switch {
	case errors.Is(err, gojs.ErrKeyNotFound), errors.Is(err, gojs.ErrKeyDeleted):
		return fmt.Errorf("%w: %s %q: %v", fnats.ErrLogNotFound, op, key, err)
	case errors.Is(err, gojs.ErrKeyExists), errors.Is(err, gojs.ErrKeyRevisionMismatch),
		errors.As(err, &apiErr) && (apiErr.ErrorCode == jsErrWrongLastSequence || apiErr.ErrorCode == jsErrWrongLastSequenceConstant):
		return fmt.Errorf("%w: %s %q: %v", fnats.ErrLogKVConflict, op, key, err)
	}
	return fmt.Errorf("nats log: kv %s %q: %w", op, key, k.client.wrapError(err))
}

func (k *logKV) Get(ctx context.Context, key string) (fnats.LogKVEntry, error) {
	if err := k.client.admit(); err != nil {
		return fnats.LogKVEntry{}, err
	}
	entry, err := k.kv.Get(ctx, key)
	if err != nil {
		return fnats.LogKVEntry{}, k.kvError("get", key, err)
	}
	return fnats.LogKVEntry{Key: entry.Key(), Value: append([]byte(nil), entry.Value()...), Revision: entry.Revision()}, nil
}

func (k *logKV) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := k.client.admit(); err != nil {
		return 0, err
	}
	revision, err := k.kv.Create(ctx, key, value)
	if err != nil {
		return 0, k.kvError("create", key, err)
	}
	return revision, nil
}

func (k *logKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if err := k.client.admit(); err != nil {
		return 0, err
	}
	next, err := k.kv.Update(ctx, key, value, revision)
	if err != nil {
		return 0, k.kvError("update", key, err)
	}
	return next, nil
}

func (k *logKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := k.client.admit(); err != nil {
		return 0, err
	}
	revision, err := k.kv.Put(ctx, key, value)
	if err != nil {
		return 0, k.kvError("put", key, err)
	}
	return revision, nil
}

func (k *logKV) Delete(ctx context.Context, key string) error {
	if err := k.client.admit(); err != nil {
		return err
	}
	if err := k.kv.Delete(ctx, key); err != nil {
		return k.kvError("delete", key, err)
	}
	return nil
}

// Watch 的循环读 nats.go KeyWatcher 的通道：nil 条目标记“现有值已交完”，删除 / 清除标记（含 LimitMarkerTTL 下的过期
// 标记）交成 Deleted。watcher 用自己的 context，生命期由 LogFollow.Stop 决定。
func (k *logKV) Watch(ctx context.Context, pattern string, onEntry func(fnats.LogKVEntry), onReady func()) (fnats.LogFollow, error) {
	if err := k.client.admit(); err != nil {
		return nil, err
	}
	if ctx == nil || onEntry == nil || onReady == nil {
		return nil, errors.New("nats log: kv watch needs a context and both callbacks")
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	var (
		watcher gojs.KeyWatcher
		err     error
	)
	if pattern == "" || pattern == ">" {
		watcher, err = k.kv.WatchAll(watchCtx)
	} else {
		watcher, err = k.kv.Watch(watchCtx, pattern)
	}
	if err != nil {
		cancel()
		return nil, k.kvError("watch", pattern, err)
	}
	follow := newLogFollow(func() {
		_ = watcher.Stop()
		cancel()
	})
	updates := watcher.Updates()
	go follow.run(func() bool {
		select {
		case <-follow.stopCh:
			return false
		case entry, ok := <-updates:
			if !ok {
				return false
			}
			if entry == nil {
				onReady()
				return true
			}
			deleted := entry.Operation() == gojs.KeyValueDelete || entry.Operation() == gojs.KeyValuePurge
			value := []byte(nil)
			if !deleted {
				value = append(value, entry.Value()...)
			}
			onEntry(fnats.LogKVEntry{Key: entry.Key(), Value: value, Revision: entry.Revision(), Deleted: deleted})
			return true
		}
	})
	return follow, nil
}

func (c *JetStreamLogClient) Objects(ctx context.Context, config fnats.LogObjectConfig) (fnats.LogObjects, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	store, err := c.js.CreateOrUpdateObjectStore(ctx, gojs.ObjectStoreConfig{
		Bucket:   config.Bucket,
		Replicas: config.Replicas,
		MaxBytes: config.MaxBytes,
		Storage:  gojs.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("nats log: object bucket %q: %w", config.Bucket, c.client.wrapError(err))
	}
	return &logObjects{store: store, client: c.client}, nil
}

// logObjects 是 fnats.LogObjects 的实现；已关闭判据在 client 上。
type logObjects struct {
	store  gojs.ObjectStore
	client *Client
}

func (o *logObjects) objectError(op, name string, err error) error {
	if o.client.closed() {
		return fnats.ErrClosed
	}
	if errors.Is(err, gojs.ErrObjectNotFound) {
		return fmt.Errorf("%w: object %s %q", fnats.ErrLogNotFound, op, name)
	}
	return fmt.Errorf("nats log: object %s %q: %w", op, name, o.client.wrapError(err))
}

func (o *logObjects) Put(ctx context.Context, name string, data []byte) (fnats.LogObjectInfo, error) {
	if err := o.client.admit(); err != nil {
		return fnats.LogObjectInfo{}, err
	}
	info, err := o.store.PutBytes(ctx, name, data)
	if err != nil {
		return fnats.LogObjectInfo{}, o.objectError("put", name, err)
	}
	return fnats.LogObjectInfo{Name: info.Name, Size: info.Size, Digest: info.Digest}, nil
}

func (o *logObjects) Get(ctx context.Context, name string) ([]byte, error) {
	if err := o.client.admit(); err != nil {
		return nil, err
	}
	data, err := o.store.GetBytes(ctx, name)
	if err != nil {
		return nil, o.objectError("get", name, err)
	}
	return data, nil
}

func (o *logObjects) Delete(ctx context.Context, name string) error {
	if err := o.client.admit(); err != nil {
		return err
	}
	if err := o.store.Delete(ctx, name); err != nil {
		return o.objectError("delete", name, err)
	}
	return nil
}

// RequestMany 用一个专用收件箱订阅收集多条回复：先订阅再发请求（同一连接上按序），回复逐条交给 more。
func (c *JetStreamLogClient) RequestMany(ctx context.Context, subject string, data []byte, more func([]byte) bool) error {
	if err := c.admit(); err != nil {
		return err
	}
	if ctx == nil || more == nil || !validSubject(subject, false) {
		return fmt.Errorf("nats log: request many: invalid subject %q or missing context / callback", subject)
	}
	inbox := c.client.conn.NewInbox()
	sub, err := c.client.conn.SubscribeSync(inbox)
	if err != nil {
		return fmt.Errorf("nats log: request many %s: %w", subject, c.client.wrapError(err))
	}
	defer func() { _ = sub.Unsubscribe() }()
	request := gonats.NewMsg(subject)
	request.Reply = inbox
	request.Data = data
	if err := c.client.conn.PublishMsg(request); err != nil {
		return fmt.Errorf("nats log: request many %s: %w", subject, c.client.wrapError(err))
	}
	received := 0
	for {
		reply, err := sub.NextMsgWithContext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("nats log: request many %s: %d replies before the context ended: %w", subject, received, ctx.Err())
			}
			return fmt.Errorf("nats log: request many %s: %w", subject, c.client.wrapError(err))
		}
		// 没有订阅者时服务端回一条只有 503 状态头的空消息。
		if len(reply.Data) == 0 && reply.Header.Get("Status") == "503" {
			if received == 0 {
				return fmt.Errorf("nats log: request many %s: %w", subject, fnats.ErrNoResponders)
			}
			continue
		}
		received++
		if !more(append([]byte(nil), reply.Data...)) {
			return nil
		}
	}
}

func (c *JetStreamLogClient) ServerVersion() string {
	if c == nil || c.client == nil || c.client.closed() {
		return ""
	}
	return c.client.conn.ConnectedServerVersion()
}

// serverVersionAtLeast 比较 "主.次.修订"（可带 "v" 前缀与 "-xxx" 后缀）；解析不了按不满足处理。
func serverVersionAtLeast(version, minimum string) bool {
	have, ok := parseServerVersion(version)
	want, wantOK := parseServerVersion(minimum)
	if !ok || !wantOK {
		return false
	}
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

func parseServerVersion(version string) ([3]int, bool) {
	var out [3]int
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if i := strings.IndexAny(version, "-+"); i >= 0 {
		version = version[:i]
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

var (
	_ fnats.JetStreamLog = (*JetStreamLogClient)(nil)
	_ fnats.LogKV        = (*logKV)(nil)
	_ fnats.LogObjects   = (*logObjects)(nil)
	_ fnats.LogFollow    = (*logFollow)(nil)
)
