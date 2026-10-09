package entity

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// B2 结构守卫（v1.23.0 发版文档 REM-1 疑点 1 闭环）：RemoteSnapshotCache 的 L1 / L2 写入点是一张封闭的表。
//
// B2 的不变量是“写进 L1 的值要么被 L2 接受过（admitLocked），要么是刚从 L2 / 权威读到的值，要么是删除；
// 一个 key 的全部写入都在它的 publish 分片锁下”。行为用例（TestB2*、首载缓冲矩阵）只覆盖现有路径，挡不住
// 以后新加一处直接写 L1 的绕行。这里按源码 AST 检查：
//
//  1. 直接写 L1（setL1Locked、l1 上除读以外的方法）与直接写 L2（l2 上除 Get 以外的方法）只出现在下表列出的
//     函数里，每处写明为什么不经 admitLocked；表里的函数不再写时也报错，表随源码一起收缩。
//  2. 需要分片锁的 helper（*Locked 方法与 writeShared）只被 *Locked 方法或自己取 publishMu 的函数调用；
//     直接写 L1 / L2 的函数同样要么是 *Locked，要么自己取 publishMu。
//
// 新增写入点：先想清楚它为什么不能经 admitLocked，再把理由加进表里，并同步类型注释（remote_snapshot.go）。
var remoteSnapshotDirectWriters = map[string]string{
	"setL1Locked":       "L1 写入原语：AtomicLocal 的 SetWithTTL，L1 自己的 Stale / Conflict 准入在这里执行",
	"admitLocked":       "唯一的“先 L2 后 L1”写入口：按 L2 的判定调用 setL1Locked",
	"writeShared":       "admitLocked 的 L2 半步：快照版本 CAS、带版本删除（没有该能力时退化为无条件删除）",
	"adoptSharedLocked": "L2 以 stale 拒绝之后：记下刚从 L2 读到的值；L2 已没有活值时删掉不比被拒写入新的 L1 快照",
	"refresh":           "重新确认：记下刚从 L2 读到的值、同值改记确认时刻、L2 没有活值时给删除标记改记确认时刻；L1 比 L2 新时经 admitLocked",
	"loadForRefresh":    "权威说不存在：删掉加载开始之前确认的 L1 快照（不写 L2，L2 本就没有活值）",
	"Delete":            "不带版本的失效（旧发布者）：L1 与 L2 都删、不留水位；带版本的删除走 DeleteAtVersion → admitLocked",
}

// 需要调用方持有 publish 分片锁的 helper（除 *Locked 方法外）。
var remoteSnapshotLockedHelpers = map[string]bool{"writeShared": true}

var remoteSnapshotL1Reads = map[string]bool{"Get": true, "Stats": true, "Len": true, "Bytes": true}

func TestRemoteSnapshotCacheWritesStayInTheListedFunctions(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	writers := map[string][]string{}
	var violations []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			onCache := snapshotGuardReceiverIs(fn, "RemoteSnapshotCache")
			takesLock := snapshotGuardTakesPublishMu(fn.Body)
			locked := onCache && strings.HasSuffix(fn.Name.Name, "Locked")
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pos := fset.Position(call.Pos())
				site := fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
				method := sel.Sel.Name
				// 直接写 L1 / L2。
				if inner, ok := sel.X.(*ast.SelectorExpr); ok {
					store := inner.Sel.Name
					if (store == "l1" && !remoteSnapshotL1Reads[method]) || (store == "l2" && method != "Get") {
						writers[fn.Name.Name] = append(writers[fn.Name.Name], site)
						if !locked && !takesLock && !remoteSnapshotLockedHelpers[fn.Name.Name] {
							violations = append(violations, site+": "+fn.Name.Name+" writes "+store+"."+method+" without the publish shard lock")
						}
					}
				}
				if method == "setL1Locked" {
					writers[fn.Name.Name] = append(writers[fn.Name.Name], site)
				}
				// 需要分片锁的 helper 的调用方。
				if _, isIdent := sel.X.(*ast.Ident); isIdent && (strings.HasSuffix(method, "Locked") || remoteSnapshotLockedHelpers[method]) {
					if onCache && !locked && !takesLock {
						violations = append(violations, site+": "+fn.Name.Name+" calls "+method+" without taking publishMu")
					}
				}
				return true
			})
		}
	}
	for fn, sites := range writers {
		if _, ok := remoteSnapshotDirectWriters[fn]; !ok {
			violations = append(violations, strings.Join(sites, ", ")+": "+fn+" writes the snapshot cache directly; route it through admitLocked or list it with a reason")
		}
	}
	for fn := range remoteSnapshotDirectWriters {
		if _, ok := writers[fn]; !ok {
			violations = append(violations, "listed writer "+fn+" no longer writes the snapshot cache; remove it from remoteSnapshotDirectWriters")
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

func snapshotGuardReceiverIs(fn *ast.FuncDecl, typeName string) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == typeName
}

// snapshotGuardTakesPublishMu 报告函数体是否取 publish 分片锁（引用 publishMu 并调用 Lock）。
func snapshotGuardTakesPublishMu(body *ast.BlockStmt) bool {
	var mu, lock bool
	ast.Inspect(body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "publishMu":
				mu = true
			case "Lock":
				lock = true
			}
		}
		return true
	})
	return mu && lock
}
