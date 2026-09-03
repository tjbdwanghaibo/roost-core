package nats

import (
	"errors"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

func TestNatsClientNilBoundaryFailsClosed(t *testing.T) {
	var client *natsClient
	if err := client.Publish("topic", nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("Publish error=%v, want ErrClosed", err)
	}
	client.Close()
}

func TestInvokeNatsHandlerContainsPanic(t *testing.T) {
	returned := false
	invokeNatsHandler(func(*fnats.Msg) { panic("boom") }, &fnats.Msg{Subject: "test"})
	returned = true
	if !returned {
		t.Fatal("panic escaped callback boundary")
	}
}
