package driver

import (
	"context"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// RR-20260926-56：流名跟随 prefix。
//
// 流名曾固定为 ROOST_SYNC：两个 prefix 不同的部署共用一个 NATS 时，EnsureStream
// （CreateOrUpdateStream）会把同一个流的 subjects 改成后启动者的 prefix，先启动者的
// 发布从此不再入流。流名缺省由 prefix 派生；默认 prefix roost.sync 仍得到 ROOST_SYNC，
// 已部署的流与 durable 游标不变；显式配置的流名优先。
func TestJetStreamSyncStreamFollowsThePrefix(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, stream, want string
	}{
		{name: "default prefix keeps the deployed stream", want: "ROOST_SYNC"},
		{name: "explicit default prefix keeps the deployed stream", prefix: "roost.sync", want: "ROOST_SYNC"},
		{name: "isolated prefix", prefix: "zz3640.sync", want: "ZZ3640_SYNC"},
		{name: "deeper prefix", prefix: "tenant.b.sync", want: "TENANT_B_SYNC"},
		{name: "explicit stream wins", prefix: "zz3640.sync", stream: "ROOST_SYNC", want: "ROOST_SYNC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamFor(t, tc.prefix, tc.stream); got != tc.want {
				t.Fatalf("prefix %q stream %q: the bus ensured stream %q, want %q", tc.prefix, tc.stream, got, tc.want)
			}
		})
	}
	// 大小写与分隔符在流名里会被折叠；折叠后相撞的不同 prefix 仍须得到不同的流。
	seen := map[string]string{}
	for _, prefix := range []string{"roost.sync", "zz.sync", "zz_sync", "ZZ.sync", "zz-sync", "zz.sync.a", "zz.sync_a"} {
		stream := streamFor(t, prefix, "")
		if other, taken := seen[stream]; taken {
			t.Errorf("prefixes %q and %q share stream %q", other, prefix, stream)
		}
		seen[stream] = prefix
	}
}

func streamFor(t *testing.T, prefix, stream string) string {
	t.Helper()
	js := newFakeJetStream()
	if _, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{
		LocalSid: 7, Prefix: prefix, Stream: stream, Storage: fnats.JetStreamStorageMemory,
	}); err != nil {
		t.Fatalf("prefix %q: %v", prefix, err)
	}
	if len(js.streams) != 1 {
		t.Fatalf("prefix %q: %d streams ensured", prefix, len(js.streams))
	}
	wantPrefix := prefix
	if wantPrefix == "" {
		wantPrefix = defaultJetStreamSyncPrefix
	}
	if subjects := js.streams[0].Subjects; len(subjects) != 1 || subjects[0] != wantPrefix+".>" {
		t.Fatalf("prefix %q: stream subjects %v", prefix, subjects)
	}
	return js.streams[0].Name
}
