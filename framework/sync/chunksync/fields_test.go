package chunksync

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestFieldsDeltaMigrationReentryAndAtomicFailure(t *testing.T) {
	layout := FieldLayout{2, 3}
	var c Cursor
	var r Replica
	s := Snapshot{Epoch: 1, Tick: 1, Objects: []Object{{ID: 1, Data: []byte{1, 2, 3, 4, 5}}}}
	full, err := c.PlanFields(s, layout)
	if err != nil {
		t.Fatal(err)
	}
	if full.Data[2] != 2 || full.Data[58] != 3 {
		t.Fatalf("full fields %v", full.Data)
	}
	if err = r.ApplyFields(full.Data, layout); err != nil {
		t.Fatal(err)
	}
	if err = c.Commit(full); err != nil {
		t.Fatal(err)
	}
	s.Tick++
	s.Objects[0].Data = []byte{9, 2, 3, 4, 5}
	p, err := c.PlanFields(s, layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Data) != 61 || p.Data[58] != 1 {
		t.Fatalf("expected only first field: %v", p.Data)
	}
	before := r.Objects
	for n := 0; n < len(p.Data); n++ {
		if err = r.ApplyFields(p.Data[:n], layout); err == nil {
			t.Fatalf("accepted prefix %d", n)
		}
		if r.Tick != 1 || !reflect.DeepEqual(before, r.Objects) {
			t.Fatal("partial update")
		}
	}
	bad := bytes.Clone(p.Data)
	bad[58] = 4
	if err = r.ApplyFields(bad, layout); err == nil {
		t.Fatal("unknown mask bit")
	}
	var fresh Replica
	if err = fresh.ApplyFields(p.Data, layout); err == nil {
		t.Fatal("delta without baseline")
	}
	if err = r.ApplyFields(p.Data, layout); err != nil {
		t.Fatal(err)
	}
	_ = c.Commit(p)
	if !bytes.Equal(r.Objects[1].Data, s.Objects[0].Data) {
		t.Fatal("field merge")
	}
	// Chunk 改变，即使对象字节不变也必须完整更新。
	s.Tick++
	s.Objects[0].Chunk = Key{X: 1}
	p, err = c.PlanFields(s, layout)
	if err != nil {
		t.Fatal(err)
	}
	if p.Data[58] != 3 {
		t.Fatal("migration is not full")
	}
	bad = bytes.Clone(p.Data)
	bad[58] = 1
	bad = bad[:len(bad)-3]
	binary.LittleEndian.PutUint16(bad[56:], 3)
	if err = r.ApplyFields(bad, layout); err == nil {
		t.Fatal("partial migration accepted")
	}
	if err = r.ApplyFields(p.Data, layout); err != nil {
		t.Fatal(err)
	}
	_ = c.Commit(p)
	object := s.Objects[0]
	s.Tick++
	s.Objects = nil
	p, _ = c.PlanFields(s, layout)
	if err = r.ApplyFields(p.Data, layout); err != nil {
		t.Fatal(err)
	}
	_ = c.Commit(p)
	s.Tick++
	s.Objects = []Object{object}
	p, _ = c.PlanFields(s, layout)
	if p.Data[58] != 3 {
		t.Fatal("reentry needs full")
	}
	if err = r.ApplyFields(p.Data, layout); err != nil {
		t.Fatal(err)
	}
	// 全量重连新游标不继承旧字段基线。
	var reconnect Cursor
	p, _ = reconnect.PlanFields(s, layout)
	if err = r.ApplyFields(p.Data, layout); err != nil {
		t.Fatal(err)
	}
	if err = r.ApplyFields(p.Data, FieldLayout{5}); err == nil {
		t.Fatal("wrong layout accepted")
	}
}
func TestFieldPlansRetryAndDoNotAlias(t *testing.T) {
	var c Cursor
	layout := FieldLayout{1, 1}
	s := Snapshot{Epoch: 1, Objects: []Object{{ID: 1, Data: []byte{1, 2}}}}
	p, err := c.PlanFields(s, layout)
	if err != nil {
		t.Fatal(err)
	}
	retry, _ := c.PlanFields(s, layout)
	if !bytes.Equal(p.Data, retry.Data) {
		t.Fatal("retry changed")
	}
	s.Objects[0].Data[0] = 9
	if err = c.Commit(p); err != nil {
		t.Fatal(err)
	}
	next, _ := c.PlanFields(s, layout)
	if next.Data[58] != 1 {
		t.Fatal("caller mutation changed baseline")
	}
	if err = c.Commit(retry); err == nil {
		t.Fatal("stale commit accepted")
	}
	for _, bad := range []FieldLayout{nil, {}, {0}, {65535}, {1, 1, 1, 1, 1, 1, 1, 1, 1}} {
		if _, err = c.PlanFields(s, bad); err == nil {
			t.Fatal("invalid layout")
		}
	}
}
