package driver

import (
	"context"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// P3b: Assemble needs a live server for anything beyond refusing a bad
// configuration; the kit Mod's integration test exercises the connected path.
func TestAssembleRefusesConfigurationWithoutURL(t *testing.T) {
	if _, err := Assemble(nil, ClientOptions{}); err == nil {
		t.Fatal("nil configuration was assembled")
	}
	if _, err := Assemble(&fnats.Config{}, ClientOptions{}); err == nil {
		t.Fatal("configuration without a URL was assembled")
	}
	var none *Assembly
	if none.Connected() {
		t.Fatal("nil assembly reports connected")
	}
	if err := none.Close(context.Background()); err != nil {
		t.Fatalf("nil assembly Close = %v", err)
	}
}
