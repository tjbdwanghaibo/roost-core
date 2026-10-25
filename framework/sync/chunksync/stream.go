// Package chunksync 将已提交的区域对象状态编码为基线和增量。
// 它不拥有业务锁或持久化；调用者必须交入不可变的已提交快照。
package chunksync

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

const MaxPayload = 1 << 20
const maxObjects = 16384

var ErrSequence = errors.New("chunksync: baseline or sequence mismatch")

type Key struct{ X, Z int32 }
type Object struct {
	ID    uint64
	Chunk Key
	Data  []byte
}
type Snapshot struct {
	Epoch, Tick uint64
	Objects     []Object
}

// Cursor 属于单个可靠连接。Plan 不推进游标，整包写出成功后才 Commit。
// 调用方串行执行 Plan/发送/Commit，失败必须关闭连接或重试同一计划。
type Cursor struct {
	epoch, seq, tick uint64
	objects          map[uint64]Object
}
type Plan struct {
	Data     []byte
	previous uint64
	next     Cursor
}

func (c *Cursor) Plan(s Snapshot) (*Plan, error) {
	if s.Epoch == 0 || len(s.Objects) > maxObjects {
		return nil, errors.New("chunksync: invalid snapshot")
	}
	full := c.epoch != s.Epoch || c.objects == nil
	if !full && s.Tick < c.tick {
		return nil, ErrSequence
	}
	next := Cursor{epoch: s.Epoch, seq: c.seq + 1, tick: s.Tick, objects: make(map[uint64]Object, len(s.Objects))}
	if c.seq == math.MaxUint64 {
		return nil, ErrSequence
	}
	groups := make(map[Key][]Object)
	snapshotBytes := 36
	for _, o := range s.Objects {
		snapshotBytes += 22 + len(o.Data)
		if snapshotBytes > MaxPayload {
			return nil, errors.New("chunksync: snapshot exceeds limit")
		}
		if o.ID == 0 || len(o.Data) > 65535 {
			return nil, errors.New("chunksync: invalid object")
		}
		if _, exists := next.objects[o.ID]; exists {
			return nil, errors.New("chunksync: duplicate object")
		}
		o.Data = bytes.Clone(o.Data)
		next.objects[o.ID] = o
		old, ok := c.objects[o.ID]
		if full || !ok || old.Chunk != o.Chunk || !bytes.Equal(old.Data, o.Data) {
			groups[o.Chunk] = append(groups[o.Chunk], o)
		}
	}
	var removed []uint64
	if !full {
		for id := range c.objects {
			if _, ok := next.objects[id]; !ok {
				removed = append(removed, id)
			}
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	keys := make([]Key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].X != keys[j].X {
			return keys[i].X < keys[j].X
		}
		return keys[i].Z < keys[j].Z
	})
	var b bytes.Buffer
	b.WriteString("RC")
	b.WriteByte(1)
	if full {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	put := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	put(s.Epoch)
	put(next.seq)
	put(s.Tick)
	put(uint32(len(removed)))
	put(uint32(len(keys)))
	for _, id := range removed {
		put(id)
	}
	for _, k := range keys {
		entries := groups[k]
		sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
		put(k.X)
		put(k.Z)
		put(uint32(len(entries)))
		for _, o := range entries {
			put(o.ID)
			put(uint16(len(o.Data)))
			b.Write(o.Data)
		}
	}
	if b.Len() > MaxPayload {
		return nil, errors.New("chunksync: frame exceeds limit")
	}
	return &Plan{Data: b.Bytes(), previous: c.seq, next: next}, nil
}
func (c *Cursor) Commit(p *Plan) error {
	if p == nil || p.previous != c.seq {
		return ErrSequence
	}
	*c = p.next
	return nil
}

// Replica 先验证整包再替换状态；跨 Chunk 更新以稳定 ID 覆盖旧归属，不暴露半迁移状态。
// 与 Cursor 一样由调用方串行访问。
type Replica struct {
	Epoch, Sequence, Tick uint64
	Objects               map[uint64]Object
}

func (r *Replica) Apply(data []byte) error {
	if len(data) > MaxPayload || len(data) < 36 || string(data[:3]) != "RC\x01" || data[3] > 1 {
		return errors.New("chunksync: invalid frame")
	}
	b := bytes.NewReader(data[4:])
	get := func(v any) error { return binary.Read(b, binary.LittleEndian, v) }
	var epoch, seq, tick uint64
	var deletes, groups uint32
	for _, v := range []any{&epoch, &seq, &tick, &deletes, &groups} {
		if err := get(v); err != nil {
			return err
		}
	}
	full := data[3] == 1
	if epoch == 0 || seq == 0 || deletes > maxObjects || groups > maxObjects {
		return errors.New("chunksync: invalid header")
	}
	if full {
		if deletes != 0 {
			return errors.New("chunksync: baseline deletes")
		}
	} else if r.Objects == nil || epoch != r.Epoch || seq != r.Sequence+1 || tick < r.Tick {
		return ErrSequence
	}
	objects := make(map[uint64]Object)
	if !full {
		for id, o := range r.Objects {
			objects[id] = o
		}
	}
	seen := make(map[uint64]bool)
	for range deletes {
		var id uint64
		if err := get(&id); err != nil {
			return err
		}
		if id == 0 || seen[id] {
			return errors.New("chunksync: duplicate change")
		}
		seen[id] = true
		delete(objects, id)
	}
	for range groups {
		var k Key
		var n uint32
		for _, v := range []any{&k.X, &k.Z, &n} {
			if err := get(v); err != nil {
				return err
			}
		}
		if n > maxObjects {
			return errors.New("chunksync: object limit")
		}
		for range n {
			var id uint64
			var size uint16
			if err := get(&id); err != nil {
				return err
			}
			if err := get(&size); err != nil {
				return err
			}
			if id == 0 || seen[id] {
				return errors.New("chunksync: duplicate change")
			}
			seen[id] = true
			payload := make([]byte, int(size))
			if _, err := io.ReadFull(b, payload); err != nil {
				return err
			}
			objects[id] = Object{ID: id, Chunk: k, Data: payload}
			if len(objects) > maxObjects {
				return errors.New("chunksync: object limit")
			}
		}
	}
	if b.Len() != 0 {
		return fmt.Errorf("chunksync: %d trailing bytes", b.Len())
	}
	r.Epoch, r.Sequence, r.Tick, r.Objects = epoch, seq, tick, objects
	return nil
}
