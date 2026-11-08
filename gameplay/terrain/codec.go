package terrain

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// 块的存储编码（包头 codec = codecPlaneDeltaFlate）：
//
//	明文 = 高度 4096 个 uvarint ‖ 材质 4096 字节
//	存储 = raw DEFLATE（RFC 1951，无 zlib / gzip 头，BestCompression）压缩的明文
//
// 高度按行（j = 0..63）、行内按 i = 0..63 依次写；每个值是与“预测值”之差的 zigzag uvarint（d → (d<<1) ^ (d>>63)）。
// 预测值是平面预测：左邻 + 下邻 − 左下邻；第一行只用左邻，每行行首只用下邻，块的第一个点预测为 0。平滑地形上预测误差
// 很小，绝大多数差值 1 字节，再交给 DEFLATE。选择依据见 README“块编码”的实测：同一批块平面预测约 2.7KB，只用左邻
// 约 4.1KB，只用下邻约 4.0KB，原始小端直接 DEFLATE 约 6.6KB（程序化地形，真实地形以实测为准）。
const codecPlaneDeltaFlate = 1

// maxPlainChunk 是明文上限：预测值在 [−65535, 131070]，差值的绝对值小于 2^17，zigzag 后小于 2^18，uvarint 最多 3 字节。
const maxPlainChunk = 3*ChunkPoints + ChunkPoints

// errCorrupt 是解码失败的内部原因，由调用方包成带块坐标的 ErrCorruptChunk。
var errCorrupt = errors.New("malformed chunk payload")

// encodeChunk 把块编成存储字节。调用方保证 c 已通过 Validate。
func encodeChunk(c Chunk) ([]byte, error) {
	plain := make([]byte, 0, 2*ChunkPoints+ChunkPoints)
	for k, h := range c.Heights {
		d := int64(h) - predict(c.Heights, k)
		plain = binary.AppendUvarint(plain, uint64(d<<1^d>>63))
	}
	plain = append(plain, c.Materials...)
	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// decodeChunk 是 encodeChunk 的逆：明文必须恰好是 4096 个合法高度加 4096 字节材质，多一字节、少一字节、
// 高度越出 uint16 都算损坏。
func decodeChunk(stored []byte) (Chunk, error) {
	plain, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(stored)), maxPlainChunk+1))
	if err != nil {
		return Chunk{}, fmt.Errorf("%w: inflate: %v", errCorrupt, err)
	}
	if len(plain) > maxPlainChunk {
		return Chunk{}, fmt.Errorf("%w: plain text longer than %d bytes", errCorrupt, maxPlainChunk)
	}
	c := NewChunk()
	pos := 0
	for k := range c.Heights {
		u, n := binary.Uvarint(plain[pos:])
		if n <= 0 {
			return Chunk{}, fmt.Errorf("%w: height %d: bad varint", errCorrupt, k)
		}
		pos += n
		v := predict(c.Heights, k) + (int64(u>>1) ^ -int64(u&1))
		if v < 0 || v > math.MaxUint16 {
			return Chunk{}, fmt.Errorf("%w: height %d out of range: %d", errCorrupt, k, v)
		}
		c.Heights[k] = uint16(v)
	}
	if len(plain)-pos != ChunkPoints {
		return Chunk{}, fmt.Errorf("%w: %d material bytes, want %d", errCorrupt, len(plain)-pos, ChunkPoints)
	}
	copy(c.Materials, plain[pos:])
	return c, nil
}

// predict 返回下标 k 的平面预测值，只读取 k 之前已解出的点。
func predict(h []uint16, k int) int64 {
	i, j := k%ChunkSamples, k/ChunkSamples
	switch {
	case j == 0 && i == 0:
		return 0
	case j == 0:
		return int64(h[k-1])
	case i == 0:
		return int64(h[k-ChunkSamples])
	}
	return int64(h[k-1]) + int64(h[k-ChunkSamples]) - int64(h[k-ChunkSamples-1])
}
