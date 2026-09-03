package action

import (
	"strconv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
)

// Params is the tolerant accessor over an action's raw param plus the
// robot's blackboard: param wins, blackboard falls back, then the caller's
// default — the value-resolution chain every cube action re-implemented by
// hand.
type Params struct {
	rb  *robot.Context
	raw map[string]any
}

// ParamsOf wraps a raw action param (nil or map[string]any).
func ParamsOf(rb *robot.Context, raw any) Params {
	params, _ := raw.(map[string]any)
	return Params{rb: rb, raw: params}
}

func (p Params) String(key string, fallback string) string {
	if raw, ok := p.lookup(key); ok {
		switch typed := raw.(type) {
		case string:
			return typed
		default:
			return fallback
		}
	}
	return fallback
}

func (p Params) Int64(key string, fallback int64) int64 {
	if raw, ok := p.lookup(key); ok {
		if n, ok := asInt64(raw); ok {
			return n
		}
		if s, ok := raw.(string); ok {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
		}
	}
	return fallback
}

func (p Params) Uint32(key string, fallback uint32) uint32 {
	n := p.Int64(key, int64(fallback))
	if n < 0 {
		return fallback
	}
	return uint32(n)
}

func (p Params) Bool(key string, fallback bool) bool {
	if raw, ok := p.lookup(key); ok {
		if b, ok := raw.(bool); ok {
			return b
		}
	}
	return fallback
}

func (p Params) Duration(key string, fallback time.Duration) time.Duration {
	if raw, ok := p.lookup(key); ok {
		switch typed := raw.(type) {
		case time.Duration:
			return typed
		case string:
			if d, err := time.ParseDuration(typed); err == nil {
				return d
			}
		default:
			if n, ok := asInt64(raw); ok {
				return time.Duration(n) * time.Millisecond
			}
		}
	}
	return fallback
}

func (p Params) lookup(key string) (any, bool) {
	if p.raw != nil {
		if raw, ok := p.raw[key]; ok {
			return raw, true
		}
	}
	if p.rb != nil && p.rb.Blackboard != nil {
		if raw, ok := p.rb.Blackboard.Get(key); ok {
			return raw, true
		}
	}
	return nil, false
}
