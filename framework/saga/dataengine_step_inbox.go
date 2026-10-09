package saga

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	// dataEngineOperationCollection 是原生步骤的操作状态文档集合（每个操作实例一份，_id = IdempotencyKey）；
	// 投影的 lease fence 指向这里（docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md）。
	dataEngineOperationCollection = "_dataengine_step_operations"
	dataEngineReceiptCollection   = "_dataengine_receipts"
	// dataEngineStepNamespace 是原生步骤回执的 ID 前缀：投影把回执写成 `saga-step/<CommandID>`。
	dataEngineStepNamespace = StepReceiptNamespace
)

type DataEngineStepInboxOptions struct {
	Owner         string
	LeaseDuration time.Duration
	ReceiptTTL    time.Duration
	PollInterval  time.Duration
}

// DataEngineStepInbox 是原生步骤（Nest 事务 + DataEngine WAL）的收件箱。操作实例的状态文档与判定在
// stepOperationInbox（与 Mongo 步骤共用）；这里只有原生特有的部分：回执由投影写进 `_dataengine_receipts`，
// 生效点是投影事务对状态文档的条件写（Bind 把 lease fence 绑进 Nest 事务）。
type DataEngineStepInbox struct {
	stepOperationInbox
	options DataEngineStepInboxOptions
}

type reservationContextKey struct{}

func withReservation(ctx context.Context, reservation Reservation) context.Context {
	return context.WithValue(ctx, reservationContextKey{}, reservation)
}

// ReservationFromContext returns the lease fence allocated for the current
// synchronous step delivery. Business adapters pass it explicitly to Bind;
// it must not be copied into detached goroutines as ambient context.
func ReservationFromContext(ctx context.Context) (Reservation, bool) {
	if ctx == nil {
		return Reservation{}, false
	}
	reservation, ok := ctx.Value(reservationContextKey{}).(Reservation)
	return reservation, ok && reservation.Token > 0 && !reservation.Duplicate && reservation.commandID != "" && reservation.owner != "" && len(reservation.digest) > 0
}

type dataEngineReceipt struct {
	ID      string `bson:"_id"`
	Digest  []byte `bson:"digest"`
	Payload []byte `bson:"payload"`
}

func NewDataEngineStepInbox(client fmongo.IMongo, database string, options DataEngineStepInboxOptions) (*DataEngineStepInbox, error) {
	if client == nil || database == "" || options.Owner == "" {
		return nil, errors.New("saga dataengine inbox: client, database and owner are required")
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = time.Minute
	}
	if options.ReceiptTTL <= 0 {
		options.ReceiptTTL = 30 * 24 * time.Hour
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 25 * time.Millisecond
	}
	inbox := &DataEngineStepInbox{options: options}
	inbox.stepOperationInbox = stepOperationInbox{
		client: client, database: database, operationCollection: dataEngineOperationCollection,
		owner: options.Owner, leaseDuration: options.LeaseDuration, receiptTTL: options.ReceiptTTL,
		now: time.Now, receipt: inbox.readReceipt,
	}
	return inbox, nil
}

func (inbox *DataEngineStepInbox) EnsureInfrastructure(ctx context.Context) error {
	if inbox == nil || inbox.client == nil {
		return ErrInvalidRecord
	}
	return inbox.ensureOperationIndexes(ctx)
}

// Bind is called from inside the native Nest handler. The inbox owns the
// validated receipt retention policy; core Saga only binds the supplied time.
func (inbox *DataEngineStepInbox) Bind(command Command, reservations ...Reservation) error {
	if inbox == nil || inbox.options.ReceiptTTL <= 0 {
		return ErrInvalidRecord
	}
	if len(reservations) != 1 || reservations[0].Token == 0 || reservations[0].Duplicate {
		return fmt.Errorf("saga dataengine inbox: an active reservation is required")
	}
	reservation := reservations[0]
	digest, err := commandDigest(command)
	if err != nil {
		return err
	}
	if reservation.operationKey != command.IdempotencyKey || reservation.commandID != command.ID || reservation.owner != inbox.options.Owner || !bytes.Equal(reservation.digest, digest) {
		return fmt.Errorf("saga dataengine inbox: reservation does not match command identity")
	}
	// fence 指向这个操作的状态文档：投影时它必须仍以本命令为当前尝试、token 相同、pending、租约未过期。
	fence := coredata.LeaseFence{
		Database: inbox.database, Resource: dataEngineOperationCollection,
		DocumentID: command.IdempotencyKey,
		Owner:      reservation.owner, Token: reservation.Token, Digest: append([]byte(nil), digest...),
	}
	return BindCommand(command, inbox.now().UTC().Add(inbox.options.ReceiptTTL), fence)
}

// Reserve 为一次原生步骤投递取得（或拒绝）执行权，判定见 stepOperationInbox.reserveInTransaction。
func (inbox *DataEngineStepInbox) Reserve(ctx context.Context, command Command) (Reservation, error) {
	if inbox == nil || inbox.client == nil {
		return Reservation{}, ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return Reservation{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Reservation{}, err
	}
	return inbox.reserve(ctx, command, digest)
}

func (inbox *DataEngineStepInbox) Replay(ctx context.Context, command Command) (Completion, bool, error) {
	if inbox == nil || inbox.client == nil {
		return Completion{}, false, ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return Completion{}, false, err
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Completion{}, false, err
	}
	completion, found, err := inbox.readReceipt(ctx, command.ID, digest)
	if err != nil || !found {
		return completion, found, err
	}
	if err := inbox.markCompleted(ctx, command.IdempotencyKey, command.ID, completion); err != nil {
		return Completion{}, false, err
	}
	return completion, true, nil
}

func (inbox *DataEngineStepInbox) readReceipt(ctx context.Context, commandID string, digest []byte) (Completion, bool, error) {
	var receipt dataEngineReceipt
	err := inbox.receipts().FindOne(ctx, bson.M{"_id": dataEngineStepNamespace + "/" + commandID}, &receipt)
	if errors.Is(err, fmongo.ErrNotFound) {
		return Completion{}, false, nil
	}
	if err != nil {
		return Completion{}, false, err
	}
	if !bytes.Equal(receipt.Digest, digest) {
		return Completion{}, false, ErrIdentityConflict
	}
	completion, err := DecodeCompletionEffect(receipt.Payload)
	if err != nil {
		return Completion{}, false, err
	}
	return completion, true, nil
}

func (inbox *DataEngineStepInbox) waitReplay(ctx context.Context, command Command) (Completion, error) {
	ticker := time.NewTicker(inbox.options.PollInterval)
	defer ticker.Stop()
	for {
		completion, found, err := inbox.Replay(ctx, command)
		if err != nil || found {
			return completion, err
		}
		select {
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (inbox *DataEngineStepInbox) receipts() fmongo.ICollection {
	return inbox.client.Database(inbox.database).Collection(dataEngineReceiptCollection)
}
