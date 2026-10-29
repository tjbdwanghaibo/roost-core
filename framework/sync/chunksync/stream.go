// Package chunksync 将已提交的区域对象状态编码为基线和增量。
// 它不拥有业务锁或持久化；调用者必须交入不可变的已提交快照。
package chunksync

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
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
// Cursor 不能按值复制后分别使用：两份副本会共用 spare 基线 map。
type Cursor struct {
	epoch, seq, tick uint64
	objects          map[uint64]Object
	layout           string
	// spare 是 Commit 换下并已清空的上一代基线 map，下一次 plan 取走复用。
	// 换下后只有本游标持有它：旧 Plan 的 next 只在 Commit 通过序号校验时才被读取，
	// 而持有这张 map 的那个 Plan 已经提交过，序号不会再匹配。
	spare map[uint64]Object
	// changes 是 plan 内部的临时变化表，只在一次 plan 调用期间使用，返回前清零留给下次。
	changes []change
}

// change 是一次 plan 中要上线的对象；mask 是 RC2 变化字段位图，组帧时只写这些字段。
type change struct {
	object Object
	mask   byte
}
type Plan struct {
	Data     []byte
	previous uint64
	next     Cursor
}

func (c *Cursor) Plan(s Snapshot) (*Plan, error) { return c.plan(s, nil) }

// PlanFields 使用 RC2 字段增量，Plan/Commit 的可靠发送边界与普通 Plan 相同。
func (c *Cursor) PlanFields(s Snapshot, layout FieldLayout) (*Plan, error) {
	if _, err := layout.size(); err != nil {
		return nil, err
	}
	return c.plan(s, layout)
}
func (c *Cursor) plan(s Snapshot, layout FieldLayout) (*Plan, error) {
	signature := ""
	fieldBytes := 0
	if layout != nil {
		signature = fmt.Sprint(layout)
		fieldBytes, _ = layout.size() // PlanFields 已校验布局。
	}

	if s.Epoch == 0 || len(s.Objects) > maxObjects {
		return nil, errors.New("chunksync: invalid snapshot")
	}
	full := c.epoch != s.Epoch || c.objects == nil || c.layout != signature
	if !full && s.Tick < c.tick {
		return nil, ErrSequence
	}
	if c.seq == math.MaxUint64 {
		return nil, ErrSequence
	}
	objects := c.spare
	c.spare = nil // 取走后归这个计划；计划未提交时随它一起丢弃。
	if objects == nil {
		objects = make(map[uint64]Object, len(s.Objects))
	}
	next := Cursor{layout: signature, epoch: s.Epoch, seq: c.seq + 1, tick: s.Tick, objects: objects}
	// changes 记录本帧要上线的对象；按 Chunk、ID 排序后连续同 Chunk 即一组，
	// 线格式与逐 Chunk 分组后组内按 ID 排序完全相同。
	changes := c.changes[:0]
	snapshotBytes := 36
	frameBytes := 36
	for _, o := range s.Objects {
		snapshotBytes += 22 + len(o.Data)
		if layout != nil {
			snapshotBytes++
		}
		if snapshotBytes > MaxPayload {
			return nil, errors.New("chunksync: snapshot exceeds limit")
		}
		if o.ID == 0 || len(o.Data) > 65535 {
			return nil, errors.New("chunksync: invalid object")
		}
		if _, exists := next.objects[o.ID]; exists {
			return nil, errors.New("chunksync: duplicate object")
		}
		old, ok := c.objects[o.ID]
		same := ok && bytes.Equal(old.Data, o.Data)
		// 游标基线只保存自己持有的副本，调用方之后修改快照不影响基线。
		// 内容未变时沿用上一基线那份（基线字节从不原地修改），不再重复复制。
		if same {
			o.Data = old.Data
		} else {
			o.Data = bytes.Clone(o.Data)
		}
		next.objects[o.ID] = o
		if full || !ok || old.Chunk != o.Chunk || !same {
			ch := change{object: o}
			wire := len(o.Data)
			if layout != nil {
				if len(o.Data) != fieldBytes {
					return nil, errors.New("chunksync: invalid field baseline")
				}
				var base []byte // nil 表示首次、重入或换 Chunk，写全部字段
				if !full && ok && old.Chunk == o.Chunk {
					base = old.Data
				}
				ch.mask, wire = layout.mask(base, o.Data)
			}
			frameBytes += 10 + wire
			changes = append(changes, ch)
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
	slices.Sort(removed)
	slices.SortFunc(changes, func(a, b change) int {
		if a.object.Chunk.X != b.object.Chunk.X {
			return cmp.Compare(a.object.Chunk.X, b.object.Chunk.X)
		}
		if a.object.Chunk.Z != b.object.Chunk.Z {
			return cmp.Compare(a.object.Chunk.Z, b.object.Chunk.Z)
		}
		return cmp.Compare(a.object.ID, b.object.ID)
	})
	groups := 0
	for i := range changes {
		if i == 0 || changes[i].object.Chunk != changes[i-1].object.Chunk {
			groups++
		}
	}
	frameBytes += 8*len(removed) + 12*groups
	if frameBytes > MaxPayload {
		return nil, errors.New("chunksync: frame exceeds limit")
	}
	// 帧长度已精确算出，一次分配；Plan.Data 归计划所有，重试发送同一份。
	b := make([]byte, 0, frameBytes)
	b = append(b, 'R', 'C')
	if layout == nil {
		b = append(b, 1)
	} else {
		b = append(b, 2)
	}
	if full {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	le := binary.LittleEndian
	b = le.AppendUint64(b, s.Epoch)
	b = le.AppendUint64(b, next.seq)
	b = le.AppendUint64(b, s.Tick)
	b = le.AppendUint32(b, uint32(len(removed)))
	b = le.AppendUint32(b, uint32(groups))
	for _, id := range removed {
		b = le.AppendUint64(b, id)
	}
	for i := 0; i < len(changes); {
		chunk := changes[i].object.Chunk
		end := i + 1
		for end < len(changes) && changes[end].object.Chunk == chunk {
			end++
		}
		b = le.AppendUint32(b, uint32(chunk.X))
		b = le.AppendUint32(b, uint32(chunk.Z))
		b = le.AppendUint32(b, uint32(end-i))
		for _, ch := range changes[i:end] {
			b = le.AppendUint64(b, ch.object.ID)
			if layout == nil {
				b = le.AppendUint16(b, uint16(len(ch.object.Data)))
				b = append(b, ch.object.Data...)
				continue
			}
			lengthAt := len(b)
			b = le.AppendUint16(b, 0)
			b = layout.appendFields(b, ch.mask, ch.object.Data)
			le.PutUint16(b[lengthAt:], uint16(len(b)-lengthAt-2))
		}
		i = end
	}
	clear(changes) // 释放对基线字节的引用，容量留给下一次 plan。
	c.changes = changes[:0]
	return &Plan{Data: b, previous: c.seq, next: next}, nil
}
func (c *Cursor) Commit(p *Plan) error {
	if p == nil || p.previous != c.seq {
		return ErrSequence
	}
	replaced := c.objects
	*c = p.next
	if replaced != nil {
		clear(replaced)
		c.spare = replaced
	}
	return nil
}

// Replica 先验证整包再替换状态；跨 Chunk 更新以稳定 ID 覆盖旧归属，不暴露半迁移状态。
// 与 Cursor 一样由调用方串行访问。
type Replica struct {
	Epoch, Sequence, Tick uint64
	Objects               map[uint64]Object
	layout                string
}

func (r *Replica) Apply(data []byte) error { return r.apply(data, nil) }

// ApplyFields 原子验证字段补丁；缺基线或迁移时的非完整对象一律拒绝。
func (r *Replica) ApplyFields(data []byte, layout FieldLayout) error {
	if _, err := layout.size(); err != nil {
		return err
	}
	return r.apply(data, layout)
}
func (r *Replica) apply(data []byte, layout FieldLayout) error {
	version := byte(1)
	signature := ""
	if layout != nil {
		version = 2
		signature = fmt.Sprint(layout)
	}

	if len(data) > MaxPayload || len(data) < 36 || (string(data[:2]) != "RC" || data[2] != version) || data[3] > 1 {
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
	} else if r.layout != signature || r.Objects == nil || epoch != r.Epoch || seq != r.Sequence+1 || tick < r.Tick {
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
			if layout != nil {
				var base []byte
				if old, ok := objects[id]; !full && ok && old.Chunk == k {
					base = old.Data
				}
				decoded, err := layout.decode(base, payload)
				if err != nil {
					return err
				}
				payload = decoded
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
	r.layout = signature
	return nil
}
