//go:build integration

package driver

// JetStreamLog 在真实私有三节点集群上的语义（R3、文件存储）：通配期望序号、并发写恰好一个成功、原子批量与期望序号 /
// MsgID / 缺号 / 条数上限 / 同时打开的批数上限、去重先于期望序号、ack 丢失后的判定、有序读取、清理、ack floor、
// KV 与对象存储、散播请求，以及 leader kill -9 下“已确认的写不丢、结果未知能分辨”。
//
// 环境：scripts/nats-cluster.sh 起的私有集群。ROOST_NATS_CLUSTER_URL（逗号分隔的三个地址）没设置就跳过；leader kill
// 用例另需 ROOST_NATS_CLUSTER_ROOT（脚本的数据目录，读 pid 文件、调脚本拉起节点）。一键入口：scripts/nats-cluster.sh test。
// 2026-10-28 在 macOS、nats-server v2.14.5、nats.go v1.53.1 上通过。不读共享隔离环境的变量。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

var logTestSeq atomic.Int64

func logUniqueName(prefix string) string {
	return fmt.Sprintf("%s%d%d", prefix, time.Now().UnixNano()%1_000_000_000, logTestSeq.Add(1))
}

// logCluster 连私有集群，返回日志原语（与生产同一装配：Client → JetStreamClient → JetStreamLogClient）。
func logCluster(t *testing.T) *JetStreamLogClient {
	t.Helper()
	url := os.Getenv("ROOST_NATS_CLUSTER_URL")
	if url == "" {
		t.Skip("ROOST_NATS_CLUSTER_URL is not set: start a private three-node cluster with scripts/nats-cluster.sh start and eval \"$(scripts/nats-cluster.sh env)\"")
	}
	cfg := fnats.DefaultConfig(url)
	cfg.ReconnectWait = 100 * time.Millisecond
	client, err := NewClient(cfg, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	js, err := NewJetStreamClient(client)
	if err != nil {
		t.Fatal(err)
	}
	log, err := NewJetStreamLog(js)
	if err != nil {
		t.Fatal(err)
	}
	return log
}

func testCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// logStream 建一条 R3 日志流（subjects = <name>.>），用例结束删掉。
func logStream(t *testing.T, log *JetStreamLogClient) (string, fnats.LogStreamInfo) {
	t.Helper()
	name := logUniqueName("RLOG")
	info, err := log.EnsureLogStream(testCtx(t, 30*time.Second), fnats.LogStreamConfig{
		Name: name, Subjects: []string{name + ".>"}, Replicas: 3, Duplicates: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("ensure log stream: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = log.js.DeleteStream(ctx, name)
	})
	return name, info
}

func streamMsgCount(t *testing.T, log *JetStreamLogClient, stream string) uint64 {
	t.Helper()
	handle, err := log.js.Stream(testCtx(t, 5*time.Second), stream)
	if err != nil {
		t.Fatal(err)
	}
	return handle.CachedInfo().State.Msgs
}

func ev(subject, id string) fnats.LogMsg {
	return fnats.LogMsg{Subject: subject, MsgID: id, Data: []byte(id)}
}

func TestRealLogEnsureStreamFixesTheAuthoritativeSettings(t *testing.T) {
	log := logCluster(t)
	version := log.ServerVersion()
	t.Logf("nats-server %s (minimum %s)", version, fnats.MinLogServerVersion)
	if !serverVersionAtLeast(version, fnats.MinLogServerVersion) {
		t.Fatalf("the private cluster runs %s, older than %s", version, fnats.MinLogServerVersion)
	}
	name, info := logStream(t, log)
	if info.Replicas != 3 || info.Leader == "" || info.Name != name {
		t.Fatalf("info = %+v", info)
	}
	handle, err := log.js.Stream(testCtx(t, 5*time.Second), name)
	if err != nil {
		t.Fatal(err)
	}
	got := handle.CachedInfo().Config
	if !got.AllowAtomicPublish || !got.AllowDirect || got.Discard != gojs.DiscardNew || got.MaxAge != 0 || got.Storage != gojs.FileStorage || got.Duplicates != 2*time.Minute {
		t.Fatalf("effective config = %+v", got)
	}
	// 再 Ensure 一次是幂等的（已有流按同一配置更新）。
	if again, err := log.EnsureLogStream(testCtx(t, 30*time.Second), fnats.LogStreamConfig{Name: name, Subjects: []string{name + ".>"}, Replicas: 3, Duplicates: 2 * time.Minute}); err != nil || again.Name != name {
		t.Fatalf("second ensure: %+v %v", again, err)
	}
}

// 通配期望序号：序号按“匹配过滤的最后一条”比较；过滤外的写入（输入、别的分区）不推进它；旧序号被拒且不写入。
func TestRealLogWildcardFenceSerializesOneWriter(t *testing.T) {
	log := logCluster(t)
	name, _ := logStream(t, log)
	filter := name + ".0.ev.>"
	ctx := testCtx(t, 30*time.Second)

	if _, err := log.LastMsg(ctx, name, filter); !errors.Is(err, fnats.ErrLogNotFound) {
		t.Fatalf("LastMsg on an empty filter = %v, want ErrLogNotFound", err)
	}
	first, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".0.ev.cmd.acct", "w.0.1.0")}, fnats.LogFence{Filter: filter, LastSeq: 0})
	if err != nil || first.Count != 1 || first.Duplicate {
		t.Fatalf("first fenced write: %+v %v", first, err)
	}
	for i, subject := range []string{name + ".0.in.res", name + ".1.ev.none"} {
		if _, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(subject, "other."+strconv.Itoa(i))}, fnats.LogFence{}); err != nil {
			t.Fatal(err)
		}
	}
	reply := fnats.LogMsg{Subject: name + ".0.ev.done", MsgID: "w.1.2.0", Data: []byte("done"), Headers: map[string]string{"Roost-Saga-Reply": "_INBOX.abc"}}
	second, err := log.PublishFenced(ctx, []fnats.LogMsg{reply}, fnats.LogFence{Filter: filter, LastSeq: first.Seq})
	if err != nil {
		t.Fatalf("fenced write after writes outside the filter: %v", err)
	}
	_, err = log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".0.ev.none", "w.1.3.0")}, fnats.LogFence{Filter: filter, LastSeq: first.Seq})
	t.Logf("stale wildcard fence: %v", err)
	if !errors.Is(err, fnats.ErrLogWrongLastSequence) || errors.Is(err, fnats.ErrLogOutcomeUnknown) {
		t.Fatalf("stale fence = %v, want ErrLogWrongLastSequence", err)
	}
	last, err := log.LastMsg(ctx, name, filter)
	if err != nil || last.Seq != second.Seq || last.MsgID != "w.1.2.0" || last.Headers["Roost-Saga-Reply"] != "_INBOX.abc" || string(last.Data) != "done" {
		t.Fatalf("LastMsg = %+v %v; want the second write with its business header", last, err)
	}
	if streamMsgCount(t, log, name) != 4 {
		t.Fatal("the rejected write was stored")
	}
}

// 多写者用同一个期望序号并发写：每轮恰好一个成功，其余都是 ErrLogWrongLastSequence（R3 并发提案时服务端为 10164）。
func TestRealLogConcurrentWritersExactlyOneWins(t *testing.T) {
	log := logCluster(t)
	name, _ := logStream(t, log)
	filter := name + ".0.ev.>"
	var last uint64
	const writers, rounds = 16, 30
	codes := map[string]int{}
	for round := 0; round < rounds; round++ {
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			wins   int
			winSeq uint64
		)
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				ack, err := log.PublishFenced(testCtx(t, 10*time.Second), []fnats.LogMsg{ev(fmt.Sprintf("%s.0.ev.t%d", name, w), fmt.Sprintf("r%d.w%d", round, w))}, fnats.LogFence{Filter: filter, LastSeq: last})
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					wins++
					winSeq = ack.Seq
					return
				}
				if !errors.Is(err, fnats.ErrLogWrongLastSequence) {
					t.Errorf("round %d writer %d: %v", round, w, err)
					return
				}
				code := "10071"
				if strings.Contains(err.Error(), "10164") {
					code = "10164"
				}
				codes[code]++
			}(w)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d writers won with the same expected sequence, want exactly 1", round, wins)
		}
		last = winSeq
	}
	t.Logf("%d rounds x %d writers: exactly one winner per round; loser codes %v", rounds, writers, codes)
}

// 原子批量：提交整批写入；首条期望序号过时则整批拒绝；同 MsgID 的批重发返回 10201、不写入，且过滤上最后一条是
// 自己的（结果未知的判定规则）；单条重发同 MsgID 时去重先于期望序号；缺号整批丢弃；未提交的半批不落库。
func TestRealLogAtomicBatchSemantics(t *testing.T) {
	log := logCluster(t)
	name, _ := logStream(t, log)
	filter := name + ".0.ev.>"
	ctx := testCtx(t, 60*time.Second)

	batch := []fnats.LogMsg{ev(name+".0.ev.cmd.acct", "b1.0"), ev(name+".0.ev.cmd.bank", "b1.1"), ev(name+".0.ev.done", "b1.2")}
	ack, err := log.PublishFenced(ctx, batch, fnats.LogFence{Filter: filter, LastSeq: 0})
	if err != nil || ack.Count != 3 || ack.Seq != 3 {
		t.Fatalf("commit: %+v %v", ack, err)
	}
	var read []string
	if err := log.ReadRange(ctx, name, []string{filter}, 1, ack.Seq, func(m fnats.LogMsg) error {
		read = append(read, fmt.Sprintf("%d:%s", m.Seq, m.MsgID))
		return nil
	}); err != nil || strings.Join(read, ",") != "1:b1.0,2:b1.1,3:b1.2" {
		t.Fatalf("read back %v %v", read, err)
	}

	before := streamMsgCount(t, log, name)
	_, err = log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".0.ev.none", "b2.0"), ev(name+".0.ev.none", "b2.1")}, fnats.LogFence{Filter: filter, LastSeq: 0})
	t.Logf("stale fence on the first message of a batch: %v", err)
	if !errors.Is(err, fnats.ErrLogWrongLastSequence) || streamMsgCount(t, log, name) != before {
		t.Fatalf("stale batch: %v (stored %d)", err, streamMsgCount(t, log, name)-before)
	}

	_, err = log.PublishFenced(ctx, batch, fnats.LogFence{Filter: filter, LastSeq: 0})
	t.Logf("same batch resent with the same MsgIDs: %v", err)
	if !errors.Is(err, fnats.ErrLogDuplicateBatch) || streamMsgCount(t, log, name) != before {
		t.Fatalf("duplicate batch: %v", err)
	}
	if last, err := log.LastMsg(ctx, name, filter); err != nil || last.MsgID != "b1.2" {
		t.Fatalf("after 10201 the filter's last message must be ours: %+v %v", last, err)
	}

	single, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".0.ev.none", "s1")}, fnats.LogFence{Filter: filter, LastSeq: ack.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".1.ev.none", "other")}, fnats.LogFence{}); err != nil {
		t.Fatal(err)
	}
	again, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".0.ev.none", "s1")}, fnats.LogFence{Filter: filter, LastSeq: ack.Seq})
	t.Logf("single resent with the same MsgID and the old fence: %+v %v", again, err)
	if err != nil || !again.Duplicate || again.Seq != single.Seq {
		t.Fatalf("dedup must come before the fence check: %+v %v", again, err)
	}

	// 缺号与未提交：只能用核心连接手写头制造。
	before = streamMsgCount(t, log, name)
	gap, err := buildLogMessages([]fnats.LogMsg{ev(name+".0.ev.none", "g.0"), ev(name+".0.ev.none", "g.1"), ev(name+".0.ev.none", "g.2")}, fnats.LogFence{}, "gap"+logUniqueName(""), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.client.conn.PublishMsg(gap[0]); err != nil {
		t.Fatal(err)
	}
	reply, err := log.client.conn.RequestMsgWithContext(ctx, gap[2])
	if err != nil {
		t.Fatal(err)
	}
	_, err = parseBatchAck(reply.Data, 3)
	t.Logf("batch with a sequence gap (1, 3+commit): reply=%s -> %v", reply.Data, err)
	if !errors.Is(err, fnats.ErrLogBatchIncomplete) {
		t.Fatalf("gap: %v", err)
	}
	half, err := buildLogMessages([]fnats.LogMsg{ev(name+".0.ev.none", "h.0"), ev(name+".0.ev.none", "h.1")}, fnats.LogFence{}, "half"+logUniqueName(""), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.client.conn.PublishMsg(half[0]); err != nil {
		t.Fatal(err)
	}
	_ = log.client.conn.Flush()
	time.Sleep(500 * time.Millisecond)
	if got := streamMsgCount(t, log, name) - before; got != 0 {
		t.Fatalf("a gapped or uncommitted batch stored %d messages", got)
	}
}

// 原子批量的上限（设计稿 §16 要核实的项）：1000 条一批成功；1001 条驱动发送前拒绝，绕过驱动直接发给服务端得到
// 10199 "atomic publish batch is too large: 1000"；没有总字节上限（64 MiB 的批照常提交），单条受 max_payload 约束；
// 一条流同时打开的批数到 50 个时，新批被丢弃（10176），等打开的批超时（10s）清掉后恢复。
func TestRealLogAtomicBatchLimits(t *testing.T) {
	log := logCluster(t)
	name, _ := logStream(t, log)
	ctx := testCtx(t, 3*time.Minute)
	msgs := func(prefix string, n, size int) []fnats.LogMsg {
		out := make([]fnats.LogMsg, n)
		for i := range out {
			out[i] = fnats.LogMsg{Subject: name + ".0.ev.none", MsgID: fmt.Sprintf("%s.%d", prefix, i), Data: make([]byte, size)}
		}
		return out
	}

	ack, err := log.PublishFenced(ctx, msgs("max", fnats.LogBatchMaxMessages, 16), fnats.LogFence{Filter: name + ".0.ev.>"})
	if err != nil || ack.Count != fnats.LogBatchMaxMessages {
		t.Fatalf("a batch of %d: %+v %v", fnats.LogBatchMaxMessages, ack, err)
	}
	if _, err := log.PublishFenced(ctx, msgs("over", fnats.LogBatchMaxMessages+1, 16), fnats.LogFence{}); !errors.Is(err, fnats.ErrLogBatchTooLarge) {
		t.Fatalf("driver precheck: %v", err)
	}
	raw, err := buildLogMessages(msgs("raw", fnats.LogBatchMaxMessages, 16), fnats.LogFence{}, "big"+logUniqueName(""), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	extra := gonats.NewMsg(name + ".0.ev.none")
	extra.Header.Set(headerMsgID, "raw.extra")
	extra.Header.Set(headerBatchID, raw[0].Header.Get(headerBatchID))
	extra.Header.Set(headerBatchSequence, strconv.Itoa(fnats.LogBatchMaxMessages+1))
	extra.Header.Set(headerBatchCommit, "1")
	raw[len(raw)-1].Header.Del(headerBatchCommit)
	for _, m := range raw {
		if err := log.client.conn.PublishMsg(m); err != nil {
			t.Fatal(err)
		}
	}
	reply, err := log.client.conn.RequestMsgWithContext(ctx, extra)
	if err != nil {
		t.Fatal(err)
	}
	_, err = parseBatchAck(reply.Data, fnats.LogBatchMaxMessages+1)
	t.Logf("server reply to message %d of a batch: %s -> %v", fnats.LogBatchMaxMessages+1, reply.Data, err)
	if !errors.Is(err, fnats.ErrLogBatchTooLarge) || !strings.Contains(string(reply.Data), "10199") {
		t.Fatalf("server batch limit: %v", err)
	}

	maxPayload := log.client.conn.MaxPayload()
	began := time.Now()
	big, err := log.PublishFenced(ctx, msgs("bytes", 256, 256<<10), fnats.LogFence{})
	t.Logf("256 x 256 KiB = 64 MiB in one atomic batch (max_payload %d): %+v %v in %v", maxPayload, big, err, time.Since(began))
	if err != nil || big.Count != 256 {
		t.Fatalf("64 MiB batch: %v", err)
	}
	if _, err := log.PublishFenced(ctx, msgs("payload", 1, int(maxPayload)), fnats.LogFence{}); !errors.Is(err, fnats.ErrLogRejected) {
		t.Fatalf("a message over max_payload: %v", err)
	}

	// 同时打开的批：50 个只发了第一条的批占满这条流的名额，第 51 个批被丢弃。
	for i := 0; i < 50; i++ {
		open, err := buildLogMessages(msgs(fmt.Sprintf("open%d", i), 2, 8), fnats.LogFence{}, fmt.Sprintf("open%d%s", i, logUniqueName("")), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.client.conn.PublishMsg(open[0]); err != nil {
			t.Fatal(err)
		}
	}
	_ = log.client.conn.Flush()
	time.Sleep(200 * time.Millisecond)
	_, err = log.PublishFenced(ctx, msgs("blocked", 2, 8), fnats.LogFence{})
	t.Logf("a batch while 50 batches are open on the stream: %v", err)
	if !errors.Is(err, fnats.ErrLogBatchIncomplete) {
		t.Fatalf("51st open batch: %v, want ErrLogBatchIncomplete", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = log.PublishFenced(ctx, msgs("after", 2, 8), fnats.LogFence{})
		if err == nil {
			break
		}
		if !errors.Is(err, fnats.ErrLogBatchIncomplete) || time.Now().After(deadline) {
			t.Fatalf("batches did not recover after the open ones timed out: %v", err)
		}
		time.Sleep(time.Second)
	}
	t.Logf("batches accepted again after the abandoned ones timed out")
}

// ack 丢失（已落库、没收到答复）：用同样的消息与 fence 重发即可判定。单条得到 Duplicate 原序号；批得到 10201，过滤上的
// 最后一条是自己的；别的写者已推进时得到 wrong last sequence，最后一条不是自己的（= 被隔离）。
func TestRealLogLostAckIsResolvedByResending(t *testing.T) {
	log := logCluster(t)
	name, _ := logStream(t, log)
	filter := name + ".0.ev.>"
	ctx := testCtx(t, 60*time.Second)

	sendWithoutReply := func(msgs []fnats.LogMsg, fence fnats.LogFence) {
		t.Helper()
		batchID := ""
		if len(msgs) > 1 {
			batchID = "lost" + logUniqueName("")
		}
		built, err := buildLogMessages(msgs, fence, batchID, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range built {
			if err := log.client.conn.PublishMsg(m); err != nil {
				t.Fatal(err)
			}
		}
		_ = log.client.conn.Flush()
	}
	waitLanded := func(after uint64) uint64 {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			last, err := log.LastMsg(ctx, name, filter)
			if err == nil && last.Seq > after {
				return last.Seq
			}
			if time.Now().After(deadline) {
				t.Fatalf("lost-ack write never landed: %+v %v", last, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	single := []fnats.LogMsg{ev(name+".0.ev.cmd.acct", "w.0.1.0")}
	sendWithoutReply(single, fnats.LogFence{Filter: filter, LastSeq: 0})
	landed := waitLanded(0)
	ack, err := log.PublishFenced(ctx, single, fnats.LogFence{Filter: filter, LastSeq: 0})
	t.Logf("single, ack lost, resent: %+v %v", ack, err)
	if err != nil || !ack.Duplicate || ack.Seq != landed {
		t.Fatalf("want Duplicate at %d", landed)
	}

	batch := []fnats.LogMsg{ev(name+".0.ev.cmd.acct", "w.1.2.0"), ev(name+".0.ev.done", "w.1.2.1")}
	sendWithoutReply(batch, fnats.LogFence{Filter: filter, LastSeq: landed})
	batchLanded := waitLanded(landed)
	_, err = log.PublishFenced(ctx, batch, fnats.LogFence{Filter: filter, LastSeq: landed})
	t.Logf("batch, ack lost, resent: %v", err)
	if !errors.Is(err, fnats.ErrLogDuplicateBatch) {
		t.Fatalf("want ErrLogDuplicateBatch, got %v", err)
	}
	if last, err := log.LastMsg(ctx, name, filter); err != nil || last.MsgID != "w.1.2.1" || last.Seq != batchLanded {
		t.Fatalf("resolution read: %+v %v", last, err)
	}

	// 别的写者推进过滤之后，旧写者的新一轮被拒，而且最后一条不是它的。
	if _, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".0.ev.none", "intruder.1")}, fnats.LogFence{Filter: filter, LastSeq: batchLanded}); err != nil {
		t.Fatal(err)
	}
	stale := []fnats.LogMsg{ev(name+".0.ev.none", "w.3.3.0"), ev(name+".0.ev.none", "w.3.3.1")}
	_, err = log.PublishFenced(ctx, stale, fnats.LogFence{Filter: filter, LastSeq: batchLanded})
	last, lerr := log.LastMsg(ctx, name, filter)
	t.Logf("fenced-out writer: %v; last on the filter %q", err, last.MsgID)
	if !errors.Is(err, fnats.ErrLogWrongLastSequence) || lerr != nil || last.MsgID == "w.3.3.1" {
		t.Fatalf("fenced writer: err=%v last=%+v lerr=%v", err, last, lerr)
	}
}

// 有序读取、清理与 ack floor：ReadRange 只交过滤内、按序号递增；Follow 从指定序号续读并收到之后的新消息，从 0 起只收新的；
// Purge 只删过滤内、低于界限的；DurableAckFloors 只列持久消费者。
func TestRealLogReadFollowPurgeAndAckFloors(t *testing.T) {
	log := logCluster(t)
	name, _ := logStream(t, log)
	ctx := testCtx(t, 60*time.Second)
	p0, p1 := name+".0.>", name+".1.>"
	var p0Seqs []uint64
	for i := 0; i < 20; i++ {
		subject := fmt.Sprintf("%s.%d.ev.none", name, i%2)
		ack, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(subject, "m"+strconv.Itoa(i))}, fnats.LogFence{})
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			p0Seqs = append(p0Seqs, ack.Seq)
		}
	}
	var got []uint64
	collect := func(m fnats.LogMsg) error {
		if !strings.HasPrefix(m.Subject, name+".0.") {
			return fmt.Errorf("message %s outside the filter", m.Subject)
		}
		got = append(got, m.Seq)
		return nil
	}
	if err := log.ReadRange(ctx, name, []string{p0}, 1, p0Seqs[len(p0Seqs)-1], collect); err != nil || fmt.Sprint(got) != fmt.Sprint(p0Seqs) {
		t.Fatalf("ReadRange to the last = %v %v, want %v", got, err, p0Seqs)
	}
	got = nil
	if err := log.ReadRange(ctx, name, []string{p0}, p0Seqs[5], 0, collect); err != nil || fmt.Sprint(got) != fmt.Sprint(p0Seqs[5:]) {
		t.Fatalf("ReadRange to catch-up = %v %v", got, err)
	}
	if err := log.ReadRange(ctx, name, []string{p0}, 1000, 0, collect); err != nil {
		t.Fatalf("ReadRange past the end: %v", err)
	}
	stop := errors.New("stop")
	if err := log.ReadRange(ctx, name, []string{p0}, 1, 0, func(fnats.LogMsg) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error not returned: %v", err)
	}

	// Follow：从第 6 个 p0 序号起，再写 3 条新消息。
	followed := make(chan uint64, 64)
	follow, err := log.Follow(ctx, name, []string{p0}, p0Seqs[5], func(m fnats.LogMsg) { followed <- m.Seq })
	if err != nil {
		t.Fatal(err)
	}
	fresh := make(chan uint64, 64)
	newOnly, err := log.Follow(ctx, name, []string{p0}, 0, func(m fnats.LogMsg) { fresh <- m.Seq })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // DeliverNew 的消费者建好之后再写
	want := append([]uint64(nil), p0Seqs[5:]...)
	var newSeqs []uint64
	for i := 0; i < 3; i++ {
		ack, err := log.PublishFenced(ctx, []fnats.LogMsg{ev(name+".0.in.res", "n"+strconv.Itoa(i))}, fnats.LogFence{})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, ack.Seq)
		newSeqs = append(newSeqs, ack.Seq)
	}
	receive := func(ch chan uint64, n int) []uint64 {
		var out []uint64
		for len(out) < n {
			select {
			case seq := <-ch:
				out = append(out, seq)
			case <-time.After(10 * time.Second):
				t.Fatalf("follow delivered %v, waiting for %d", out, n)
			}
		}
		return out
	}
	if seqs := receive(followed, len(want)); fmt.Sprint(seqs) != fmt.Sprint(want) {
		t.Fatalf("Follow from %d = %v, want %v", p0Seqs[5], seqs, want)
	}
	if seqs := receive(fresh, len(newSeqs)); fmt.Sprint(seqs) != fmt.Sprint(newSeqs) {
		t.Fatalf("Follow from 0 = %v, want only the new %v", seqs, newSeqs)
	}
	for _, f := range []fnats.LogFollow{follow, newOnly} {
		if err := f.Stop(testCtx(t, 5*time.Second)); err != nil {
			t.Fatal(err)
		}
		<-f.Done()
		if err := f.Stop(testCtx(t, time.Second)); err != nil {
			t.Fatalf("second Stop: %v", err)
		}
	}

	// 持久消费者确认到某处；有序消费者（Follow / ReadRange 用的）不算。
	durable, err := log.js.CreateOrUpdateConsumer(ctx, name, gojs.ConsumerConfig{Durable: "guard", FilterSubject: p1, AckPolicy: gojs.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := durable.Fetch(4, gojs.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var ackedUpTo uint64
	for m := range batch.Messages() {
		meta, _ := m.Metadata()
		if err := m.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		ackedUpTo = meta.Sequence.Stream
	}
	floors, err := log.DurableAckFloors(ctx, name)
	if err != nil || len(floors) != 1 || floors["guard"] != ackedUpTo {
		t.Fatalf("ack floors = %v %v, want guard=%d only", floors, err, ackedUpTo)
	}

	// 清理：只删 p0 里低于界限的；p1 与界限之上的 p0 都还在。
	bound := p0Seqs[5]
	if err := log.Purge(ctx, name, p0, bound); err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := log.ReadRange(ctx, name, []string{p0}, 1, 0, collect); err != nil || got[0] != bound {
		t.Fatalf("after purge below %d the first p0 message is %v (%v)", bound, got, err)
	}
	var p1Count int
	if err := log.ReadRange(ctx, name, []string{p1}, 1, 0, func(fnats.LogMsg) error { p1Count++; return nil }); err != nil || p1Count != 10 {
		t.Fatalf("p1 after a p0 purge: %d %v", p1Count, err)
	}
	if err := log.Purge(ctx, name, p0, 1); err != nil {
		t.Fatalf("purge below 1 is a no-op: %v", err)
	}
}

func TestRealLogKeyValueAndObjects(t *testing.T) {
	log := logCluster(t)
	ctx := testCtx(t, 60*time.Second)
	bucket := logUniqueName("RLOGKV")
	kv, err := log.KeyValue(ctx, fnats.LogKVConfig{Bucket: bucket, Replicas: 3, MaxAge: 3 * time.Second, LimitMarkerTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.js.DeleteKeyValue(context.Background(), bucket) })

	if _, err := kv.Get(ctx, "p0"); !errors.Is(err, fnats.ErrLogNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	rev, err := kv.Create(ctx, "p0", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Create(ctx, "p0", []byte("b")); !errors.Is(err, fnats.ErrLogKVConflict) {
		t.Fatalf("Create existing = %v", err)
	}
	next, err := kv.Update(ctx, "p0", []byte("b"), rev)
	if err != nil {
		t.Fatal(err)
	}
	_, err = kv.Update(ctx, "p0", []byte("c"), rev)
	t.Logf("stale Update on an R3 bucket: %v", err)
	if !errors.Is(err, fnats.ErrLogKVConflict) {
		t.Fatalf("stale Update = %v", err)
	}
	if entry, err := kv.Get(ctx, "p0"); err != nil || string(entry.Value) != "b" || entry.Revision != next {
		t.Fatalf("Get = %+v %v", entry, err)
	}

	type event struct {
		key     string
		deleted bool
		ready   bool
	}
	events := make(chan event, 64)
	watch, err := kv.Watch(ctx, "", func(e fnats.LogKVEntry) { events <- event{key: e.Key, deleted: e.Deleted} }, func() { events <- event{ready: true} })
	if err != nil {
		t.Fatal(err)
	}
	next1 := func() event {
		select {
		case e := <-events:
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("watch delivered nothing")
			return event{}
		}
	}
	if e := next1(); e.key != "p0" || e.deleted {
		t.Fatalf("initial value = %+v", e)
	}
	if e := next1(); !e.ready {
		t.Fatalf("ready marker = %+v", e)
	}
	if _, err := kv.Put(ctx, "p1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if e := next1(); e.key != "p1" || e.deleted {
		t.Fatalf("put = %+v", e)
	}
	if err := kv.Delete(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if e := next1(); e.key != "p1" || !e.deleted {
		t.Fatalf("delete = %+v", e)
	}
	if _, err := kv.Get(ctx, "p1"); !errors.Is(err, fnats.ErrLogNotFound) {
		t.Fatalf("Get deleted = %v", err)
	}
	// MaxAge 过期：带 LimitMarkerTTL 的桶由服务端写删除标记，Watch 看得到（准入信号的“参与方下线”靠它）。
	expired := false
	deadline := time.After(15 * time.Second)
	for !expired {
		select {
		case e := <-events:
			expired = e.key == "p0" && e.deleted
		case <-deadline:
			t.Fatal("MaxAge expiry of p0 never reached the watcher")
		}
	}
	if err := watch.Stop(testCtx(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}

	objectBucket := logUniqueName("RLOGOBJ")
	objects, err := log.Objects(ctx, fnats.LogObjectConfig{Bucket: objectBucket, Replicas: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.js.DeleteObjectStore(context.Background(), objectBucket) })
	data := make([]byte, 3<<20) // 大于 max_payload：服务端分块
	for i := range data {
		data[i] = byte(i)
	}
	info, err := objects.Put(ctx, "p0-42", data)
	if err != nil || info.Size != uint64(len(data)) || !strings.HasPrefix(info.Digest, "SHA-256=") {
		t.Fatalf("Put = %+v %v", info, err)
	}
	back, err := objects.Get(ctx, "p0-42")
	if err != nil || string(back) != string(data) {
		t.Fatalf("Get = %d bytes %v", len(back), err)
	}
	if err := objects.Delete(ctx, "p0-42"); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Get(ctx, "p0-42"); !errors.Is(err, fnats.ErrLogNotFound) {
		t.Fatalf("Get deleted object = %v", err)
	}
}

func TestRealLogRequestManyGathersEveryReply(t *testing.T) {
	log := logCluster(t)
	subject := logUniqueName("rlog.q.")
	for i := 0; i < 3; i++ {
		sub, err := log.client.Subscribe(subject, func(msg *fnats.Msg) {
			_ = log.client.PublishOnce(msg.Reply, []byte(fmt.Sprintf("p%d", i)))
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	_ = log.client.conn.Flush()
	var replies []string
	err := log.RequestMany(testCtx(t, 5*time.Second), subject, []byte("q"), func(b []byte) bool {
		replies = append(replies, string(b))
		return len(replies) < 3
	})
	if err != nil || len(replies) != 3 {
		t.Fatalf("replies = %v %v", replies, err)
	}
	// 只等得到 3 个、却要 4 个：ctx 到期，错误可 errors.Is 到 ctx 错误。
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = log.RequestMany(short, subject, nil, func([]byte) bool { return true })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing replies = %v", err)
	}
	if err := log.RequestMany(testCtx(t, 5*time.Second), logUniqueName("rlog.nobody."), nil, func([]byte) bool { return true }); !errors.Is(err, fnats.ErrNoResponders) {
		t.Fatalf("no responders = %v", err)
	}
}

// --- leader kill -9 ---

func clusterRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("ROOST_NATS_CLUSTER_ROOT")
	if root == "" {
		t.Skip("ROOST_NATS_CLUSTER_ROOT is not set (the data directory of scripts/nats-cluster.sh; the test reads its pid files)")
	}
	return root
}

func clusterKill(t *testing.T, root string, node int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("n%d.pid", node)))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 node %d: %v", node, err)
	}
}

func clusterRestart(t *testing.T, node int) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "scripts", "nats-cluster.sh"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script, "restart", strconv.Itoa(node)).CombinedOutput()
	if err != nil {
		t.Fatalf("restart node %d: %v %s", node, err, out)
	}
}

func streamLeaderNode(t *testing.T, log *JetStreamLogClient, stream string) int {
	t.Helper()
	handle, err := log.js.Stream(testCtx(t, 10*time.Second), stream)
	if err != nil {
		t.Fatal(err)
	}
	info := handle.CachedInfo()
	if info.Cluster == nil || !strings.HasPrefix(info.Cluster.Leader, "roostjs-n") {
		t.Fatalf("stream has no known leader: %+v", info.Cluster)
	}
	node, err := strconv.Atoi(strings.TrimPrefix(info.Cluster.Leader, "roostjs-n"))
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// waitClusterReady 等三个节点都能承载 R3 流（被杀节点重新入群之前建 R3 流会报 no suitable peers），后面的用例才不受影响。
func waitClusterReady(t *testing.T, log *JetStreamLogClient) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		name := logUniqueName("PROBE")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := log.js.CreateStream(ctx, gojs.StreamConfig{Name: name, Subjects: []string{name + ".>"}, Replicas: 3, Storage: gojs.FileStorage})
		if err == nil {
			_ = log.js.DeleteStream(ctx, name)
		}
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster did not recover three peers: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// logWriter 是测试里的单写者：按协调器的规则发布并在结果未知时重发判定。
type logWriter struct {
	t        *testing.T
	log      *JetStreamLogClient
	stream   string
	filter   string
	last     uint64
	acked    map[string]uint64 // MsgID → PubAck（或判定）给出的序号
	outcomes map[string]int
	stall    time.Duration
}

// commit 提交一轮；返回时这轮已确定落库（否则 Fatal）。结果未知就用同样的消息与 fence 重发：成功 / Duplicate = 已提交；
// 10201 或 wrong last sequence 且过滤上最后一条是这一轮的最后一条 = 之前已提交；批被丢弃（10176）= 没写入，重发。
func (w *logWriter) commit(msgs []fnats.LogMsg) {
	w.t.Helper()
	fence := fnats.LogFence{Filter: w.filter, LastSeq: w.last}
	began := time.Now()
	unknown := false
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ack, err := w.log.PublishFenced(ctx, msgs, fence)
		cancel()
		switch {
		case err == nil:
			w.settle(msgs, ack.Seq, unknown, map[bool]string{true: "resolved_duplicate", false: "resolved_new"}[ack.Duplicate])
		case errors.Is(err, fnats.ErrLogOutcomeUnknown), errors.Is(err, fnats.ErrLogBatchIncomplete):
			if errors.Is(err, fnats.ErrLogOutcomeUnknown) {
				unknown = true
			}
			if attempt == 0 {
				w.outcomes["first_attempt_"+map[bool]string{true: "unknown", false: "incomplete"}[unknown]]++
			}
			if time.Since(began) > 60*time.Second {
				w.t.Fatalf("round with %s unresolved after 60s: %v", msgs[0].MsgID, err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		case errors.Is(err, fnats.ErrLogDuplicateBatch), errors.Is(err, fnats.ErrLogWrongLastSequence):
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			last, lerr := w.log.LastMsg(ctx, w.stream, w.filter)
			cancel()
			if lerr != nil {
				w.t.Fatalf("resolution read after %v: %v", err, lerr)
			}
			if last.MsgID != msgs[len(msgs)-1].MsgID {
				w.t.Fatalf("single writer fenced out: %v; last on the filter is %q", err, last.MsgID)
			}
			w.settle(msgs, last.Seq, true, "resolved_by_last")
		default:
			w.t.Fatalf("round with %s: %v", msgs[0].MsgID, err)
		}
		if d := time.Since(began); d > w.stall {
			w.stall = d
		}
		return
	}
}

// settle 记下这一轮每条消息的序号：批是连续的，末条序号为 lastSeq。
func (w *logWriter) settle(msgs []fnats.LogMsg, lastSeq uint64, afterUnknown bool, how string) {
	for i, m := range msgs {
		w.acked[m.MsgID] = lastSeq - uint64(len(msgs)-1-i)
	}
	w.last = lastSeq
	if afterUnknown {
		w.outcomes[how]++
	}
}

func TestRealLogLeaderKillKeepsAckedWritesAndResolvesUnknowns(t *testing.T) {
	root := clusterRoot(t)
	log := logCluster(t)
	name, _ := logStream(t, log)
	w := &logWriter{t: t, log: log, stream: name, filter: name + ".0.ev.>", acked: map[string]uint64{}, outcomes: map[string]int{}}
	leader := streamLeaderNode(t, log, name)
	t.Cleanup(func() { clusterRestart(t, leader); waitClusterReady(t, log) })

	const rounds, killAt = 600, 200
	total := 0
	var killedAt time.Time
	for round := 0; round < rounds; round++ {
		if round == killAt {
			clusterKill(t, root, leader)
			killedAt = time.Now()
			t.Logf("kill -9 stream leader roostjs-n%d after %d rounds", leader, round)
		}
		// 交替单条与 3 条原子批，覆盖两种结果判定。
		n := 1
		if round%2 == 1 {
			n = 3
		}
		msgs := make([]fnats.LogMsg, n)
		for i := range msgs {
			msgs[i] = ev(fmt.Sprintf("%s.0.ev.t%d", name, i), fmt.Sprintf("w.%d.%d", round, i))
		}
		w.commit(msgs)
		total += n
	}
	newLeader := streamLeaderNode(t, log, name)
	t.Logf("rounds=%d messages=%d outcomes=%v longest round=%v new leader=n%d since kill=%v", rounds, total, w.outcomes, w.stall, newLeader, time.Since(killedAt))
	if newLeader == leader {
		t.Fatal("leader did not move away from the killed node")
	}

	// 拉起被杀节点，等它追上，从 leader 读整条流：每个已确认的 MsgID 恰好一条，序号与确认一致，没有多余消息。
	clusterRestart(t, leader)
	verifyStream := func(want int) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for {
			handle, err := log.js.Stream(testCtx(t, 5*time.Second), name)
			if err == nil {
				info := handle.CachedInfo()
				current := 0
				for _, r := range info.Cluster.Replicas {
					if r.Current && !r.Offline {
						current++
					}
				}
				if current == 2 && info.State.Msgs == uint64(want) {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("restarted replica did not catch up (want %d messages): %v", want, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		seen := map[string]uint64{}
		if err := log.ReadRange(testCtx(t, 60*time.Second), name, []string{name + ".>"}, 1, uint64(want), func(m fnats.LogMsg) error {
			if prev, dup := seen[m.MsgID]; dup {
				return fmt.Errorf("MsgID %s stored twice (seq %d and %d)", m.MsgID, prev, m.Seq)
			}
			seen[m.MsgID] = m.Seq
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(seen) != len(w.acked) {
			t.Fatalf("stream holds %d messages, %d were acknowledged", len(seen), len(w.acked))
		}
		for id, seq := range w.acked {
			if seen[id] != seq {
				t.Fatalf("acknowledged %s at seq %d, stream has it at %d", id, seq, seen[id])
			}
		}
	}
	verifyStream(total)
	t.Logf("all %d acknowledged messages present exactly once at their acknowledged sequence after the killed node rejoined", total)

	// 第二段：确定性制造“已落库、答复丢失”，立刻 kill -9 当前 leader，在新 leader 上重发判定（去重表随流复制）。
	for _, shape := range []int{1, 3} {
		current := streamLeaderNode(t, log, name)
		msgs := make([]fnats.LogMsg, shape)
		for i := range msgs {
			msgs[i] = ev(fmt.Sprintf("%s.0.ev.t%d", name, i), fmt.Sprintf("lost%d.%d", shape, i))
		}
		batchID := ""
		if shape > 1 {
			batchID = "lost" + logUniqueName("")
		}
		built, err := buildLogMessages(msgs, fnats.LogFence{Filter: w.filter, LastSeq: w.last}, batchID, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range built {
			if err := log.client.conn.PublishMsg(m); err != nil {
				t.Fatal(err)
			}
		}
		_ = log.client.conn.Flush()
		var landed uint64
		for deadline := time.Now().Add(10 * time.Second); ; {
			last, err := log.LastMsg(testCtx(t, 5*time.Second), name, w.filter)
			if err == nil && last.MsgID == msgs[shape-1].MsgID {
				landed = last.Seq
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("lost-ack write never landed: %+v %v", last, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		clusterKill(t, root, current)
		began := time.Now()
		w.commit(msgs)
		t.Logf("shape %d: landed at %d, leader n%d killed, resolved after %v as %v (seq %d)", shape, landed, current, time.Since(began), w.outcomes, w.last)
		if w.last != landed {
			t.Fatalf("shape %d resolved at %d, the write landed at %d", shape, w.last, landed)
		}
		total += shape
		clusterRestart(t, current)
		waitClusterReady(t, log)
	}
	verifyStream(total)
}
