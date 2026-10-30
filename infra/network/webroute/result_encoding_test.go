package webroute_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/network/httpclient"
	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
)

// 生成路由的形状（DecodeJSON → 业务 → WriteResult）经 Registrar、Engine 和真实回环 HTTP 由 httpclient 消费：
// 业务已执行但结果不可编码时，调用方必须拿到 500，不能是 nil 错误加零值。

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

// compress=gzip 的路由（生成代码调 WriteCompressedResult）经 Registrar、Engine 与真实回环 HTTP：
//   - 客户端 Accept-Encoding 接受 gzip、成功响应编码后不小于 1 KiB：Content-Encoding: gzip，解压后就是 JSON；
//   - 不接受 gzip（没写、gzip;q=0、只接受别的编码）或响应太小：明文 JSON，没有 Content-Encoding；
//   - 错误响应一律明文；所有响应都带 Vary: Accept-Encoding，缓存不会把压缩版本交给不接受的客户端。
type rankResponse struct {
	Entries []string `json:"entries"`
}

func TestACompressedRouteNegotiatesGzipWithTheClient(t *testing.T) {
	big := rankResponse{Entries: make([]string, 200)}
	for i := range big.Entries {
		big.Entries[i] = "player-entry-with-some-padding"
	}
	engine := httpserver.NewEngine()
	register := func(path string, response any, err error) {
		if regErr := webroute.NewRegistrar(engine).Register(http.MethodGet, path, func(w http.ResponseWriter, r *http.Request) {
			webroute.WriteCompressedResult(w, r, http.StatusOK, response, err)
		}); regErr != nil {
			t.Fatal(regErr)
		}
	}
	register("/rank/big", big, nil)
	register("/rank/small", rankResponse{Entries: []string{"one"}}, nil)
	register("/rank/failed", nil, errors.New("database down"))
	server := httptest.NewServer(engine)
	defer server.Close()
	// 关掉 Transport 的自动解压，才能看到线上的真实编码。
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	get := func(path, acceptEncoding string) (*http.Response, []byte) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if acceptEncoding != "" {
			request.Header.Set("Accept-Encoding", acceptEncoding)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, body
	}
	wantVary := func(response *http.Response) {
		t.Helper()
		if got := response.Header.Values("Vary"); len(got) != 1 || got[0] != "Accept-Encoding" {
			t.Fatalf("Vary = %q, want exactly Accept-Encoding", got)
		}
	}
	decodeBig := func(body []byte) {
		t.Helper()
		var got rankResponse
		if err := json.Unmarshal(body, &got); err != nil || len(got.Entries) != len(big.Entries) {
			t.Fatalf("decoded %d entries, err %v", len(got.Entries), err)
		}
	}

	for _, accept := range []string{"gzip", "deflate, gzip;q=0.5", "*", "GZIP"} {
		response, body := get("/rank/big", accept)
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("Accept-Encoding %q: status %d Content-Encoding %q, want 200 gzip", accept, response.StatusCode, response.Header.Get("Content-Encoding"))
		}
		wantVary(response)
		if got := response.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("Content-Type = %q", got)
		}
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("Accept-Encoding %q: body is not gzip: %v", accept, err)
		}
		plain, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		decodeBig(plain)
		if len(body) >= len(plain) {
			t.Fatalf("gzip body %d bytes is not smaller than the %d-byte JSON", len(body), len(plain))
		}
	}
	for _, accept := range []string{"", "gzip;q=0", "br, deflate", "*;q=0", "gzip;q=0, *"} {
		response, body := get("/rank/big", accept)
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" {
			t.Fatalf("Accept-Encoding %q: status %d Content-Encoding %q, want 200 identity", accept, response.StatusCode, response.Header.Get("Content-Encoding"))
		}
		wantVary(response)
		decodeBig(body)
	}

	response, body := get("/rank/small", "gzip")
	if response.Header.Get("Content-Encoding") != "" || !strings.Contains(string(body), `"one"`) {
		t.Fatalf("a response under the threshold: Content-Encoding %q body %q, want plain JSON", response.Header.Get("Content-Encoding"), body)
	}
	wantVary(response)

	response, body = get("/rank/failed", "gzip")
	if response.StatusCode != http.StatusInternalServerError || response.Header.Get("Content-Encoding") != "" || !strings.Contains(string(body), `"server error"`) {
		t.Fatalf("error response: status %d Content-Encoding %q body %q, want plain 500", response.StatusCode, response.Header.Get("Content-Encoding"), body)
	}
	wantVary(response)
}
