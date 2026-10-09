package driver

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Client implements fetcd.IEtcd by wrapping clientv3.Client.
type Client struct {
	cli *clientv3.Client

	// clientv3.Client.Close 不幂等：第二次调用关闭已关闭的 gRPC 连接，返回 context.Canceled。
	// 停止入口按“三步停机”必须幂等（A3 契约骨架发现，RR-20261005-NC-173 复核补修），所以只关一次，
	// 见 Close。
	closeOnce sync.Once
	// closed 在 Close 开始时置位，之后的调用直接返回 fetcd.ErrClosed，见 checkOpen。
	closed atomic.Bool
}

// Raw exposes the underlying etcd client for assembly code (discovery,
// elections, health probes); business code should stay on fetcd.IEtcd.
func (c *Client) Raw() *clientv3.Client {
	if c == nil {
		return nil
	}
	return c.cli
}

func NewClient(cfg *fetcd.Config) (*Client, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		DialTimeout: cfg.DialTimeout,
		Username:    cfg.Username,
		Password:    cfg.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd: connect: %w", err)
	}
	return &Client{cli: cli}, nil
}

// --- fetcd.KV ---

func (c *Client) Get(ctx context.Context, key string) (*fetcd.KV, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	resp, err := c.cli.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, fetcd.ErrKeyNotFound
	}
	return convertKV((*mvccpb.KeyValue)(resp.Kvs[0])), nil
}

func (c *Client) GetWithPrefix(ctx context.Context, prefix string) ([]*fetcd.KV, error) {
	snapshot, err := c.GetPrefixSnapshot(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return snapshot.KVs, nil
}

func (c *Client) GetPrefixSnapshot(ctx context.Context, prefix string) (*fetcd.PrefixSnapshot, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	resp, err := c.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	kvs := make([]*fetcd.KV, len(resp.Kvs))
	for i, kv := range resp.Kvs {
		kvs[i] = convertKV((*mvccpb.KeyValue)(kv))
	}
	return &fetcd.PrefixSnapshot{KVs: kvs, Revision: resp.Header.Revision}, nil
}

func (c *Client) Put(ctx context.Context, key, value string) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	_, err := c.cli.Put(ctx, key, value)
	return err
}

func (c *Client) PutWithLease(ctx context.Context, key, value string, leaseID int64) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	_, err := c.cli.Put(ctx, key, value, clientv3.WithLease(clientv3.LeaseID(leaseID)))
	return err
}

func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	_, err := c.cli.Delete(ctx, key)
	return err
}

func (c *Client) DeleteWithPrefix(ctx context.Context, prefix string) (int64, error) {
	if err := c.checkOpen(); err != nil {
		return 0, err
	}
	resp, err := c.cli.Delete(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return 0, err
	}
	return resp.Deleted, nil
}

// --- Txn ---

func (c *Client) Txn(ctx context.Context, cmp fetcd.Cmp, onSuccess, onFailure []fetcd.Op) (*fetcd.TxnResponse, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	etcdCmp := buildCmp(cmp)
	successOps := buildOps(onSuccess)
	failureOps := buildOps(onFailure)

	txn := c.cli.Txn(ctx).If(etcdCmp)
	if len(successOps) > 0 {
		txn = txn.Then(successOps...)
	}
	if len(failureOps) > 0 {
		txn = txn.Else(failureOps...)
	}

	resp, err := txn.Commit()
	if err != nil {
		return nil, err
	}
	return &fetcd.TxnResponse{
		Succeeded: resp.Succeeded,
		Revision:  resp.Header.Revision,
	}, nil
}

// --- Lease ---

func (c *Client) Grant(ctx context.Context, ttl int64) (int64, error) {
	if err := c.checkOpen(); err != nil {
		return 0, err
	}
	resp, err := c.cli.Grant(ctx, ttl)
	if err != nil {
		return 0, err
	}
	return int64(resp.ID), nil
}

func (c *Client) KeepAlive(ctx context.Context, leaseID int64) (<-chan struct{}, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	ch, err := c.cli.KeepAlive(ctx, clientv3.LeaseID(leaseID))
	if err != nil {
		return nil, err
	}
	// Convert keepalive channel to a simple "lost" signal
	lostCh := make(chan struct{})
	go func() {
		for range ch {
			// drain keepalive responses
		}
		close(lostCh)
	}()
	return lostCh, nil
}

func (c *Client) Revoke(ctx context.Context, leaseID int64) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	_, err := c.cli.Revoke(ctx, clientv3.LeaseID(leaseID))
	return err
}

// --- Watch ---

func (c *Client) Watch(ctx context.Context, key string, opts ...fetcd.WatchOption) fetcd.IWatcher {
	watchOpts := buildWatchOpts(opts)
	watchCtx, cancel := context.WithCancel(ctx)
	wch := c.cli.Watch(watchCtx, key, watchOpts...)
	return newWatcher(watchCtx, wch, cancel)
}

func (c *Client) WatchPrefix(ctx context.Context, prefix string, opts ...fetcd.WatchOption) fetcd.IWatcher {
	watchOpts := buildWatchOpts(opts)
	watchOpts = append(watchOpts, clientv3.WithPrefix())
	watchCtx, cancel := context.WithCancel(ctx)
	wch := c.cli.Watch(watchCtx, prefix, watchOpts...)
	return newWatcher(watchCtx, wch, cancel)
}

// --- Connection ---

// Close 关闭客户端，幂等：只有第一次真正关闭，第一次的错误只报给那一次调用，之后（含并发的后到者，
// 它们等第一次做完）都返回 nil；Close 之后的调用返回 fetcd.ErrClosed（RR-20261006-10）。旧实现粘滞
// 返回第一次的结果：第一次出错，之后每次都是同一个错误，停机重试永远不收敛。
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		err = c.cli.Close()
	})
	return err
}

// checkOpen 让 Close 之后发起的调用快速失败。clientv3 关闭后，一元调用拿到“连接正在关闭”
// （codes.Canceled），它的重试拦截器把它当可重试错误一直重试，调用阻塞到调用方的截止时间、没有截止
// 时间就一直阻塞（RR-20261006-10）。与 Close 并发、已经在途的调用不在此列，仍受调用方 ctx 约束。
func (c *Client) checkOpen() error {
	if c.closed.Load() {
		return fetcd.ErrClosed
	}
	return nil
}

// --- helpers ---

func convertKV(kv *mvccpb.KeyValue) *fetcd.KV {
	return &fetcd.KV{
		Key:            string(kv.Key),
		Value:          string(kv.Value),
		CreateRevision: kv.CreateRevision,
		ModRevision:    kv.ModRevision,
		Version:        kv.Version,
		Lease:          kv.Lease,
	}
}

func buildCmp(cmp fetcd.Cmp) clientv3.Cmp {
	var result clientv3.Cmp
	switch cmp.Target {
	case fetcd.CmpVersion:
		result = clientv3.Compare(clientv3.Version(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	case fetcd.CmpCreateRevision:
		result = clientv3.Compare(clientv3.CreateRevision(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	case fetcd.CmpModRevision:
		result = clientv3.Compare(clientv3.ModRevision(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	case fetcd.CmpValue:
		result = clientv3.Compare(clientv3.Value(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	}
	return result
}

func cmpOpStr(op fetcd.CmpOp) string {
	switch op {
	case fetcd.CmpEqual:
		return "="
	case fetcd.CmpNotEqual:
		return "!="
	case fetcd.CmpLess:
		return "<"
	case fetcd.CmpGreater:
		return ">"
	}
	return "="
}

func buildOps(ops []fetcd.Op) []clientv3.Op {
	if len(ops) == 0 {
		return nil
	}
	result := make([]clientv3.Op, len(ops))
	for i, op := range ops {
		switch op.Type {
		case fetcd.OpPut:
			if op.Lease != 0 {
				result[i] = clientv3.OpPut(op.Key, op.Value, clientv3.WithLease(clientv3.LeaseID(op.Lease)))
			} else {
				result[i] = clientv3.OpPut(op.Key, op.Value)
			}
		case fetcd.OpDelete:
			result[i] = clientv3.OpDelete(op.Key)
		}
	}
	return result
}

func buildWatchOpts(opts []fetcd.WatchOption) []clientv3.OpOption {
	var result []clientv3.OpOption
	for _, opt := range opts {
		if opt.WithPrevKV {
			result = append(result, clientv3.WithPrevKV())
		}
		if opt.WithRevision > 0 {
			result = append(result, clientv3.WithRev(opt.WithRevision))
		}
		if opt.CreatedNotify {
			result = append(result, clientv3.WithCreatedNotify())
		}
	}
	return result
}

var _ fetcd.IEtcd = (*Client)(nil)
var _ fetcd.IPrefixSnapshotReader = (*Client)(nil)
