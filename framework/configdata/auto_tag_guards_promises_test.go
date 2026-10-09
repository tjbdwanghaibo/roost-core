package configdata

import (
	"strings"
	"testing"
)

// U-0149 · C2 · gap map core `configdata` 2/20：两个同深度嵌入字段在 json 名上打平且带 cfg 标签时拒绝
// （否则 encoding/json 会悄悄丢掉它）；skipempty 必须与 index 同用。
func TestAutoTableRefusesTaggedTiesAndSkipEmptyWithoutIndex(t *testing.T) {
	type left struct {
		ID int32 `cfg:"key"`
	}
	type right struct {
		ID int32
	}
	type tied struct {
		left
		right
		Key int32 `json:"key" cfg:"key"`
	}
	if err := RegisterAutoTable[int32, tied](NewRegistry()); err == nil || !strings.Contains(err.Error(), "tie on json name") || !strings.Contains(err.Error(), "carries a cfg tag") {
		t.Fatalf("tied embedded fields with a cfg tag = %v", err)
	}
	// 镜像顺序：带标签的嵌入字段后到，走的是"后来者带标签"那条检查。
	type tiedLater struct {
		right
		left
		Key int32 `json:"key" cfg:"key"`
	}
	if err := RegisterAutoTable[int32, tiedLater](NewRegistry()); err == nil || !strings.Contains(err.Error(), "tie on json name") || !strings.Contains(err.Error(), "carries a cfg tag") {
		t.Fatalf("tied embedded fields with the tagged one second = %v", err)
	}
	type skipEmptyOnly struct {
		ID   int32  `json:"id" cfg:"key"`
		Name string `json:"name" cfg:"skipempty"`
	}
	if err := RegisterAutoTable[int32, skipEmptyOnly](NewRegistry()); err == nil || !strings.Contains(err.Error(), "skipempty requires index") {
		t.Fatalf("skipempty without index = %v", err)
	}
	type indexed struct {
		ID   int32  `json:"id" cfg:"key"`
		Name string `json:"name" cfg:"index=name,skipempty"`
	}
	if err := RegisterAutoTable[int32, indexed](NewRegistry()); err != nil {
		t.Fatalf("skipempty with index refused: %v", err)
	}
}
