package nestwal

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// U-0108 · C2（空洞测试）· nightly gap map `nestwal` 14/20。
//
// WAL 记录编解码是磁盘格式：写侧的守卫决定一条记录能不能用旧的 v1 写法落盘
// （v1 读者不认识回执、延迟生效、非 put 的变更——写进去就是旧进程起来读不
// 懂），条目数上限决定一条坏记录不能让读者分配无界内存；读侧每个字段的错误
// 分支决定截断的记录必须报错而不是拿零值继续。这些分支此前只有"patch 在 v1
// 下被拒"一条间接触达。

func v1Compatible() corenest.CommitRecord {
	record := canonicalRecord(dataengine.MutationPut)
	record.Effects[0].AvailableAt = 0
	record.Receipts = nil
	return record
}

func TestWriterV1RefusesEveryV2OnlyFeature(t *testing.T) {
	if _, err := encodeRecordVersion(v1Compatible(), WriterVersionV1); err != nil {
		t.Fatalf("baseline v1 record must encode: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*corenest.CommitRecord)
	}{
		{"receipts", func(r *corenest.CommitRecord) {
			r.Receipts = []dataengine.Receipt{{Namespace: "saga-step", ID: "step-1", Digest: []byte{3}, Payload: []byte{4}, ExpiresAt: 789}}
		}},
		{"delayed effect", func(r *corenest.CommitRecord) { r.Effects[0].AvailableAt = 456 }},
		{"non-put mutation without remote commit", func(r *corenest.CommitRecord) {
			r.Mutations[0].Kind = dataengine.MutationDelete
			r.Mutations[0].Data = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := v1Compatible()
			tc.mutate(&record)
			if _, err := encodeRecordVersion(record, WriterVersionV1); !errors.Is(err, ErrWriterVersionUnsupported) {
				t.Fatalf("v1 encode error=%v, want ErrWriterVersionUnsupported", err)
			}
			// 同一条记录 v2 必须能写，否则守卫拒绝的就不是"版本"而是"内容"。
			if _, err := encodeRecordVersion(record, WriterVersionV2); err != nil {
				t.Fatalf("v2 encode error=%v", err)
			}
		})
	}
}

func TestEncodeRejectsUnboundedUnsetPathsAndHeaders(t *testing.T) {
	record := canonicalRecord(dataengine.MutationPatch)
	unset := make([]string, maxEntryCount+1)
	for i := range unset {
		unset[i] = "f" + strconv.Itoa(i)
	}
	record.Mutations[0].Patch.Unset = unset
	if _, err := encodeRecordVersion(record, WriterVersionV2); err == nil || !strings.Contains(err.Error(), "too many unset paths") {
		t.Fatalf("unset paths over the limit: err=%v", err)
	}

	record = canonicalRecord(dataengine.MutationPut)
	headers := make(map[string]string, maxEntryCount+1)
	for i := 0; i <= maxEntryCount; i++ {
		headers[strconv.Itoa(i)] = ""
	}
	record.Effects[0].Headers = headers
	if _, err := encodeRecordVersion(record, WriterVersionV2); err == nil || !strings.Contains(err.Error(), "too many effect headers") {
		t.Fatalf("headers over the limit: err=%v", err)
	}
}

// 每一个可能的截断点都必须以错误结束：读侧任何一个字段吞掉 EOF 继续，都会
// 把半条记录当成完整记录回放。
func TestDecodeRejectsEveryTruncationPoint(t *testing.T) {
	for _, version := range []WriterVersion{WriterVersionV1, WriterVersionV2} {
		record := canonicalRecord(dataengine.MutationPut)
		if version == WriterVersionV1 {
			record = v1Compatible()
		}
		raw, err := encodeRecordVersion(record, version)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeRecord(raw); err != nil {
			t.Fatalf("v%d: complete record failed to decode: %v", version, err)
		}
		for n := 0; n < len(raw); n++ {
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("v%d: decode panicked on %d/%d bytes: %v", version, n, len(raw), recovered)
					}
				}()
				if _, err := decodeRecord(raw[:n]); err == nil {
					t.Fatalf("v%d: %d/%d bytes decoded without error", version, n, len(raw))
				}
			}()
		}
	}
}
