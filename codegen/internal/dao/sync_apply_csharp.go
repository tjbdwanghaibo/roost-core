package dao

import (
	"fmt"
	"strings"
)

func buildTrackedSyncCSharp(defs *Definitions) []byte {
	var b strings.Builder
	fmt.Fprintln(&b, generatedHeader)
	b.WriteString("#nullable disable\nnamespace Roost.Dao.Sync {\npublic static class DaoSyncApply {\n")
	for _, nested := range defs.Nested {
		fmt.Fprintf(&b, "private static Sync%[1]s ApplyPatch(SyncPatch%[1]s patch,Sync%[1]s previous){\nif(previous==null)throw new System.IO.InvalidDataException(\"Missing nested baseline.\");\nvar next=previous.Clone();\n", nested.Name)
		for _, f := range nested.Fields {
			name := syncCSharpProperty(toSnake(f.Name), "Sync"+nested.Name)
			pn := syncCSharpProperty(toSnake(f.Name), "SyncPatch"+nested.Name)
			wrapper := "SyncPatch" + nested.Name + f.Name + "Field"
			fmt.Fprintf(&b, "if(patch.%s!=null){\nvar baseline=new %s();\n", pn, wrapper)
			collection := f.Kind == KindMap || (f.Kind == KindSlice && !f.IsBytes())
			if collection {
				fmt.Fprintf(&b, "baseline.Value.Add(previous.%s);\n", name)
			} else {
				fmt.Fprintf(&b, "baseline.Value=previous.%s;\n", name)
			}
			fmt.Fprintf(&b, "var applied=ApplyField(patch.%s,baseline);\n", pn)
			if collection {
				fmt.Fprintf(&b, "next.%s.Clear();next.%s.Add(applied.Value);\n", name, name)
			} else {
				fmt.Fprintf(&b, "next.%s=applied.Value;\n", name)
			}
			b.WriteString("}\n")
		}
		b.WriteString("return next;}\n")
		for _, f := range nested.Fields {
			emitTrackedCSharpField(&b, defs, f, "SyncPatch"+nested.Name+f.Name+"Field")
		}
	}
	for _, dao := range defs.Daos {
		if len(filterSync(dao.Fields)) == 0 {
			continue
		}
		fmt.Fprintf(&b, `// 一个补丁全部验证成功后才交回新视图，旧视图不会部分更新。
public static Sync%[1]s Apply(this Sync%[1]s patch,Sync%[1]s previous){
 if(patch==null||patch.SyncFormat!=3)throw new System.IO.InvalidDataException("Unsupported DAO sync format.");
 if(previous!=null&&(previous.SyncFormat!=3||previous.EntityId!=patch.EntityId))throw new System.IO.InvalidDataException("DAO identity or format mismatch.");
 var next=previous==null?new Sync%[1]s():previous.Clone();next.EntityId=patch.EntityId;next.SyncFormat=3;
`, dao.Name)
		for _, f := range filterSync(dao.Fields) {
			name := syncCSharpProperty(toSnake(f.Name), "Sync"+dao.Name)
			fmt.Fprintf(&b, "if(patch.%[1]s!=null)next.%[1]s=ApplyField(patch.%[1]s,previous==null?null:previous.%[1]s);\n", name)
		}
		b.WriteString("return next;}\n")
		for _, f := range filterSync(dao.Fields) {
			emitTrackedCSharpField(&b, defs, f, "Sync"+dao.Name+f.Name+"Field")
		}
	}
	b.WriteString("}}\n")
	return []byte(b.String())
}

func emitTrackedCSharpField(b *strings.Builder, defs *Definitions, f FieldDef, wrapper string) {
	nested := syncNestedName(defs, f)
	fmt.Fprintf(b, "private static %[1]s ApplyField(%[1]s patch,%[1]s previous){\nvar update=patch.Clone();\n", wrapper)
	switch {
	case f.Kind == KindMap:
		extra := ""
		if nested != "" {
			extra = "||update.Patches.Count!=0"
		}
		fmt.Fprintf(b, "if(!update.Delta){if(update.Removed.Count!=0%s)throw new System.IO.InvalidDataException(\"Operations on replacement.\");return update;}\nif(previous==null)throw new System.IO.InvalidDataException(\"Missing map baseline.\");\nvar next=previous.Clone();\nforeach(var key in update.Removed)next.Value.Remove(key);\nforeach(var pair in update.Value)next.Value[pair.Key]=pair.Value;\n", extra)
		if nested != "" {
			b.WriteString("foreach(var pair in update.Patches){\nif(!next.Value.TryGetValue(pair.Key,out var old))throw new System.IO.InvalidDataException(\"Missing map element.\");\n")
			if f.IsPtr {
				b.WriteString("if(old.Value==null)throw new System.IO.InvalidDataException(\"Nil nested baseline.\");\nold.Value=ApplyPatch(pair.Value,old.Value);\n")
			} else {
				b.WriteString("next.Value[pair.Key]=ApplyPatch(pair.Value,old);\n")
			}
			b.WriteString("}\nnext.Patches.Clear();\n")
		}
		b.WriteString("next.Delta=false;next.Removed.Clear();return next;\n")
	case f.Kind == KindSlice && !f.IsBytes():
		extra := ""
		if nested != "" {
			extra = "||update.Patches.Count!=0"
		}
		fmt.Fprintf(b, "if(!update.Delta){if(update.Length!=0||update.Updates.Count!=0%s)throw new System.IO.InvalidDataException(\"Operations on replacement.\");return update;}\nif(previous==null||update.Length>(ulong)previous.Value.Count+(ulong)update.Updates.Count||update.Length>int.MaxValue)throw new System.IO.InvalidDataException(\"Invalid list length or missing baseline.\");\n", extra)
		b.WriteString("foreach(var pair in update.Updates)if(pair.Key>=update.Length)throw new System.IO.InvalidDataException(\"List index out of bounds.\");\n")
		if nested != "" {
			b.WriteString("foreach(var pair in update.Patches)if(pair.Key>=update.Length||pair.Key>=(ulong)previous.Value.Count||update.Updates.ContainsKey(pair.Key))throw new System.IO.InvalidDataException(\"Invalid nested list operation.\");\n")
		}
		fmt.Fprintf(b, "var baseline=previous.Clone();var next=new %s();\nfor(ulong index=0;index<update.Length;index++){\nif(update.Updates.TryGetValue(index,out var value))next.Value.Add(value);\nelse if(index<(ulong)baseline.Value.Count)next.Value.Add(baseline.Value[(int)index]);\nelse throw new System.IO.InvalidDataException(\"List append has a hole.\");}\n", wrapper)
		if nested != "" {
			b.WriteString("foreach(var pair in update.Patches){\nvar old=next.Value[(int)pair.Key];\n")
			if f.IsPtr {
				b.WriteString("if(old.Value==null)throw new System.IO.InvalidDataException(\"Nil nested baseline.\");\nold.Value=ApplyPatch(pair.Value,old.Value);\n")
			} else {
				b.WriteString("next.Value[(int)pair.Key]=ApplyPatch(pair.Value,old);\n")
			}
			b.WriteString("}\n")
		}
		b.WriteString("return next;\n")
	case nested != "":
		b.WriteString("if(update.Patch==null)return update;\nif(previous==null||previous.Value==null)throw new System.IO.InvalidDataException(\"Missing nested baseline.\");\nvar next=previous.Clone();\n")
		if f.IsPtr {
			b.WriteString("if(next.Value.Value==null)throw new System.IO.InvalidDataException(\"Nil nested baseline.\");\nnext.Value.Value=ApplyPatch(update.Patch,next.Value.Value);\n")
		} else {
			b.WriteString("next.Value=ApplyPatch(update.Patch,next.Value);\n")
		}
		b.WriteString("next.Patch=null;return next;\n")
	default:
		b.WriteString("return update;\n")
	}
	b.WriteString("}\n")
}
