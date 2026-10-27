package jslogproto

// 进程级 kill -9：协调器进程被杀后另一个进程等租约过期接管、重放，不重复推进、不丢步骤；参与方进程被杀后用同一
// WAL 目录重启、重建实体，重投的命令按回执回放。负载是闭环 16 路，混 20% 补偿。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type proc struct {
	cmd   *exec.Cmd
	ready string
}

func startProc(t *testing.T, args ...string) *proc {
	t.Helper()
	// 子进程就是本测试二进制（TestMain 见 SAGAJS_ROLE_ARGS 走进程入口）。
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), roleArgsEnv+"="+strings.Join(args, "\x1f"))
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{cmd: cmd}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "ready") {
				lines <- sc.Text()
			}
		}
	}()
	select {
	case p.ready = <-lines:
	case <-time.After(60 * time.Second):
		t.Fatalf("process %v not ready", args)
	}
	return p
}

func (p *proc) kill9(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = p.cmd.Wait()
}

// closedLoop 跑 n 笔：workers 路各自“发起 → 等终态 → 下一笔”；onDone 每完成一笔调用一次（参数是已完成数）。
func (e *env) closedLoop(n, workers int, timeout time.Duration, onDone func(int)) (ids []string, data map[string]TransferData) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	data = map[string]TransferData{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("k-%d", i)
		ids = append(ids, id)
		data[id] = e.transfer(i, i%5 == 0)
	}
	var next, done atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				ch, err := e.client.Start(ctx, ids[i], data[ids[i]])
				if err != nil {
					errs <- err
					return
				}
				select {
				case <-ch:
					if onDone != nil {
						onDone(int(done.Add(1)))
					}
				case <-ctx.Done():
					errs <- fmt.Errorf("saga %s not terminal within %v", ids[i], timeout)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		e.t.Fatal(err)
	}
	return ids, data
}

func (e *env) url() string { return clusterURL(e.t) }

func TestCoordinatorKill9Takeover(t *testing.T) { coordinatorKill9(t, false) }

// 投影打开：冷副本投影在协调器进程里，随它一起被 kill -9；B 进程接着用同一个 durable 投影，冷副本最终与终态一致。
func TestCoordinatorKill9TakeoverProjected(t *testing.T) { coordinatorKill9(t, true) }

func coordinatorKill9(t *testing.T, projected bool) {
	e := newEnv(t, 4)
	if projected {
		e.withMongo()
	}
	part := e.participant(nil)
	common := []string{"-role", "coord", "-url", e.url(), "-prefix", e.l.Prefix, "-parts", fmt.Sprint(e.l.Partitions),
		"-stream-groups", fmt.Sprint(e.l.StreamGroups), "-lease-ttl", "3s"}
	if projected {
		common = append(common, "-project", "-db", e.db)
	}
	a := startProc(t, append(common, "-instance", "A")...)
	var killed atomic.Bool
	var killedAt time.Time
	var b *proc
	var bMu sync.Mutex
	all, _ := e.closedLoop(400, 16, 120*time.Second, func(done int) {
		if done == 150 && killed.CompareAndSwap(false, true) {
			a.kill9(t)
			killedAt = time.Now()
			go func() {
				p := startProc(t, append(common, "-instance", "B")...)
				bMu.Lock()
				b = p
				bMu.Unlock()
			}()
		}
	})
	bMu.Lock()
	t.Logf("A killed after 150 terminal sagas; B: %q; all 400 terminal %v after the kill", b.ready, time.Since(killedAt))
	bMu.Unlock()
	time.Sleep(time.Second)
	final := e.client.Latest()
	for i, id := range all {
		want := StatusCompleted
		if i%5 == 0 {
			want = StatusCompensated
		}
		if final[id].Status != want {
			t.Fatalf("%s: %s, want %s", id, final[id], want)
		}
	}
	e.checkBalances(balanceOf(part), final, all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations after coordinator kill -9:\n%s", strings.Join(v, "\n"))
	}
	if projected {
		// 只读：Backlog / ColdState 不启动消费，投影由 B 进程做。
		caught := e.waitColdCopy(NewSagaProjector(e.js, e.l, e.mongo, e.db, quietLog()), all, 60*time.Second)
		e.checkMongoAccounts(part)
		t.Logf("cold copy matched all %d terminal states %v after the last one", len(all), caught)
	}
}

func TestParticipantKill9Recovery(t *testing.T) { participantKill9(t, false) }

// 投影打开：参与方进程 kill -9 后用同一 WAL 重启，先把 WAL 尾巴投影进 Mongo、再从 Mongo 加载（现行恢复顺序）；
// 结束时 Mongo 账户与预期余额逐账户一致，冷副本与终态一致。
func TestParticipantKill9RecoveryProjected(t *testing.T) { participantKill9(t, true) }

func participantKill9(t *testing.T, projected bool) {
	e := newEnv(t, 2)
	var sp *SagaProjector
	if projected {
		e.withMongo()
		sp = e.sagaProjector()
	}
	coord := e.coordinator("A", nil)
	e.takeAll(coord, false)
	wal := t.TempDir()
	args := []string{"-role", "part", "-url", e.url(), "-prefix", e.l.Prefix, "-parts", fmt.Sprint(e.l.Partitions),
		"-stream-groups", fmt.Sprint(e.l.StreamGroups), "-accounts", fmt.Sprint(e.pairs * 2), "-wal", wal}
	if projected {
		args = append(args, "-project", "-db", e.db)
	}
	first := startProc(t, args...)
	var killed atomic.Bool
	var second *proc
	var mu sync.Mutex
	all, _ := e.closedLoop(300, 16, 180*time.Second, func(done int) {
		if done == 100 && killed.CompareAndSwap(false, true) {
			first.kill9(t)
			go func() {
				p := startProc(t, args...)
				mu.Lock()
				second = p
				mu.Unlock()
			}()
		}
	})
	mu.Lock()
	t.Logf("participant killed after 100 terminal sagas; restarted: %q", second.ready)
	mu.Unlock()
	time.Sleep(time.Second)
	balances := func(n int64) (int64, error) {
		resp, err := e.nc.Request("sagajs_admin."+e.l.Prefix+".balances", nil, 10*time.Second)
		if err != nil {
			return 0, err
		}
		var out map[int64]int64
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return 0, fmt.Errorf("%v: %s", err, resp.Data)
		}
		return out[n], nil
	}
	final := e.client.Latest()
	e.checkBalances(balances, final, all)
	if v := e.chainViolations(); len(v) > 0 {
		t.Fatalf("event chain violations after participant kill -9:\n%s", strings.Join(v, "\n"))
	}
	if projected {
		caught := e.waitColdCopy(sp, all, 60*time.Second)
		// 参与方进程里的投影是后台的：等 Mongo 账户追上预期余额（与上面核对过的内存一致）。
		deadline := time.Now().Add(30 * time.Second)
		for {
			mongo, err := e.mongoAccounts()
			if err != nil {
				t.Fatal(err)
			}
			mismatch := 0
			for n := int64(0); n < int64(e.pairs*2); n++ {
				want, _ := balances(n)
				if mongo[accountEntityID(n)] != want {
					mismatch++
				}
			}
			if mismatch == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("Mongo accounts still differ from the participant after 30s: %d accounts", mismatch)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("after participant kill -9: cold copy caught up %v, Mongo accounts equal the participant's entities", caught)
	}
	t.Logf("coordinator: accepted=%d duplicate=%d stale=%d", coord.Stats.Accepted.Load(), coord.Stats.Duplicate.Load(), coord.Stats.StaleAttempt.Load())
	_ = context.Background
}
