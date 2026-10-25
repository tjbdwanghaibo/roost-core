package chunksync

import (
	"bytes"
	"testing"
)

func TestMigrationReconnectAndRejectedFrame(t *testing.T) {
	var c Cursor
	var r Replica
	s := Snapshot{Epoch: 1, Tick: 1, Objects: []Object{{ID: 7, Data: []byte("ball")}}}
	p, e := c.Plan(s)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Apply(p.Data); e != nil {
		t.Fatal(e)
	}
	_ = c.Commit(p)
	s.Tick++
	s.Objects[0].Chunk = Key{1, 1}
	p, e = c.Plan(s)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Apply(p.Data[:len(p.Data)-1]); e == nil {
		t.Fatal("truncation accepted")
	}
	if r.Tick != 1 {
		t.Fatal("partial apply")
	}
	if e = r.Apply(p.Data); e != nil {
		t.Fatal(e)
	}
	if len(r.Objects) != 1 || r.Objects[7].Chunk.X != 1 {
		t.Fatal("migration")
	}
	_ = c.Commit(p)
	if e = r.Apply(p.Data); e == nil {
		t.Fatal("replay accepted")
	}
	s.Tick++
	s.Objects = nil
	p, _ = c.Plan(s)
	if e = r.Apply(p.Data); e != nil {
		t.Fatal(e)
	}
	_ = c.Commit(p)
	if len(r.Objects) != 0 {
		t.Fatal("remove")
	}
	var reconnect Cursor
	p, _ = reconnect.Plan(s)
	if e = r.Apply(p.Data); e != nil {
		t.Fatal(e)
	}
}
func TestPlanOwnsBytesAndDoesNotAdvanceBeforeCommit(t *testing.T) {
	var c Cursor
	s := Snapshot{Epoch: 2, Objects: []Object{{ID: 1, Data: []byte{1}}}}
	a, _ := c.Plan(s)
	b, _ := c.Plan(s)
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatal("plan advanced")
	}
	s.Objects[0].Data[0] = 2
	_ = c.Commit(a)
	p, _ := c.Plan(s)
	var r Replica
	_ = r.Apply(a.Data)
	if e := r.Apply(p.Data); e != nil {
		t.Fatal(e)
	}
	if r.Objects[1].Data[0] != 2 {
		t.Fatal("alias")
	}
}
