package saga

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	coresaga "github.com/tjbdwanghaibo/cube-core/saga"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	dataEngineClaimCollection   = "_dataengine_inbox_claims"
	dataEngineReceiptCollection = "_dataengine_receipts"
	dataEngineStepNamespace     = "saga-step"
	claimStatusPending          = "pending"
	claimStatusCompleted        = "completed"
)

type DataEngineStepInboxOptions struct {
	Owner         string
	LeaseDuration time.Duration
	ReceiptTTL    time.Duration
	PollInterval  time.Duration
}

type DataEngineStepInbox struct {
	client   fmongo.IMongo
	database string
	options  DataEngineStepInboxOptions
	now      func() time.Time
}

type Reservation struct {
	Token      uint64
	Duplicate  bool
	Completion coresaga.Completion
	commandID  string
	owner      string
	digest     []byte
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

type dataEngineClaim struct {
	ID         string    `bson:"_id"`
	Namespace  string    `bson:"namespace"`
	CommandID  string    `bson:"command_id"`
	Digest     []byte    `bson:"digest"`
	Owner      string    `bson:"owner"`
	LeaseUntil time.Time `bson:"lease_until"`
	LeaseToken uint64    `bson:"lease_token"`
	Status     string    `bson:"status"`
	Completion []byte    `bson:"completion,omitempty"`
	CreatedAt  time.Time `bson:"created_at"`
	UpdatedAt  time.Time `bson:"updated_at"`
	ExpiresAt  time.Time `bson:"expires_at"`
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
	return &DataEngineStepInbox{client: client, database: database, options: options, now: time.Now}, nil
}

func (inbox *DataEngineStepInbox) EnsureInfrastructure(ctx context.Context) error {
	if inbox == nil || inbox.client == nil {
		return coresaga.ErrInvalidRecord
	}
	return inbox.claims().EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "lease_until", Value: 1}}, Name: "claim_expired"},
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "command_id", Value: 1}}, Name: "uniq_command", Unique: true},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Name: "ttl_expires_at", ExpireAt: true, RecreateOnConflict: true},
	})
}

// Bind is called from inside the native Nest handler. The inbox owns the
// validated receipt retention policy; core Saga only binds the supplied time.
func (inbox *DataEngineStepInbox) Bind(command coresaga.Command, reservations ...Reservation) error {
	if inbox == nil || inbox.options.ReceiptTTL <= 0 {
		return coresaga.ErrInvalidRecord
	}
	if len(reservations) != 1 || reservations[0].Token == 0 || reservations[0].Duplicate {
		return fmt.Errorf("saga dataengine inbox: an active reservation is required")
	}
	reservation := reservations[0]
	digest := commandDigest(command)
	if reservation.commandID != command.ID || reservation.owner != inbox.options.Owner || !bytes.Equal(reservation.digest, digest) {
		return fmt.Errorf("saga dataengine inbox: reservation does not match command identity")
	}
	fence := coredata.LeaseFence{
		Database: inbox.database, Resource: dataEngineClaimCollection,
		DocumentID: dataEngineStepNamespace + "/" + command.ID,
		Owner:      reservation.owner, Token: reservation.Token, Digest: append([]byte(nil), digest...),
	}
	return coresaga.BindCommand(command, inbox.now().UTC().Add(inbox.options.ReceiptTTL), fence)
}

func (inbox *DataEngineStepInbox) Reserve(ctx context.Context, command coresaga.Command) (Reservation, error) {
	if inbox == nil || inbox.client == nil {
		return Reservation{}, coresaga.ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return Reservation{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	digest := commandDigest(command)
	for attempt := 0; attempt < 2; attempt++ {
		session, err := inbox.client.StartSession(ctx)
		if err != nil {
			return Reservation{}, err
		}
		var reservation Reservation
		err = session.WithTransaction(ctx, func(txCtx context.Context) error {
			var reserveErr error
			reservation, reserveErr = inbox.reserveInTransaction(txCtx, command.ID, digest)
			return reserveErr
		})
		session.EndSession(ctx)
		if errors.Is(err, fmongo.ErrDuplicateKey) {
			continue
		}
		return reservation, err
	}
	return Reservation{}, fmongo.ErrDuplicateKey
}

func (inbox *DataEngineStepInbox) reserveInTransaction(ctx context.Context, commandID string, digest []byte) (Reservation, error) {
	if completion, found, err := inbox.readReceipt(ctx, commandID, digest); err != nil || found {
		if found {
			_ = inbox.markCompleted(ctx, commandID, completion)
		}
		return Reservation{Duplicate: found, Completion: completion}, err
	}
	now := inbox.now().UTC()
	claimID := dataEngineStepNamespace + "/" + commandID
	var claim dataEngineClaim
	err := inbox.claims().FindOne(ctx, bson.M{"_id": claimID}, &claim)
	if errors.Is(err, fmongo.ErrNotFound) {
		claim = dataEngineClaim{
			ID: claimID, Namespace: dataEngineStepNamespace, CommandID: commandID, Digest: append([]byte(nil), digest...),
			Owner: inbox.options.Owner, LeaseUntil: now.Add(inbox.options.LeaseDuration), LeaseToken: 1,
			Status: claimStatusPending, CreatedAt: now, UpdatedAt: now,
			ExpiresAt: now.Add(inbox.options.ReceiptTTL),
		}
		if _, err := inbox.claims().InsertOne(ctx, claim); err != nil {
			return Reservation{}, err
		}
		return inbox.activeReservation(commandID, digest, 1), nil
	}
	if err != nil {
		return Reservation{}, err
	}
	if !bytes.Equal(claim.Digest, digest) {
		return Reservation{}, coresaga.ErrIdentityConflict
	}
	if claim.Status == claimStatusCompleted {
		completion, err := coresaga.DecodeCompletionEffect(claim.Completion)
		return Reservation{Token: claim.LeaseToken, Duplicate: true, Completion: completion}, err
	}
	if claim.Status != claimStatusPending {
		return Reservation{}, coresaga.ErrConflict
	}
	if claim.LeaseUntil.After(now) {
		return Reservation{Token: claim.LeaseToken, Duplicate: true}, nil
	}
	filter := bson.M{"_id": claimID, "digest": digest, "status": claimStatusPending, "lease_token": claim.LeaseToken, "lease_until": bson.M{"$lte": now}}
	update := bson.M{"$set": bson.M{"owner": inbox.options.Owner, "lease_until": now.Add(inbox.options.LeaseDuration), "updated_at": now}, "$inc": bson.M{"lease_token": 1}}
	var renewed dataEngineClaim
	if err := inbox.claims().FindOneAndUpdate(ctx, filter, update, &renewed, fmongo.FindOneAndUpdateOption{ReturnAfter: true}); err != nil {
		if errors.Is(err, fmongo.ErrNotFound) {
			return Reservation{Token: claim.LeaseToken, Duplicate: true}, nil
		}
		return Reservation{}, err
	}
	return inbox.activeReservation(commandID, digest, renewed.LeaseToken), nil
}

func (inbox *DataEngineStepInbox) activeReservation(commandID string, digest []byte, token uint64) Reservation {
	return Reservation{
		Token: token, commandID: commandID, owner: inbox.options.Owner,
		digest: append([]byte(nil), digest...),
	}
}

func (inbox *DataEngineStepInbox) Replay(ctx context.Context, command coresaga.Command) (coresaga.Completion, bool, error) {
	if inbox == nil || inbox.client == nil {
		return coresaga.Completion{}, false, coresaga.ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return coresaga.Completion{}, false, err
	}
	completion, found, err := inbox.readReceipt(ctx, command.ID, commandDigest(command))
	if err != nil || !found {
		return completion, found, err
	}
	if err := inbox.markCompleted(ctx, command.ID, completion); err != nil {
		return coresaga.Completion{}, false, err
	}
	return completion, true, nil
}

func (inbox *DataEngineStepInbox) readReceipt(ctx context.Context, commandID string, digest []byte) (coresaga.Completion, bool, error) {
	var receipt dataEngineReceipt
	err := inbox.receipts().FindOne(ctx, bson.M{"_id": dataEngineStepNamespace + "/" + commandID}, &receipt)
	if errors.Is(err, fmongo.ErrNotFound) {
		return coresaga.Completion{}, false, nil
	}
	if err != nil {
		return coresaga.Completion{}, false, err
	}
	if !bytes.Equal(receipt.Digest, digest) {
		return coresaga.Completion{}, false, coresaga.ErrIdentityConflict
	}
	completion, err := coresaga.DecodeCompletionEffect(receipt.Payload)
	if err != nil {
		return coresaga.Completion{}, false, err
	}
	return completion, true, nil
}

func (inbox *DataEngineStepInbox) markCompleted(ctx context.Context, commandID string, completion coresaga.Completion) error {
	effect, err := coresaga.NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	now := inbox.now().UTC()
	result, err := inbox.claims().UpdateOne(ctx, bson.M{"_id": dataEngineStepNamespace + "/" + commandID}, bson.M{"$set": bson.M{
		"status": claimStatusCompleted, "completion": effect.Payload, "lease_until": time.Unix(0, 0).UTC(), "updated_at": now,
		"expires_at": now.Add(inbox.options.ReceiptTTL),
	}})
	if err != nil {
		return err
	}
	// A receipt may survive a claim cleanup/migration. It remains authoritative
	// and replayable even when no coordination row exists.
	if result == nil || result.MatchedCount == 0 {
		return nil
	}
	return nil
}

func (inbox *DataEngineStepInbox) waitReplay(ctx context.Context, command coresaga.Command) (coresaga.Completion, error) {
	ticker := time.NewTicker(inbox.options.PollInterval)
	defer ticker.Stop()
	for {
		completion, found, err := inbox.Replay(ctx, command)
		if err != nil || found {
			return completion, err
		}
		select {
		case <-ctx.Done():
			return coresaga.Completion{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (inbox *DataEngineStepInbox) claims() fmongo.ICollection {
	return inbox.client.Database(inbox.database).Collection(dataEngineClaimCollection)
}

func (inbox *DataEngineStepInbox) receipts() fmongo.ICollection {
	return inbox.client.Database(inbox.database).Collection(dataEngineReceiptCollection)
}

func (reservation Reservation) String() string {
	return fmt.Sprintf("token=%d duplicate=%t", reservation.Token, reservation.Duplicate)
}
