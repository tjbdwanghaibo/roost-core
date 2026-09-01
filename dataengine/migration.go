package dataengine

import (
	"context"
	"errors"
)

var ErrMigrationConflict = errors.New("dataengine: migration projection conflict")

// Descriptor is implemented by generated DAOs to declare their current
// persisted schema.
type Descriptor interface {
	SchemaVersion() uint32
}

// Migrator upgrades one complete persisted DAO document in memory. The
// returned document is committed as a versioned system transaction before it
// is exposed as a live Entity.
type Migrator interface {
	Migrate([]byte, uint32) ([]byte, error)
}

type ProjectionTicket interface {
	Done() <-chan struct{}
	Err() error
}

type SystemCommitter interface {
	CommitSystem(context.Context, CommitRecord) (ProjectionTicket, error)
}

func WaitProjection(ctx context.Context, ticket ProjectionTicket) error {
	if ticket == nil {
		return errors.New("dataengine: projection ticket is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ticket.Done():
		return ticket.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
