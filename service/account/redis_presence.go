package account

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// RedisPresenceStore 是登记表的 Redis 实现：**一个哈希键**（字段 = 服号，值 = "<到期毫秒>|<条目 JSON>"）。
//
// 为什么这样存：
//
//   - 到期时刻由脚本里的 redis.call('TIME') 算，用的是 Redis 服务器自己的时钟：多个 account 副本写、多个副本读，
//     谁都不拿自己的时钟比较，副本之间的时钟偏差不影响“过期”的判断。
//   - 一个键：清单是一次 HGETALL，不扫描、不跨槽（Redis Cluster 下也是一个槽），也不依赖区服索引。
//   - 读不删：List 只过滤掉到期的；到期的字段在登记一个**新**服号时（那时要数名额）一起删掉。所以哈希里至多
//     MaxServers 个没过期的字段加上一批等着被清的过期字段，总数仍以登记过的服号为界。
//
// 键不带 TTL：登记表本身不是缓存，空了就是“没有服在报”；每个字段各自的期限在值里。
type RedisPresenceStore struct {
	client fredis.ScriptRunner
	key    string
}

// NewRedisPresenceStore 构造。key 是哈希键的完整名字（NewRedisStores 用 "<prefix>:presence"）。
func NewRedisPresenceStore(client fredis.ScriptRunner, key string) (*RedisPresenceStore, error) {
	if client == nil {
		return nil, fmt.Errorf("account: presence store needs a redis client")
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("account: presence store needs a key")
	}
	return &RedisPresenceStore{client: client, key: key}, nil
}

// presencePutScript：KEYS[1] 哈希；ARGV[1] 服号，ARGV[2] 条目 JSON，ARGV[3] 期限毫秒，ARGV[4] 名额。
// 新的服号先清掉到期的字段再数名额，满了返回 0、什么都不写；否则写入并返回 1。
const presencePutScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
if redis.call('HEXISTS', KEYS[1], ARGV[1]) == 0 then
  local all = redis.call('HGETALL', KEYS[1])
  local live = 0
  for i = 1, #all, 2 do
    local sep = string.find(all[i + 1], '|', 1, true)
    local expires = sep and tonumber(string.sub(all[i + 1], 1, sep - 1)) or 0
    if expires and expires > now then
      live = live + 1
    else
      redis.call('HDEL', KEYS[1], all[i])
    end
  end
  if live >= tonumber(ARGV[4]) then
    return 0
  end
end
redis.call('HSET', KEYS[1], ARGV[1], tostring(now + tonumber(ARGV[3])) .. '|' .. ARGV[2])
return 1
`

// presenceListScript：KEYS[1] 哈希。返回没到期的条目 JSON 列表。
const presenceListScript = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local all = redis.call('HGETALL', KEYS[1])
local out = {}
for i = 1, #all, 2 do
  local sep = string.find(all[i + 1], '|', 1, true)
  if sep then
    local expires = tonumber(string.sub(all[i + 1], 1, sep - 1))
    if expires and expires > now then
      out[#out + 1] = string.sub(all[i + 1], sep + 1)
    end
  end
end
return out
`

// presenceDeleteScript：KEYS[1] 哈希；ARGV[1] 服号。
const presenceDeleteScript = `return redis.call('HDEL', KEYS[1], ARGV[1])`

// Put 实现 PresenceStore。期限按毫秒取整，不足 1 毫秒按 1 毫秒。
func (store *RedisPresenceStore) Put(ctx context.Context, presence ServerPresence, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: presence ttl must be positive", ErrRangeInvalid)
	}
	body, err := json.Marshal(presence)
	if err != nil {
		return fmt.Errorf("account: encode server presence: %w", err)
	}
	millis := max(ttl.Milliseconds(), 1)
	result, err := store.client.Eval(ctx, presencePutScript, []string{store.key},
		strconv.FormatInt(int64(presence.ServerID), 10), string(body), strconv.FormatInt(millis, 10), strconv.Itoa(MaxServers))
	if err != nil {
		return fmt.Errorf("account: put server presence: %w", err)
	}
	if written, ok := result.(int64); !ok || written != 1 {
		return fmt.Errorf("%w: %d servers are present, which is the limit; server %d was not added", ErrRangeInvalid, MaxServers, presence.ServerID)
	}
	return nil
}

// Delete 实现 PresenceStore。
func (store *RedisPresenceStore) Delete(ctx context.Context, serverID int32) error {
	if _, err := store.client.Eval(ctx, presenceDeleteScript, []string{store.key}, strconv.FormatInt(int64(serverID), 10)); err != nil {
		return fmt.Errorf("account: delete server presence: %w", err)
	}
	return nil
}

// List 实现 PresenceStore。解不开的值跳过（ListServerPresence 还会再检查一遍）。
func (store *RedisPresenceStore) List(ctx context.Context) ([]ServerPresence, error) {
	result, err := store.client.Eval(ctx, presenceListScript, []string{store.key})
	if err != nil {
		return nil, fmt.Errorf("account: list server presence: %w", err)
	}
	items, ok := result.([]any)
	if !ok && result != nil {
		return nil, fmt.Errorf("%w: list answered %T", errPresenceStoreBroken, result)
	}
	out := make([]ServerPresence, 0, len(items))
	for _, item := range items {
		raw, ok := item.(string)
		if !ok {
			continue
		}
		var presence ServerPresence
		if json.Unmarshal([]byte(raw), &presence) != nil {
			continue
		}
		out = append(out, presence)
	}
	return out, nil
}

var _ PresenceStore = (*RedisPresenceStore)(nil)
