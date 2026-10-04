package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

const MigrationHandler = "__dataengine_migration"

var (
	ErrMigrationUnsupported         = errors.New("dataengine migration: DAO does not expose generated migration contracts")
	ErrRemoteMigrationLeaseRequired = errors.New("dataengine migration: remote envelope requires an ownership lease")
)

type MigrationRunner struct {
	committer coredata.SystemCommitter
	now       func() time.Time
	newID     func() (coredata.TransactionID, error)
}

func NewMigrationRunner(committer coredata.SystemCommitter) (*MigrationRunner, error) {
	if committer == nil {
		return nil, errors.New("dataengine migration: system committer is required")
	}
	return &MigrationRunner{committer: committer, now: time.Now, newID: newSystemTransactionID}, nil
}

// Migrate 将普通 DAO 的 schema 升级作为带版本的全量 mutation 提交，并等待投影可见。
// 等待被取消不证明提交未生效；调用方应重读权威存储，不能用旧数据直接补偿。
// Remote 信封必须经持有所有权租约的 RemoteCommit 迁移，此入口拒绝它以保护聚合版本向量。
func (runner *MigrationRunner) Migrate(ctx context.Context, dao any, doc coredata.RawDocument) (bool, error) {
	if runner == nil || runner.committer == nil {
		return false, errors.New("dataengine migration: runner is not configured")
	}
	descriptor, descriptorOK := dao.(coredata.Descriptor)
	migrator, migratorOK := dao.(coredata.Migrator)
	if !descriptorOK || !migratorOK {
		return false, ErrMigrationUnsupported
	}
	target := descriptor.SchemaVersion()
	if doc.Schema == target {
		return false, nil
	}
	if doc.Schema > target {
		return false, fmt.Errorf("dataengine migration: stored schema %d is newer than runtime schema %d", doc.Schema, target)
	}
	if doc.Enveloped {
		return false, ErrRemoteMigrationLeaseRequired
	}
	payload, schema, err := persistedPayload(doc)
	if err != nil {
		return false, err
	}
	payload, err = migrator.Migrate(payload, schema)
	if err != nil {
		return false, fmt.Errorf("dataengine migration: resource=%s id=%d schema=%d->%d: %w", doc.Key.Resource, doc.Key.ID, schema, target, err)
	}
	id, err := runner.newID()
	if err != nil {
		return false, err
	}
	record := coredata.CommitRecord{
		ID: id, Handler: MigrationHandler, CreatedAt: runner.now().UTC().UnixNano(), Durability: corenest.DurabilityStrict,
		Mutations: []coredata.Mutation{{
			Key: doc.Key, Kind: coredata.MutationPut, ExpectedVersion: doc.Version, NextVersion: doc.Version + 1,
			Mask: coredata.AllFields, Schema: target, Codec: "bson-v2", Data: payload,
		}},
	}
	ticket, err := runner.committer.CommitSystem(ctx, record)
	if err != nil {
		return false, err
	}
	// 迁移需要 Mongo 投影完成；仅有 WAL durable 还不能让下一次装载读到新 schema。
	if err := coredata.WaitProjection(ctx, ticket); err != nil {
		return false, err
	}
	return true, nil
}

func newSystemTransactionID() (coredata.TransactionID, error) {
	var id coredata.TransactionID
	if _, err := rand.Read(id[:]); err != nil {
		return coredata.TransactionID{}, fmt.Errorf("dataengine migration: transaction id: %w", err)
	}
	return id, nil
}
