package driver

import (
	"context"
	"testing"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// P3b: the Mod publishes exactly what Assemble returns, so Assemble must
// refuse what the Mod used to refuse (no address) and must put the client and
// the lock factory on one connection pool — a lock factory on a second pool
// would double the connection count and make the Mod's Ping meaningless for
// locks.
func TestAssembleBuildsClientAndLocksOnOnePool(t *testing.T) {
	if _, err := Assemble(nil); err == nil {
		t.Fatal("nil configuration was assembled")
	}
	if _, err := Assemble(&fredis.Config{}); err == nil {
		t.Fatal("configuration without an address was assembled")
	}
	asm, err := Assemble(fredis.DefaultConfig("127.0.0.1:6379"))
	if err != nil {
		t.Fatal(err)
	}
	if asm.Client == nil || asm.Locks == nil {
		t.Fatalf("assembly incomplete: %+v", asm)
	}
	if asm.Locks.rdb != asm.Client.rdb {
		t.Fatal("lock factory does not share the client's connection pool")
	}
	var _ fredis.IRedis = asm.Client
	var _ fredis.IDistLockFactory = asm.Locks
	if err := asm.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}

	var none *Assembly
	if err := none.Ping(context.Background()); err == nil {
		t.Fatal("nil assembly Ping returned no error")
	}
	if err := none.Close(); err != nil {
		t.Fatalf("nil assembly Close = %v", err)
	}
}
