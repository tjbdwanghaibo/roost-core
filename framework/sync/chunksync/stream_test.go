package chunksync

import (
	"bytes"
	"reflect"
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

// 删除与更新必须整包生效，TCP 在任意字节断开都不能留下半帧状态。
func TestEveryTruncationPreservesReplica(t *testing.T) {
	var c Cursor
	first, err := c.Plan(Snapshot{Epoch: 7, Tick: 1, Objects: []Object{{ID: 1, Data: []byte("old")}, {ID: 2, Data: []byte("removed")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Commit(first); err != nil {
		t.Fatal(err)
	}
	next, err := c.Plan(Snapshot{Epoch: 7, Tick: 2, Objects: []Object{{ID: 1, Chunk: Key{2, -1}, Data: []byte("new")}}})
	if err != nil {
		t.Fatal(err)
	}
	for size := range len(next.Data) {
		var r Replica
		if err := r.Apply(first.Data); err != nil {
			t.Fatal(err)
		}
		before := r
		if err := r.Apply(next.Data[:size]); err == nil {
			t.Fatalf("accepted truncated frame at %d", size)
		}
		if !reflect.DeepEqual(before, r) {
			t.Fatalf("partial mutation at %d", size)
		}
	}
	var r Replica
	if err := r.Apply(first.Data); err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(next.Data); err != nil {
		t.Fatal(err)
	}
	if len(r.Objects) != 1 || string(r.Objects[1].Data) != "new" || r.Tick != 2 {
		t.Fatalf("complete frame not applied: %+v", r)
	}
}
