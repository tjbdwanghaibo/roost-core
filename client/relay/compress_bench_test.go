package relay_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/client/relay"
)

// BenchmarkSnapshotCompression 量缺省 deflate 对房主快照的压缩率与耗时（交接 C4 “先量一次”，2026-11-18）。
// 三种载荷、四档大小（上限 256KiB 是 relay 的快照上限）：
//   - json：房间状态的 JSON（成员数组：号、名字、坐标、血量、背包），字段名重复，最贴近“游戏直接 json.Marshal”；
//   - packed：同样的状态按定长小端整数排成二进制（坐标是随机游走，相邻值接近）；
//   - random：随机字节，压不动的上界。
//
// 报告 ratio（压缩后 / 原始）、compress / decompress 两个子项的 ns/op、B/op。
// 测量：go test ./client/relay -run '^$' -bench SnapshotCompression -benchmem -count 6。
func BenchmarkSnapshotCompression(b *testing.B) {
	for _, kind := range []string{"json", "packed", "random"} {
		for _, size := range []int{1 << 10, 16 << 10, 64 << 10, 256 << 10} {
			data := snapshotPayload(kind, size)
			packed, err := relay.Deflate.Compress(data)
			if err != nil {
				b.Fatal(err)
			}
			name := fmt.Sprintf("%s/%dKiB", kind, size>>10)
			b.Run(name+"/compress", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				for b.Loop() {
					if _, err := relay.Deflate.Compress(data); err != nil {
						b.Fatal(err)
					}
				}
				// 放在循环之后：b.Loop 第一次调用会重置计时器，连同之前报告的自定义指标一起清掉。
				b.ReportMetric(float64(len(packed))/float64(len(data)), "ratio")
			})
			b.Run(name+"/decompress", func(b *testing.B) {
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				for b.Loop() {
					if _, err := relay.Deflate.Decompress(packed, relay.MaxStateBytes); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

type benchMember struct {
	ID    uint64   `json:"id"`
	Name  string   `json:"name"`
	X     int32    `json:"x"`
	Y     int32    `json:"y"`
	HP    int32    `json:"hp"`
	MaxHP int32    `json:"max_hp"`
	Items []uint32 `json:"items"`
}

func snapshotPayload(kind string, size int) []byte {
	r := rand.New(rand.NewSource(int64(size)))
	if kind == "random" {
		out := make([]byte, size)
		r.Read(out)
		return out
	}
	var members []benchMember
	var out []byte
	x, y := int32(1000), int32(1000)
	for len(out) < size {
		x += int32(r.Intn(21) - 10)
		y += int32(r.Intn(21) - 10)
		m := benchMember{ID: 10_000 + uint64(len(members)), Name: fmt.Sprintf("player_%04d", len(members)), X: x, Y: y,
			HP: int32(r.Intn(1000)), MaxHP: 1000, Items: []uint32{uint32(r.Intn(64)), uint32(r.Intn(64)), uint32(r.Intn(64)), uint32(r.Intn(64))}}
		members = append(members, m)
		if kind == "json" {
			if len(members)%64 == 0 { // 每 64 人编一次，截到 size（截断的 JSON 不影响压缩率）
				out, _ = json.Marshal(map[string]any{"tick": 12345, "members": members})
			}
		} else {
			out = binary.LittleEndian.AppendUint64(out, m.ID)
			for _, v := range []int32{m.X, m.Y, m.HP, m.MaxHP} {
				out = binary.LittleEndian.AppendUint32(out, uint32(v))
			}
			for _, it := range m.Items {
				out = binary.LittleEndian.AppendUint32(out, it)
			}
		}
	}
	return out[:size]
}
