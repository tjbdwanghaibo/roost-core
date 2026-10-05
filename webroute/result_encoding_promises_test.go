// RR-20261005-NC-80：生成路由的形状（DecodeJSON → 业务 → WriteResult）经 Registrar、
// Engine 和真实回环 HTTP 由 httpclient 消费；业务已执行但结果不可编码时，调用方必须拿到 500，
// 不能是 nil 错误加零值。
package webroute_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/httpclient"
	"github.com/tjbdwanghaibo/roost-core/httpserver"
	"github.com/tjbdwanghaibo/roost-core/webroute"
)

type winRateRequest struct {
	Wins  int `json:"wins"`
	Games int `json:"games"`
}

type winRateResponse struct {
	WinRate float64 `json:"win_rate"`
}

func TestUnencodableRouteResultReachesTheClientAs500(t *testing.T) {
	var calls atomic.Int32
	engine := httpserver.NewEngine()
	if err := webroute.NewRegistrar(engine).Register(http.MethodPost, "/gm/win-rate", func(w http.ResponseWriter, r *http.Request) {
		var request winRateRequest
		if !webroute.DecodeJSON(w, r, &request) {
			return
		}
		calls.Add(1)
		response := winRateResponse{WinRate: float64(request.Wins) / float64(request.Games)}
		webroute.WriteResult(w, http.StatusOK, response, nil)
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(engine)
	defer server.Close()
	client := httpclient.New(httpclient.WithBaseURL(server.URL))

	var ok winRateResponse
	if err := client.PostJSON(context.Background(), "/gm/win-rate", winRateRequest{Wins: 1, Games: 4}, &ok); err != nil || ok.WinRate != 0.25 {
		t.Fatalf("encodable result: err=%v out=%+v", err, ok)
	}
	var out winRateResponse
	err := client.PostJSON(context.Background(), "/gm/win-rate", winRateRequest{}, &out) // 0/0 = NaN
	var statusErr *httpclient.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("NaN result: err=%v out=%+v, want StatusError 500", err, out)
	}
	if calls.Load() != 2 {
		t.Fatalf("business ran %d times, want 2", calls.Load())
	}
}
