package cfggen

import (
	"strings"
	"testing"
)

// U-0151 · C2 · gap map codegen `internal/cfggen` 1/20：一个 bean 的字段全被目标组排除时拒绝生成，而不是产出空结构体。
func TestExportRefusesABeanWithEveryFieldExcluded(t *testing.T) {
	meta := `package: cfg
groups:
  names: [c, s]
  target: [s]
beans:
  - name: Icon
    fields:
      - { name: path, type: string, group: c }
tables:
  - name: monster
    key: id
    fields:
      - { name: id, type: int32 }
`
	_, _, err := runCfggenArgs(t, meta)
	if err == nil || !strings.Contains(err.Error(), "bean Icon: every field is excluded from target groups") {
		t.Fatalf("export with an emptied bean = %v", err)
	}
}
