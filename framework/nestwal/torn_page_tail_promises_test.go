package nestwal

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// OPEN-ITEMS B02 / C08：RR-20260926-41 判据内“跨页半写回”仍拒绝启动的形态。
//
// 最后一帧跨 4 KiB 页边界：前页（含 20 字节帧头）已写回，后页未写回、读出为 0，文件长度不变。
// 坏点是这一帧的起点，帧头 magic / 版本 / 头 CRC 都有效、帧体完整在文件长度之内，只是 payload CRC 不符；
// 自坏点至文件末尾并非全 0（帧头与前页的 payload 非零），不满足“零填充尾”，也不是“不完整尾帧”。
// 按 NEST_TRANSACTION_WAL.md §5 第 3 条“帧头有效但 CRC 不符”必须 ErrCorrupt 拒绝启动，且拒绝前不改动文件——
// 即使坏点在 checkpoint fence 之后（这一帧从未被确认）也不放宽：WAL 无法区分“后页丢写回”与真实的 payload 损坏，
// 放宽会吞掉真实的 CRC 错误（C08 维护者决定：不放宽判据，固化为测试与文档）。
func TestOpenRefusesFrameWhosePayloadPageWasNotWrittenBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ackFirst bool
	}{
		{name: "no checkpoint", ackFirst: false},
		{name: "torn frame after checkpoint fence", ackFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := testOptions(dir)
			opts.SegmentBytes = 64 << 10
			opts.MaxRecordBytes = 16 << 10
			w, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			first, err := w.Append(context.Background(), bulkyRecord(1, 3000))
			if err != nil {
				t.Fatal(err)
			}
			second, err := w.Append(context.Background(), bulkyRecord(2, 3000))
			if err != nil {
				t.Fatal(err)
			}
			if tc.ackFirst {
				if err := w.Ack(context.Background(), first); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(context.Background()); err != nil {
				t.Fatal(err)
			}

			// 前提：第二帧起点（= 第一帧的结束偏移）连同整个帧头落在第一页，帧体跨过 4096 且是最后一帧。
			if first.Segment != second.Segment {
				t.Fatalf("premise: records span segments %d/%d", first.Segment, second.Segment)
			}
			path := segmentPath(dir, second.Segment)
			if first.Offset+frameHeaderSize > tailBlockSize || second.Offset <= tailBlockSize || fileSize(t, path) != second.Offset {
				t.Fatalf("premise: second frame [%d,%d) must keep its header in the first page and cross %d; file size %d",
					first.Offset, second.Offset, tailBlockSize, fileSize(t, path))
			}
			// 模拟后页未写回：自页边界起至文件末尾清零，文件长度不变。
			patchSegment(t, dir, second.Segment, tailBlockSize, make([]byte, second.Offset-tailBlockSize))

			err = expectRefusedUntouched(t, opts, path)
			if !strings.Contains(err.Error(), "invalid frame payload checksum") {
				t.Fatalf("Open = %v, want the payload checksum refusal", err)
			}
		})
	}
}

// bulkyRecord 是带 dataBytes 字节 mutation 数据的测试记录，用来让帧跨过页边界。
func bulkyRecord(sequence byte, dataBytes int) corenest.CommitRecord {
	record := testRecord(sequence, corenest.DurabilityStrict)
	record.Mutations[0].Data = bytes.Repeat([]byte{sequence}, dataBytes)
	return record
}
