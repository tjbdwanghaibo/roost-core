package etcd

import (
	"context"
	"fmt"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// etcdClient implements IEtcd by wrapping clientv3.Client.
type etcdClient struct {
	cli *clientv3.Client
}

func newEtcdClient(cfg *Config) (*etcdClient, error) {
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

func (c *etcdClient) Get(ctx context.Context, key string) (*KV, error) {
	resp, err := c.cli.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, ErrKeyNotFound
	}
	return convertKV((*mvccpb.KeyValue)(resp.Kvs[0])), nil
}

func (c *etcdClient) GetWithPrefix(ctx context.Context, prefix string) ([]*KV, error) {
	snapshot, err := c.GetPrefixSnapshot(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return snapshot.KVs, nil
}

func (c *etcdClient) GetPrefixSnapshot(ctx context.Context, prefix string) (*PrefixSnapshot, error) {
	resp, err := c.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	kvs := make([]*KV, len(resp.Kvs))
	for i, kv := range resp.Kvs {
		kvs[i] = convertKV((*mvccpb.KeyValue)(kv))
	}
	return &PrefixSnapshot{KVs: kvs, Revision: resp.Header.Revision}, nil
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

func (c *etcdClient) Txn(ctx context.Context, cmp Cmp, onSuccess, onFailure []Op) (*TxnResponse, error) {
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
	return &TxnResponse{
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

func (c *etcdClient) Watch(ctx context.Context, key string, opts ...WatchOption) IWatcher {
	watchOpts := buildWatchOpts(opts)
	watchCtx, cancel := context.WithCancel(ctx)
	wch := c.cli.Watch(watchCtx, key, watchOpts...)
	return newWatcher(watchCtx, wch, cancel)
}

func (c *etcdClient) WatchPrefix(ctx context.Context, prefix string, opts ...WatchOption) IWatcher {
	watchOpts := buildWatchOpts(opts)
	watchOpts = append(watchOpts, clientv3.WithPrefix())
	watchCtx, cancel := context.WithCancel(ctx)
	wch := c.cli.Watch(watchCtx, prefix, watchOpts...)
	return newWatcher(watchCtx, wch, cancel)
}

// --- Connection ---

func (c *etcdClient) Close() error {
	return c.cli.Close()
}

// --- helpers ---

func convertKV(kv *mvccpb.KeyValue) *KV {
	return &KV{
		Key:            string(kv.Key),
		Value:          string(kv.Value),
		CreateRevision: kv.CreateRevision,
		ModRevision:    kv.ModRevision,
		Version:        kv.Version,
		Lease:          kv.Lease,
	}
}

func buildCmp(cmp Cmp) clientv3.Cmp {
	var result clientv3.Cmp
	switch cmp.Target {
	case CmpVersion:
		result = clientv3.Compare(clientv3.Version(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	case CmpCreateRevision:
		result = clientv3.Compare(clientv3.CreateRevision(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	case CmpModRevision:
		result = clientv3.Compare(clientv3.ModRevision(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	case CmpValue:
		result = clientv3.Compare(clientv3.Value(cmp.Key), cmpOpStr(cmp.Op), cmp.Value)
	}
	return result
}

func cmpOpStr(op CmpOp) string {
	switch op {
	case CmpEqual:
		return "="
	case CmpNotEqual:
		return "!="
	case CmpLess:
		return "<"
	case CmpGreater:
		return ">"
	}
	return "="
}

func buildOps(ops []Op) []clientv3.Op {
	if len(ops) == 0 {
		return nil
	}
	result := make([]clientv3.Op, len(ops))
	for i, op := range ops {
		switch op.Type {
		case OpPut:
			if op.Lease != 0 {
				result[i] = clientv3.OpPut(op.Key, op.Value, clientv3.WithLease(clientv3.LeaseID(op.Lease)))
			} else {
				result[i] = clientv3.OpPut(op.Key, op.Value)
			}
		case OpDelete:
			result[i] = clientv3.OpDelete(op.Key)
		}
	}
	return result
}

func buildWatchOpts(opts []WatchOption) []clientv3.OpOption {
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

var _ IEtcd = (*etcdClient)(nil)
var _ IPrefixSnapshotReader = (*etcdClient)(nil)
