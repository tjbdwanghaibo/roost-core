package servicerpc

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"testing"
)

func TestBusinessWorkersRefuseRPCBeforeTransportAndDiscovery(t *testing.T) {
	for _, option := range []fctx.Option{fctx.WithFastWorker(), fctx.WithLongWorker()} {
		func() {
			_, release := fctx.NewContext(option)
			defer release()
			var client *BusClient
			if err := client.Call(context.Background(), 1, "rank", nil, nil); !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
				t.Fatal(err)
			}
			if _, err := client.PickServer(context.Background()); !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
				t.Fatal(err)
			}
		}()
	}
	_, release := fctx.NewContext(fctx.WithIOWorker())
	defer release()
	var client *BusClient
	if err := client.Call(context.Background(), 1, "rank", nil, nil); !errors.Is(err, ErrBusNil) {
		t.Fatalf("I/O rejected before transport: %v", err)
	}
}
