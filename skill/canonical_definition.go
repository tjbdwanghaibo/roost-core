package skill

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"sort"
)

// canonicalDefinitionDigest 是源文档 digest（ProgramIdentityView.SourceDocumentDigest）：Definition 的规范表示的摘要。
//
// Definition 是封闭的 Wire 树，规范表示逐字段写出整棵树：
//   - 接口值（effect、flow、value、activation、cast policy、input schema、select 的 consume / shape / filter、
//     motion 各部分）先写具体类型名，再写值——具体类型就是文档里的 "type" / "flow" / "mode" 判别字段；
//   - 结构体写类型名与全部字段（字段名 + 值），不看 json tag，未导出字段同样写出；
//   - map 按 key 的规范表示排序；nil 指针 / 切片 / map / 接口与空值区分；每段都带长度前缀，不会两段拼成同一串字节。
//
// 之前是 json.Marshal(Definition) 的摘要（RR-20261006-33）：接口值只写出具体类型的字段、不写类型，字段完全相同只差
// 类型的两个定义（set_memory / add_memory、hold / toggle 策略、entity / direction 输入、line / rectangle 形状等）摘要
// 相同；带 `json:"-"` 的字段（Cost.Amount、cast window 的 windup / recovery 表达式）整个被跳过，只改消耗数量摘要也不变。
// Go 类型名、字段名改名会改变这个摘要（json 编码时字段名同样在摘要里），这是改名的直接后果。
func canonicalDefinitionDigest(definition *Definition) string {
	var encoder canonicalEncoder
	encoder.value(reflect.ValueOf(definition))
	return stableDigest("roost.skill/v2/source-document", encoder.bytes)
}

type canonicalEncoder struct{ bytes []byte }

func (encoder *canonicalEncoder) tag(tag byte) { encoder.bytes = append(encoder.bytes, tag) }

func (encoder *canonicalEncoder) text(text string) {
	encoder.bytes = binary.AppendUvarint(encoder.bytes, uint64(len(text)))
	encoder.bytes = append(encoder.bytes, text...)
}

func (encoder *canonicalEncoder) number(value uint64) {
	encoder.bytes = binary.BigEndian.AppendUint64(encoder.bytes, value)
}

func (encoder *canonicalEncoder) value(value reflect.Value) {
	switch value.Kind() {
	case reflect.Invalid:
		encoder.tag('0')
	case reflect.Bool:
		encoder.tag('b')
		if value.Bool() {
			encoder.number(1)
		} else {
			encoder.number(0)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		encoder.tag('i')
		encoder.number(uint64(value.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		encoder.tag('u')
		encoder.number(value.Uint())
	case reflect.Float32, reflect.Float64:
		encoder.tag('f')
		encoder.number(math.Float64bits(value.Float()))
	case reflect.String:
		encoder.tag('s')
		encoder.text(value.String())
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			encoder.tag('0')
			return
		}
		if value.Kind() == reflect.Interface {
			encoder.tag('I')
			encoder.text(value.Elem().Type().String())
		} else {
			encoder.tag('P')
		}
		encoder.value(value.Elem())
	case reflect.Struct:
		encoder.tag('S')
		encoder.text(value.Type().String())
		encoder.number(uint64(value.NumField()))
		for index := range value.NumField() {
			encoder.text(value.Type().Field(index).Name)
			encoder.value(value.Field(index))
		}
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			encoder.tag('0')
			return
		}
		encoder.tag('L')
		encoder.number(uint64(value.Len()))
		for index := range value.Len() {
			encoder.value(value.Index(index))
		}
	case reflect.Map:
		if value.IsNil() {
			encoder.tag('0')
			return
		}
		type entry struct{ key, value []byte }
		entries := make([]entry, 0, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			var key, item canonicalEncoder
			key.value(iterator.Key())
			item.value(iterator.Value())
			entries = append(entries, entry{key: key.bytes, value: item.bytes})
		}
		sort.Slice(entries, func(left, right int) bool { return string(entries[left].key) < string(entries[right].key) })
		encoder.tag('M')
		encoder.number(uint64(len(entries)))
		for _, entry := range entries {
			encoder.bytes = append(encoder.bytes, entry.key...)
			encoder.bytes = append(encoder.bytes, entry.value...)
		}
	default:
		// 封闭的 Wire 树里没有函数、通道、复数、unsafe 指针；出现说明 Definition 加了不能规范化的字段。
		panic(fmt.Sprintf("skill: source document has a %s field that has no canonical form", value.Type()))
	}
}

func stableDigest(domain string, payload []byte) string {
	hash := sha256.New()
	writeDigestPart := func(part []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(part)
	}
	writeDigestPart([]byte(domain))
	writeDigestPart(payload)
	return hex.EncodeToString(hash.Sum(nil))
}
