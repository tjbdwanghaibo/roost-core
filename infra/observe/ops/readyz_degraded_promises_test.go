package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
)

// 维护者决定 D1（2026-10-06 第五轮）：Degraded 算就绪。/readyz 在就绪位为真、没有 checker 为
// Fail 时返回 200，`ok` 为 true；有 Degraded 时响应体用 `degraded: true` 和
// `degraded_dependencies`（名字、原因）标出降级项，`dependencies` 照样列出全部结果。只有 Fail
// 或就绪位为假返回 503。之前 Degraded 与 Fail 一样返回 503（health.Snapshot 非 OK 即失败），
// 单副本服务被摘掉唯一的 endpoint；四个 Degraded 来源（单实例锁续期结果未知、entitysync ≥ 80%
// 容量、remoteentity 写许可用满、DataEngine 积压告警）都是“还能服务、需要关注”。

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
