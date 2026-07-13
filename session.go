package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// session implements fmongo.ISession.
type session struct {
	sess *mongo.Session
}

func (s *session) WithTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	_, err := s.sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		if err := fn(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return err
}

func (s *session) EndSession(ctx context.Context) {
	s.sess.EndSession(ctx)
}
