// 生成的路由经 Registrar、Engine 与真实回环 HTTP 跑一遍（不由 `go test ./...` 编译：testdata 对 go 工具不可见，
// ../webroute_test.go 的 TestGeneratedRoutesRunAgainstTheRuntime 把它与 fixture、生成物放进一次性模块运行）。
package routes

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/network/httpserver"
	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
)

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	engine := httpserver.NewEngine()
	if err := RegisterRoutes(webroute.NewRegistrar(engine), &Service{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	return server
}

// 关掉 Transport 的自动解压，才能看到线上的真实编码。
var client = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func do(t *testing.T, request *http.Request) (*http.Response, []byte) {
	t.Helper()
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

// headers=：头名不区分大小写；同名多值取第一个；请求头覆盖 JSON 体里的同名字段；头缺失时字段是空串（JSON 体填不进来）；
// 没有映射的字段照常来自 JSON 体。
func TestGeneratedRouteBindsRequestHeaders(t *testing.T) {
	server := serve(t)
	post := func(body string, header http.Header) KeepAliveResponse {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+"/player/keepalive", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for name, values := range header {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
		response, raw := do(t, request)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d body %s", response.StatusCode, raw)
		}
		var out KeepAliveResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := post(`{"note":"hi","sid":"from-body"}`, http.Header{
		"X-Player-Id":   {"42", "43"},
		"x-game-sid":    {"7"},
		"Authorization": {"Bearer token"},
	})
	want := KeepAliveResponse{PlayerID: "42", SID: "7", Token: "Bearer token", Note: "hi"}
	if got != want {
		t.Fatalf("bound = %+v, want %+v", got, want)
	}

	got = post(`{"note":"hi","sid":"from-body"}`, nil)
	if got != (KeepAliveResponse{Note: "hi"}) {
		t.Fatalf("missing headers: %+v, want only the JSON note and empty header fields", got)
	}

	// 空请求体（DecodeJSON 接受）也照样绑定请求头。
	got = post("", http.Header{"X-Game-Sid": {"9"}})
	if got != (KeepAliveResponse{SID: "9"}) {
		t.Fatalf("empty body: %+v", got)
	}
}

// compress=gzip + RawRequest.Query：接受 gzip 的客户端拿到压缩体，不接受的拿到明文；两者都带 Vary；查询参数可读、多值保留。
func TestGeneratedRouteNegotiatesGzipAndReadsTheQuery(t *testing.T) {
	server := serve(t)
	get := func(acceptEncoding string) (*http.Response, RankResponse) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+"/rank/info?season=s3&part=head&part=tail", nil)
		if err != nil {
			t.Fatal(err)
		}
		if acceptEncoding != "" {
			request.Header.Set("Accept-Encoding", acceptEncoding)
		}
		response, raw := do(t, request)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d", response.StatusCode)
		}
		if response.Header.Get("Content-Encoding") == "gzip" {
			reader, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if raw, err = io.ReadAll(reader); err != nil {
				t.Fatal(err)
			}
		}
		var out RankResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("body is not JSON (Content-Encoding %q): %v", response.Header.Get("Content-Encoding"), err)
		}
		return response, out
	}
	check := func(response *http.Response, out RankResponse, wantEncoding string) {
		t.Helper()
		if got := response.Header.Get("Content-Encoding"); got != wantEncoding {
			t.Fatalf("Content-Encoding = %q, want %q", got, wantEncoding)
		}
		if got := response.Header.Get("Vary"); got != "Accept-Encoding" {
			t.Fatalf("Vary = %q", got)
		}
		if out.Season != "s3" || len(out.Parts) != 2 || out.Parts[0] != "head" || out.Parts[1] != "tail" || len(out.Entries) != 100 {
			t.Fatalf("response = season %q parts %q entries %d", out.Season, out.Parts, len(out.Entries))
		}
	}
	response, out := get("gzip, deflate")
	check(response, out, "gzip")
	response, out = get("")
	check(response, out, "")
	response, out = get("gzip;q=0")
	check(response, out, "")
}

// 没写 compress= 的路由照旧：即使客户端接受 gzip 也回明文、不加 Vary。
func TestAPlainGeneratedRouteIsNotCompressed(t *testing.T) {
	server := serve(t)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/echo", strings.NewReader(`{"text":"`+strings.Repeat("x", 4096)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	response, raw := do(t, request)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Vary") != "" {
		t.Fatalf("status %d Content-Encoding %q Vary %q", response.StatusCode, response.Header.Get("Content-Encoding"), response.Header.Get("Vary"))
	}
	var out EchoResponse
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Text) != 4096 {
		t.Fatalf("echo = %d bytes, err %v", len(out.Text), err)
	}
}
