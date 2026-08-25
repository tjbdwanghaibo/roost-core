package saga

import "errors"

var (
	ErrInvalidDefinition = errors.New("saga: invalid definition")
	ErrInvalidRecord     = errors.New("saga: invalid record")
	ErrDefinitionMissing = errors.New("saga: definition not registered")
	ErrAlreadyExists     = errors.New("saga: business operation already exists")
	ErrNotFound          = errors.New("saga: not found")
	ErrConflict          = errors.New("saga: optimistic concurrency conflict")
	ErrIdentityConflict  = errors.New("saga: idempotency identity conflict")
	ErrNotWaiting        = errors.New("saga: step is not waiting for a result")
	ErrDeadlineExpired   = errors.New("saga: resume requires a future or explicitly cleared deadline")
	ErrClosed            = errors.New("saga: engine is closed")
)
