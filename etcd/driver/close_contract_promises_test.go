package driver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fetcd "github.com/tjbdwanghaibo/roost-core/etcd"
)

// RR-20261006-10（W-2026-10-06-02 转 RR）：驱动 Close 的统一口径——重复 Close 幂等返回 nil，第一次的
// 错误只报一次；并发 Close 的后到者等第一个做完；Close 之后的其他调用返回已关闭错误（fetcd.ErrClosed）。
//
// 旧行为：Close 用 sync.Once 粘滞返回第一次的结果——第一次出错，之后每次都返回同一个错误，停机重试
// 永远不收敛；Close 之后的 Get / Put 等不快速失败，clientv3 的重试拦截器对“连接正在关闭”一直重试，
// 阻塞到调用方截止时间（没有截止时间就一直阻塞）。
//
// 不可达地址、DialTimeout 0（clientv3.New 不拨号），不需要真实 etcd。
func closeContractClient(t *testing.T) *Client {
	t.Helper()
	cfg := fetcd.DefaultConfig([]string{"127.0.0.1:1"})
	cfg.DialTimeout = 0
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientCloseErrorIsReportedOnceThenNil(t *testing.T) {
	c := closeContractClient(t)
	// 底层客户端先被关过一次：clientv3 第二次 Close 返回 context.Canceled，用它制造“第一次 Close 出错”。
	_ = c.cli.Close()
	if err := c.Close(); err == nil {
		t.Fatal("first Close on a client closed underneath = nil, want the clientv3 error reported once")
	}
	for attempt := 2; attempt <= 3; attempt++ {
		if err := c.Close(); err != nil {
			t.Fatalf("Close #%d = %v, want nil: the first error is reported once", attempt, err)
		}
	}
}

func TestClientConcurrentCloseAllReturnNil(t *testing.T) {
	c := closeContractClient(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = c.Close() }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close #%d = %v, want nil (all = %v)", i, err, errs)
		}
	}
}

func TestClientCallsAfterCloseFailFastWithErrClosed(t *testing.T) {
	c := closeContractClient(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	// 截止时间远大于断言窗口：旧实现会一直阻塞到这里。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	calls := map[string]func() error{
		"Get":              func() error { _, err := c.Get(ctx, "k"); return err },
		"GetWithPrefix":    func() error { _, err := c.GetWithPrefix(ctx, "p/"); return err },
		"Put":              func() error { return c.Put(ctx, "k", "v") },
		"PutWithLease":     func() error { return c.PutWithLease(ctx, "k", "v", 1) },
		"Delete":           func() error { return c.Delete(ctx, "k") },
		"DeleteWithPrefix": func() error { _, err := c.DeleteWithPrefix(ctx, "p/"); return err },
		"Txn": func() error {
			_, err := c.Txn(ctx, fetcd.Cmp{Key: "k", Target: fetcd.CmpVersion, Op: fetcd.CmpEqual, Value: 0}, nil, nil)
			return err
		},
		"Grant":     func() error { _, err := c.Grant(ctx, 5); return err },
		"KeepAlive": func() error { _, err := c.KeepAlive(ctx, 1); return err },
		"Revoke":    func() error { return c.Revoke(ctx, 1) },
	}
	for name, call := range calls {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if !errors.Is(err, fetcd.ErrClosed) {
				t.Errorf("%s after Close = %v, want fetcd.ErrClosed", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s after Close still blocked after 2s, want an immediate fetcd.ErrClosed", name)
		}
	}
}
