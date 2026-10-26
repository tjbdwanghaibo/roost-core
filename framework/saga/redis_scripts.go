package saga

import (
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"fmt"
	"strings"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// 权威 Redis 上的 Lua 脚本。每个脚本 = "#!lua" 头 + common.lua + 脚本本体：带头的脚本不声明 allow-oom，内存满时在执行前被拒
// （确定没执行）；不声明 no-writes。脚本只用返回值表达业务结论（第一个元素是结果码），不用错误回复——批量里一条错误回复
// 不影响同批其他调用，但会让人分不清“业务拒绝”与“脚本坏了”。
//
// 每个脚本都先做完全部校验再写第一条：校验之后的写只会因内部错误失败，那是“部分写入”（RedisStore 计
// saga.redis.script_partial_total 并 fail-stop）。

//go:embed redis_lua/*.lua
var redisLua embed.FS

// redisScript 是一个装载好的脚本：Source 是完整原文，SHA 是它的 SHA1（与 SCRIPT LOAD 的结果相同）。
type redisScript struct {
	Name   string
	Source string
	SHA    string
}

// sagaScriptNames 是 RedisStore 用的脚本（文件名去掉 .lua）。
var sagaScriptNames = []string{"meta", "create", "claim_due", "claim_outbox", "apply", "ack_outbox", "nack_outbox", "mark_alarm"}

// loadRedisScripts 读出嵌入的脚本并算出 SHA；不访问 Redis。
func loadRedisScripts(fs embed.FS, dir string, names []string) (map[string]*redisScript, error) {
	common, err := fs.ReadFile(dir + "/common.lua")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*redisScript, len(names))
	for _, name := range names {
		body, err := fs.ReadFile(dir + "/" + name + ".lua")
		if err != nil {
			return nil, fmt.Errorf("saga redis: script %s: %w", name, err)
		}
		source := "#!lua\n" + string(common) + "\n" + string(body)
		sum := sha1.Sum([]byte(source))
		out[name] = &redisScript{Name: name, Source: source, SHA: hex.EncodeToString(sum[:])}
	}
	return out, nil
}

// preloadScripts 用 SCRIPT LOAD 把脚本装进服务端缓存并核对 SHA。缓存清空（Redis 重启）后由 DurableScriptBatch 在 NOSCRIPT 时
// 用原文重发，所以这里只是省掉第一次调用的 NOSCRIPT。
func preloadScripts(ctx context.Context, loader fredis.ScriptLoader, scripts map[string]*redisScript) error {
	for _, script := range scripts {
		sha, err := loader.ScriptLoad(ctx, script.Source)
		if err != nil {
			return fmt.Errorf("saga redis: load script %s: %w", script.Name, err)
		}
		if !strings.EqualFold(sha, script.SHA) {
			return fmt.Errorf("saga redis: script %s loaded as %s, want %s", script.Name, sha, script.SHA)
		}
	}
	return nil
}

// replyCode 取出脚本回复的结果码与其余元素。
func replyCode(value any) (string, []any, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return "", nil, fmt.Errorf("saga redis: unexpected script reply %T", value)
	}
	code, ok := items[0].(string)
	if !ok {
		return "", nil, fmt.Errorf("saga redis: unexpected script reply code %T", items[0])
	}
	return code, items[1:], nil
}

// replyString 取出回复里第 i 个字符串元素，不存在时为空串。
func replyString(items []any, i int) string {
	if i >= len(items) {
		return ""
	}
	switch typed := items[i].(type) {
	case string:
		return typed
	case int64:
		return fmt.Sprint(typed)
	default:
		return ""
	}
}
