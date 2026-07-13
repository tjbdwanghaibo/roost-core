package etcd

import (
	"context"
	fetcd "github.com/tjbdwanghaibo/cube-core/etcd"
	"fmt"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// etcdClient implements fetcd.IEtcd by wrapping clientv3.Client.
type etcdClient struct {
	cli *clientv3.Client
}

func newEtcdClient(cfg *fetcd.Config) (*etcdClient, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		DialTimeout: cfg.DialTimeout,
		Username:    cfg.Username,
		Password:    cfg.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("etcd: connect: %w", err)
	}
	return &etcdClient{cli: cli}, nil
}

// --- KV ---

func (c *etcdClient) Get(ctx context.Context, key string) (*fetcd.KV, error) {
	resp, err := c.cli.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, fetcd.ErrKeyNotFound
	}
	return convertKV((*mvccpb.KeyValue)(resp.Kvs[0])), nil
}

func (c *etcdClient) GetWithPrefix(ctx context.Context, prefix string) ([]*fetcd.KV, error) {
	resp, err := c.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	kvs := make([]*fetcd.KV, len(resp.Kvs))
	for i, kv := range resp.Kvs {
		kvs[i] = convertKV((*mvccpb.KeyValue)(kv))
	}
	return kvs, nil
}

func (c *etcdClient) Put(ctx context.Context, key, value string) error {
	_, err := c.cli.Put(ctx, key, value)
	return err
}

func (c *etcdClient) PutWithLease(ctx context.Context, key, value string, leaseID int64) error {
	_, err := c.cli.Put(ctx, key, value, clientv3.WithLease(clientv3.LeaseID(leaseID)))
	return err
}

func (c *etcdClient) Delete(ctx context.Context, key string) error {
	_, err := c.cli.Delete(ctx, key)
	return err
}

func (c *etcdClient) DeleteWithPrefix(ctx context.Context, prefix string) (int64, error) {
	resp, err := c.cli.Delete(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return 0, err
	}
	return resp.Deleted, nil
}

// --- Txn ---

func (c *etcdClient) Txn(ctx context.Context, cmp fetcd.Cmp, onSuccess, onFailure []fetcd.Op) (*fetcd.TxnResponse, error) {
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

func (c *etcdClient) Grant(ctx context.Context, ttl int64) (int64, error) {
	resp, err := c.cli.Grant(ctx, ttl)
	if err != nil {
		return 0, err
	}
	return int64(resp.ID), nil
}

func (c *etcdClient) KeepAlive(ctx context.Context, leaseID int64) (<-chan struct{}, error) {
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

func (c *etcdClient) Revoke(ctx context.Context, leaseID int64) error {
	_, err := c.cli.Revoke(ctx, clientv3.LeaseID(leaseID))
	return err
}

// --- Watch ---

func (c *etcdClient) Watch(ctx context.Context, key string, opts ...fetcd.WatchOption) fetcd.IWatcher {
	watchOpts := buildWatchOpts(opts)
	wch := c.cli.Watch(ctx, key, watchOpts...)
	return newWatcher(wch)
}

func (c *etcdClient) WatchPrefix(ctx context.Context, prefix string, opts ...fetcd.WatchOption) fetcd.IWatcher {
	watchOpts := buildWatchOpts(opts)
	watchOpts = append(watchOpts, clientv3.WithPrefix())
	wch := c.cli.Watch(ctx, prefix, watchOpts...)
	return newWatcher(wch)
}

// --- Connection ---

func (c *etcdClient) Close() error {
	return c.cli.Close()
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
	}
	return result
}

var _ fetcd.IEtcd = (*etcdClient)(nil)
