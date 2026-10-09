package driver

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// REFACTOR-2026-10-06-nats-driver-closed-state：驱动自己持有唯一的“已关闭”状态（Client.state.closed），
// 每个公开方法入口先查它。下面三组用例钉住这个判据：
//
//   - 守卫：解析本包全部非测试源码，列出每个导出方法，要求 closedStateChecks 里都有一行；Close 之后逐个
//     调用，断言返回可 errors.Is 到 fnats.ErrClosed 的错误（关闭类方法返回 nil、状态查询返回 false）。
//     以后新加导出方法而不在表里登记，守卫直接红。
//   - 屏障：调用过了入口检查、还没碰 nats.go 时插入 Close，结果必须是 fnats.ErrClosed（不能是 nats.go 的
//     其他错误，也不能成功）。
//   - 并发：Close 与调用同时进行（-race），每个结果要么成功（在 Close 之前完成）要么 fnats.ErrClosed；
//     Close 返回之后发起的调用一律 fnats.ErrClosed。
//
// 连接指向不可达地址并开启 RetryOnFailedConnect（closeContractAssembly）：连接处于重连中，nats.go 允许
// 订阅、缓冲发布，不需要真实 NATS。

// closedOutcome 是 Close 之后调用一个导出方法的期望。
type closedOutcome int

const (
	_                 closedOutcome = iota // 零值无效：漏写 want 的表项不会被当成某种期望
	wantErrClosed                          // 返回可 errors.Is 到 fnats.ErrClosed 的错误
	wantNil                                // 关闭类方法：重复调用返回 nil / 正常返回；状态查询：返回 false（call 返回 nil）
	notConnectionCall                      // 不是连接上的调用，reason 写明为什么
)

// closedStateEnv 是一套已关闭的驱动对象（同一个连接）。
type closedStateEnv struct {
	ctx    context.Context // 带期限，调用不会因为驱动没答复而挂住
	cancel context.CancelFunc
	asm    *Assembly
	js     *JetStreamClient
	sub    *subscription
}

type closedStateCheck struct {
	want   closedOutcome
	reason string
	closes bool // 关闭类方法：checkEveryMethodAfterClose 最后才调，先让其他方法在同一状态下回答
	call   func(*closedStateEnv) error
}

var closedStateHandler = func(*fnats.Msg) {}

// closedStateChecks 以“接收者类型.方法”为键，覆盖本包每个导出方法。
var closedStateChecks = map[string]closedStateCheck{
	"Client.PublishOnce": {want: wantErrClosed, call: func(e *closedStateEnv) error { return e.asm.Client.PublishOnce("roost.closed", nil) }},
	"Client.RequestContext": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.asm.Client.RequestContext(e.ctx, "roost.closed", nil)
		return err
	}},
	"Client.MaxPayload":   {want: wantErrClosed, call: func(e *closedStateEnv) error { _, err := e.asm.Client.MaxPayload(); return err }},
	"Client.FlushContext": {want: wantErrClosed, call: func(e *closedStateEnv) error { return e.asm.Client.FlushContext(e.ctx) }},
	"Client.SubscribeBounded": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.asm.Client.SubscribeBounded("roost.closed", fnats.PendingLimits{Messages: 1, Bytes: 1024}, closedStateHandler)
		return err
	}},
	"rawSubscription.Unsubscribe":  {want: wantErrClosed, call: func(e *closedStateEnv) error { return (&rawSubscription{subscription: e.sub}).Unsubscribe() }},
	"rawSubscription.DrainContext": {want: notConnectionCall, reason: "句柄等待已接纳回调实际结束，即使共享连接已关闭；真实 raw 排空回归另验 Close/超时/重试"},
	"Client.Publish": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		return e.asm.Client.Publish("roost.closed", []byte("x"))
	}},
	"Client.Request": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.asm.Client.Request("roost.closed", nil, time.Second)
		return err
	}},
	"Client.Subscribe": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.asm.Client.Subscribe("roost.closed", closedStateHandler)
		return err
	}},
	"Client.QueueSubscribe": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.asm.Client.QueueSubscribe("roost.closed", "q", closedStateHandler)
		return err
	}},
	"Client.Drain": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		return e.asm.Client.Drain()
	}},
	"Client.DrainWithContext": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		return e.asm.Client.DrainWithContext(e.ctx)
	}},
	"Client.Close": {want: wantNil, closes: true, call: func(e *closedStateEnv) error {
		e.asm.Client.Close()
		return nil
	}},
	"Client.Connected": {want: wantNil, call: func(e *closedStateEnv) error {
		return reportedTrue(e.asm.Client.Connected(), "Connected")
	}},
	"Assembly.Close": {want: wantNil, closes: true, call: func(e *closedStateEnv) error {
		return e.asm.Close(e.ctx)
	}},
	"Assembly.Connected": {want: wantNil, call: func(e *closedStateEnv) error {
		return reportedTrue(e.asm.Connected(), "Connected")
	}},
	"JetStreamClient.EnsureStream": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		return e.js.EnsureStream(e.ctx, fnats.JetStreamConfig{Name: "ROOST_CLOSED", Subjects: []string{"roost.closed.js"}})
	}},
	"JetStreamClient.Publish": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.js.Publish(e.ctx, "roost.closed.js", nil, fnats.JetStreamPublishOptions{})
		return err
	}},
	"JetStreamClient.Subscribe": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.js.Subscribe(e.ctx, fnats.JetStreamConsumerConfig{Stream: "ROOST_CLOSED", Durable: "closed"},
			func(context.Context, *fnats.JetStreamMsg) error { return nil })
		return err
	}},
	"RPCClient.Call": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.asm.RPC.Call(e.ctx, "roost.closed", nil)
		return err
	}},
	"RPCClient.CallWithTimeout": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		_, err := e.asm.RPC.CallWithTimeout("roost.closed", nil, time.Second)
		return err
	}},
	"RPCClient.CallAsync": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		return callAsyncResult(e.asm.RPC)
	}},
	"RPCClient.Reply": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		return e.asm.RPC.Reply("roost.closed.reply", nil)
	}},
	"RPCClient.Stop": {want: wantNil, closes: true, call: func(e *closedStateEnv) error {
		e.asm.RPC.Stop()
		return nil
	}},
	"RPCClient.StopWithContext": {want: wantNil, closes: true, call: func(e *closedStateEnv) error {
		return e.asm.RPC.StopWithContext(e.ctx)
	}},
	"subscription.Unsubscribe": {want: wantErrClosed, call: func(e *closedStateEnv) error {
		return e.sub.Unsubscribe()
	}},
	"subscription.IsValid": {want: wantNil, call: func(e *closedStateEnv) error {
		return reportedTrue(e.sub.IsValid(), "IsValid")
	}},
	"jetStreamSubscription.Stop": {want: notConnectionCall,
		reason: "JetStream 消费句柄的释放，没有错误返回；生命周期是 nats.go 的 ConsumeContext（jetstream_test.go）"},
	"jetStreamSubscription.Drain": {want: notConnectionCall,
		reason: "同 Stop：消费句柄的排空，没有错误返回"},
	"jetStreamSubscription.Closed": {want: notConnectionCall,
		reason: "同 Stop：消费句柄结束的通知通道"},
	"rpcTask.OnRelease": {want: notConnectionCall,
		reason: "worker 池的所有权释放协议（回调恰好一次），不是连接上的调用"},
}

func reportedTrue(v bool, name string) error {
	if v {
		return errors.New(name + "() = true after Close")
	}
	return nil
}

// driverExportedMethods 解析本包非测试源码，返回每个导出方法的“接收者类型.方法”。
func driverExportedMethods(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var methods []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			recv := fn.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if index, ok := recv.(*ast.IndexExpr); ok {
				recv = index.X
			}
			ident, ok := recv.(*ast.Ident)
			if !ok {
				t.Fatalf("%s: unexpected receiver %T on %s", name, recv, fn.Name.Name)
			}
			methods = append(methods, ident.Name+"."+fn.Name.Name)
		}
	}
	sort.Strings(methods)
	return methods
}

func TestEveryExportedDriverMethodHasAClosedStateCheck(t *testing.T) {
	methods := driverExportedMethods(t)
	if len(methods) < 20 {
		t.Fatalf("found only %d exported methods (%v); the source scan is broken", len(methods), methods)
	}
	listed := map[string]bool{}
	for _, method := range methods {
		listed[method] = true
		check, ok := closedStateChecks[method]
		if !ok {
			t.Errorf("exported method %s has no entry in closedStateChecks: say what it returns after Close", method)
			continue
		}
		if check.want == notConnectionCall && check.reason == "" {
			t.Errorf("%s is exempted without a reason", method)
		}
		if check.want != notConnectionCall && check.call == nil {
			t.Errorf("%s has no call", method)
		}
	}
	for method := range closedStateChecks {
		if !listed[method] {
			t.Errorf("closedStateChecks lists %s, which is not an exported method any more", method)
		}
	}
}

// newClosedStateEnv 建一套驱动对象并按 closeWith 关闭。订阅句柄在关闭前创建。
func newClosedStateEnv(t *testing.T, closeWith func(*Assembly)) *closedStateEnv {
	t.Helper()
	a := closeContractAssembly(t)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	js, err := NewJetStreamClient(a.Client)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := a.Client.Subscribe("roost.closed.sub", closedStateHandler)
	if err != nil {
		t.Fatal(err)
	}
	closeWith(a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return &closedStateEnv{ctx: ctx, cancel: cancel, asm: a, js: js, sub: sub.(*subscription)}
}

func TestEveryExportedDriverMethodReportsErrClosedAfterClose(t *testing.T) {
	closers := map[string]func(*Assembly){
		"Assembly.Close": func(a *Assembly) { _ = a.Close(context.Background()) }, // 不可达地址：ErrClosedUndrained
		"Client.Close":   func(a *Assembly) { a.Client.Close() },                  // 只关连接，RPC 没停
	}
	for closerName, closeWith := range closers {
		t.Run(closerName, func(t *testing.T) {
			checkEveryMethodAfterClose(t, newClosedStateEnv(t, closeWith), closerName, nil)
		})
	}
}

// checkEveryMethodAfterClose 对已关闭的 env 逐个调用表里的方法并核对期望；每个调用 500ms 内返回。关闭类
// 方法放在最后（Client.Close 会再关一次 nats.go 连接，改变 nats.go 的状态）；beforeClosers 不为 nil 时
// 在它们之前调用。
func checkEveryMethodAfterClose(t *testing.T, env *closedStateEnv, closerName string, beforeClosers func()) {
	t.Helper()
	var ordinary, closers []string
	for _, method := range driverExportedMethods(t) {
		check, ok := closedStateChecks[method]
		switch {
		case !ok || check.want == notConnectionCall:
			// 缺表项由 TestEveryExportedDriverMethodHasAClosedStateCheck 报
		case check.closes:
			closers = append(closers, method)
		default:
			ordinary = append(ordinary, method)
		}
	}
	run := func(method string) {
		check := closedStateChecks[method]
		started := time.Now()
		err := check.call(env)
		elapsed := time.Since(started)
		switch check.want {
		case wantErrClosed:
			if !errors.Is(err, fnats.ErrClosed) {
				t.Errorf("%s after %s = %v, want an error that errors.Is fnats.ErrClosed", method, closerName, err)
			}
		case wantNil:
			if err != nil {
				t.Errorf("%s after %s = %v, want nil / false", method, closerName, err)
			}
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("%s after %s took %s; a closed driver must answer from its own state", method, closerName, elapsed)
		}
	}
	for _, method := range ordinary {
		run(method)
	}
	if beforeClosers != nil {
		beforeClosers()
	}
	for _, method := range closers {
		run(method)
	}
}

// 屏障：调用通过入口检查之后、碰 nats.go 之前插入 Close。这次调用不可能在 Close 之前完成，结果必须是
// fnats.ErrClosed——由驱动的状态判定，不靠 nats.go 返回哪种错误。“ctx ended”一组在 Close 之后再让
// 调用的 ctx 结束：nats.go 对已结束的 ctx 先返回 ctx.Err()、不碰连接，只看 nats.go 的错误会得到
// ErrCancelled / context.Canceled 这样的第三种结果。
func TestACallAdmittedBeforeCloseFinishingAfterItReportsErrClosed(t *testing.T) {
	type admittedCall struct {
		name string
		do   func(*closedStateEnv) error
	}
	var calls []admittedCall
	for _, method := range driverExportedMethods(t) {
		if check, ok := closedStateChecks[method]; ok && check.want == wantErrClosed {
			calls = append(calls, admittedCall{method, check.call})
		}
	}
	for _, ctxEnds := range []bool{false, true} {
		for _, c := range calls {
			name := c.name + "/ctx live"
			if ctxEnds {
				name = c.name + "/ctx ended"
			}
			t.Run(name, func(t *testing.T) {
				admitted, release := make(chan struct{}), make(chan struct{})
				var once, releaseOnce sync.Once
				releaseCall := func() { releaseOnce.Do(func() { close(release) }) }
				env := newClosedStateEnv(t, func(*Assembly) {}) // 先不关
				t.Cleanup(releaseCall)                          // 先于 env 的清理（Assembly.Close 也过入口）
				env.asm.Client.testAfterAdmit = func() {
					once.Do(func() {
						close(admitted)
						<-release
					})
				}
				result := make(chan error, 1)
				go func() { result <- c.do(env) }()
				select {
				case <-admitted:
				case err := <-result:
					t.Fatalf("returned %v without passing the admission check", err)
				case <-time.After(2 * time.Second):
					t.Fatal("never reached the admission check")
				}
				env.asm.Client.Close()
				if ctxEnds {
					env.cancel()
				}
				releaseCall()
				select {
				case err := <-result:
					if !errors.Is(err, fnats.ErrClosed) {
						t.Fatalf("admitted before Close, finished after it = %v, want fnats.ErrClosed", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("did not return within 2s after Close")
				}
			})
		}
	}
}

// 在途的 CallAsync 只关了连接、没停 RPC：收件箱随连接关闭，5s 超时按 fnats.ErrClosed 终结。
func TestInFlightCallAsyncExpiresAsErrClosedAfterClientClose(t *testing.T) {
	a := closeContractAssembly(t)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	done := make(chan error, 1)
	a.RPC.CallAsync("roost.closed", nil, func(_ []byte, err error) { done <- err })
	var sid int64
	a.RPC.pending.Range(func(key, _ any) bool { sid = key.(int64); return false })
	if sid == 0 {
		t.Fatal("CallAsync on a reconnecting connection did not register a pending call")
	}
	a.Client.Close()
	a.RPC.expirePending(sid) // 即 5s 超时的回调
	select {
	case err := <-done:
		if !errors.Is(err, fnats.ErrClosed) {
			t.Fatalf("in-flight CallAsync expiring after Client.Close = %v, want fnats.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback did not run")
	}
}

// 并发：Close 与调用同时进行，每个结果要么成功（在 Close 之前完成）要么 fnats.ErrClosed；Close 返回之后
// 发起的调用一律 fnats.ErrClosed。-race 下跑。
func TestCallsRacingCloseEitherCompleteOrReportErrClosed(t *testing.T) {
	for round := 0; round < 20; round++ {
		env := newClosedStateEnv(t, func(*Assembly) {})
		c := env.asm.Client
		ops := []func() error{
			func() error { return c.Publish("roost.race", []byte("x")) },
			func() error {
				sub, err := c.Subscribe("roost.race", closedStateHandler)
				if err != nil {
					return err
				}
				return sub.Unsubscribe()
			},
			func() error {
				sub, err := c.QueueSubscribe("roost.race", "q", closedStateHandler)
				if err != nil {
					return err
				}
				return sub.Unsubscribe()
			},
		}
		var closeReturned sync.Map
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 1024)
		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				for i := 0; i < 40; i++ {
					_, after := closeReturned.Load("done")
					err := ops[(g+i)%len(ops)]()
					switch {
					case after && !errors.Is(err, fnats.ErrClosed):
						errs <- errors.New("call issued after Close returned: " + errString(err))
					case err != nil && !errors.Is(err, fnats.ErrClosed):
						errs <- errors.New("third outcome while racing Close: " + err.Error())
					}
				}
			}(g)
		}
		close(start)
		time.Sleep(time.Duration(round%4) * 100 * time.Microsecond)
		c.Close()
		closeReturned.Store("done", true)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
