package dao

import (
	"fmt"
	"strings"
)

// 生成的递归读取器只遍历本事务记录的路径，BSON 字段名仍取定义中的 Key。
// 整个容器回退必须在写入任何子路径之前决定，避免 Mongo 的父子更新冲突。
func persistPathMethods(defs *Definitions, owner string, fields []FieldDef, dao bool) string {
	var b strings.Builder
	if !dao {
		fmt.Fprintf(&b, "func (value *%s) appendPersistPaths(set map[string]any, unset *[]string, path string, change *dataengine.SyncChange) {\n", owner)
		for i, f := range fields {
			fmt.Fprintf(&b, "if child:=change.Child(dataengine.SyncStep{Field:%d});child!=nil{value.append%sPersist(set,unset,path+%q,child)}\n", i+1, f.Name, "."+f.Key)
		}
		b.WriteString("}\n")
	}
	for _, f := range fields {
		if dao && !f.Tag.Persist {
			continue
		}
		member := "value." + safeFieldVarName(f.Name)
		source := member
		if f.Kind == KindMap {
			if dao {
				source = "value." + mapHelperName(owner, f.Name) + "()"
			} else {
				source = member + "RawMap()"
			}
		}
		full := fmt.Sprintf("set[path]=%s;return\n", toWire(defs, f, source))
		fmt.Fprintf(&b, "func (value *%s) append%sPersist(set map[string]any,unset *[]string,path string,change *dataengine.SyncChange){\nif change.Full(){%s}\n", owner, f.Name, full)
		nested := syncNestedName(defs, f)
		switch {
		case f.Kind == KindMap:
			fmt.Fprintf(&b, "if %s==nil||change.ReplacementCount()>=%s.Len(){%s}\n", member, member, full)
			fmt.Fprintf(&b, "for step:=range change.Children{if _,ok:=dataengine.MapPatchPath(path,step.Key);!ok{%s}}\n", full)
			fmt.Fprintf(&b, "for step,child:=range change.Children {key:=step.Key.(%s);target,_:=dataengine.MapPatchPath(path,key);item,exists:=%s.Get(key);if !exists{*unset=append(*unset,target);continue}\n", f.MapKey, member)
			emitPersistElement(&b, nested, f.IsPtr, "item", "target", "child")
			b.WriteString("}\n")
		case f.Kind == KindSlice && !f.IsBytes():
			fmt.Fprintf(&b, "if change.ReplacementCount()>=len(%s){%s}\n", member, full)
			fmt.Fprintf(&b, "for step:=range change.Children{index:=step.Key.(int);if index<0||index>=len(%s){%s}}\n", member, full)
			fmt.Fprintf(&b, "for step,child:=range change.Children{index:=step.Key.(int);target,_:=dataengine.MapPatchPath(path,index);item:=%s[index]\n", member)
			emitPersistElement(&b, nested, f.IsPtr, "item", "target", "child")
			b.WriteString("}\n")
		case nested != "":
			if f.IsPtr {
				fmt.Fprintf(&b, "if %s==nil{%s}\n", member, full)
			}
			fmt.Fprintf(&b, "%s.appendPersistPaths(set,unset,path,change)\n", member)
		default:
			b.WriteString(full)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

func emitPersistElement(b *strings.Builder, nested string, pointer bool, item, path, change string) {
	if nested == "" {
		fmt.Fprintf(b, "_=%s;set[%s]=%s\n", change, path, item)
		return
	}
	condition := "!" + change + ".Full()"
	if pointer {
		condition += "&&" + item + "!=nil"
	}
	fmt.Fprintf(b, "if %s{%s.appendPersistPaths(set,unset,%s,%s)}else{\n", condition, item, path, change)
	if pointer {
		fmt.Fprintf(b, "set[%s]=%sPtrBSONDoc(%s)\n", path, lower1(nested), item)
	} else {
		fmt.Fprintf(b, "set[%s]=%s.bsonDoc()\n", path, item)
	}
	b.WriteString("}\n")
}
