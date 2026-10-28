package roost

import (
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
)

// roost add saga -native 的回执部分（设计 docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md §5.1，决定 2A）：
// 原生步骤的回执放在被改的实体里。add saga 给实体的主 DAO 加一个字段、给实体加一个适配文件，之后两样都归业务：
//
//	db/def/<entity>.go（实体主 DAO 的定义）            加 SagaReceipts map[string][]byte（persist、不同步）
//	game/entities/<entity>/saga_receipts.go            SagaStepReceipts()：把 DAO 生成的 Get/Set/Del/Range 接到 saga.StepReceipts
//
// 同一实体上的第二个原生 saga 不再改动（字段与适配已在）。生成的 Nest 分发包装（codegen/internal/nest）在原生步骤 handler 的
// 业务函数之前调用 step.Begin(第一个锁目标)，锁目标的具体实体类型就是这里加了 SagaStepReceipts 的那个。

const (
	sagaReceiptsField = "SagaReceipts"
	sagaReceiptsType  = "map[string][]byte"
)

// sagaReceiptsFile 是实体的回执适配文件（业务文件，只在缺失时创建）。
func sagaReceiptsFile(entitySnake string) string {
	return "game/entities/" + entitySnake + "/saga_receipts.go"
}

// resolveSagaReceiptHost 核对实体能放回执（不是 remote=managed / noPersist）并选它的主 DAO。
func resolveSagaReceiptHost(root, sagaSnake string, target *sagaHandlerTarget) error {
	dir := filepath.Join(root, "game", "entities", target.entity)
	host, err := entity.InspectReceiptHost(dir, target.entityPascal)
	if err != nil {
		return fmt.Errorf("saga %s -native: %w", sagaSnake, err)
	}
	dao, err := entityMainDao(target.entity, host, "the native steps' receipts")
	if err != nil {
		return fmt.Errorf("saga %s -native: %w", sagaSnake, err)
	}
	target.dao, target.daoGetter = dao, timerDaoGetter(target.entityPascal, dao)
	return nil
}

// planSagaReceipts 是回执字段与适配文件的改动：字段已在（类型相同）就不改 DAO 定义，适配文件已在就不重写。
func planSagaReceipts(root, module string, target sagaHandlerTarget) ([]syncChange, error) {
	var changes []syncChange
	daoPath, before, after, err := addSagaReceiptsField(root, target.dao)
	if err != nil {
		return nil, err
	}
	if string(before) != string(after) {
		changes = append(changes, syncChange{rel: relativeSlash(root, daoPath), path: daoPath, before: before, body: after, existed: true})
	}
	rel := sagaReceiptsFile(target.entity)
	abs := filepath.Join(root, filepath.FromSlash(rel))
	switch _, err := os.Stat(abs); {
	case err == nil:
		return changes, nil
	case !os.IsNotExist(err):
		return nil, err
	}
	body, err := format.Source([]byte(renderSagaReceipts(module, target)))
	if err != nil {
		return nil, fmt.Errorf("format %s: %w", rel, err)
	}
	return append(changes, syncChange{rel: rel, path: abs, body: body}), nil
}

// addSagaReceiptsField 在 DAO 定义的结构体末尾加回执字段（文本插在右括号前，保留业务注释）。字段已按同一类型存在时不改；
// 同名不同类型拒绝。
func addSagaReceiptsField(root, daoName string) (string, []byte, []byte, error) {
	dir := filepath.Join(root, "db", "def")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, nil, fmt.Errorf("read DAO definitions: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", nil, nil, err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, raw, parser.ParseComments)
		if err != nil {
			return "", nil, nil, fmt.Errorf("parse %s: %w", relativeSlash(root, path), err)
		}
		structure := findStruct(file, daoName)
		if structure == nil {
			continue
		}
		for _, field := range structure.Fields.List {
			for _, ident := range field.Names {
				if ident.Name != sagaReceiptsField {
					continue
				}
				if got := exprString(fset, field.Type); got != sagaReceiptsType {
					return "", nil, nil, fmt.Errorf("%s: %s.%s already exists as %s; the saga native steps keep their receipts in it as %s", relativeSlash(root, path), daoName, sagaReceiptsField, got, sagaReceiptsType)
				}
				return path, raw, raw, nil
			}
		}
		closing := fset.Position(structure.Fields.Closing).Offset
		field := `
	// SagaReceipts 是 saga 原生步骤的回执（roost add saga -native 加入）：操作实例（Command.IdempotencyKey）→ 框架编码的
	// saga.StepReceipt。只在实体的 Nest 锁内经 SagaStepReceipts（saga_receipts.go）读写，随步骤的事务提交 / 回滚；不同步给客户端，
	// 过了 saga.receipt_retention 的由原生步骤顺手剪掉。
	SagaReceipts map[string][]byte ` + "`bson:\"saga_receipts\" dao:\"persist\"`" + `
`
		edited := append(append(append([]byte(nil), raw[:closing]...), []byte(field)...), raw[closing:]...)
		formatted, err := format.Source(edited)
		if err != nil {
			return "", nil, nil, fmt.Errorf("format %s: %w", relativeSlash(root, path), err)
		}
		return path, raw, formatted, nil
	}
	return "", nil, nil, fmt.Errorf("DAO %s is not defined in db/def; the saga native steps keep their receipts in it", daoName)
}

// renderSagaReceipts 是实体的回执适配（业务文件）：SagaStepReceipts 让原生步骤 handler 的锁目标实现 saga.StepReceiptHost。
func renderSagaReceipts(module string, target sagaHandlerTarget) string {
	receiver := strings.ToLower(target.entityPascal[:1]) + target.entityPascal[1:] + "SagaReceipts"
	return fmt.Sprintf(`package %[1]s

import "github.com/tjbdwanghaibo/roost-core/framework/saga"

// SagaStepReceipts gives the saga native steps that lock a %[2]s the receipts
// kept in %[3]s.SagaReceipts (written by roost add saga -native; a business file
// afterwards). The generated Nest dispatch of a native step handler calls
// step.Begin with the handler's first lock target before the business code:
// it looks the operation's receipt up here, inside the entity's Nest lock, and
// replays it instead of running the business again. step.Complete writes the
// receipt through the same accessors, so it commits and rolls back with the
// step's transaction (roost-core SAGA.md, 原生步骤执行契约).
func (e *%[2]s) SagaStepReceipts() saga.StepReceipts { return %[4]s{owner: e} }

// %[4]s adapts the DAO's generated map accessors to saga.StepReceipts. The
// values are the framework's encoding of saga.StepReceipt; do not change them.
type %[4]s struct{ owner *%[2]s }

func (r %[4]s) StepReceipt(operation string) ([]byte, bool) {
	return r.owner.%[5]s().GetSagaReceipts(operation)
}

func (r %[4]s) PutStepReceipt(operation string, encoded []byte) {
	r.owner.%[5]s().SetSagaReceipts(operation, encoded)
}

func (r %[4]s) DeleteStepReceipt(operation string) {
	r.owner.%[5]s().DelSagaReceipts(operation)
}

func (r %[4]s) RangeStepReceipts(f func(operation string, encoded []byte) bool) {
	r.owner.%[5]s().RangeSagaReceipts(f)
}
`, target.entity, target.entityPascal, target.dao, receiver, target.daoGetter)
}
