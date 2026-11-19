package physicsroom

import (
	"errors"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	"google.golang.org/protobuf/encoding/protowire"
)

// MaxLevelViewBytes 是一份编码后的流程公开状态的上限。公开变量太多、太长以至于超过它时，
// 流程按出错处理（房间停止）：这份状态要随帧发给每个成员。
const MaxLevelViewBytes = 16 << 10

// LevelView 是下发给客户端的流程公开状态，即 Protobuf RoomLevelView（client/physics-viewer/room_input.proto）：
//
//	message RoomLevelView { uint64 revision=1; string phase=2; string result=3;
//	                        repeated Stage stages=4; repeated Variable variables=5; }
//	message Stage    { string id=1; uint64 generation=2; }
//	message Variable { string key=1; oneof value { int64 int=2; bool flag=3; string text=4; } }
//
// 只有 level.View 里对客户端有意义的部分：实例名与 Epoch 不下发。Revision 在一条连接上只增加：
// 客户端收到相同的保留手上的，收到更小的按协议错误断开（client/physics-viewer RoomLevelView.Advance）。
type LevelView struct {
	Revision  uint64
	Phase     level.Phase
	Result    string
	Stages    []level.StageView
	Variables map[string]level.Value
}

// EncodeLevelView 的输出是确定的：阶段按 level.View 给的次序（按 ID），变量按键排序，
// 每个变量恰好带 oneof 里的一项（值为零也写出，用来区分类型）。
func EncodeLevelView(v level.View) ([]byte, error) {
	out := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), v.Revision)
	out = protowire.AppendString(protowire.AppendTag(out, 2, protowire.BytesType), string(v.Phase))
	if v.Result != "" {
		out = protowire.AppendString(protowire.AppendTag(out, 3, protowire.BytesType), v.Result)
	}
	for _, stage := range v.Stages {
		item := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), stage.ID)
		item = protowire.AppendVarint(protowire.AppendTag(item, 2, protowire.VarintType), stage.Generation)
		out = protowire.AppendBytes(protowire.AppendTag(out, 4, protowire.BytesType), item)
	}
	keys := make([]string, 0, len(v.Variables))
	for key := range v.Variables {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		item := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), key)
		switch value := v.Variables[key]; value.Kind {
		case level.Integer:
			item = protowire.AppendVarint(protowire.AppendTag(item, 2, protowire.VarintType), uint64(value.Int))
		case level.Boolean:
			item = protowire.AppendVarint(protowire.AppendTag(item, 3, protowire.VarintType), protowire.EncodeBool(value.Bool))
		case level.Text:
			item = protowire.AppendString(protowire.AppendTag(item, 4, protowire.BytesType), value.Text)
		default:
			return nil, errors.New("physicsroom: level view variable of unknown kind")
		}
		out = protowire.AppendBytes(protowire.AppendTag(out, 5, protowire.BytesType), item)
	}
	if len(out) > MaxLevelViewBytes {
		return nil, errors.New("physicsroom: level view exceeds its byte limit")
	}
	return out, nil
}

// DecodeLevelView 解码 RoomLevelView，供 Go 客户端与测试使用。未知字段、类型不符、重复的标量字段、
// 缺少阶段名、变量缺值或带多个值、同名的阶段或变量一律拒绝。
func DecodeLevelView(data []byte) (LevelView, error) {
	bad := errors.New("physicsroom: malformed level view")
	if len(data) > MaxLevelViewBytes {
		return LevelView{}, bad
	}
	view := LevelView{Variables: map[string]level.Value{}}
	seen := map[protowire.Number]bool{}
	stages := map[string]bool{}
	text := func(b []byte) (string, []byte, bool) {
		v, n := protowire.ConsumeBytes(b)
		return string(v), b[max(n, 0):], n >= 0
	}
	for len(data) > 0 {
		field, typ, n := protowire.ConsumeTag(data)
		if n < 0 || field < 1 || field > 5 || field == 1 && typ != protowire.VarintType || field != 1 && typ != protowire.BytesType || field <= 3 && seen[field] {
			return LevelView{}, bad
		}
		seen[field] = true
		data = data[n:]
		if field == 1 {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return LevelView{}, bad
			}
			view.Revision, data = v, data[n:]
			continue
		}
		body, rest, ok := text(data)
		if !ok {
			return LevelView{}, bad
		}
		data = rest
		switch field {
		case 2:
			view.Phase = level.Phase(body)
		case 3:
			view.Result = body
		case 4:
			stage, err := decodeStage([]byte(body))
			if err != nil || stages[stage.ID] {
				return LevelView{}, bad
			}
			stages[stage.ID] = true
			view.Stages = append(view.Stages, stage)
		case 5:
			key, value, err := decodeVariable([]byte(body))
			if _, repeated := view.Variables[key]; err != nil || repeated {
				return LevelView{}, bad
			}
			view.Variables[key] = value
		}
	}
	if view.Phase == "" {
		return LevelView{}, bad
	}
	return view, nil
}

func decodeStage(data []byte) (level.StageView, error) {
	bad := errors.New("physicsroom: malformed level view stage")
	var stage level.StageView
	var seen [3]bool
	for len(data) > 0 {
		field, typ, n := protowire.ConsumeTag(data)
		if n < 0 || field < 1 || field > 2 || seen[field] {
			return stage, bad
		}
		seen[field] = true
		data = data[n:]
		if field == 1 && typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return stage, bad
			}
			stage.ID, data = string(v), data[n:]
		} else if field == 2 && typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return stage, bad
			}
			stage.Generation, data = v, data[n:]
		} else {
			return stage, bad
		}
	}
	if stage.ID == "" {
		return stage, bad
	}
	return stage, nil
}

func decodeVariable(data []byte) (string, level.Value, error) {
	bad := errors.New("physicsroom: malformed level view variable")
	var key string
	var value level.Value
	var seen [5]bool
	for len(data) > 0 {
		field, typ, n := protowire.ConsumeTag(data)
		if n < 0 || field < 1 || field > 4 || seen[field] || (field == 1 || field == 4) != (typ == protowire.BytesType) || (field == 2 || field == 3) != (typ == protowire.VarintType) {
			return key, value, bad
		}
		seen[field] = true
		data = data[n:]
		if typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return key, value, bad
			}
			data = data[n:]
			if field == 1 {
				key = string(v)
			} else {
				value = level.String(string(v))
			}
			continue
		}
		v, n := protowire.ConsumeVarint(data)
		if n < 0 || field == 3 && v > 1 {
			return key, value, bad
		}
		data = data[n:]
		if field == 2 {
			value = level.Int(int64(v))
		} else {
			value = level.Bool(v == 1)
		}
	}
	// oneof：恰好一项。
	if count := btoi(seen[2]) + btoi(seen[3]) + btoi(seen[4]); key == "" || count != 1 {
		return key, value, bad
	}
	return key, value, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
