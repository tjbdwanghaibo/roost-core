package jslogproto

// R3 leader 切换：单写者按“期望序号 + MsgID”连续写，写到一半 kill -9 流 leader 所在节点。
// 验证：已 PubAck 的消息一条不丢、不重复；PubAck 超时 / 断连（结果未知）的写用同一 MsgID、同一期望序号重发即可判定——
// Duplicate 说明第一次已落库，成功说明这次才落库，wrong last sequence 说明别人写过（这里只有一个写者，不应出现）。
// 需要 SAGAJS_ROOT（nats-cluster.sh 的数据目录，读 pid 文件）；测试结束时把节点拉起来。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func clusterRoot(t *testing.T) string {
	root := os.Getenv("SAGAJS_ROOT")
	if root == "" {
		t.Skip("SAGAJS_ROOT 未设置（nats-cluster.sh 的数据目录）")
	}
	return root
}

func killNode(t *testing.T, root string, node int) {
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

func restartNode(t *testing.T, node int) {
	t.Helper()
	script, err := filepath.Abs("nats-cluster.sh")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script, "restart", strconv.Itoa(node)).CombinedOutput()
	if err != nil {
		t.Fatalf("restart node %d: %v %s", node, err, out)
	}
}

func leaderNode(t *testing.T, stream jetstream.Stream) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Cluster == nil || !strings.HasPrefix(info.Cluster.Leader, "sagajs-n") {
		t.Fatalf("stream has no known leader: %+v", info.Cluster)
	}
	node, err := strconv.Atoi(strings.TrimPrefix(info.Cluster.Leader, "sagajs-n"))
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func TestLeaderKillKeepsAckedWritesAndResolvesUnknowns(t *testing.T) {
	root := clusterRoot(t)
	nc, js := connect(t)
	name := uniqueName("SEMF")
	stream := createStream(t, js, jetstream.StreamConfig{Name: name, Subjects: []string{name + ".>"}, Duplicates: 2 * time.Minute})
	filter := name + ".p0.ev.>"
	leader := leaderNode(t, stream)
	t.Cleanup(func() { restartNode(t, leader) })

	const total, killAt = 600, 200
	var last uint64
	acked := map[string]uint64{}
	var unknown, resolvedDup, resolvedNew, retries int
	var firstErrs []string
	var killedAt time.Time
	var stall time.Duration
	for i := 0; i < total; i++ {
		if i == killAt {
			killNode(t, root, leader)
			killedAt = time.Now()
			t.Logf("kill -9 stream leader sagajs-n%d after %d acked writes", leader, i)
		}
		id := fmt.Sprintf("ev-%d", i)
		subject := fmt.Sprintf("%s.p0.ev.t%d", name, i%3)
		begin := time.Now()
		attempt := 0
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			ack, err := js.Publish(ctx, subject, []byte(id), jetstream.WithMsgID(id), jetstream.WithExpectLastSequenceForSubject(last, filter), jetstream.WithRetryAttempts(0))
			cancel()
			if err == nil {
				if attempt > 0 {
					if ack.Duplicate {
						resolvedDup++
					} else {
						resolvedNew++
					}
				}
				last = ack.Sequence
				acked[id] = ack.Sequence
				break
			}
			if isWrongLastSeq(err) {
				t.Fatalf("write %d attempt %d: wrong last sequence with a single writer: %v", i, attempt, err)
			}
			if attempt == 0 {
				unknown++
				if len(firstErrs) < 6 {
					firstErrs = append(firstErrs, err.Error())
				}
			}
			attempt++
			retries++
			if time.Since(begin) > 60*time.Second {
				t.Fatalf("write %d unresolved after 60s: %v", i, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if d := time.Since(begin); d > stall {
			stall = d
		}
	}
	newLeader := leaderNode(t, stream)
	t.Logf("writes=%d unknown(first attempt failed)=%d resolved as duplicate=%d resolved as new=%d retries=%d longest write=%v new leader=n%d since kill=%v",
		total, unknown, resolvedDup, resolvedNew, retries, stall, newLeader, time.Since(killedAt))
	for _, e := range firstErrs {
		t.Logf("first-attempt error: %s", e)
	}
	if newLeader == leader {
		t.Fatalf("leader did not move away from the killed node")
	}

	// 拉起被杀节点，等它追上，再从三个节点的视角读：每个 MsgID 恰好一条，序号与 PubAck 一致。
	restartNode(t, leader)
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := stream.Info(ctx)
		cancel()
		if err == nil && info.Cluster != nil {
			current := 0
			for _, r := range info.Cluster.Replicas {
				if r.Current && !r.Offline {
					current++
				}
			}
			if current == 2 && info.State.Msgs == uint64(total) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted replica did not catch up: %+v err=%v", info, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := map[string]uint64{}
	for seq := uint64(1); seq <= uint64(total); seq++ {
		m, err := stream.GetMsg(ctx, seq)
		if err != nil {
			t.Fatalf("get seq %d: %v", seq, err)
		}
		id := m.Header.Get(jetstream.MsgIDHeader)
		if _, dup := seen[id]; dup {
			t.Fatalf("MsgID %s stored twice (seq %d and %d)", id, seen[id], seq)
		}
		seen[id] = seq
	}
	for id, seq := range acked {
		if seen[id] != seq {
			t.Fatalf("acked %s at seq %d, stream has it at %d", id, seq, seen[id])
		}
	}
	t.Logf("all %d acked writes present exactly once at their PubAck sequence after the killed node rejoined", len(acked))

	// 第二段：确定性制造“已落库、PubAck 没收到”再立刻 kill -9 新 leader，重试必须在换了 leader 之后判成 Duplicate（去重表随流复制）。
	second := leaderNode(t, stream)
	t.Cleanup(func() { restartNode(t, second) })
	lostID := "ev-lost"
	lost := nats.NewMsg(name + ".p0.ev.t0")
	lost.Data = []byte(lostID)
	lost.Header.Set(jetstream.MsgIDHeader, lostID)
	lost.Header.Set(jetstream.ExpectedLastSubjSeqSubjHeader, filter)
	lost.Header.Set(jetstream.ExpectedLastSubjSeqHeader, fmt.Sprint(last))
	if err := nc.PublishMsg(lost); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	for deadline := time.Now().Add(5 * time.Second); lastSeqOf(t, stream, filter) == last && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	landedAt := lastSeqOf(t, stream, filter)
	killNode(t, root, second)
	begin := time.Now()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ack, err := js.Publish(ctx, lost.Subject, lost.Data, jetstream.WithMsgID(lostID), jetstream.WithExpectLastSequenceForSubject(last, filter), jetstream.WithRetryAttempts(0))
		cancel()
		if err == nil {
			t.Logf("ack-lost write landed at %d, leader n%d killed, retry after %v: ack=%+v", landedAt, second, time.Since(begin), ack)
			if !ack.Duplicate || ack.Sequence != landedAt {
				t.Fatalf("retry after leader change: ack=%+v, want Duplicate at %d", ack, landedAt)
			}
			break
		}
		if isWrongLastSeq(err) || time.Since(begin) > 60*time.Second {
			t.Fatalf("retry after leader change: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	restartNode(t, second)
}
