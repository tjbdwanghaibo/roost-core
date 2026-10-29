package driver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

// 日志原语不需要服务端的部分：消息组装与校验、服务端错误码的归类、批确认的解析、版本比较、读取循环的停机契约。
// 真实三节点上的语义在 jetstream_log_real_integration_test.go。

func TestBuildLogMessagesRejectsBeforeSending(t *testing.T) {
	ok := fnats.LogMsg{Subject: "roost.log.0.ev.x", MsgID: "m"}
	tooMany := make([]fnats.LogMsg, fnats.LogBatchMaxMessages+1)
	for i := range tooMany {
		tooMany[i] = fnats.LogMsg{Subject: "roost.log.0.ev.x", MsgID: "m" + strconv.Itoa(i)}
	}
	cases := []struct {
		name    string
		msgs    []fnats.LogMsg
		fence   fnats.LogFence
		payload int64
		want    error  // 非 nil 时要求 errors.Is
		text    string // want 为 nil 时要求错误文本包含它
	}{
		{name: "empty", text: "nothing to publish"},
		{name: "too many", msgs: tooMany, want: fnats.ErrLogBatchTooLarge},
		{name: "missing msg id", msgs: []fnats.LogMsg{{Subject: "roost.log.0.ev.x"}}, text: "MsgID"},
		{name: "msg id with space", msgs: []fnats.LogMsg{{Subject: "roost.log.0.ev.x", MsgID: "a b"}}, text: "MsgID"},
		{name: "wildcard subject", msgs: []fnats.LogMsg{{Subject: "roost.log.*.ev", MsgID: "m"}}, text: "invalid subject"},
		{name: "empty token", msgs: []fnats.LogMsg{{Subject: "roost..ev", MsgID: "m"}}, text: "invalid subject"},
		{name: "reserved header", msgs: []fnats.LogMsg{{Subject: "roost.log.0.ev.x", MsgID: "m", Headers: map[string]string{"nats-expected-stream": "X"}}}, text: "reserved"},
		{name: "header key with colon", msgs: []fnats.LogMsg{{Subject: "roost.log.0.ev.x", MsgID: "m", Headers: map[string]string{"a:b": "v"}}}, text: "invalid header key"},
		{name: "header value with newline", msgs: []fnats.LogMsg{{Subject: "roost.log.0.ev.x", MsgID: "m", Headers: map[string]string{"Roost-Reply": "a\r\nb"}}}, text: "line break"},
		{name: "bad fence filter", msgs: []fnats.LogMsg{ok}, fence: fnats.LogFence{Filter: "roost.>.ev"}, text: "fence filter"},
		{name: "over max payload", msgs: []fnats.LogMsg{{Subject: "roost.log.0.ev.x", MsgID: "m", Data: make([]byte, 1000)}}, payload: 1000, want: fnats.ErrLogRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildLogMessages(tc.msgs, tc.fence, "", tc.payload)
			switch {
			case err == nil:
				t.Fatal("accepted")
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("err = %v, want %v", err, tc.want)
			case tc.want == nil && !strings.Contains(err.Error(), tc.text):
				t.Fatalf("err = %v, want it to mention %q", err, tc.text)
			case errors.Is(err, fnats.ErrLogOutcomeUnknown):
				t.Fatalf("a refusal before sending is reported as an unknown outcome: %v", err)
			}
		})
	}
}

// 期望序号只在首条（服务端按批前状态比较，后续条带上也不会更严）；批头的序号从 1 连续、只有末条提交；业务头原样带上。
func TestBuildLogMessagesPutsTheFenceOnTheFirstAndCommitOnTheLast(t *testing.T) {
	msgs := []fnats.LogMsg{
		{Subject: "roost.log.3.ev.cmd.acct", MsgID: "w.7.1.0", Data: []byte("a"), Headers: map[string]string{"Roost-Saga-Reply": "_INBOX.x"}},
		{Subject: "roost.log.3.ev.none", MsgID: "w.7.1.1"},
		{Subject: "roost.log.3.ev.done", MsgID: "w.7.1.2"},
	}
	built, err := buildLogMessages(msgs, fnats.LogFence{Filter: "roost.log.3.ev.>", LastSeq: 7}, "B1", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range built {
		if got := m.Header.Get(headerMsgID); got != msgs[i].MsgID {
			t.Fatalf("message %d MsgID = %q", i, got)
		}
		if got := m.Header.Get(headerBatchID); got != "B1" {
			t.Fatalf("message %d batch id = %q", i, got)
		}
		if got := m.Header.Get(headerBatchSequence); got != strconv.Itoa(i+1) {
			t.Fatalf("message %d batch sequence = %q", i, got)
		}
		if commit := m.Header.Get(headerBatchCommit); (commit == "1") != (i == len(built)-1) {
			t.Fatalf("message %d commit header = %q", i, commit)
		}
		if fenced := m.Header.Get(headerExpectedSubject) != ""; fenced != (i == 0) {
			t.Fatalf("message %d carries the fence = %v, want only the first", i, fenced)
		}
	}
	if built[0].Header.Get(headerExpectedSequence) != "7" || built[0].Header.Get(headerExpectedSubject) != "roost.log.3.ev.>" {
		t.Fatalf("fence headers = %v", built[0].Header)
	}
	if built[0].Header.Get("Roost-Saga-Reply") != "_INBOX.x" {
		t.Fatalf("business header lost: %v", built[0].Header)
	}
	// 单条：没有批头；不带 fence 时也没有期望序号头（只给测试变异用）。
	single, err := buildLogMessages(msgs[:1], fnats.LogFence{}, "", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{headerBatchID, headerBatchSequence, headerBatchCommit, headerExpectedSubject, headerExpectedSequence} {
		if single[0].Header.Get(key) != "" {
			t.Fatalf("single unfenced message carries %s", key)
		}
	}
	// fence.LastSeq = 0 是“过滤上还没有消息”，照样写进头里。
	first, err := buildLogMessages(msgs[:1], fnats.LogFence{Filter: "roost.log.3.ev.>"}, "", 1<<20)
	if err != nil || first[0].Header.Get(headerExpectedSequence) != "0" {
		t.Fatalf("zero fence: headers=%v err=%v", first, err)
	}
}

// 头块大小与 nats.go 编码一致：max_payload 判据（数据 + 头）和 nats.go 发送时的检查同一口径。
func TestEncodedHeaderSizeMatchesTheWireFormat(t *testing.T) {
	h := gonats.Header{}
	h.Set("A", "1")
	h.Set("Nats-Msg-Id", "xyz")
	// "NATS/1.0\r\n" + "A: 1\r\n" + "Nats-Msg-Id: xyz\r\n" + "\r\n"
	if got, want := encodedHeaderSize(h), 10+6+18+2; got != want {
		t.Fatalf("encodedHeaderSize = %d, want %d", got, want)
	}
}

// 服务端的明确拒绝都归到“这次没写入”的哨兵，没有一个被当成结果未知。
func TestLogAPIErrorClassification(t *testing.T) {
	cases := map[int]error{
		10071: fnats.ErrLogWrongLastSequence,
		10164: fnats.ErrLogWrongLastSequence,
		10201: fnats.ErrLogDuplicateBatch,
		10176: fnats.ErrLogBatchIncomplete,
		10199: fnats.ErrLogBatchTooLarge,
		10054: fnats.ErrLogRejected, // 消息超过流的 max_msg_size
		10077: fnats.ErrLogRejected, // 流满（Discard=New）
	}
	// 10158：同一 MsgID 的前一次提交还在服务端处理——前一次可能提交，归为结果未知。第 2 批在真实三节点上实际撞到的是 saga 自己的
	// MsgID 重复（两个分区共用流、首轮 MsgID 相同，已修）；“结果未知后的重发撞上还没落定的原发送”按语义推断，未单独复现。
	if err := logAPIError(10158, "duplicate message id is in process"); !errors.Is(err, fnats.ErrLogOutcomeUnknown) || errors.Is(err, fnats.ErrLogRejected) {
		t.Fatalf("10158: %v", err)
	}
	for code, want := range cases {
		err := logAPIError(code, "description")
		if !errors.Is(err, want) || errors.Is(err, fnats.ErrLogOutcomeUnknown) {
			t.Fatalf("code %d: %v, want %v", code, err, want)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(code)) {
			t.Fatalf("code %d missing from %q", code, err)
		}
	}
}

func TestParseBatchAck(t *testing.T) {
	ack, err := parseBatchAck([]byte(`{"stream":"S","seq":9,"batch":"b","count":3}`), 3)
	if err != nil || ack.Seq != 9 || ack.Count != 3 || ack.Stream != "S" {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	if _, err := parseBatchAck([]byte(`{"error":{"code":400,"err_code":10201,"description":"atomic publish batch contains duplicate message id"},"stream":"S","seq":0}`), 3); !errors.Is(err, fnats.ErrLogDuplicateBatch) {
		t.Fatalf("10201 reply: %v", err)
	}
	if _, err := parseBatchAck([]byte(`{"error":{"code":400,"err_code":10071,"description":"wrong last sequence: 3"},"stream":"S","seq":0}`), 2); !errors.Is(err, fnats.ErrLogWrongLastSequence) {
		t.Fatalf("10071 reply: %v", err)
	}
	// 说成功却对不上条数、或回复解不开：不能确定写了什么，交给调用方按结果未知重发判定。
	for _, reply := range []string{`{"stream":"S","seq":9,"count":2}`, `{"stream":"S","seq":0,"count":3}`, `not json`} {
		if _, err := parseBatchAck([]byte(reply), 3); !errors.Is(err, fnats.ErrLogOutcomeUnknown) {
			t.Fatalf("reply %s: %v, want ErrLogOutcomeUnknown", reply, err)
		}
	}
}

func TestServerVersionAtLeast(t *testing.T) {
	cases := []struct {
		version string
		ok      bool
	}{
		{"2.12.1", true}, {"2.14.5", true}, {"v2.12.1", true}, {"2.13.0-beta.1", true}, {"3.0.0", true},
		{"2.12.0", false}, {"2.11.9", false}, {"1.99.99", false}, {"", false}, {"2.12", false}, {"x.y.z", false},
	}
	for _, tc := range cases {
		if got := serverVersionAtLeast(tc.version, fnats.MinLogServerVersion); got != tc.ok {
			t.Fatalf("serverVersionAtLeast(%q) = %v, want %v", tc.version, got, tc.ok)
		}
	}
}

// 业务头往返：保留头（Nats- 前缀，大小写不敏感）读回时略过，发布时拒绝。
func TestBusinessHeadersRoundTripWithoutReservedKeys(t *testing.T) {
	h := gonats.Header{}
	if err := setBusinessHeaders(h, map[string]string{"Roost-Saga-Reply": "_INBOX.a"}); err != nil {
		t.Fatal(err)
	}
	h.Set(headerMsgID, "m")
	h.Set("NATS-Expected-Stream", "X")
	got := businessHeaders(h)
	if len(got) != 1 || got["Roost-Saga-Reply"] != "_INBOX.a" {
		t.Fatalf("business headers = %v", got)
	}
	if businessHeaders(gonats.Header{headerMsgID: {"m"}}) != nil {
		t.Fatal("only reserved headers should read back as nil")
	}
	if err := setBusinessHeaders(gonats.Header{}, map[string]string{"Nats-Msg-Id": "x"}); err == nil {
		t.Fatal("a reserved header was accepted")
	}
}

// JetStreamPublishOptions.Headers 走同一条校验：保留键在发送前就被拒绝。
func TestJetStreamPublishRejectsReservedHeadersBeforeSending(t *testing.T) {
	a := closeContractAssembly(t)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	js, err := NewJetStreamClient(a.Client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = js.Publish(ctx, "roost.headers", nil, fnats.JetStreamPublishOptions{Headers: map[string]string{"Nats-Msg-Id": "x"}})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Publish with a reserved header = %v", err)
	}
}

// 读取循环的停机三步：Stop 先停底层读取，再在 ctx 内等正在执行的回调；回调卡住时超时可重试，放行后返回 nil。
func TestLogFollowStopWaitsForTheCallbackWithinItsContext(t *testing.T) {
	var (
		follow           *logFollow
		entered          = make(chan struct{})
		release          = make(chan struct{})
		readerStopped    atomic.Bool
		callbackReturned atomic.Bool
	)
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(t testing.TB) {
			follow = newLogFollow(func() { readerStopped.Store(true) })
			first := true
			go follow.run(func() bool {
				if !first {
					return false
				}
				first = false
				close(entered)
				<-release // 卡住的回调
				callbackReturned.Store(true)
				return true
			})
		},
		Block: func(t testing.TB) {
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("callback never entered")
			}
		},
		Stop:     func(ctx context.Context) error { return follow.Stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return readerStopped.Load() && callbackReturned.Load() },
	})
	select {
	case <-follow.Done():
	default:
		t.Fatal("Done not closed after a successful Stop")
	}
}

// 单调守卫：回退或重复的序号不交给回调。
func TestLogFollowAdvanceIsMonotonic(t *testing.T) {
	f := newLogFollow(nil)
	for _, step := range []struct {
		seq  uint64
		want bool
	}{{5, true}, {5, false}, {3, false}, {6, true}} {
		if got := f.advance(step.seq); got != step.want {
			t.Fatalf("advance(%d) = %v, want %v", step.seq, got, step.want)
		}
	}
}

// --- 清理守卫的 floor（DurableAckFloors）---

// fakeFloorReader 模拟 durableAckFloors 要的三样服务端事实：流 leader 上的消息（序号 → subject，1～last）、各消费者 leader
// 报的 ConsumerInfo（NumPending / NumAckPending 按它本机落后的副本算，可能对流 leader 已有的消息报 0）、没回答的消费者。
// calls 记调用顺序；afterNext 在每次 nextSeq 回答之后执行（模拟之后才到的新消息）。
type fakeFloorReader struct {
	subjects   map[uint64]string
	last       uint64
	infos      []*gojs.ConsumerInfo
	unanswered map[string]bool
	calls      []string
	afterNext  func(f *fakeFloorReader)
}

func (f *fakeFloorReader) append(subject string) {
	f.last++
	f.subjects[f.last] = subject
}

func (f *fakeFloorReader) streamLastSeq(context.Context, string) (uint64, error) {
	f.calls = append(f.calls, "last")
	return f.last, nil
}

func (f *fakeFloorReader) consumers(context.Context, string) ([]*gojs.ConsumerInfo, map[string]bool, error) {
	f.calls = append(f.calls, "consumers")
	return f.infos, f.unanswered, nil
}

func (f *fakeFloorReader) nextSeq(_ context.Context, _ string, filter string, from uint64) (uint64, bool, error) {
	f.calls = append(f.calls, "next")
	defer func() {
		if f.afterNext != nil {
			f.afterNext(f)
		}
	}()
	for seq := from; seq <= f.last; seq++ {
		if subjectMatches(filter, f.subjects[seq]) {
			return seq, true, nil
		}
	}
	return 0, false, nil
}

// subjectMatches 是 NATS 的 subject 过滤匹配（* 一个 token，> 其余全部）。
func subjectMatches(filter, subject string) bool {
	if subject == "" {
		return false
	}
	ft, st := strings.Split(filter, "."), strings.Split(subject, ".")
	for i, token := range ft {
		if token == ">" {
			return len(st) > i
		}
		if i >= len(st) || token != "*" && token != st[i] {
			return false
		}
	}
	return len(ft) == len(st)
}

func durableInfo(name string, ackFloor uint64, numPending uint64, filters ...string) *gojs.ConsumerInfo {
	info := &gojs.ConsumerInfo{Name: name, Config: gojs.ConsumerConfig{Durable: name}, NumPending: numPending}
	info.AckFloor.Stream = ackFloor
	if len(filters) == 1 {
		info.Config.FilterSubject = filters[0]
	} else {
		info.Config.FilterSubjects = filters
	}
	return info
}

func newFakeFloorReader(subjects ...string) *fakeFloorReader {
	f := &fakeFloorReader{subjects: map[uint64]string{}}
	for _, subject := range subjects {
		f.append(subject)
	}
	return f
}

// 消费者 leader 在落后的流副本上（KNOWN-LIMITS“清理守卫的 floor”，2026-10-29 私有三节点 SIGSTOP 实测复现）：它报
// NumPending = 0、NumAckPending = 0，但流 leader 已有一条匹配它过滤、它还没取的命令 4。floor 不能越过 4，否则按 floor 清理会
// 删掉它。空闲的过滤 durable（第 3 批的修复）、确认到最后一条的 durable 仍到流末。
func TestDurableAckFloorsDoNotTrustTheConsumerReplicaView(t *testing.T) {
	f := newFakeFloorReader(
		"p.0.ev.cmd.step", "p.0.ev.cmd.step", "p.0.ev.none", "p.1.ev.cmd.step", // 4 = S：step 还没取
		"p.0.ev.none", "p.1.ev.none", "p.0.ev.done", "p.1.ev.none",
	)
	ephemeral := durableInfo("ordered", 0, 0, "p.0.>")
	ephemeral.Config.Durable = ""
	f.infos = []*gojs.ConsumerInfo{
		durableInfo("step", 2, 0, "p.*.ev.cmd.step"),                    // 副本只应用到 3：报没有待投递
		durableInfo("idle", 0, 0, "p.*.ev.cmd.quiet"),                   // 过滤上没有消息
		durableInfo("done", 4, 0, "p.*.ev.cmd.step"),                    // 确认到它的最后一条
		durableInfo("busy", 1, 3, "p.0.ev.cmd.step", "p.1.ev.cmd.step"), // 多个过滤，下一条是 2
		durableInfo("whole", 5, 0),                                      // 不过滤：下一条是 6
		ephemeral,
	}
	floors, err := durableAckFloors(context.Background(), f, "S")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{"step": 3, "idle": 8, "done": 8, "busy": 1, "whole": 5}
	if fmt.Sprint(floors) != fmt.Sprint(want) {
		t.Fatalf("floors = %v, want %v", floors, want)
	}
}

// 没回答的消费者（服务端列表的 missing / offline：消费者 leader 4s 内没回答，或承载它的副本全部离线、以零状态出现）状态未知，
// floor 记 0；全部副本离线、配置表明是临时 / 有序消费者的不列。实测：R1 durable 所在节点 SIGSTOP 时 nats.go 的列表里它一次
// 都不出现。
func TestDurableAckFloorsHoldUnansweredConsumersAtZero(t *testing.T) {
	f := newFakeFloorReader("p.0.ev.cmd.step", "p.0.ev.none", "p.0.ev.none")
	offlineOrdered := durableInfo("offline-ordered", 0, 0, "p.0.>")
	offlineOrdered.Config.Durable = ""
	f.infos = []*gojs.ConsumerInfo{
		durableInfo("zz-idle", 0, 0, "p.*.ev.cmd.quiet"),
		durableInfo("offline-durable", 0, 0, "p.*.ev.cmd.other"),
		offlineOrdered,
	}
	f.unanswered = map[string]bool{"step": true, "offline-durable": true, "offline-ordered": true}
	floors, err := durableAckFloors(context.Background(), f, "S")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{"step": 0, "offline-durable": 0, "zz-idle": 3}
	if fmt.Sprint(floors) != fmt.Sprint(want) {
		t.Fatalf("floors = %v, want %v", floors, want)
	}
}

// 读的先后（守卫，修复前后都成立）：流末序号必须在问“下一条”之前读。fake 在每次回答“下一条”之后追加一条匹配的新命令；
// 先问下一条、后读流末的实现会把 floor 抬到这条还没取的新命令上。
func TestDurableAckFloorsReadTheStreamEndFirst(t *testing.T) {
	f := newFakeFloorReader("p.0.ev.cmd.step", "p.0.ev.none")
	f.infos = []*gojs.ConsumerInfo{durableInfo("step", 1, 0, "p.*.ev.cmd.step")}
	f.afterNext = func(f *fakeFloorReader) { f.append("p.0.ev.cmd.step") }
	floors, err := durableAckFloors(context.Background(), f, "S")
	if err != nil {
		t.Fatal(err)
	}
	if floors["step"] != 2 || len(f.calls) == 0 || f.calls[0] != "last" {
		t.Fatalf("floors = %v, calls = %v; want step=2 with the stream end read first", floors, f.calls)
	}
}
