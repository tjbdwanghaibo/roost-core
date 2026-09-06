package etcd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

type idleMirrorClient struct{}

func (idleMirrorClient) GetPrefixSnapshot(context.Context, string) (*fetcd.PrefixSnapshot, error) {
	return &fetcd.PrefixSnapshot{}, nil
}
func (idleMirrorClient) WatchPrefix(context.Context, string, ...fetcd.WatchOption) fetcd.IWatcher {
	return idleWatcher{events: make(chan *fetcd.WatchEvent)}
}
func (idleMirrorClient) Put(context.Context, string, string) error                 { return nil }
func (idleMirrorClient) PutWithLease(context.Context, string, string, int64) error { return nil }
func (idleMirrorClient) Delete(context.Context, string) error                      { return nil }
func (idleMirrorClient) Txn(context.Context, fetcd.Cmp, []fetcd.Op, []fetcd.Op) (*fetcd.TxnResponse, error) {
	return &fetcd.TxnResponse{}, nil
}

type idleWatcher struct{ events chan *fetcd.WatchEvent }

func (w idleWatcher) EventChan() <-chan *fetcd.WatchEvent { return w.events }
func (w idleWatcher) Close() error                        { return nil }

func mirrorConfig() fetcd.LocalMirrorConfig[string] {
	return fetcd.LocalMirrorConfig[string]{
		Prefix: "/roost/test/",
		Decode: func(_, value string) (string, error) { return value, nil },
		Encode: func(value string) (string, error) { return value, nil },
		Clone:  func(value string) (string, error) { return value, nil },
	}
}

// A mirror with no prefix would watch the whole keyspace; one without codecs
// could not turn events into values; a retry window whose max is below its
// min would spin. Each is refused at construction with the config sentinel.
func TestNewLocalMirrorRefusesEachInvalidConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := newLocalMirror[string](ctx, nil, mirrorConfig()); err == nil || !strings.Contains(err.Error(), "client is nil") {
		t.Fatalf("nil client = %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*fetcd.LocalMirrorConfig[string])
		text   string
	}{
		{"empty prefix", func(c *fetcd.LocalMirrorConfig[string]) { c.Prefix = "" }, "prefix is empty"},
		{"no decoder", func(c *fetcd.LocalMirrorConfig[string]) { c.Decode = nil }, "Decode, Encode, and Clone are required"},
		{"no encoder", func(c *fetcd.LocalMirrorConfig[string]) { c.Encode = nil }, "Decode, Encode, and Clone are required"},
		{"no clone", func(c *fetcd.LocalMirrorConfig[string]) { c.Clone = nil }, "Decode, Encode, and Clone are required"},
		{"retry max below min", func(c *fetcd.LocalMirrorConfig[string]) {
			c.RetryMinInterval, c.RetryMaxInterval = time.Second, time.Millisecond
		}, "retry max interval is smaller than retry min interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mirrorConfig()
			tc.mutate(&cfg)
			_, err := newLocalMirror[string](ctx, idleMirrorClient{}, cfg)
			if !errors.Is(err, fetcd.ErrMirrorInvalidConfig) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("newLocalMirror = %v, want ErrMirrorInvalidConfig containing %q", err, tc.text)
			}
		})
	}
}
