package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// 单实例锁的 etcd 后端在连 etcd 之前就拒绝的配置（不需要 etcd）。

// 启用单实例锁的服务必须显式配置 etcd：opener 不沿用 etcd Mod 声明的 localhost:2379 缺省。
func TestSingletonStoreRequiresExplicitEtcdEndpoints(t *testing.T) {
	cfg := viper.New()
	cfg.Set("singleton.enabled", true)
	cfg.Set("singleton.key_prefix", "/planet/singleton")
	if _, err := SingletonStore(cfg); err == nil || !strings.Contains(err.Error(), "etcd.endpoints") {
		t.Fatalf("SingletonStore without etcd.endpoints = %v, want a refusal naming etcd.endpoints", err)
	}
}

// 地址与凭据走 etcd Mod 的同一条 `_env` 规则：声明了变量而变量为空，点名键与变量；用户名与口令要同时有。
func TestSingletonStoreReadsEtcdConnectionLikeTheEtcdMod(t *testing.T) {
	t.Setenv("ROOST_TEST_ETCD_EMPTY", "")
	cfg := viper.New()
	cfg.Set("etcd.endpoints_env", "ROOST_TEST_ETCD_EMPTY")
	if _, err := SingletonStore(cfg); err == nil || !strings.Contains(err.Error(), "etcd.endpoints_env") || !strings.Contains(err.Error(), "ROOST_TEST_ETCD_EMPTY") {
		t.Fatalf("SingletonStore with an empty endpoints variable = %v, want a refusal naming etcd.endpoints_env and the variable", err)
	}
	cfg = viper.New()
	cfg.Set("etcd.endpoints", []string{"127.0.0.1:1"})
	cfg.Set("etcd.username", "root")
	if _, err := SingletonStore(cfg); err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("SingletonStore with a username but no password = %v, want the together refusal", err)
	}
}

// etcd 租约以整秒计：singleton.ttl 不是整秒时打开就拒绝，CompareAndSet 收到这样的 ttl 也拒绝，不偷偷取整。
func TestSingletonStoreRefusesATTLThatIsNotWholeSeconds(t *testing.T) {
	cfg := viper.New()
	cfg.Set("etcd.endpoints", []string{"127.0.0.1:1"})
	cfg.Set("singleton.enabled", true)
	cfg.Set("singleton.key_prefix", "/planet/singleton")
	cfg.Set("singleton.ttl", "1500ms")
	if _, err := SingletonStore(cfg); err == nil || !strings.Contains(err.Error(), "singleton.ttl") || !strings.Contains(err.Error(), "whole number of seconds") {
		t.Fatalf("SingletonStore with singleton.ttl 1500ms = %v, want a refusal naming singleton.ttl", err)
	}

	s, err := newSingletonStore([]string{"127.0.0.1:1"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ttl := range []time.Duration{1500 * time.Millisecond, -time.Second} {
		if _, _, err := s.CompareAndSet(context.Background(), "/planet/singleton:game:1", nil, []byte("v"), ttl); err == nil || !strings.Contains(err.Error(), "whole number of seconds") {
			t.Fatalf("CompareAndSet with ttl %v = %v, want a refusal", ttl, err)
		}
	}
}

// Close 幂等：App 的收尾与 bootstrap 自己的清理可能各关一次，第二次不能报错（clientv3.Client.Close 本身不幂等）。
// 不需要 etcd：客户端建立时不拨号。
func TestSingletonStoreCloseIsIdempotent(t *testing.T) {
	s, err := newSingletonStore([]string{"127.0.0.1:1"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.lock == s.live {
		t.Fatal("the lock and Live share one client: a burst of Live queries could starve the renewal")
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d = %v, want nil", attempt, err)
		}
	}
}
