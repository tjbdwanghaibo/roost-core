//go:build !race

// race 模式下 sync.Pool 会随机丢弃对象，分配量不代表正常构建，故只在非 race 构建运行。
package httpserver

import (
	"net/http"
	"strings"
	"testing"
)

type discardResponse struct{ header http.Header }

func (d *discardResponse) Header() http.Header         { return d.header }
func (d *discardResponse) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardResponse) WriteHeader(int)             {}

// RR-20261005-NC-80 复核：先编码后写状态不能为每个成功响应再复制一份响应体。
// json.Encoder 已在池化缓冲里完整编码后一次写出，JSON 应直接复用它，
// 成功响应的堆分配与响应体大小无关。
func TestJSONDoesNotCopyTheEncodedBodyPerResponse(t *testing.T) {
	type row struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	rows := make([]row, 4096)
	for i := range rows {
		rows[i] = row{ID: int64(i), Name: strings.Repeat("x", 48)}
	}
	value := map[string]any{"ok": true, "data": rows}
	result := testing.Benchmark(func(b *testing.B) {
		w := &discardResponse{header: http.Header{}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			JSON(w, http.StatusOK, value)
		}
	})
	const bodyBytes = 4096 * 60 // 每行编码后约 70 字节，取保守下界
	if perOp := result.AllocedBytesPerOp(); perOp > bodyBytes/8 {
		t.Fatalf("JSON allocated %d bytes per %d-byte response, want it independent of the body size (< %d)", perOp, bodyBytes, bodyBytes/8)
	}
}
