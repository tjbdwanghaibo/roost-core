package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
)

// 维护者第十二轮决定（readyz checker 期限，N01b 观察 O-H1）：/readyz 给每个 checker 一个短期限，
// 不配合 ctx、卡住不返回的 checker 报 Fail 并写明原因，不能拖住整个 /readyz。旧实现用请求 ctx 串行
// 调用全部 checker：一个不返回的 checker 让 handler 永远挂着，k8s 探针超时后每次探针再留下一个卡住的
// handler，Ops 停机还要等它们。卡住的 checker 同一时刻只有一次在跑：后来的探针等同一次调用，
// 不再每次新开一个。
func TestReadyzReportsAStuckCheckerAsFailInsteadOfHanging(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	m := New()
	m.health = health.NewRegistry()
	m.health.Register("stuck", health.CheckerFunc(func(context.Context) health.Result {
		calls.Add(1)
		<-release // 不看 ctx：只有测试结束才返回
		return health.Result{Status: health.StatusOK}
	}))
	m.health.Register("redis", health.CheckerFunc(func(context.Context) health.Result {
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))
	m.SetReady(true, "ready")

	probe := func() (int, readyBody) {
		t.Helper()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rec := httptest.NewRecorder()
			m.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			done <- rec
		}()
		select {
		case rec := <-done:
			var body readyBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode /readyz: %v", err)
			}
			return rec.Code, body
		case <-time.After(health.DefaultCheckTimeout + 2*time.Second):
			t.Fatalf("/readyz did not answer within %s: one checker that never returns hangs the whole probe", health.DefaultCheckTimeout+2*time.Second)
			return 0, readyBody{}
		}
	}

	for round := 1; round <= 2; round++ {
		started := time.Now()
		code, body := probe()
		if elapsed := time.Since(started); elapsed > health.DefaultCheckTimeout+time.Second {
			t.Fatalf("round %d: /readyz took %s, want about the per-checker deadline %s", round, elapsed, health.DefaultCheckTimeout)
		}
		if code != http.StatusServiceUnavailable || body.OK {
			t.Fatalf("round %d: /readyz with a stuck checker = %d ok=%v, want 503", round, code, body.OK)
		}
		var stuck, redis map[string]any
		for _, dep := range body.Dependencies {
			switch dep["name"] {
			case "stuck":
				stuck = dep
			case "redis":
				redis = dep
			}
		}
		if stuck == nil || stuck["status"] != string(health.StatusFail) || !strings.Contains(stuck["error"].(string), "did not return within") {
			t.Fatalf("round %d: stuck checker entry = %v, want status fail with the deadline as the reason", round, stuck)
		}
		if redis == nil || redis["status"] != string(health.StatusOK) {
			t.Fatalf("round %d: the healthy checker = %v, want it still reported ok next to the stuck one", round, redis)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the stuck checker was started %d times across two probes, want 1: a second probe joins the call still in flight", got)
	}
}
