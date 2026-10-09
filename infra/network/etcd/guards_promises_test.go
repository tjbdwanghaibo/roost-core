package etcd

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// plainMirror is an ILocalMirror that does not implement ILocalMirrorSubscriber.
type plainMirror struct{ ILocalMirror[string] }

// U-0147 · C2 · nightly gap map core `etcd` 4/4：nil（含类型化 nil）或不支持订阅的镜像不能
// 订阅；WatchCallback 拒绝 nil watcher（含类型化 nil）与 nil 处理器。
func TestSubscribeAndWatchCallbackRefuseMissingParts(t *testing.T) {
	ctx := context.Background()
	handler := func(context.Context, LocalMirrorChange[string]) error { return nil }
	if _, err := SubscribeLocalMirror[string](nil, ctx, handler, LocalMirrorSubscribeOptions{}); !errors.Is(err, ErrMirrorSubscribeUnsupported) {
		t.Fatalf("SubscribeLocalMirror(nil) = %v", err)
	}
	var typedNil *plainMirror
	if _, err := SubscribeLocalMirror[string](typedNil, ctx, handler, LocalMirrorSubscribeOptions{}); !errors.Is(err, ErrMirrorSubscribeUnsupported) {
		t.Fatalf("SubscribeLocalMirror(typed nil) = %v", err)
	}
	if _, err := SubscribeLocalMirror[string](plainMirror{}, ctx, handler, LocalMirrorSubscribeOptions{}); !errors.Is(err, ErrMirrorSubscribeUnsupported) {
		t.Fatalf("SubscribeLocalMirror(non-subscriber) = %v", err)
	}
	watch := func(context.Context, *WatchEvent) error { return nil }
	if _, err := WatchCallback(ctx, nil, watch); !errors.Is(err, ErrWatchInvalidCallback) || !strings.Contains(err.Error(), "watcher is nil") {
		t.Fatalf("WatchCallback(nil watcher) = %v", err)
	}
	var typedNilWatcher *callbackTestWatcher
	if _, err := WatchCallback(ctx, typedNilWatcher, watch); !errors.Is(err, ErrWatchInvalidCallback) || !strings.Contains(err.Error(), "watcher is nil") {
		t.Fatalf("WatchCallback(typed nil watcher) = %v", err)
	}
	if _, err := WatchCallback(ctx, newCallbackTestWatcher(), nil); !errors.Is(err, ErrWatchInvalidCallback) || !strings.Contains(err.Error(), "handler is nil") {
		t.Fatalf("WatchCallback(nil handler) = %v", err)
	}
}
