package nestwal

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
)

// 条目上限与截断检查保证坏记录不能越过编解码边界。

func TestEncodeRejectsUnboundedUnsetPathsAndHeaders(t *testing.T) {
	record := canonicalRecord(dataengine.MutationPatch)
	unset := make([]string, maxEntryCount+1)
	for i := range unset {
		unset[i] = "f" + strconv.Itoa(i)
	}
	record.Mutations[0].Patch.Unset = unset
	if _, err := encodeRecord(record); err == nil || !strings.Contains(err.Error(), "too many unset paths") {
		t.Fatalf("unset paths over the limit: err=%v", err)
	}

	record = canonicalRecord(dataengine.MutationPut)
	headers := make(map[string]string, maxEntryCount+1)
	for i := 0; i <= maxEntryCount; i++ {
		headers[strconv.Itoa(i)] = ""
	}
	record.Effects[0].Headers = headers
	if _, err := encodeRecord(record); err == nil || !strings.Contains(err.Error(), "too many effect headers") {
		t.Fatalf("headers over the limit: err=%v", err)
	}
}

// 每一个可能的截断点都必须以错误结束：读侧任何一个字段吞掉 EOF 继续，都会
// 把半条记录当成完整记录回放。
func TestDecodeRejectsEveryTruncationPoint(t *testing.T) {
	raw, err := encodeRecord(canonicalRecord(dataengine.MutationPut))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRecord(raw); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(raw); n++ {
		if _, err := decodeRecord(raw[:n]); err == nil {
			t.Fatalf("%d/%d bytes decoded without error", n, len(raw))
		}
	}
}
