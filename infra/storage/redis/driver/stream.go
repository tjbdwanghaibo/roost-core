package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// 本文件是只读的可选能力：流读取（XRANGE / XREAD / XLEN）、多字段哈希读（HMGET）、脚本装载（SCRIPT LOAD）与服务端检查
// （INFO / CONFIG GET）。读命令交给 go-redis 按 MaxRetries 自动重试（README §1）；SCRIPT LOAD 对同一原文幂等，结果
// 只是摘要，重放无害。

// XRangeAfter 实现 fredis.StreamReader：返回 ID 严格大于 afterID 的至多 count 条。
func (c *Client) XRangeAfter(ctx context.Context, key, afterID string, count int64) ([]fredis.StreamEntry, error) {
	messages, err := c.rdb.XRangeN(ctx, key, "("+afterID, "+", count).Result()
	if err != nil {
		return nil, err
	}
	return streamEntries(messages)
}

// XReadAfter 实现 fredis.StreamReader：阻塞至多 block 等 afterID 之后的新条目，超时返回空。
func (c *Client) XReadAfter(ctx context.Context, key, afterID string, count int64, block time.Duration) ([]fredis.StreamEntry, error) {
	if block <= 0 {
		return nil, errors.New("redis: XReadAfter needs a positive block duration")
	}
	streams, err := c.rdb.XRead(ctx, &goredis.XReadArgs{Streams: []string{key, afterID}, Count: count, Block: block}).Result()
	if errors.Is(err, goredis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []fredis.StreamEntry
	for _, stream := range streams {
		entries, err := streamEntries(stream.Messages)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	return out, nil
}

// XLen 实现 fredis.StreamReader。
func (c *Client) XLen(ctx context.Context, key string) (int64, error) {
	return c.rdb.XLen(ctx, key).Result()
}

func streamEntries(messages []goredis.XMessage) ([]fredis.StreamEntry, error) {
	out := make([]fredis.StreamEntry, len(messages))
	for i, message := range messages {
		fields := make(map[string]string, len(message.Values))
		for name, value := range message.Values {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("redis: stream %s field %s is %T, want a string", message.ID, name, value)
			}
			fields[name] = text
		}
		out[i] = fredis.StreamEntry{ID: message.ID, Fields: fields}
	}
	return out, nil
}

// HMGet 实现 fredis.HashFieldsReader：结果按 fields 的位置给出，nil 元素表示字段不存在。
func (c *Client) HMGet(ctx context.Context, key string, fields ...string) ([][]byte, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	values, err := c.rdb.HMGet(ctx, key, fields...).Result()
	if err != nil {
		return nil, err
	}
	if len(values) != len(fields) {
		return nil, fmt.Errorf("redis: HMGET returned %d values for %d fields", len(values), len(fields))
	}
	out := make([][]byte, len(fields))
	for i, value := range values {
		switch typed := value.(type) {
		case nil:
		case string:
			out[i] = []byte(typed)
		default:
			return nil, fmt.Errorf("redis: HMGET element %d is %T, want a string", i, value)
		}
	}
	return out, nil
}

// ScriptLoad 实现 fredis.ScriptLoader。
func (c *Client) ScriptLoad(ctx context.Context, source string) (string, error) {
	return c.rdb.ScriptLoad(ctx, source).Result()
}

// Info 实现 fredis.ServerInspector。
func (c *Client) Info(ctx context.Context, section string) (string, error) {
	return c.rdb.Info(ctx, section).Result()
}

// ConfigGet 实现 fredis.ServerInspector。
func (c *Client) ConfigGet(ctx context.Context, parameter string) (map[string]string, error) {
	return c.rdb.ConfigGet(ctx, parameter).Result()
}

var (
	_ fredis.StreamReader     = (*Client)(nil)
	_ fredis.HashFieldsReader = (*Client)(nil)
	_ fredis.ScriptLoader     = (*Client)(nil)
	_ fredis.ServerInspector  = (*Client)(nil)
)
