package saga

import (
	"context"
	"embed"
	"fmt"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// 权威 Redis 上 saga 的 Lua 脚本：脚本本体在 redis_lua/*.lua，fredis.NewLuaScript 拼上 "#!lua" 头与公共前言（驱动的
// lua/prelude.lua）。约定见 fredis.LuaScript：只用返回值表达业务结论，先做完全部校验再写第一条；校验之后的写出错按部分写入
// 处理（RedisStore 计 saga.redis.script_partial_total 并 fail-stop）。

//go:embed redis_lua/*.lua
var redisLua embed.FS

// sagaScriptNames 是 RedisStore 与 saga 投影用的脚本（文件名去掉 .lua）。
var sagaScriptNames = []string{"meta", "create", "claim_due", "claim_outbox", "apply", "ack_outbox", "nack_outbox", "mark_alarm", "reload", "evict"}

// loadRedisScripts 读出嵌入的脚本并装配；不访问 Redis。
func loadRedisScripts(fs embed.FS, dir string, names []string) (map[string]*fredis.LuaScript, error) {
	out := make(map[string]*fredis.LuaScript, len(names))
	for _, name := range names {
		body, err := fs.ReadFile(dir + "/" + name + ".lua")
		if err != nil {
			return nil, fmt.Errorf("saga redis: script %s: %w", name, err)
		}
		out[name] = fredis.NewLuaScript(name, string(body))
	}
	return out, nil
}

// preloadScripts 用 SCRIPT LOAD 装进服务端缓存（见 fredis.PreloadScripts）。
func preloadScripts(ctx context.Context, loader fredis.ScriptLoader, scripts map[string]*fredis.LuaScript) error {
	all := make([]*fredis.LuaScript, 0, len(scripts))
	for _, script := range scripts {
		all = append(all, script)
	}
	return fredis.PreloadScripts(ctx, loader, all...)
}

// replyCode / replyString：脚本回复的结果码与元素（fredis.ReplyCode / ReplyString）。
func replyCode(value any) (string, []any, error) { return fredis.ReplyCode(value) }
func replyString(items []any, i int) string      { return fredis.ReplyString(items, i) }
