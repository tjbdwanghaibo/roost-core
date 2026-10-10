package ops

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

type readyBody struct {
	OK                   bool             `json:"ok"`
	Degraded             bool             `json:"degraded"`
	DegradedDependencies []map[string]any `json:"degraded_dependencies"`
	Dependencies         []map[string]any `json:"dependencies"`
}

func readyWith(t *testing.T, ready bool, results map[string]health.Result) (int, readyBody) {
	t.Helper()
	m := New()
	m.health = health.NewRegistry()
	for name, result := range results {
		result := result
		m.health.Register(name, health.CheckerFunc(func(context.Context) health.Result { return result }))
	}
	m.SetReady(ready, "ready")
	rec := httptest.NewRecorder()
	m.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body readyBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode /readyz: %v", err)
	}
	return rec.Code, body
}

func TestReadyzTreatsDegradedAsReadyAndNamesTheDegradedChecker(t *testing.T) {
	code, body := readyWith(t, true, map[string]health.Result{
		"redis":     {Status: health.StatusOK},
		"singleton": {Status: health.StatusDegraded, Message: "renewal outcome unknown; window ends in 3s", Err: errors.New("i/o timeout")},
	})
	if code != http.StatusOK || !body.OK {
		t.Fatalf("/readyz with a degraded checker = %d ok=%v, want 200 ok=true: degraded still serves", code, body.OK)
	}
	if !body.Degraded || len(body.DegradedDependencies) != 1 {
		t.Fatalf("degraded=%v degraded_dependencies=%v, want the degraded checker named", body.Degraded, body.DegradedDependencies)
	}
	got := body.DegradedDependencies[0]
	if got["name"] != "singleton" || got["status"] != string(health.StatusDegraded) ||
		got["message"] != "renewal outcome unknown; window ends in 3s" || got["error"] != "i/o timeout" {
		t.Fatalf("degraded entry = %v, want name, status, message and error of the singleton checker", got)
	}
	if len(body.Dependencies) != 2 {
		t.Fatalf("dependencies = %v, want every checker listed", body.Dependencies)
	}
}

func TestReadyzStillFailsOnFailOrNotReady(t *testing.T) {
	cases := []struct {
		name    string
		ready   bool
		results map[string]health.Result
	}{
		{"fail beside degraded", true, map[string]health.Result{
			"dataengine": {Status: health.StatusDegraded, Message: "backlog"},
			"redis":      {Status: health.StatusFail, Message: "ping failed"},
		}},
		{"unknown status counts as fail", true, map[string]health.Result{"custom": {Status: "warn"}}},
		{"not ready with only degraded", false, map[string]health.Result{"entitysync": {Status: health.StatusDegraded}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := readyWith(t, tc.ready, tc.results)
			if code != http.StatusServiceUnavailable || body.OK {
				t.Fatalf("/readyz = %d ok=%v, want 503 ok=false", code, body.OK)
			}
		})
	}
	code, body := readyWith(t, true, map[string]health.Result{"redis": {Status: health.StatusOK}})
	if code != http.StatusOK || !body.OK || body.Degraded || len(body.DegradedDependencies) != 0 {
		t.Fatalf("all OK: %d %+v, want 200 ok without degraded entries", code, body)
	}
}
