// RR-20261005-NC-80 / NC-81：响应编码失败与响应开始后的 panic 不能被客户端看成成功。
package httpserver

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) { return nil, errors.New("cannot marshal") }

type panickingMarshaler struct{}

func (panickingMarshaler) MarshalJSON() ([]byte, error) { panic("marshal panic") }

func getOver(t *testing.T, handler http.HandlerFunc) (*http.Response, []byte, error) {
	t.Helper()
	engine := NewEngine()
	engine.Get("/x", handler)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	response, err := http.Get(server.URL + "/x")
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	return response, body, readErr
}

func TestJSONEncodeFailureAnswers500InsteadOfAnEmptySuccess(t *testing.T) {
	for name, value := range map[string]any{
		"nan":           map[string]any{"ratio": math.NaN()},
		"inf":           []float64{math.Inf(-1)},
		"channel":       map[string]any{"c": make(chan int)},
		"marshal_error": failingMarshaler{},
		"marshal_panic": panickingMarshaler{},
	} {
		t.Run(name, func(t *testing.T) {
			response, body, err := getOver(t, func(w http.ResponseWriter, _ *http.Request) { JSON(w, http.StatusCreated, value) })
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusInternalServerError {
				t.Fatalf("status %d body %q, want 500", response.StatusCode, body)
			}
			if !strings.Contains(response.Header.Get("Content-Type"), "application/json") || !strings.Contains(string(body), `"ok":false`) {
				t.Fatalf("content-type %q body %q", response.Header.Get("Content-Type"), body)
			}
		})
	}
}

func TestJSONSuccessKeepsTheEncoderByteShape(t *testing.T) {
	recorder := httptest.NewRecorder()
	JSON(recorder, http.StatusAccepted, map[string]any{"html": "<a&b>", "n": 1})
	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "{\"html\":\"\\u003ca\\u0026b\\u003e\",\"n\":1}\n" {
		t.Fatalf("status %d body %q", recorder.Code, recorder.Body.String())
	}
}

func TestPanicAfterTheResponseStartedAbortsTheConnection(t *testing.T) {
	for name, start := range map[string]func(http.ResponseWriter){
		"write":        func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"items":[1`)) },
		"write_header": func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
		"flush":        func(w http.ResponseWriter) { w.(http.Flusher).Flush() },
	} {
		t.Run(name, func(t *testing.T) {
			response, body, err := getOver(t, func(w http.ResponseWriter, _ *http.Request) {
				start(w)
				w.(http.Flusher).Flush()
				panic("after start")
			})
			if err == nil {
				t.Fatalf("a panic after the response started produced a complete response: status %d body %q", response.StatusCode, body)
			}
		})
	}
}

func TestErrAbortHandlerIsPropagatedNotAnswered(t *testing.T) {
	response, body, err := getOver(t, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	if err == nil {
		t.Fatalf("ErrAbortHandler answered status %d body %q", response.StatusCode, body)
	}
}

func TestEarlyHintsDoNotCountAsAStartedResponse(t *testing.T) {
	response, body, err := getOver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		panic("after early hints")
	})
	if err != nil || response.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "internal server error") {
		t.Fatalf("status %v body %q err %v", response, body, err)
	}
}

// 包装 writer 后，Hijack（如 websocket 升级）、Flush 与 ResponseController 仍可用。
func TestTrackedWriterKeepsHijackFlushAndResponseController(t *testing.T) {
	engine := NewEngine()
	engine.Get("/controller", func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Errorf("SetWriteDeadline through the wrapper: %v", err)
		}
		_, _ = io.WriteString(w, "ok")
	})
	engine.Get("/hijack", func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("wrapped writer lost http.Hijacker")
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		_ = buffered.Flush()
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	for path, want := range map[string]string{"/controller": "ok", "/hijack": "hijacked"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(body) != want {
			t.Fatalf("%s: body %q", path, body)
		}
	}
	// 原 writer 不是 Hijacker 时，包装后也不能声称支持。
	tracked, _ := trackResponse(httptest.NewRecorder())
	if _, ok := tracked.(http.Hijacker); ok {
		t.Fatal("wrapper claimed http.Hijacker for a writer without it")
	}
	if _, ok := tracked.(http.Flusher); !ok {
		t.Fatal("wrapper lost http.Flusher")
	}
}

func TestPanicAfterHijackWritesNothingMore(t *testing.T) {
	engine := NewEngine()
	engine.Get("/h", func(w http.ResponseWriter, _ *http.Request) {
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
		_ = buffered.Flush()
		_ = connection.Close()
		panic("after hijack")
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	response, err := http.Get(server.URL + "/h")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "hi" {
		t.Fatalf("status %d body %q", response.StatusCode, body)
	}
}
