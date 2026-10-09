package httpclient

// RR-20261003-NC-02/03：Clone 的超时必须影响实际请求 deadline 且不改父实例；
// 非 2xx 的坏/异型响应必须保留 StatusError，成功坏 JSON 仍返回解码错误。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type boundaryDeadlineTransport struct {
	remaining time.Duration
	headers   http.Header
}

func (rt *boundaryDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	if ok {
		rt.remaining = time.Until(deadline)
	}
	rt.headers = req.Header.Clone()
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"ok"}`)), Request: req}, nil
}

// Observe the deadline net/http actually delivers to a transport. No sleeps and
// no inspection of Client.timeout or http.Client.Timeout are needed.
func TestClientDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name      string
		clone     bool
		requested time.Duration
		want      time.Duration
	}{
		{"base", false, 300 * time.Millisecond, 300 * time.Millisecond},
		{"clone_inherit", true, 0, 300 * time.Millisecond},
		{"new_short_timeout", false, 20 * time.Millisecond, 20 * time.Millisecond},
		{"clone_short_timeout", true, 20 * time.Millisecond, 20 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &boundaryDeadlineTransport{}
			timeout := tc.requested
			if tc.clone {
				timeout = 300 * time.Millisecond
			}
			c := New(WithBaseURL("http://review.invalid"), WithTimeout(timeout))
			c.client.Transport = rt // retain the library-created http.Client and its timeout
			if tc.clone {
				c = c.Clone(WithTimeout(tc.requested))
			}
			var out struct {
				Message string `json:"message"`
			}
			if err := c.PostJSON(context.Background(), "/test", nil, &out); err != nil {
				t.Fatal(err)
			}
			t.Logf("requested=%s transport_deadline_remaining=%s", tc.want, rt.remaining)
			if rt.remaining <= 0 || rt.remaining > tc.want+5*time.Millisecond || rt.remaining < tc.want-15*time.Millisecond {
				t.Fatalf("transport deadline=%s, want approximately %s", rt.remaining, tc.want)
			}
		})
	}
}

func TestCloneHeaderIsolation(t *testing.T) {
	rt := &boundaryDeadlineTransport{}
	base := New(WithBaseURL("http://review.invalid"), WithHeader("X-Test", "base"))
	base.client.Transport = rt
	clone := base.Clone(WithHeader("X-Test", "clone"))
	if err := clone.PostJSON(context.Background(), "/test", nil, nil); err != nil {
		t.Fatal(err)
	}
	if rt.headers.Get("X-Test") != "clone" {
		t.Fatal(rt.headers)
	}
	if err := base.PostJSON(context.Background(), "/test", nil, nil); err != nil {
		t.Fatal(err)
	}
	if rt.headers.Get("X-Test") != "base" {
		t.Fatal("clone changed parent headers")
	}
}

func TestStatusErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		nilOut     bool
		wantStatus bool
		wantDecode bool
	}{
		{"success", 200, `{"message":"ok"}`, false, false, false},
		{"plaintext_503", 503, "upstream unavailable", false, true, false},
		{"incompatible_json_401", 401, `{"message":123}`, false, true, false},
		{"compatible_json_502", 502, `{"message":"failed"}`, false, true, false},
		{"plaintext_503_without_out", 503, "upstream unavailable", true, true, false},
		{"bad_json_success", 200, "bad JSON", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			c := New(WithBaseURL(server.URL), WithTimeout(time.Second))
			var out struct {
				Message string `json:"message"`
			}
			var target any = &out
			if tc.nilOut {
				target = nil
			}
			err := c.PostJSON(context.Background(), "/test", nil, target)
			var statusErr *StatusError
			classified := errors.As(err, &statusErr)
			t.Logf("http_status=%d error_type=%T classified_status=%v", tc.status, err, classified)
			if classified != tc.wantStatus {
				t.Fatalf("error=%v (%T), StatusError=%v, want %v", err, err, classified, tc.wantStatus)
			}
			if tc.wantStatus && (statusErr.StatusCode != tc.status || string(statusErr.Body) != tc.body) {
				t.Fatalf("lost status/body: %+v", statusErr)
			}
			if tc.wantDecode && err == nil {
				t.Fatal("invalid success JSON accepted")
			}
			if !tc.wantStatus && !tc.wantDecode && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloneTimeoutPreservesParentAndCustomClient(t *testing.T) {
	rt := &boundaryDeadlineTransport{}
	base := New(WithBaseURL("http://review.invalid"), WithTimeout(300*time.Millisecond))
	base.client.Transport = rt
	short := base.Clone(WithTimeout(20 * time.Millisecond))
	longer := short.Clone(WithTimeout(80 * time.Millisecond))
	custom := &http.Client{Transport: rt, Timeout: 150 * time.Millisecond}
	var none *Client
	for _, tc := range []struct {
		name   string
		client *Client
		want   time.Duration
	}{
		{"short", short, 20 * time.Millisecond},
		{"longer_child", longer, 80 * time.Millisecond},
		{"short_parent_unchanged", short, 20 * time.Millisecond},
		{"base_unchanged", base, 300 * time.Millisecond},
		{"invalid_timeout_ignored", short.Clone(WithTimeout(-1)), 20 * time.Millisecond},
		{"custom_new", New(WithHTTPClient(custom), WithTimeout(20*time.Millisecond)), 150 * time.Millisecond},
		{"custom_clone", New(WithHTTPClient(custom)).Clone(WithTimeout(20 * time.Millisecond)), 150 * time.Millisecond},
		{"custom_replacement", base.Clone(WithTimeout(20*time.Millisecond), WithHTTPClient(custom)), 150 * time.Millisecond},
		{"custom_option_order", base.Clone(WithHTTPClient(custom), WithTimeout(20*time.Millisecond)), 150 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.client.PostJSON(context.Background(), "http://review.invalid/test", nil, nil); err != nil {
				t.Fatal(err)
			}
			if rt.remaining <= 0 || rt.remaining > tc.want+5*time.Millisecond || rt.remaining < tc.want-15*time.Millisecond {
				t.Fatalf("deadline=%s want approximately %s", rt.remaining, tc.want)
			}
		})
	}
	t.Run("nil_clone", func(t *testing.T) {
		client := none.Clone(WithTimeout(20 * time.Millisecond))
		client.client.Transport = rt
		if err := client.PostJSON(context.Background(), "http://review.invalid/test", nil, nil); err != nil {
			t.Fatal(err)
		}
		if rt.remaining <= 0 || rt.remaining > 25*time.Millisecond {
			t.Fatalf("deadline=%s", rt.remaining)
		}
	})
	t.Run("context_deadline_wins", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := base.PostJSON(ctx, "http://review.invalid/test", nil, nil); err != nil {
			t.Fatal(err)
		}
		if rt.remaining <= 0 || rt.remaining > 15*time.Millisecond {
			t.Fatalf("deadline=%s", rt.remaining)
		}
	})
}

type boundaryConcurrentTransport struct{ observations chan time.Duration }

func (rt *boundaryConcurrentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, _ := req.Context().Deadline()
	rt.observations <- time.Until(deadline)
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func TestCloneTimeoutConcurrentRequestsKeepParentDeadline(t *testing.T) {
	rt := &boundaryConcurrentTransport{observations: make(chan time.Duration, 17)}
	base := New(WithBaseURL("http://review.invalid"), WithTimeout(300*time.Millisecond))
	base.client.Transport = rt
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := base.Clone(WithTimeout(20*time.Millisecond)).PostJSON(context.Background(), "/test", nil, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for range 16 {
		if deadline := <-rt.observations; deadline <= 0 || deadline > 25*time.Millisecond {
			t.Fatalf("clone deadline=%s", deadline)
		}
	}
	if err := base.PostJSON(context.Background(), "/test", nil, nil); err != nil {
		t.Fatal(err)
	}
	if deadline := <-rt.observations; deadline < 250*time.Millisecond || deadline > 305*time.Millisecond {
		t.Fatalf("parent deadline changed: %s", deadline)
	}
}

type boundaryReadErrorBody struct {
	closed  bool
	failure error
}

func (b *boundaryReadErrorBody) Read(dst []byte) (int, error) {
	return copy(dst, "partial error body"), b.failure
}
func (b *boundaryReadErrorBody) Close() error { b.closed = true; return nil }

type boundaryResponseTransport struct {
	status int
	body   io.ReadCloser
}

func (rt boundaryResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: rt.status, Status: http.StatusText(rt.status), Header: make(http.Header), Body: rt.body, Request: req}, nil
}

func TestStatusErrorPreservesReadAndDecodeFailures(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			readFailure := errors.New("response body interrupted")
			body := &boundaryReadErrorBody{failure: readFailure}
			client := New(WithHTTPClient(&http.Client{Transport: boundaryResponseTransport{status: status, body: body}}))
			err := client.PostJSON(context.Background(), "http://review.invalid/test", nil, nil)
			if !errors.Is(err, readFailure) || !body.closed {
				t.Fatalf("lost read error or leaked response: err=%v closed=%v", err, body.closed)
			}
			var statusErr *StatusError
			if errors.As(err, &statusErr) != (status == 503) {
				t.Fatalf("unexpected status classification: %v", err)
			}
			if status == 503 && (statusErr.StatusCode != status || string(statusErr.Body) != "partial error body") {
				t.Fatalf("lost partial response: %+v", statusErr)
			}
		})
	}
	t.Run("decode_failure_still_inspectable", func(t *testing.T) {
		client := New(WithHTTPClient(&http.Client{Transport: boundaryResponseTransport{status: 503, body: io.NopCloser(strings.NewReader("not JSON"))}}))
		var out clientResponse
		err := client.PostJSON(context.Background(), "http://review.invalid/test", nil, &out)
		var statusErr *StatusError
		var syntaxErr *json.SyntaxError
		if !errors.As(err, &statusErr) || !errors.As(err, &syntaxErr) {
			t.Fatalf("lost status or decoder cause: %v", err)
		}
	})
}
