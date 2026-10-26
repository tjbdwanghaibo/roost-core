package physicsroom

import (
	"encoding/binary"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// recordFrame 只读本地编码器已验证的 RC2 输出，固定标签不包含房间/玩家 ID。
// 字节为 Poll 交付载荷，重试另计，不冒充 socket/TCP 实际发送字节。
func recordFrame(data []byte) {
	mode := "delta"
	if data[3] == 1 {
		mode = "full"
	}
	metrics.IncCounter("physics.room.frames.total", metrics.Labels{"mode": mode}, 1)
	offset := 36 + 8*int(binary.LittleEndian.Uint32(data[28:]))
	groups := int(binary.LittleEndian.Uint32(data[32:]))
	names := []string{"position", "rotation", "velocity", "angular_velocity", "sleep"}
	counts := make([]int64, len(names))
	for g := 0; g < groups; g++ {
		count := int(binary.LittleEndian.Uint32(data[offset+8:]))
		offset += 12
		for i := 0; i < count; i++ {
			size := int(binary.LittleEndian.Uint16(data[offset+8:]))
			offset += 10
			mask := data[offset]
			for bit := range names {
				if mask&(1<<bit) != 0 {
					counts[bit]++
				}
			}
			offset += size
		}
	}
	for i, count := range counts {
		if count > 0 {
			metrics.IncCounter("physics.room.fields.total", metrics.Labels{"field": names[i]}, count)
		}
	}
}
