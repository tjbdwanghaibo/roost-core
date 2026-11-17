package lobby

import (
	"context"
	"testing"

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
)

type fakeEtcd []*fetcd.KV

func (f fakeEtcd) GetWithPrefix(_ context.Context, prefix string) ([]*fetcd.KV, error) { return f, nil }

// etcd 广播的读法：键的最后一段就是实例名；解不开、实例名与键不符、实例名不合法的条目跳过。
func TestEtcdRelaysReadsTheRegistryAndSkipsBadEntries(t *testing.T) {
	directory := EtcdRelays{Prefix: "/party/relays/", Client: fakeEtcd{
		{Key: "/party/relays/relay-1", Value: `{"instance":"relay-1","tcp_addr":"10.0.0.1:7100","rooms":3}`},
		{Key: "/party/relays/relay-2", Value: `{"instance":"relay-2","tcp_addr":"10.0.0.2:7100","ws_addr":"wss://r2/relay","rooms":1}`},
		{Key: "/party/relays/relay-3", Value: `not json`},
		{Key: "/party/relays/relay-4", Value: `{"instance":"relay-x","tcp_addr":"10.0.0.4:7100"}`},
		{Key: "/party/relays/a.b", Value: `{"instance":"a.b","tcp_addr":"10.0.0.5:7100"}`},
	}}
	relays, err := directory.Relays(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(relays) != 2 || relays[0].Instance != "relay-1" || relays[1].Instance != "relay-2" {
		t.Fatalf("relays = %+v, want relay-1 and relay-2 only", relays)
	}
	if picked, ok := pickRelay(relays, TransportTCP); !ok || picked.Instance != "relay-2" {
		t.Fatalf("tcp pick = %+v, want the least loaded relay-2", picked)
	}
	only := []Relay{{Instance: "relay-1", TCPAddr: "a", Rooms: 0}}
	if _, ok := pickRelay(only, TransportWS); ok {
		t.Fatal("picked a relay without a ws address for a ws client")
	}
	if _, err := (EtcdRelays{}).Relays(context.Background()); err == nil {
		t.Fatal("an unconfigured etcd directory answered")
	}
}
