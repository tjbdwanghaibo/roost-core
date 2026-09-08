package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// session implements fmongo.ISession.
type session struct {
	sess    *mongo.Session
	timeout time.Duration
	options *options.TransactionOptionsBuilder
}

func (s *session) WithTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	_, err := s.sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		if err := fn(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}, s.options)
	return err
}

func (s *session) EndSession(ctx context.Context) {
	s.sess.EndSession(ctx)
}
