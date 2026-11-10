package terrain

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// writeBatch 是 WritePack 一批并行取块与压缩的块数：一批算完再按顺序写出，内存有界（约 batch × 16KB），
// 输出与并行度无关、逐字节确定。
const writeBatch = 256

// WritePack 按 meta 生成原始地形包写到 w：先写块数据（按索引顺序），最后写索引表与包头。src 对地图内每块调用一次，
// 可能被多个 goroutine 并发调用（见 ChunkSource）；返回错误或形状不对的块时写包失败。返回写出的总字节数。
//
// w 写一半失败时内容不可用；要原子地落成文件用 WritePackFile。
func WritePack(w io.WriterAt, meta Meta, src ChunkSource) (int64, error) {
	if err := meta.Validate(); err != nil {
		return 0, err
	}
	n := meta.Chunks()
	index := make([]indexEntry, n)
	offset := int64(packHeaderSize) + int64(n)*packIndexEntry + int64(len(meta.WaterRegions))*packRegionEntry
	workers := runtime.GOMAXPROCS(0)
	encoded := make([][]byte, writeBatch)
	errs := make([]error, writeBatch)
	for first := 0; first < n; first += writeBatch {
		count := min(writeBatch, n-first)
		var wg sync.WaitGroup
		next := make(chan int)
		for range workers {
			wg.Go(func() {
				for slot := range next {
					encoded[slot], errs[slot] = encodeFrom(src, meta, first+slot)
				}
			})
		}
		for slot := range count {
			next <- slot
		}
		close(next)
		wg.Wait()
		for slot := range count {
			if errs[slot] != nil {
				return 0, errs[slot]
			}
			data := encoded[slot]
			if _, err := w.WriteAt(data, offset); err != nil {
				return 0, fmt.Errorf("terrain: write chunk data: %w", err)
			}
			index[first+slot] = indexEntry{offset: uint64(offset), length: uint32(len(data)), sum: sha256.Sum256(data)}
			offset += int64(len(data))
		}
	}
	head, table := encodeHeader(meta, index)
	if _, err := w.WriteAt(table, packHeaderSize); err != nil {
		return 0, fmt.Errorf("terrain: write index: %w", err)
	}
	if _, err := w.WriteAt(head, 0); err != nil {
		return 0, fmt.Errorf("terrain: write header: %w", err)
	}
	return offset, nil
}

// encodeFrom 取第 i 块（行优先）并编码。
func encodeFrom(src ChunkSource, meta Meta, i int) ([]byte, error) {
	key := ChunkKey{X: int32(i % int(meta.ChunksX)), Y: int32(i / int(meta.ChunksX))}
	c, err := src(key)
	if err != nil {
		return nil, fmt.Errorf("terrain: source chunk %v: %w", key, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("terrain: source chunk %v: %w", key, err)
	}
	data, err := encodeChunk(c)
	if err != nil {
		return nil, fmt.Errorf("terrain: encode chunk %v: %w", key, err)
	}
	if len(data) > maxStoredChunk {
		return nil, fmt.Errorf("terrain: encoded chunk %v is %d bytes, over %d", key, len(data), maxStoredChunk)
	}
	return data, nil
}

// encodeHeader 编出包头与紧随其后的两张表（索引表、区域表，连在一起；格式见 pack.go）。
func encodeHeader(meta Meta, index []indexEntry) (head, table []byte) {
	le := binary.LittleEndian
	table = make([]byte, len(index)*packIndexEntry+len(meta.WaterRegions)*packRegionEntry)
	for i, e := range index {
		b := table[i*packIndexEntry:]
		le.PutUint64(b, e.offset)
		le.PutUint32(b[8:], e.length)
		copy(b[12:], e.sum[:])
	}
	for i, r := range meta.WaterRegions {
		b := table[len(index)*packIndexEntry+i*packRegionEntry:]
		le.PutUint32(b, uint32(r.MinX))
		le.PutUint32(b[4:], uint32(r.MinY))
		le.PutUint32(b[8:], uint32(r.MaxX))
		le.PutUint32(b[12:], uint32(r.MaxY))
		le.PutUint32(b[16:], uint32(r.Level))
	}
	head = make([]byte, packHeaderSize)
	copy(head, packMagic)
	le.PutUint32(head[8:], packVersion)
	le.PutUint32(head[12:], packHeaderSize)
	le.PutUint64(head[16:], uint64(meta.BaseRevision))
	le.PutUint32(head[24:], uint32(meta.ChunksX))
	le.PutUint32(head[28:], uint32(meta.ChunksY))
	le.PutUint32(head[32:], ChunkSamples)
	le.PutUint32(head[36:], SampleSpacing)
	le.PutUint32(head[40:], HeightStep)
	le.PutUint32(head[44:], uint32(meta.HeightBase))
	le.PutUint32(head[48:], uint32(meta.WaterLevel))
	le.PutUint32(head[52:], codecPlaneDeltaFlate)
	le.PutUint64(head[56:], packHeaderSize)
	le.PutUint32(head[64:], packIndexEntry)
	le.PutUint32(head[68:], uint32(len(meta.WaterRegions)))
	sum := metaSum(head, table)
	copy(head[packMetaHashOff:], sum[:])
	return head, table
}

// WritePackFile 把包原子地写成文件 path（权限 0644）：先写同目录的临时文件并 fsync，成功后改名覆盖 path；
// 失败时删除临时文件，path 原有内容不变。
func WritePackFile(path string, meta Meta, src ChunkSource) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err = WritePack(tmp, meta, src); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
