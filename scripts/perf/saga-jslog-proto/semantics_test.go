package jslogproto

// JetStream 语义验证（设计稿 §2 的依据）：期望序号三种检查、原子批量、MsgID 去重。每条用例都把服务端的原始错误打到
// 日志里（-v 可见），设计稿引用的就是这些文本。只连私有三节点集群（SAGAJS_URL），流 R3、文件存储。

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func pub(t *testing.T, js jetstream.JetStream, subject string, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return js.Publish(ctx, subject, []byte("x"), opts...)
}

// 单 subject：Nats-Expected-Last-Subject-Sequence 只看这个 subject 自己的最后序号。
func TestExpectedLastSubjectSequenceSingleSubject(t *testing.T) {
	_, js := connect(t)
	name := uniqueName("SEMA")
	stream := createStream(t, js, jetstream.StreamConfig{Name: name, Subjects: []string{name + ".>"}})
	a, b := name+".p0.ev", name+".p0.res"

	ack, err := pub(t, js, a, jetstream.WithExpectLastSequencePerSubject(0))
	if err != nil {
		t.Fatalf("first write with expected 0: %v", err)
	}
	// 别的 subject 的写入不影响 a 的检查。
	if _, err := pub(t, js, b); err != nil {
		t.Fatal(err)
	}
	if _, err := pub(t, js, a, jetstream.WithExpectLastSequencePerSubject(ack.Sequence)); err != nil {
		t.Fatalf("expected=%d after an unrelated subject write: %v", ack.Sequence, err)
	}
	_, err = pub(t, js, a, jetstream.WithExpectLastSequencePerSubject(ack.Sequence))
	if !isWrongLastSeq(err) {
		t.Fatalf("stale expected seq: err=%v code=%d, want wrong last sequence", err, apiErrorCode(err))
	}
	t.Logf("single subject stale: code=%d err=%q", apiErrorCode(err), err)
	if got := lastSeqOf(t, stream, a); got != 3 {
		t.Fatalf("last seq of %s = %d, want 3 (rejected write not stored)", a, got)
	}
}

// 通配：Nats-Expected-Last-Subject-Sequence-Subject 给出过滤（可以带通配），序号按“匹配过滤的最后一条”比较。
// 这是分区单写者的形状：saga.<k>.ev.<目标> 有多个目标 subject，fence 检查整个 saga.<k>.ev.>。
func TestExpectedLastSubjectSequenceWildcard(t *testing.T) {
	_, js := connect(t)
	name := uniqueName("SEMW")
	stream := createStream(t, js, jetstream.StreamConfig{Name: name, Subjects: []string{name + ".>"}})
	filter := name + ".p0.ev.>"

	ack1, err := pub(t, js, name+".p0.ev.acct", jetstream.WithExpectLastSequenceForSubject(0, filter))
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	// 结果与别的分区的写入不在过滤里：不改变 p0.ev.> 的最后序号。
	for _, s := range []string{name + ".p0.res.x", name + ".p1.ev.acct"} {
		if _, err := pub(t, js, s); err != nil {
			t.Fatal(err)
		}
	}
	ack2, err := pub(t, js, name+".p0.ev.bank", jetstream.WithExpectLastSequenceForSubject(ack1.Sequence, filter))
	if err != nil {
		t.Fatalf("second write to another target under the filter with expected=%d: %v", ack1.Sequence, err)
	}
	// 用旧的序号写过滤里的任一 subject 都被拒。
	_, err = pub(t, js, name+".p0.ev.acct", jetstream.WithExpectLastSequenceForSubject(ack1.Sequence, filter))
	if !isWrongLastSeq(err) {
		t.Fatalf("stale wildcard expected: err=%v code=%d", err, apiErrorCode(err))
	}
	t.Logf("wildcard stale: code=%d err=%q", apiErrorCode(err), err)
	// 只带 -Subject 不带序号头：服务端怎么处理（记录行为，不断言）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := nats.NewMsg(name + ".p0.ev.acct")
	m.Header.Set(jetstream.ExpectedLastSubjSeqSubjHeader, filter)
	_, err = js.PublishMsg(ctx, m)
	t.Logf("-Subject header alone: err=%v", err)
	// 过滤与发布 subject 无交集也照样检查（序号按过滤算）。
	_, err = pub(t, js, name+".p0.res.y", jetstream.WithExpectLastSequenceForSubject(ack2.Sequence, filter))
	t.Logf("publish outside the filter with the filter's current seq: err=%v", err)
	_, err = pub(t, js, name+".p0.res.y", jetstream.WithExpectLastSequenceForSubject(ack1.Sequence, filter))
	t.Logf("publish outside the filter with a stale seq: code=%d err=%v", apiErrorCode(err), err)
	_ = stream
}

// 流级：Nats-Expected-Last-Sequence 看整条流的最后序号；每分区一条流时就是分区单写者。
func TestExpectedLastSequenceStream(t *testing.T) {
	_, js := connect(t)
	name := uniqueName("SEMS")
	createStream(t, js, jetstream.StreamConfig{Name: name, Subjects: []string{name + ".>"}})
	ack, err := pub(t, js, name+".ev", jetstream.WithExpectLastSequence(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub(t, js, name+".res"); err != nil {
		t.Fatal(err)
	}
	_, err = pub(t, js, name+".ev", jetstream.WithExpectLastSequence(ack.Sequence))
	if !isWrongLastSeq(err) {
		t.Fatalf("stream-level stale: err=%v", err)
	}
	t.Logf("stream-level stale (a result landed in between): code=%d err=%q", apiErrorCode(err), err)
}

// 多写者并发：同一个期望序号，每轮恰好一个成功；失败的一律是 wrong last sequence。
func TestExpectedSequenceConcurrentWritersOnlyOneWins(t *testing.T) {
	_, js := connect(t)
	name := uniqueName("SEMC")
	createStream(t, js, jetstream.StreamConfig{Name: name, Subjects: []string{name + ".>"}})
	filter := name + ".p0.ev.>"
	var last uint64
	const writers, rounds = 16, 40
	codes := map[jetstream.ErrorCode]int{}
	for round := 0; round < rounds; round++ {
		var wins atomic.Int32
		var mu sync.Mutex
		var wg sync.WaitGroup
		var winSeq uint64
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				ack, err := pub(t, js, fmt.Sprintf("%s.p0.ev.t%d", name, w), jetstream.WithExpectLastSequenceForSubject(last, filter))
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					wins.Add(1)
					winSeq = ack.Sequence
					return
				}
				if codes[apiErrorCode(err)] == 0 {
					t.Logf("first loser with code %d: %q", apiErrorCode(err), err)
				}
				codes[apiErrorCode(err)]++
				if !isWrongLastSeq(err) {
					t.Errorf("round %d writer %d: unexpected error %v", round, w, err)
				}
			}(w)
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("round %d: %d writers won with the same expected seq, want exactly 1", round, wins.Load())
		}
		last = winSeq
	}
	t.Logf("%d rounds × %d writers: exactly one winner each round; loser codes=%v", rounds, writers, codes)
}

// --- 原子批量 ---

type batchAck struct {
	Error *struct {
		Code        int    `json:"code"`
		ErrCode     int    `json:"err_code"`
		Description string `json:"description"`
	} `json:"error"`
	Stream    string `json:"stream"`
	Seq       uint64 `json:"seq"`
	Batch     string `json:"batch"`
	Count     int    `json:"count"`
	Duplicate bool   `json:"duplicate"`
}

// publishAtomic 按 ADR-50 手写头发原子批量：Nats-Batch-Id / Nats-Batch-Sequence（从 1 起）/ 最后一条 Nats-Batch-Commit: 1。
// nats.go 1.53.1 没有批量发布的高层 API。firstAsRequest 时第一条用请求发（服务端对非提交消息的回复原样记录）。
func publishAtomic(t *testing.T, nc *nats.Conn, id string, msgs []*nats.Msg, firstAsRequest bool) (batchAck, string, error) {
	t.Helper()
	for i, m := range msgs {
		if m.Header == nil {
			m.Header = nats.Header{}
		}
		m.Header.Set("Nats-Batch-Id", id)
		m.Header.Set("Nats-Batch-Sequence", fmt.Sprint(i+1))
		if i == len(msgs)-1 {
			m.Header.Set("Nats-Batch-Commit", "1")
			resp, err := nc.RequestMsg(m, 5*time.Second)
			if err != nil {
				return batchAck{}, "", err
			}
			var ack batchAck
			if err := json.Unmarshal(resp.Data, &ack); err != nil {
				return batchAck{}, string(resp.Data), err
			}
			return ack, string(resp.Data), nil
		}
		if i == 0 && firstAsRequest {
			resp, err := nc.RequestMsg(m, 5*time.Second)
			if err != nil {
				t.Logf("first batch message as request: err=%v", err)
			} else {
				t.Logf("first batch message as request: reply=%q", resp.Data)
			}
			continue
		}
		if err := nc.PublishMsg(m); err != nil {
			return batchAck{}, "", err
		}
	}
	return batchAck{}, "", fmt.Errorf("empty batch")
}

func msg(subject string, kv ...string) *nats.Msg {
	m := nats.NewMsg(subject)
	m.Data = []byte(subject)
	for i := 0; i+1 < len(kv); i += 2 {
		m.Header.Set(kv[i], kv[i+1])
	}
	return m
}

func streamMsgs(t *testing.T, stream jetstream.Stream) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

func TestAtomicBatchPublish(t *testing.T) {
	nc, js := connect(t)
	name := uniqueName("SEMB")
	stream := createStream(t, js, jetstream.StreamConfig{Name: name, Subjects: []string{name + ".>"}, AllowAtomicPublish: true})
	filter := name + ".p0.ev.>"

	t.Run("commit_writes_all", func(t *testing.T) {
		before := streamMsgs(t, stream)
		ack, raw, err := publishAtomic(t, nc, uniqueName("b"), []*nats.Msg{
			msg(name+".p0.ev.acct", jetstream.ExpectedLastSubjSeqSubjHeader, filter, jetstream.ExpectedLastSubjSeqHeader, "0"),
			msg(name + ".p0.ev.bank"),
			msg(name + ".p0.ev.mail"),
		}, false)
		t.Logf("commit reply=%s err=%v", raw, err)
		if err != nil || ack.Error != nil || ack.Count != 3 {
			t.Fatalf("batch commit: ack=%+v err=%v", ack, err)
		}
		if got := streamMsgs(t, stream) - before; got != 3 {
			t.Fatalf("stored %d messages, want 3", got)
		}
	})
	t.Run("stale_expected_on_first_rejects_whole_batch", func(t *testing.T) {
		before := streamMsgs(t, stream)
		ack, raw, err := publishAtomic(t, nc, uniqueName("b"), []*nats.Msg{
			msg(name+".p0.ev.acct", jetstream.ExpectedLastSubjSeqSubjHeader, filter, jetstream.ExpectedLastSubjSeqHeader, "0"),
			msg(name + ".p0.ev.bank"),
		}, false)
		t.Logf("stale expected on the first message: reply=%s err=%v", raw, err)
		if err == nil && ack.Error == nil {
			t.Fatalf("stale batch was accepted: %+v", ack)
		}
		if got := streamMsgs(t, stream) - before; got != 0 {
			t.Fatalf("a rejected batch stored %d messages", got)
		}
	})
	t.Run("expected_on_later_message", func(t *testing.T) {
		last := lastSeqOf(t, stream, filter)
		before := streamMsgs(t, stream)
		// 第二条带“过滤 p0.ev.>”的期望序号 = 批前的最后序号：批内第一条已经写进同一过滤，看服务端按批前还是批内状态比较。
		_, raw, err := publishAtomic(t, nc, uniqueName("b"), []*nats.Msg{
			msg(name + ".p0.ev.acct"),
			msg(name+".p0.ev.bank", jetstream.ExpectedLastSubjSeqSubjHeader, filter, jetstream.ExpectedLastSubjSeqHeader, fmt.Sprint(last)),
		}, false)
		t.Logf("expected header on message 2 (= pre-batch last %d): reply=%s err=%v stored=%d", last, raw, err, streamMsgs(t, stream)-before)
	})
	t.Run("stream_level_expected_on_later_message", func(t *testing.T) {
		before := streamMsgs(t, stream)
		_, raw, err := publishAtomic(t, nc, uniqueName("b"), []*nats.Msg{
			msg(name + ".p0.ev.acct"),
			msg(name+".p0.ev.bank", jetstream.ExpectedLastSeqHeader, "1"),
		}, false)
		t.Logf("Nats-Expected-Last-Sequence on message 2: reply=%s err=%v stored=%d", raw, err, streamMsgs(t, stream)-before)
	})
	t.Run("msg_id_in_batch", func(t *testing.T) {
		before := streamMsgs(t, stream)
		id := uniqueName("mid")
		ack, raw, err := publishAtomic(t, nc, uniqueName("b"), []*nats.Msg{
			msg(name+".p0.ev.acct", jetstream.MsgIDHeader, id),
			msg(name + ".p0.ev.bank"),
		}, false)
		t.Logf("Nats-Msg-Id inside a batch: reply=%s err=%v stored=%d", raw, err, streamMsgs(t, stream)-before)
		if err == nil && ack.Error == nil {
			before = streamMsgs(t, stream)
			_, raw, err = publishAtomic(t, nc, uniqueName("b"), []*nats.Msg{
				msg(name+".p0.ev.acct", jetstream.MsgIDHeader, id),
				msg(name + ".p0.ev.bank"),
			}, false)
			t.Logf("same batch again with the same Nats-Msg-Id: reply=%s err=%v stored=%d", raw, err, streamMsgs(t, stream)-before)
		}
	})
	t.Run("first_as_request", func(t *testing.T) {
		before := streamMsgs(t, stream)
		_, raw, err := publishAtomic(t, nc, uniqueName("b"), []*nats.Msg{msg(name + ".p0.ev.acct"), msg(name + ".p0.ev.bank")}, true)
		t.Logf("first message as request: commit reply=%s err=%v stored=%d", raw, err, streamMsgs(t, stream)-before)
	})
	t.Run("uncommitted_batch_is_discarded", func(t *testing.T) {
		before := streamMsgs(t, stream)
		id := uniqueName("b")
		for i, s := range []string{".p0.ev.acct", ".p0.ev.bank"} {
			m := msg(name + s)
			m.Header.Set("Nats-Batch-Id", id)
			m.Header.Set("Nats-Batch-Sequence", fmt.Sprint(i+1))
			if err := nc.PublishMsg(m); err != nil {
				t.Fatal(err)
			}
		}
		_ = nc.Flush()
		time.Sleep(500 * time.Millisecond)
		if got := streamMsgs(t, stream) - before; got != 0 {
			t.Fatalf("an uncommitted batch stored %d messages", got)
		}
	})
	t.Run("gap_in_sequence_rejected", func(t *testing.T) {
		before := streamMsgs(t, stream)
		id := uniqueName("b")
		m1 := msg(name+".p0.ev.acct", "Nats-Batch-Id", id, "Nats-Batch-Sequence", "1")
		m3 := msg(name+".p0.ev.bank", "Nats-Batch-Id", id, "Nats-Batch-Sequence", "3", "Nats-Batch-Commit", "1")
		_ = nc.PublishMsg(m1)
		resp, err := nc.RequestMsg(m3, 5*time.Second)
		raw := ""
		if resp != nil {
			raw = string(resp.Data)
		}
		t.Logf("sequence gap (1, 3+commit): reply=%s err=%v stored=%d", raw, err, streamMsgs(t, stream)-before)
		if got := streamMsgs(t, stream) - before; got != 0 {
			t.Fatalf("a batch with a gap stored %d messages", got)
		}
	})
}

// MsgID 去重：同一 Nats-Msg-Id 在窗口内重发，返回原序号且 Duplicate=true；与期望序号组合时看哪一条检查先生效。
func TestMsgIDDeduplication(t *testing.T) {
	nc, js := connect(t)
	name := uniqueName("SEMD")
	stream := createStream(t, js, jetstream.StreamConfig{Name: name, Subjects: []string{name + ".>"}, Duplicates: 2 * time.Minute})
	filter := name + ".p0.ev.>"
	first, err := pub(t, js, name+".p0.ev.a", jetstream.WithMsgID("ev-1"), jetstream.WithExpectLastSequenceForSubject(0, filter))
	if err != nil {
		t.Fatal(err)
	}
	again, err := pub(t, js, name+".p0.ev.a", jetstream.WithMsgID("ev-1"), jetstream.WithExpectLastSequenceForSubject(0, filter))
	t.Logf("retry with the same MsgID and the same (now stale) expected seq: ack=%+v err=%v", again, err)
	if err != nil || !again.Duplicate || again.Sequence != first.Sequence {
		t.Fatalf("retry of a landed CAS write: ack=%+v err=%v; want Duplicate with seq %d", again, err, first.Sequence)
	}
	// 另一个写者先写了一条，再用同一 MsgID 重发旧写：仍判重复（去重在期望序号检查之前）。
	if _, err := pub(t, js, name+".p0.ev.b", jetstream.WithExpectLastSequenceForSubject(first.Sequence, filter)); err != nil {
		t.Fatal(err)
	}
	late, err := pub(t, js, name+".p0.ev.a", jetstream.WithMsgID("ev-1"), jetstream.WithExpectLastSequenceForSubject(0, filter))
	t.Logf("retry after another writer moved the filter: ack=%+v err=%v", late, err)
	// 新 MsgID + 旧期望序号：wrong last sequence。
	_, err = pub(t, js, name+".p0.ev.a", jetstream.WithMsgID("ev-2"), jetstream.WithExpectLastSequenceForSubject(first.Sequence, filter))
	t.Logf("new MsgID with a stale expected seq: code=%d err=%v", apiErrorCode(err), err)
	if !isWrongLastSeq(err) {
		t.Fatalf("want wrong last sequence, got %v", err)
	}

	// PubAck 丢失（结果未知但已落库）的确定性模拟：不带 reply 发出 CAS 写，再用同一 MsgID、同一期望序号按正常发布重试。
	cur := lastSeqOf(t, stream, filter)
	lost := nats.NewMsg(name + ".p0.ev.c")
	lost.Data = []byte("x")
	lost.Header.Set(jetstream.MsgIDHeader, "ev-3")
	lost.Header.Set(jetstream.ExpectedLastSubjSeqSubjHeader, filter)
	lost.Header.Set(jetstream.ExpectedLastSubjSeqHeader, fmt.Sprint(cur))
	if err := nc.PublishMsg(lost); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	deadline := time.Now().Add(5 * time.Second)
	for lastSeqOf(t, stream, filter) == cur && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	landed := lastSeqOf(t, stream, filter)
	retry, err := pub(t, js, name+".p0.ev.c", jetstream.WithMsgID("ev-3"), jetstream.WithExpectLastSequenceForSubject(cur, filter))
	t.Logf("ack-lost write (landed at %d) retried with the same MsgID+expected: ack=%+v err=%v", landed, retry, err)
	if err != nil || !retry.Duplicate || retry.Sequence != landed {
		t.Fatalf("want Duplicate at %d, got ack=%+v err=%v", landed, retry, err)
	}
}
