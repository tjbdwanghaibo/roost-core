package httpserver

// RR-20261003-NC-04：JSON body 必须完整为单值，尾随垃圾不能触发业务；
// 修前只 Decode 一次，合法首值后的垃圾/第二个值也返回 200 并调用业务。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONRequestBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limit      int64
		status     int
		calls      int
	}{
		{"valid", `{"amount":1}`, 1024, 200, 1},
		{"trailing_whitespace", "{\"amount\":1}\n\t ", 1024, 200, 1},
		{"trailing_garbage", `{"amount":1}garbage`, 1024, 400, 0},
		{"second_json_value", `{"amount":1}{"amount":999}`, 1024, 400, 0},
		{"invalid_prefix", `garbage{"amount":1}`, 1024, 400, 0},
		{"oversize", `{"amount":1}`, 4, 413, 0},
		{"empty_current_contract", "", 1024, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			engine := NewEngine(WithMaxBodyBytes(tc.limit))
			engine.Post("/mutate", HandleJSON(func(_ context.Context, req struct {
				Amount int `json:"amount"`
			}) (int, error) {
				calls++
				return req.Amount, nil
			}))
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", strings.NewReader(tc.body)))
			t.Logf("status=%d business_calls=%d response=%q", rec.Code, calls, rec.Body.String())
			if rec.Code != tc.status || calls != tc.calls {
				t.Fatalf("status=%d calls=%d, want status=%d calls=%d", rec.Code, calls, tc.status, tc.calls)
			}
		})
	}
}
