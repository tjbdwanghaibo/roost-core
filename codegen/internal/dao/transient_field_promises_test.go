package dao

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A1（维护者 2026-10-05：回滚统一走 DAO，docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md）：事务内会改、回滚时要恢复、
// 却不该落库也不该同步的状态放进 `dao:"nopersist,nosync"` 字段。承诺：这种字段和其他字段一样生成 mutator（undo 策略下登记逆操作），
// map 字段同样有 Get / Range / Len；它出现在回滚快照里，不出现在提交记录、存储读写与同步里。旧生成器只给 persist 或 sync 的字段
// 生成 mutator，nopersist,nosync 的标量只有 getter、map 连 getter 都没有，业务无从改它。
func TestTransientFieldsHaveMutatorsAndStayOutOfStorageAndSync(t *testing.T) {
	defs, err := parseDefDir(mustAbs(t, "./testdata/def"))
	if err != nil {
		t.Fatal(err)
	}
	var variety *DaoDef
	for i := range defs.Daos {
		if defs.Daos[i].Name == "VarietyDao" {
			variety = &defs.Daos[i]
		}
	}
	if variety == nil {
		t.Fatal("fixture has no VarietyDao")
	}
	outFile := filepath.Join(t.TempDir(), "gen_variety_dao.go")
	if _, err := generateDao(*variety, defs, "testdata", outFile, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, want := range []string{
		"func (d *VarietyDao) SetNeither(v int64)",
		"func (d *VarietyDao) SetPending(key int32, val int64)",
		"func (d *VarietyDao) DelPending(key int32)",
		"func (d *VarietyDao) GetPending(key int32) (int64, bool)",
		"func (d *VarietyDao) RangePending(",
		"func (d *VarietyDao) PendingLen() int",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the generated DAO has no %q: a nopersist,nosync field cannot be changed inside a transaction", want)
		}
	}
	bodies := generatedFunctions(source)
	if body := bodies["CaptureRollbackState"]; !strings.Contains(body, `bson:"pending"`) || !strings.Contains(body, `bson:"neither"`) {
		t.Error("the rollback snapshot does not cover the transient fields")
	}
	for _, name := range []string{"marshalCommitState", "marshalPersistData", "marshalPersistPatchBSON", "Unmarshal", "MarshalSync", "ApplySync", "markPendingKeyDirty", "markNeitherDirty"} {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("generated DAO has no %s", name)
		}
		if strings.Contains(body, "pending") || strings.Contains(body, "Pending") || strings.Contains(body, "neither") || strings.Contains(body, "Neither") {
			t.Errorf("%s touches a transient field; it must not reach storage, the commit record or sync:\n%s", name, body)
		}
	}
}

// generatedFunctions splits generated source into top-level function bodies,
// keyed by function name (methods by method name).
func generatedFunctions(source string) map[string]string {
	out := make(map[string]string)
	for _, chunk := range strings.Split(source, "\nfunc ")[1:] {
		header, _, _ := strings.Cut(chunk, "(")
		name := header
		if strings.HasPrefix(chunk, "(") {
			// method: "(d *X) Name(" — the name follows the receiver.
			_, rest, _ := strings.Cut(chunk, ") ")
			name, _, _ = strings.Cut(rest, "(")
		}
		body := chunk
		if end := strings.Index(chunk, "\n}\n"); end >= 0 {
			body = chunk[:end]
		}
		// The mark functions' own names contain the field name; check the body only.
		if _, after, ok := strings.Cut(body, "{"); ok {
			body = after
		}
		out[strings.TrimSpace(name)] = body
	}
	return out
}
