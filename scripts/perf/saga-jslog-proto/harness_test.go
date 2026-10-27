package jslogproto

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// 变异开关（先红后绿）：SAGAJS_MUTATE=nofence 让协调器发布不带期望序号；=noreceipt 让参与方不查回执。
// 正常用例在变异下必须失败。
func mutation() string { return os.Getenv("SAGAJS_MUTATE") }

const initialBalance = 1_000_000

type env struct {
	t      *testing.T
	nc     *nats.Conn
	js     jetstream.JetStream
	l      Layout
	client *Client
	pairs  int
}

func quietLog() *slog.Logger {
	if os.Getenv("SAGAJS_LOG") != "" {
		return slog.Default()
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newEnv(t *testing.T, partitions int) *env { return newEnvGroups(t, partitions, 1) }

func newEnvGroups(t *testing.T, partitions, groups int) *env {
	t.Helper()
	nc, js := connect(t)
	l := Layout{Prefix: strings.ToLower(uniqueName("t")), Partitions: partitions, StreamGroups: groups}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Setup(ctx, js, l, 3, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Teardown(ctx, js, l)
	})
	client, err := NewClient(ctx, js, l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return &env{t: t, nc: nc, js: js, l: l, client: client, pairs: 32}
}

func (e *env) coordinator(instance string, mod func(*CoordinatorOptions)) *Coordinator {
	opts := CoordinatorOptions{Layout: e.l, Instance: instance, PurgeGuards: []string{"part_acct"}, Log: quietLog(),
		DisableFence: mutation() == "nofence"}
	if mod != nil {
		mod(&opts)
	}
	c := NewCoordinator(e.nc, e.js, opts)
	e.t.Cleanup(c.Stop)
	return c
}

func (e *env) takeAll(c *Coordinator, steal bool) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for k := 0; k < e.l.Partitions; k++ {
		if err := c.Takeover(ctx, k, steal); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) participant(mod func(*ParticipantOptions)) *Participant {
	e.t.Helper()
	opts := ParticipantOptions{Layout: e.l, WALDir: e.t.TempDir(), Accounts: e.pairs * 2, InitialBal: initialBalance, Workers: 16,
		Log: quietLog(), DisableReceiptCheck: mutation() == "noreceipt"}
	if mod != nil {
		mod(&opts)
	}
	p, err := NewParticipant(e.nc, e.js, opts)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(p.Stop)
	return p
}

// transfer 给第 i 笔分配一对账户；compensate 时第二步被业务拒绝。
func (e *env) transfer(i int, compensate bool) TransferData {
	pair := i % e.pairs
	return TransferData{From: int64(2 * pair), To: int64(2*pair + 1), Amount: int64(1 + i%7), RefuseCredit: compensate}
}

// startAll 同时发起全部 saga，等每一笔第一次到终态。
func (e *env) startAll(ids []string, data map[string]TransferData, timeout time.Duration) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var chans []<-chan struct{}
	for _, id := range ids {
		ch, err := e.client.Start(ctx, id, data[id])
		if err != nil {
			e.t.Fatal(err)
		}
		chans = append(chans, ch)
	}
	for i, ch := range chans {
		select {
		case <-ch:
		case <-ctx.Done():
			e.t.Fatalf("saga %s did not reach a terminal state within %v", ids[i], timeout)
		}
	}
}

// waitFor 等观察到的终态满足条件（重开后再次到终态的情况）。
func (e *env) waitFor(timeout time.Duration, what string, ok func(map[string]*Saga) bool) map[string]*Saga {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		latest := e.client.Latest()
		if ok(latest) {
			return latest
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkBalances 按终态推算每个账户应有的余额并与参与方的实体核对（守恒 + 逐账户）。
func (e *env) checkBalances(balance func(int64) (int64, error), final map[string]*Saga, ids []string) {
	e.t.Helper()
	want := map[int64]int64{}
	for n := 0; n < e.pairs*2; n++ {
		want[int64(n)] = initialBalance
	}
	for _, id := range ids {
		s := final[id]
		if s == nil {
			e.t.Fatalf("no terminal state observed for %s", id)
		}
		switch s.Status {
		case StatusCompleted:
			want[s.Data.From] -= s.Data.Amount
			want[s.Data.To] += s.Data.Amount
		case StatusCompensated, StatusFailed:
		default:
			e.t.Fatalf("saga %s ended %s; cannot predict balances", id, s)
		}
	}
	var total, wantTotal int64
	var bad []string
	for n := int64(0); n < int64(e.pairs*2); n++ {
		got, err := balance(n)
		if err != nil {
			e.t.Fatal(err)
		}
		total += got
		wantTotal += want[n]
		if got != want[n] {
			bad = append(bad, fmt.Sprintf("account %d: %d, want %d", n, got, want[n]))
		}
	}
	if len(bad) > 0 {
		e.t.Fatalf("balances do not match the terminal states (sum %d, want %d):\n%s", total, wantTotal, strings.Join(bad, "\n"))
	}
}

// chainViolations 读整条流的事件，核对每笔 saga 的版本从 1 起逐一递增、没有重复（单写者 + 不重复推进），
// 每个 CmdID 只派发一次。流被清理过时只核对剩下的部分。
func (e *env) chainViolations() []string {
	e.t.Helper()
	versions := map[string][]uint64{}
	dispatched := map[string]int{}
	for _, name := range e.l.Streams() {
		e.collectEvents(name, versions, dispatched)
	}
	var violations []string
	for id, vs := range versions {
		for i := 1; i < len(vs); i++ {
			if vs[i] != vs[i-1]+1 {
				violations = append(violations, fmt.Sprintf("saga %s versions %v", id, vs))
				break
			}
		}
	}
	for cmd, n := range dispatched {
		if n > 1 {
			violations = append(violations, fmt.Sprintf("command %s dispatched %d times", cmd, n))
		}
	}
	sort.Strings(violations)
	return violations
}

func (e *env) collectEvents(name string, versions map[string][]uint64, dispatched map[string]int) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := e.js.Stream(ctx, name)
	if err != nil {
		e.t.Fatal(err)
	}
	last, err := stream.GetLastMsgForSubject(ctx, e.l.Prefix+".*.ev.>")
	if err != nil {
		return // 这条流没有事件
	}
	cons, err := e.js.OrderedConsumer(ctx, name, jetstream.OrderedConsumerConfig{FilterSubjects: []string{e.l.Prefix + ".*.ev.>"}})
	if err != nil {
		e.t.Fatal(err)
	}
	it, err := cons.Messages()
	if err != nil {
		e.t.Fatal(err)
	}
	defer it.Stop()
	for {
		msg, err := it.Next()
		if err != nil {
			e.t.Fatal(err)
		}
		var ev Event
		if err := json.Unmarshal(msg.Data(), &ev); err != nil {
			e.t.Fatal(err)
		}
		versions[ev.Saga.ID] = append(versions[ev.Saga.ID], ev.Saga.Version)
		if ev.Cmd != nil {
			dispatched[ev.Cmd.CmdID]++
		}
		meta, _ := msg.Metadata()
		if meta.Sequence.Stream >= last.Sequence {
			return
		}
	}
}
