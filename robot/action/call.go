package action

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
)

// RegisterCall registers one protocol-call action from its request and
// response types — the convention-over-configuration replacement for the
// hand-written five-part action template (build request, call, assert type,
// check code, remember response):
//
//	action.MustRegisterCall[pb.GetBossReq, pb.GetBossResp](reg, protocols,
//	    "get_boss", msgid.MsgGetBoss,
//	    action.OnResp(func(rb *robot.Context, resp *pb.GetBossResp) error {
//	        state.BossID.Set(rb, resp.Boss.Id)   // only the real business survives
//	        return nil
//	    }),
//	)
//
// Conventions the generic body applies:
//
//   - Request fields fill by name: each exported scalar field looks up its
//     snake_case name (json tag first) in the action param map, then in the
//     blackboard, else stays zero. MapField overrides individual sources.
//     A param that already IS a *Req is used verbatim.
//   - Codec wiring is automatic: the request/response codecs register on
//     the protocol registry via its Codec if not already present (generated
//     codec tables win — RegisterMessage is idempotent per direction).
//   - Responses implementing GetCode() (int32 or int64) fail the action on
//     a non-zero code unless IgnoreCode is set.
//   - RespMsgID defaults to the request id (the common shared-id protocol);
//     override with RespID for split-id protocols.
func RegisterCall[Req any, Resp any](reg *Registry, protocols *protocol.Registry, name string, msgID uint32, opts ...CallOption[Resp]) error {
	if protocols == nil {
		return fmt.Errorf("robot action: call %q needs a protocol registry", name)
	}
	var cfg callConfig[Resp]
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	// Direction-split codec wiring: the request only needs the encoder and
	// the response only the decoder — critical for the common protocol shape
	// where both share one msg id.
	if err := protocol.EnsureEncoder(protocols, msgID); err != nil {
		return fmt.Errorf("robot action: call %q: %w", name, err)
	}
	respID := cfg.respID
	if respID == 0 {
		respID = msgID
	}
	if err := protocol.EnsureDecoder[Resp](protocols, respID); err != nil {
		return fmt.Errorf("robot action: call %q: %w", name, err)
	}
	fields, err := callFieldsOf[Req](cfg.fieldSources)
	if err != nil {
		return fmt.Errorf("robot action: call %q: %w", name, err)
	}
	return reg.Register(Func{
		ActionName: name,
		Handle: func(ctx context.Context, rb *robot.Context, param any) error {
			session := rb.Session()
			if session == nil {
				return fmt.Errorf("session not connected")
			}
			req, err := buildRequest[Req](rb, param, fields)
			if err != nil {
				return err
			}
			if cfg.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
				defer cancel()
			}
			msg, err := session.Call(ctx, msgID, respID, req)
			if err != nil {
				return err
			}
			resp, ok := msg.Value.(*Resp)
			if !ok {
				return fmt.Errorf("unexpected response type %T", msg.Value)
			}
			if !cfg.ignoreCode {
				if code, checked := responseCode(resp); checked && code != 0 {
					return fmt.Errorf("response code %d", code)
				}
			}
			if cfg.onResp != nil {
				return cfg.onResp(rb, resp)
			}
			return nil
		},
	})
}

// MustRegisterCall is RegisterCall panicking on error.
func MustRegisterCall[Req any, Resp any](reg *Registry, protocols *protocol.Registry, name string, msgID uint32, opts ...CallOption[Resp]) {
	if err := RegisterCall[Req, Resp](reg, protocols, name, msgID, opts...); err != nil {
		panic(err)
	}
}

// CallOption customizes one generated call action.
type CallOption[Resp any] func(*callConfig[Resp])

type callConfig[Resp any] struct {
	onResp       func(*robot.Context, *Resp) error
	fieldSources map[string]string
	respID       uint32
	timeout      time.Duration
	ignoreCode   bool
}

// OnResp adds the response-memory callback — the only part of a protocol
// call that stays business code.
func OnResp[Resp any](fn func(*robot.Context, *Resp) error) CallOption[Resp] {
	return func(c *callConfig[Resp]) { c.onResp = fn }
}

// MapField overrides the lookup key for one request field (Go field name ->
// param/blackboard key) when the names don't line up.
func MapField[Resp any](field, key string) CallOption[Resp] {
	return func(c *callConfig[Resp]) {
		if c.fieldSources == nil {
			c.fieldSources = make(map[string]string)
		}
		c.fieldSources[field] = key
	}
}

// RespID sets a distinct response message id for split-id protocols.
func RespID[Resp any](id uint32) CallOption[Resp] {
	return func(c *callConfig[Resp]) { c.respID = id }
}

// CallTimeout bounds one call (default: the scenario's context).
func CallTimeout[Resp any](d time.Duration) CallOption[Resp] {
	return func(c *callConfig[Resp]) { c.timeout = d }
}

// IgnoreCode skips the GetCode() convention check.
func IgnoreCode[Resp any]() CallOption[Resp] {
	return func(c *callConfig[Resp]) { c.ignoreCode = true }
}

// callField is one auto-fillable request field resolved at registration.
type callField struct {
	index []int
	key   string
	kind  reflect.Kind
}

func callFieldsOf[Req any](overrides map[string]string) ([]callField, error) {
	reqType := reflect.TypeOf((*Req)(nil)).Elem()
	if reqType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("request type %s must be a struct", reqType)
	}
	var fields []callField
	for i := 0; i < reqType.NumField(); i++ {
		field := reqType.Field(i)
		if !field.IsExported() {
			continue
		}
		switch field.Type.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64, reflect.String, reflect.Bool:
		default:
			continue // composite fields are only settable by passing *Req directly
		}
		key := overrides[field.Name]
		if key == "" {
			key = jsonFieldName(field)
		}
		fields = append(fields, callField{index: field.Index, key: key, kind: field.Type.Kind()})
	}
	for field := range overrides {
		if _, ok := reqType.FieldByName(field); !ok {
			return nil, fmt.Errorf("MapField %q: no such field on %s", field, reqType)
		}
	}
	return fields, nil
}

func buildRequest[Req any](rb *robot.Context, param any, fields []callField) (*Req, error) {
	// A typed request passed as the param is used verbatim — the escape
	// hatch for composite payloads.
	if typed, ok := param.(*Req); ok {
		return typed, nil
	}
	req := new(Req)
	value := reflect.ValueOf(req).Elem()
	params, _ := param.(map[string]any)
	for _, field := range fields {
		raw, ok := lookupValue(rb, params, field.key)
		if !ok {
			continue
		}
		if err := assignScalar(value.FieldByIndex(field.index), field.kind, raw); err != nil {
			return nil, fmt.Errorf("field %s: %w", field.key, err)
		}
	}
	return req, nil
}

// lookupValue implements the convention chain: action param first, then the
// blackboard, else absent.
func lookupValue(rb *robot.Context, params map[string]any, key string) (any, bool) {
	if params != nil {
		if raw, ok := params[key]; ok {
			return raw, true
		}
	}
	if rb != nil && rb.Blackboard != nil {
		if raw, ok := rb.Blackboard.Get(key); ok {
			return raw, true
		}
	}
	return nil, false
}

func assignScalar(target reflect.Value, kind reflect.Kind, raw any) error {
	if !target.CanSet() {
		return fmt.Errorf("field not settable")
	}
	value := reflect.ValueOf(raw)
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := asInt64(raw)
		if !ok {
			return fmt.Errorf("cannot convert %T to integer", raw)
		}
		target.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, ok := asInt64(raw)
		if !ok || n < 0 {
			return fmt.Errorf("cannot convert %T to unsigned integer", raw)
		}
		target.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		switch typed := raw.(type) {
		case float64:
			target.SetFloat(typed)
		case float32:
			target.SetFloat(float64(typed))
		default:
			n, ok := asInt64(raw)
			if !ok {
				return fmt.Errorf("cannot convert %T to float", raw)
			}
			target.SetFloat(float64(n))
		}
	case reflect.String:
		if s, ok := raw.(string); ok {
			target.SetString(s)
		} else {
			target.SetString(fmt.Sprint(raw))
		}
	case reflect.Bool:
		b, ok := raw.(bool)
		if !ok {
			return fmt.Errorf("cannot convert %T to bool", raw)
		}
		target.SetBool(b)
	default:
		if value.Type().AssignableTo(target.Type()) {
			target.Set(value)
		} else {
			return fmt.Errorf("unsupported kind %s", kind)
		}
	}
	return nil
}

func asInt64(raw any) (int64, bool) {
	switch typed := raw.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	default:
		return 0, false
	}
}

// responseCode applies the GetCode convention (int32 or int64 getter).
func responseCode(resp any) (int64, bool) {
	switch typed := resp.(type) {
	case interface{ GetCode() int32 }:
		return int64(typed.GetCode()), true
	case interface{ GetCode() int64 }:
		return typed.GetCode(), true
	default:
		return 0, false
	}
}

// jsonFieldName resolves a request field's lookup key: the json tag name
// when present, else the snake_case of the Go field name.
func jsonFieldName(field reflect.StructField) string {
	if tag, ok := field.Tag.Lookup("json"); ok {
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			return name
		}
	}
	return snakeCase(field.Name)
}

func snakeCase(s string) string {
	var b strings.Builder
	previousLower := false
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			if previousLower {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
			previousLower = false
			continue
		}
		b.WriteRune(r)
		previousLower = true
	}
	return b.String()
}
