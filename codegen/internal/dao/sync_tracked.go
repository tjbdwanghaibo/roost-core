package dao

import (
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
)

func syncNestedName(defs *Definitions, field FieldDef) string {
	name := strings.TrimPrefix(field.TypeStr, "*")
	if field.Kind == KindMap {
		name = field.MapVal
	}
	if field.Kind == KindSlice {
		name = field.SliceElem
	}
	if isNestedType(defs, name) {
		return name
	}
	return ""
}
func syncMapKeyType(shape syncShape) string {
	return strings.SplitN(strings.TrimPrefix(shape.protobuf, "map["), "]", 2)[0]
}
func syncElementType(shape syncShape) string {
	if strings.HasPrefix(shape.protobuf, "map[") {
		return strings.SplitN(shape.protobuf, "]", 2)[1]
	}
	return strings.TrimPrefix(shape.protobuf, "[]")
}
func syncIsList(shape syncShape) bool {
	return strings.HasPrefix(shape.protobuf, "[]") && shape.protobuf != "[]byte"
}

// 完整值继续使用原类型；Patch/Patches 单独使用类型化的递归补丁消息。
func (s *syncSchema) trackedWrapper(name string, field FieldDef, shape syncShape) {
	fields := []protocol.FieldDef{syncField("Value", shape.protobuf, 1)}
	nested := syncNestedName(s.defs, field)
	switch {
	case field.Kind == KindMap:
		key := syncMapKeyType(shape)
		fields = append(fields, syncField("Delta", "bool", 2), syncField("Removed", "[]"+key, 3))
		if nested != "" {
			fields = append(fields, syncField("Patches", "map["+key+"]SyncPatch"+nested, 4))
		}
	case syncIsList(shape):
		fields = append(fields, syncField("Delta", "bool", 2), syncField("Updates", "map[uint64]"+syncElementType(shape), 3), syncField("Length", "uint64", 4))
		if nested != "" {
			fields = append(fields, syncField("Patches", "map[uint64]SyncPatch"+nested, 5))
		}
	case nested != "":
		fields = append(fields, syncField("Patch", "*SyncPatch"+nested, 2))
	}
	s.messages = append(s.messages, protocol.StructDef{Name: name, Fields: fields})
}

func (s *syncSchema) emitTrackedField(owner string, isDAO bool, field FieldDef, shape syncShape, wrapper string) {
	member := "value." + safeFieldVarName(field.Name)
	source := member
	if field.Kind == KindMap {
		if isDAO {
			source = "value." + mapHelperName(owner, field.Name) + "()"
		} else {
			source = member + "RawMap()"
		}
	}
	encode := func(src string) string { return fmt.Sprintf(shape.encode, toWire(s.defs, field, src)) }
	nested := syncNestedName(s.defs, field)
	var b strings.Builder
	fmt.Fprintf(&b, "func capture%s(value *%s,change *dataengine.SyncChange)*%s {\n", wrapper, owner, wrapper)
	fmt.Fprintf(&b, "full:=func()*%s{return &%s{Value:%s}}\nif change.Full(){return full()}\n", wrapper, wrapper, encode(source))
	switch {
	case field.Kind == KindMap:
		keyType := syncMapKeyType(shape)
		key := "key"
		if field.MapKey == "int" {
			key = "int64(key)"
		}
		if field.MapKey == "uint" {
			key = "uint64(key)"
		}
		fmt.Fprintf(&b, "if %s==nil || len(change.Children)>=%s.Len(){return full()}\n", member, member)
		fmt.Fprintf(&b, "patch:=&%s{Delta:true,Value:make(%s)}\n", wrapper, shape.protobuf)
		if nested != "" {
			fmt.Fprintf(&b, "patch.Patches=make(map[%s]SyncPatch%s)\n", keyType, nested)
		}
		fmt.Fprintf(&b, "for step,child:=range change.Children {key:=step.Key.(%s);item,exists:=%s.Get(key)\nif !exists{patch.Removed=append(patch.Removed,%s);continue}\n", field.MapKey, member, key)
		if nested != "" {
			condition := "!child.Full()"
			if field.IsPtr {
				condition += " && item!=nil"
			}
			fmt.Fprintf(&b, "if %s{patch.Patches[%s]=*item.captureSyncPatch(child);continue}\n", condition, key)
		} else {
			b.WriteString("_ = child\n")
		}
		fmt.Fprintf(&b, "single:=%s{%s:item};for k,v:=range %s{patch.Value[k]=v}\n}\n", rawMapType(field), "key", encode("single"))
		less := "patch.Removed[i]<patch.Removed[j]"
		if keyType == "bool" {
			less = "!patch.Removed[i]&&patch.Removed[j]"
		}
		fmt.Fprintf(&b, "if len(patch.Removed)>1{sort.Slice(patch.Removed,func(i,j int)bool{return %s})}\nreturn patch\n", less)
	case syncIsList(shape):
		fmt.Fprintf(&b, "if len(change.Children)>=len(%s){return full()}\npatch:=&%s{Delta:true,Length:uint64(len(%s)),Updates:make(map[uint64]%s)}\n", member, wrapper, member, syncElementType(shape))
		if nested != "" {
			fmt.Fprintf(&b, "patch.Patches=make(map[uint64]SyncPatch%s)\n", nested)
		}
		fmt.Fprintf(&b, "for step,child:=range change.Children{index:=step.Key.(int);item:=%s[index]\n", member)
		if nested != "" {
			condition := "!child.Full()"
			if field.IsPtr {
				condition += " && item!=nil"
			}
			fmt.Fprintf(&b, "if %s{patch.Patches[uint64(index)]=*item.captureSyncPatch(child);continue}\n", condition)
		} else {
			b.WriteString("_ = child\n")
		}
		fmt.Fprintf(&b, "single:=%s{item};patch.Updates[uint64(index)]=(%s)[0]\n}\nreturn patch\n", field.TypeStr, encode("single"))
	case nested != "":
		if field.IsPtr {
			fmt.Fprintf(&b, "if %s==nil{return full()}\n", member)
		}
		fmt.Fprintf(&b, "return &%s{Patch:%s.captureSyncPatch(change)}\n", wrapper, member)
	default:
		b.WriteString("return full()\n")
	}
	b.WriteString("}\n")
	s.helpers.WriteString(b.String())
	s.emitTrackedFieldApply(field, shape, wrapper)
}

func (s *syncSchema) emitTrackedFieldApply(field FieldDef, shape syncShape, wrapper string) {
	var b strings.Builder
	nested := syncNestedName(s.defs, field)
	fmt.Fprintf(&b, "func apply%s(patch *%s,previous %s,exists bool)(%s,error){\n", wrapper, wrapper, shape.protobuf, shape.protobuf)
	switch {
	case field.Kind == KindMap:
		fmt.Fprintf(&b, "if !patch.Delta {if len(patch.Removed)!=0%s{return nil,fmt.Errorf(\"operations on map replacement\")};return patch.Value,nil}\nif !exists{return nil,fmt.Errorf(\"missing map baseline\")}\n", patchNonempty(nested, "Patches"))
		fmt.Fprintf(&b, "next:=make(%s,len(previous)+len(patch.Value));for k,v:=range previous{next[k]=v};for _,k:=range patch.Removed{delete(next,k)};for k,v:=range patch.Value{next[k]=v}\n", shape.protobuf)
		if nested != "" {
			b.WriteString("for key,child:=range patch.Patches {old,ok:=next[key];if !ok{return nil,fmt.Errorf(\"missing map element baseline\")}\n")
			if field.IsPtr {
				b.WriteString("if old.Value==nil{return nil,fmt.Errorf(\"nil nested baseline\")};updated,err:=applySyncPatch" + nested + "(&child,*old.Value);if err!=nil{return nil,err};old.Value=&updated;next[key]=old\n")
			} else {
				fmt.Fprintf(&b, "updated,err:=applySyncPatch%s(&child,old);if err!=nil{return nil,err};next[key]=updated\n", nested)
			}
			b.WriteString("}\n")
		}
		b.WriteString("return next,nil\n")
	case syncIsList(shape):
		fmt.Fprintf(&b, "if !patch.Delta {if len(patch.Updates)!=0||patch.Length!=0%s{return nil,fmt.Errorf(\"operations on list replacement\")};return patch.Value,nil}\n", patchNonempty(nested, "Patches"))
		b.WriteString("if !exists||patch.Length>uint64(len(previous))+uint64(len(patch.Updates)){return nil,fmt.Errorf(\"invalid list length or missing baseline\")}\nfor index:=range patch.Updates{if index>=patch.Length{return nil,fmt.Errorf(\"list index out of bounds\")}}\n")
		if nested != "" {
			b.WriteString("for index:=range patch.Patches{if index>=patch.Length||index>=uint64(len(previous)){return nil,fmt.Errorf(\"missing list element baseline\")};if _,overlap:=patch.Updates[index];overlap{return nil,fmt.Errorf(\"conflicting list operations\")}}\n")
		}
		b.WriteString("for i:=uint64(len(previous));i<patch.Length;i++{if _,ok:=patch.Updates[i];!ok{return nil,fmt.Errorf(\"list append has a hole\")}}\n")
		fmt.Fprintf(&b, "next:=make(%s,int(patch.Length));copy(next,previous);for index,value:=range patch.Updates{next[index]=value}\n", shape.protobuf)
		if nested != "" {
			b.WriteString("for index,child:=range patch.Patches {old:=next[index]\n")
			if field.IsPtr {
				fmt.Fprintf(&b, "if old.Value==nil{return nil,fmt.Errorf(\"nil nested baseline\")};updated,err:=applySyncPatch%s(&child,*old.Value);if err!=nil{return nil,err};old.Value=&updated;next[index]=old\n", nested)
			} else {
				fmt.Fprintf(&b, "updated,err:=applySyncPatch%s(&child,old);if err!=nil{return nil,err};next[index]=updated\n", nested)
			}
			b.WriteString("}\n")
		}
		b.WriteString("return next,nil\n")
	case nested != "":
		fmt.Fprintf(&b, "if patch.Patch==nil{return patch.Value,nil}\nif !exists{return previous,fmt.Errorf(\"missing nested baseline\")}\n")
		if field.IsPtr {
			fmt.Fprintf(&b, "if previous.Value==nil{return previous,fmt.Errorf(\"nil nested baseline\")};next,err:=applySyncPatch%s(patch.Patch,*previous.Value);if err!=nil{return previous,err};previous.Value=&next;return previous,nil\n", nested)
		} else {
			fmt.Fprintf(&b, "return applySyncPatch%s(patch.Patch,previous)\n", nested)
		}
	default:
		b.WriteString("return patch.Value,nil\n")
	}
	b.WriteString("}\n")
	s.helpers.WriteString(b.String())
}
func patchNonempty(nested, field string) string {
	if nested == "" {
		return ""
	}
	return "||len(patch." + field + ")!=0"
}

func (s *syncSchema) buildNestedPatches() error {
	for _, nested := range s.defs.Nested {
		message := protocol.StructDef{Name: "SyncPatch" + nested.Name}
		var capture, apply strings.Builder
		fmt.Fprintf(&capture, "func(value *%s)captureSyncPatch(change *dataengine.SyncChange)*SyncPatch%s{patch:=&SyncPatch%s{}\n", nested.Name, nested.Name, nested.Name)
		fmt.Fprintf(&apply, "func applySyncPatch%s(patch *SyncPatch%s,previous Sync%s)(Sync%s,error){next:=previous\n", nested.Name, nested.Name, nested.Name, nested.Name)
		for i, field := range nested.Fields {
			shape, err := s.shape(field.TypeStr)
			if err != nil {
				return err
			}
			wrapper := message.Name + field.Name + "Field"
			s.trackedWrapper(wrapper, field, shape)
			s.emitTrackedField(nested.Name, false, field, shape, wrapper)
			message.Fields = append(message.Fields, syncField(field.Name, "*"+wrapper, i+1))
			fmt.Fprintf(&capture, "if child:=change.Child(dataengine.SyncStep{Field:%d});child!=nil{patch.%s=capture%s(value,child)}\n", i+1, field.Name, wrapper)
			fmt.Fprintf(&apply, "if patch.%[1]s!=nil{value,err:=apply%[2]s(patch.%[1]s,previous.%[1]s,true);if err!=nil{return previous,err};next.%[1]s=value}\n", field.Name, wrapper)
		}
		capture.WriteString("return patch}\n")
		apply.WriteString("return next,nil}\n")
		s.helpers.WriteString(capture.String())
		s.helpers.WriteString(apply.String())
		s.messages = append(s.messages, message)
	}
	return nil
}

func (s *syncSchema) emitDAOTracked(dao DaoDef, shapes map[string]syncShape) {
	var capture, apply strings.Builder
	name := "Sync" + dao.Name
	fmt.Fprintf(&capture, `// MarshalSyncChanges 只读取 setter 标记的路径；不编码、解码或比较整份历史快照。
func(d *%[1]s) MarshalSyncChanges(mask uint64)[]byte {
 return d.tracker.ReadSyncChanges(func(change *dataengine.SyncChange) []byte {
 if change.Full(){return d.marshalSyncPB(mask)}
 patch:=&%[2]s{EntityID:d.id,SyncFormat:3};changed:=false
`, dao.Name, name)
	fmt.Fprintf(&apply, `// Apply%[1]s 验证整个补丁后发布新快照，previous 与 patch 均按只读使用。
func Apply%[1]s(patch,previous *%[1]s)(*%[1]s,error){
 if patch==nil||patch.SyncFormat!=3{return nil,fmt.Errorf("unsupported DAO sync format")}
 if previous!=nil&&(previous.SyncFormat!=3||previous.EntityID!=patch.EntityID){return nil,fmt.Errorf("sync baseline identity or format mismatch")}
 next:=&%[1]s{EntityID:patch.EntityID,SyncFormat:3};if previous!=nil{*next=*previous}
`, name)
	for i, field := range dao.Fields {
		if !field.Tag.Sync || field.Tag.Skip {
			continue
		}
		shape := shapes[field.Name]
		wrapper := name + field.Name + "Field"
		s.emitTrackedField(dao.Name, true, field, shape, wrapper)
		fmt.Fprintf(&capture, "if mask&%s!=0{patch.%s=capture%s(d,change.Child(dataengine.SyncStep{Field:%d}));changed=true}\n", fieldMaskName(dao.Name, field.Name), field.Name, wrapper, i+2)
		fmt.Fprintf(&apply, "if patch.%[1]s!=nil{var old %[3]s;exists:=previous!=nil&&previous.%[1]s!=nil;if exists{old=previous.%[1]s.Value};value,err:=apply%[2]s(patch.%[1]s,old,exists);if err!=nil{return nil,err};next.%[1]s=&%[2]s{Value:value}}\n", field.Name, wrapper, shape.protobuf)
	}
	fmt.Fprintf(&capture, "if !changed{return nil};raw,err:=Marshal%s(patch);if err!=nil{panic(err)};return raw})}\n", name)
	apply.WriteString("return next,nil}\n")
	s.helpers.WriteString(capture.String())
	s.helpers.WriteString(apply.String())
}
