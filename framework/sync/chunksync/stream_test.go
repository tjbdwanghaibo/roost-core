package chunksync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
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

// 固定伪随机场景下逐帧比较 RC1/RC2 线字节：覆盖进入、离开、跨 Chunk、字段增量、
// 未确认重做和 epoch 切换。编码实现的优化不得改变任何一帧的字节。
func TestPlanFramesStayByteIdentical(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	layout := FieldLayout{2, 1, 3}
	var raw, fields Cursor
	var rawReplica, fieldReplica Replica
	digest := sha256.New()
	epoch := uint64(1)
	data := make(map[uint64][]byte)
	chunks := make(map[uint64]Key)
	for step := 1; step <= 120; step++ {
		if step == 70 {
			epoch++
		}
		s := Snapshot{Epoch: epoch, Tick: uint64(step)}
		for id := uint64(1); id <= 40; id++ {
			if rng.IntN(10) < 2 {
				continue
			}
			if data[id] == nil || rng.IntN(4) == 0 {
				data[id] = make([]byte, 6)
				for i := range data[id] {
					data[id][i] = byte(rng.IntN(4))
				}
			} else if rng.IntN(3) == 0 {
				data[id][rng.IntN(6)] = byte(rng.IntN(4))
			}
			if _, ok := chunks[id]; !ok || rng.IntN(10) == 0 {
				chunks[id] = Key{X: int32(rng.IntN(3)) - 1, Z: int32(rng.IntN(3)) - 1}
			}
			s.Objects = append(s.Objects, Object{ID: id, Chunk: chunks[id], Data: data[id]})
		}
		rng.Shuffle(len(s.Objects), func(i, j int) { s.Objects[i], s.Objects[j] = s.Objects[j], s.Objects[i] })
		commit := rng.IntN(7) != 0
		for _, c := range []struct {
			cursor  *Cursor
			replica *Replica
			layout  FieldLayout
		}{{&raw, &rawReplica, nil}, {&fields, &fieldReplica, layout}} {
			var p *Plan
			var err error
			if c.layout == nil {
				p, err = c.cursor.Plan(s)
			} else {
				p, err = c.cursor.PlanFields(s, c.layout)
			}
			if err != nil {
				t.Fatal(err)
			}
			digest.Write(p.Data)
			if !commit {
				continue
			}
			if c.layout == nil {
				err = c.replica.Apply(p.Data)
			} else {
				err = c.replica.ApplyFields(p.Data, c.layout)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = c.cursor.Commit(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	const want = "5ebd17bd992bfbd7d32ae138cb46556909d478fa71f0c700bf13f5fbc610103c"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Fatalf("frame bytes changed: %s", got)
	}
}
