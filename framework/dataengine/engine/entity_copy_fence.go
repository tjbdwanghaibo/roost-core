package engine

import (
	"context"
	"errors"
	"fmt"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// CopyFence identifies the proposed destination binding (source BindEpoch + 1).
// An aborted transfer does not consume an account epoch, so ID is also required.
type CopyFence struct {
	Epoch uint64 `bson:"epoch"`
	ID    string `bson:"transfer"`
}

const copyFenceCollection = "_dataengine_transfer_fences"

var errStaleCopyFence = errors.New("dataengine copy: a different transfer owns this cleanup")

type copyFenceRow struct {
	CopyFence `bson:",inline"`
	From      int32 `bson:"from"`
	To        int32 `bson:"to"`
}

// The unsuffixed physical prefix of the first sid DAO is shared by both endpoints,
// just like that DAO's source/target mapping. One row per moved entity, not per move.
// Never expire the row: a paused cleanup can outlive any timeout. Even an identical
// copy touches it, giving opposite transfers and deletes a common Mongo write conflict.
func (store *MongoStore) touchCopyFence(ctx context.Context, key coredata.DocumentKey, copy *EntityCopy, outbound, drop bool) error {
	prefix, err := store.physicalDatabase(key)
	if err != nil {
		return err
	}
	c := store.client.Database(prefix).Collection(copyFenceCollection)
	filter := bson.M{"_id": copy.EntityID}
	var prev copyFenceRow
	err = c.FindOne(ctx, filter, &prev)
	missing := errors.Is(err, fmongo.ErrNotFound)
	if err != nil && !missing {
		return err
	}
	next := copy.fence
	stale := func() error {
		return fmt.Errorf("%w: %w: entity %d (epoch %d, transfer %q)", ErrEntityCopyConflict, errStaleCopyFence, copy.EntityID, prev.Epoch, prev.ID)
	}
	if copy.fence.ID == "" {
		if !missing {
			return stale()
		}
		return nil
	} // legacy non-transfer API never overrides a known generation
	// A startup scan at A can follow A→B→C. If C is still the same generation,
	// reconcile A against C without replacing the B→C fence (B may still be cleaning up).
	if !missing && !drop && !outbound && prev.Epoch == next.Epoch && prev.To == next.To {
		next = prev
		copy.fence = prev
	}
	if drop && (missing || prev != next) {
		return stale()
	}
	if !missing && (prev.Epoch > next.Epoch || (prev.Epoch == next.Epoch && prev != next && !outbound)) {
		return stale()
	}
	row := bson.M{"_id": copy.EntityID, "epoch": next.Epoch, "transfer": next.ID, "from": next.From, "to": next.To, "touch": bson.NewObjectID()}
	if missing {
		_, err = c.InsertOne(ctx, row)
	} else {
		_, err = c.ReplaceOne(ctx, filter, row)
	}
	return err
}
