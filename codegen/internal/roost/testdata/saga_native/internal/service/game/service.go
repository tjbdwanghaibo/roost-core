package game

import (
	"context"
	"errors"
	"fmt"

	db "example.com/planet/db"
	gift "example.com/planet/saga/gift"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
)

// Service wires the two lines each generator asked for: the gift saga's step
// consumers and the level-up effect consumer, started in Init and drained in
// Shutdown.
type Service struct {
	gift     *kitsaga.Steps
	levelUps fnats.IJetStreamSubscription
}

func New() *Service                    { return &Service{} }
func (*Service) Name() app.ServiceName { return app.ServiceName("game") }

func (s *Service) Init(registry *app.Registry) error {
	mongoClient, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
	if !ok || mongoClient == nil {
		return errors.New("game: mongo is not available")
	}
	// Player DAO 的 db=main 是逻辑库名：审计记录写在它经 dataengine.databases 映射到的物理库。
	databases, err := kitdataengine.Databases(registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	audit := mongoClient.Database(databases.Physical(db.DatabaseMain)).Collection("gift_audit")
	steps, err := gift.StartSteps(context.Background(), registry, gift.StepDeps{Audit: audit})
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.gift = steps
	if s.levelUps, err = startPlayerLevelUpEffectConsumer(context.Background(), registry); err != nil {
		return fmt.Errorf("game: %w", err)
	}
	return nil
}

func (*Service) Serve(ctx context.Context) error { <-ctx.Done(); return nil }

func (s *Service) Shutdown(ctx context.Context) error {
	if s.levelUps != nil {
		s.levelUps.Drain()
	}
	if s.gift != nil {
		if err := s.gift.Drain(ctx); err != nil {
			s.gift.Stop()
			return err
		}
	}
	return nil
}

var _ app.Service = (*Service)(nil)
