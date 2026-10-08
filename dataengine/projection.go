package dataengine

import (
	"context"
	"errors"
)

var ErrSchemaMismatch = errors.New("dataengine: persisted schema does not match runtime schema")

// Descriptor is implemented by generated DAOs to declare their current
// persisted schema.
type Descriptor interface {
	SchemaVersion() uint32
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
