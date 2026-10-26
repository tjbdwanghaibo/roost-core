package redis

import (
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// 权威 Redis 上的 Lua 脚本（saga 的 RedisStore、Store Service）共用的写法：原文 = "#!lua" 头 + 公共前言（lua/prelude.lua）
// + 脚本本体。带头的脚本不声明 allow-oom，内存满时在执行前被拒（确定没执行）；不声明 no-writes。脚本只用返回值表达业务
// 结论（第一个元素是结果码），不用错误回复——批量里一条错误回复不影响同批其他调用，但会让人分不清“业务拒绝”与“脚本坏了”。
// 每个脚本都先做完全部校验再写第一条：校验之后的写只会因内部错误失败，调用方按“部分写入”处理。

//go:embed lua/prelude.lua
var luaPrelude string

// LuaScript 是一个装配好的脚本：Source 是完整原文，SHA 是它的 SHA1（与 SCRIPT LOAD 的结果相同）。
type LuaScript struct {
	Name   string
	Source string
	SHA    string
}

// NewLuaScript 把公共前言与 body 拼成完整脚本并算出 SHA；不访问 Redis。
func NewLuaScript(name, body string) *LuaScript {
	source := "#!lua\n" + luaPrelude + "\n" + body
	sum := sha1.Sum([]byte(source))
	return &LuaScript{Name: name, Source: source, SHA: hex.EncodeToString(sum[:])}
}

// Call 是这个脚本的一次调用（DurableScriptBatch 的一项）。
func (s *LuaScript) Call(keys []string, args ...any) ScriptCall {
	return ScriptCall{SHA: s.SHA, Source: s.Source, Keys: keys, Args: args}
}

// PreloadScripts 用 SCRIPT LOAD 把脚本装进服务端缓存并核对 SHA。缓存清空（Redis 重启）后由 DurableScriptBatch 在 NOSCRIPT 时
// 用原文重发，所以这里只是省掉第一次调用的 NOSCRIPT。
func PreloadScripts(ctx context.Context, loader ScriptLoader, scripts ...*LuaScript) error {
	for _, script := range scripts {
		sha, err := loader.ScriptLoad(ctx, script.Source)
		if err != nil {
			return fmt.Errorf("redis: load script %s: %w", script.Name, err)
		}
		if !strings.EqualFold(sha, script.SHA) {
			return fmt.Errorf("redis: script %s loaded as %s, want %s", script.Name, sha, script.SHA)
		}
	}
	return nil
}

// ReplyCode 取出脚本回复的结果码与其余元素（脚本约定返回 {结果码, ...}）。
func ReplyCode(value any) (string, []any, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return "", nil, fmt.Errorf("redis: unexpected script reply %T", value)
	}
	code, ok := items[0].(string)
	if !ok {
		return "", nil, fmt.Errorf("redis: unexpected script reply code %T", items[0])
	}
	return code, items[1:], nil
}

// ReplyString 取出回复里第 i 个元素的文本，不存在时为空串。
func ReplyString(items []any, i int) string {
	if i >= len(items) {
		return ""
	}
	switch typed := items[i].(type) {
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}
