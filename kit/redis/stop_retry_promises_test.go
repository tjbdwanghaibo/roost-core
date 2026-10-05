package redis

import (
	"context"
	"testing"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// RR-20261005-NC-233（N14 观察 O3）：Redis Mod 的停止入口幂等——一次 Close 之后连接池已经关闭，
// 再调用 Stop 返回 nil（停机契约第 4 步“已停完的对象再调用返回 nil”）。
//
// go-redis 的 Close 先把连接池标记为关闭、清空连接，再返回逐个关连接时遇到的第一个错误：返回错误时
// 资源同样已经释放，重试也做不了任何事。旧行为：Close 出错时 Mod 保留 asm，之后每次 Stop 都调 Close、
// 都得到 “redis: client is closed”，永远不会成功（调用方据此以为连接还没释放）。用例先关闭一次底层
// 客户端来制造“Close 返回错误”。
func TestRedisModStopConvergesAfterACloseError(t *testing.T) {
	asm, err := redisdriver.Assemble(fredis.DefaultConfig("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	m := &RedisMod{asm: asm, cfg: fredis.DefaultConfig("127.0.0.1:1")}
	_ = asm.Client.Close() // 连接池已关闭；Mod 的 Close 将返回错误

	if err := m.StopWithContext(context.Background()); err == nil {
		t.Fatal("first Stop = nil, want the Close error reported once")
	}
	if err := m.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop after the pool was closed = %v, want nil: retrying can never succeed", err)
	}
}
