package decision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Decode 只接受单个 v1 原生对象。不接受外部编辑器配置、大小写别名、重复键、null、
// 小数或指数字面量，以及 do/check/stage 等变体无关的字段（即使显式值为零）。
func Decode(raw []byte, reg Registry) (*Program, error) {
	var d Definition
	if err := decodeWire(raw, "definition", &d); err != nil {
		return nil, err
	}
	return Compile(d, reg)
}

// DecodeState 是持久化宿主的恢复入口：先限原始字节数，再检查 wire 形状，最后对照该程序验证。
// 失败不返回部分解析的状态。可选空切片按标准 JSON 的 omitempty 规则往返。
func (p *Program) DecodeState(raw []byte) (State, error) {
	var s State
	// 直接调用而非 json.Unmarshal(raw, &s)，保证限长发生在标准库扫描输入之前。
	if err := s.UnmarshalJSON(raw); err != nil {
		return State{}, err
	}
	if err := p.ValidateState(s); err != nil {
		return State{}, err
	}
	return s, nil
}

// UnmarshalJSON 不把缺字段、重复键等畸形快照默认为零值。它不知道具体 Program；
// 实际恢复还必须由该 Program.ValidateState 检查形状、身份、逻辑时钟和引用。
func (s *State) UnmarshalJSON(raw []byte) error {
	type wire State
	var tmp wire
	if err := decodeWire(raw, "state", &tmp); err != nil {
		return err
	}
	*s = State(tmp)
	return nil
}

type shape struct {
	required string
	fields   map[string]string
}

var shapes = map[string]shape{
	"definition": {"schema name start stages", map[string]string{"schema": "str", "name": "str", "start": "str", "stages": "*stage"}},
	"stage":      {"name", map[string]string{"name": "str", "tick": "*node", "events": "*listener"}},
	"listener":   {"id event", map[string]string{"id": "int", "event": "str", "enabled": "bool", "once": "bool", "filter": "test", "body": "*node"}},
	"test":       {"field op value", map[string]string{"field": "str", "op": "str", "value": "int"}},
	"node":       {"kind", map[string]string{"kind": "str", "test": "test", "children": "*node", "actions": "*action"}},
	"action":     {"id kind", map[string]string{"id": "int", "kind": "str", "name": "str", "arg": "int", "delay": "int", "limit": "int"}},
	"state":      {"plan stage now last_tick", map[string]string{"plan": "str", "stage": "str", "now": "int", "last_tick": "int", "active": "*bool", "pending": "*pending", "counts": "*int"}},
	"pending":    {"node_id due", map[string]string{"node_id": "int", "due": "int"}},
}

func decodeWire(raw []byte, root string, out any) error {
	if len(raw) == 0 || len(raw) > MaxJSONBytes || !utf8.Valid(raw) {
		return errors.New("invalid JSON size or UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	w := wireReader{dec: d, budget: 16*MaxItems + 64}
	if _, err := w.value(root, 0); err != nil {
		return fmt.Errorf("decision %s JSON: %w", root, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data after decision JSON")
	}
	return json.Unmarshal(raw, out)
}

type wireReader struct {
	dec    *json.Decoder
	budget int
}

func (w *wireReader) value(typ string, depth int) (string, error) {
	if depth > 2*MaxDepth+12 || w.budget <= 0 {
		return "", errors.New("JSON nesting or item budget exceeded")
	}
	w.budget--
	t, err := w.dec.Token()
	if err != nil {
		return "", err
	}
	switch typ {
	case "str":
		if s, ok := t.(string); ok && len(s) <= MaxNameBytes {
			return s, nil
		}
	case "bool":
		if _, ok := t.(bool); ok {
			return "", nil
		}
	case "int":
		if n, ok := t.(json.Number); ok {
			if _, err = n.Int64(); err == nil {
				return "", nil
			}
		}
	default:
		if elem, ok := strings.CutPrefix(typ, "*"); ok {
			if t != json.Delim('[') {
				break
			}
			limit := MaxItems
			switch elem {
			case "stage":
				limit = MaxStages
			case "listener", "bool":
				limit = MaxListenersPerStage
			}
			for i := 0; w.dec.More(); i++ {
				if i >= limit {
					return "", errors.New("JSON array too long")
				}
				if _, err = w.value(elem, depth+1); err != nil {
					return "", err
				}
			}
			_, err = w.dec.Token()
			return "", err
		}
		if t != json.Delim('{') {
			break
		}
		sh := shapes[typ]
		seen := make(map[string]bool, len(sh.fields))
		var kind string
		for w.dec.More() {
			k, err := w.dec.Token()
			if err != nil {
				return "", err
			}
			key, ok := k.(string)
			if !ok || len(key) > 16 || sh.fields[key] == "" || seen[key] {
				return "", fmt.Errorf("unknown or duplicate %s field", typ)
			}
			seen[key] = true
			v, err := w.value(sh.fields[key], depth+1)
			if err != nil {
				return "", fmt.Errorf("%s: %w", key, err)
			}
			if key == "kind" {
				kind = v
			}
		}
		if _, err = w.dec.Token(); err != nil {
			return "", err
		}
		for key := range strings.FieldsSeq(sh.required) {
			if !seen[key] {
				return "", fmt.Errorf("missing %s.%s", typ, key)
			}
		}
		switch typ + ":" + kind {
		case "node:check":
			if seen["actions"] || seen["children"] {
				return "", errors.New("check has unrelated fields")
			}
		case "node:do":
			if seen["test"] || seen["children"] {
				return "", errors.New("do has unrelated fields")
			}
		case "node:all", "node:any":
			if seen["test"] {
				return "", errors.New("composition has unrelated test")
			}
		case "action:stage":
			if seen["arg"] || seen["delay"] || seen["limit"] {
				return "", errors.New("stage has unrelated fields")
			}
		case "action:open", "action:close":
			if seen["name"] || seen["delay"] || seen["limit"] {
				return "", errors.New("listener control has unrelated fields")
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("expected %s", typ)
}
