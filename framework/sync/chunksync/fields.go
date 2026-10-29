package chunksync

import (
	"bytes"
	"errors"
)

// FieldLayout 定义应用层固定字段长度（1～8 项）。RC2 对象载荷为一字节 mask
// 加变化字段；首次出现、重入或换 Chunk 必须携带全部字段。游标始终保存完整基线。
// 布局属于应用协议，调用双方必须固定一致；它不识别物理或游戏字段。
type FieldLayout []uint16

func (l FieldLayout) size() (int, error) {
	if len(l) == 0 || len(l) > 8 {
		return 0, errors.New("chunksync: invalid field layout")
	}
	size := 0
	for _, n := range l {
		if n == 0 {
			return 0, errors.New("chunksync: empty field")
		}
		size += int(n)
	}
	if size+1 > 65535 {
		return 0, errors.New("chunksync: field layout too large")
	}
	return size, nil
}

// mask 返回 current 相对 old 变化的字段位图和编码后长度（含 mask 字节）；
// old 为 nil 时写全部字段。调用方已确认 old/current 长度等于布局总长。
func (l FieldLayout) mask(old, current []byte) (byte, int) {
	mask, size := byte(0), 1
	offset := 0
	for i, n := range l {
		end := offset + int(n)
		if old == nil || !bytes.Equal(old[offset:end], current[offset:end]) {
			mask |= 1 << i
			size += int(n)
		}
		offset = end
	}
	return mask, size
}

// appendFields 按 mask 把 mask 字节和变化字段追加到 dst。
func (l FieldLayout) appendFields(dst []byte, mask byte, current []byte) []byte {
	dst = append(dst, mask)
	offset := 0
	for i, n := range l {
		end := offset + int(n)
		if mask&(1<<i) != 0 {
			dst = append(dst, current[offset:end]...)
		}
		offset = end
	}
	return dst
}
func (l FieldLayout) decode(old, payload []byte) ([]byte, error) {
	size, err := l.size()
	if err != nil {
		return nil, err
	}
	all := byte((1 << len(l)) - 1)
	if len(payload) == 0 || payload[0] == 0 || payload[0]&^all != 0 || old == nil && payload[0] != all || old != nil && len(old) != size {
		return nil, errors.New("chunksync: invalid field mask or baseline")
	}
	out := make([]byte, size)
	copy(out, old)
	mask := payload[0]
	payload = payload[1:]
	offset := 0
	for i, n := range l {
		end := offset + int(n)
		if mask&(1<<i) != 0 {
			if len(payload) < int(n) {
				return nil, errors.New("chunksync: truncated field")
			}
			copy(out[offset:end], payload[:n])
			payload = payload[n:]
		}
		offset = end
	}
	if len(payload) != 0 {
		return nil, errors.New("chunksync: trailing field bytes")
	}
	return out, nil
}
