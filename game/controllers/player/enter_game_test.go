package player

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	playerentity "example.com/planet/game/entities/player"
	lifecycle "example.com/planet/game/lifecycle"
	player_agent "example.com/planet/game/player_agent"
	apperrors "example.com/planet/internal/errors"
	"example.com/planet/protocol/msgid"
	"example.com/planet/protocol/pb"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20260926-36: a cold login waits for the player's admitted transactions
// to be projected before it reads the store. With the store down the
// projector retries without end, so that wait never finishes on its own.
// Before the fix EnterGame ran it on the connection's context — no deadline at
// all — and the connection stayed stuck until the process stopped.
//
// The pieces are the real ones: the projector (with a store that keeps
// failing), its WaitEntityProjection gate and the ManagerAccess load flight.
// Only the store behind the gate and the ownership table are stood in.

type alwaysOwner struct{}

func (alwaysOwner) Claim(context.Context, int64) (bool, error)     { return true, nil }
func (alwaysOwner) OwnerSID(context.Context, int64) (int32, error) { return 0, nil }

// fixedLoginBudget stands in for the player TCP Runtime's LoginTimeout.
type fixedLoginBudget time.Duration

func (budget fixedLoginBudget) LoginTimeout() time.Duration { return time.Duration(budget) }

type unavailableStore struct{ attempts atomic.Int64 }

func (store *unavailableStore) Project(context.Context, coredata.CommitRecord) error {
	store.attempts.Add(1)
	return errors.New("mongo: server selection timeout (transient)")
}

// projectionGatedLoader is the Data Engine repository's cold-load order: wait
// for this entity's projections, then read. The read is never reached here.
// Only a load flight's leader calls it, so loads counts flights, not waiters.
type projectionGatedLoader struct {
	projector *engine.Projector
	loads     chan struct{}
}

func (loader projectionGatedLoader) LoadEntity(ctx context.Context, id int64, _ entity.EntityKind) (entity.IThreadSafeEntity, error) {
	loader.loads <- struct{}{}
	if err := loader.projector.WaitEntityProjection(ctx, id); err != nil {
		return nil, err
	}
	return nil, errors.New("the projection finished, which this test's store never lets happen")
}

type stuckLogin struct {
	controller *Controller
	players    *lifecycle.PlayerLifecycle
	projector  *engine.Projector
	store      *unavailableStore
	loads      chan struct{}
}

const (
	stuckPlayerID    = int64(4242)
	stuckLoginBudget = 200 * time.Millisecond
)

// newStuckLogin assembles EnterGame over a player whose one admitted
// transaction the store will not take.
func newStuckLogin(t *testing.T) *stuckLogin {
	t.Helper()
	playerentity.RegisterEntity()
	fullID, err := entity.BuildEntityID(stuckPlayerID, playerentity.EntityKindPlayer)
	if err != nil {
		t.Fatal(err)
	}
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	store := &unavailableStore{}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{ReplayBatchRecords: 1, RetryMin: time.Millisecond, RetryMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	payload, err := bson.Marshal(bson.M{"_id": fullID})
	if err != nil {
		t.Fatal(err)
	}
	var id coredata.TransactionID
	id[15] = 1
	record := coredata.CommitRecord{ID: id, Durability: corenest.DurabilityStrict, Mutations: []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: "game", Resource: "player", ID: fullID},
		Kind: coredata.MutationPut, NextVersion: 1, Data: payload,
	}}}
	if err := projector.Commit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(record.ID)

	loads := make(chan struct{}, 8)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	if _, err := access.ConfigureLoader(projectionGatedLoader{projector: projector, loads: loads}); err != nil {
		t.Fatal(err)
	}
	players, err := lifecycle.NewPlayerLifecycle(access)
	if err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(viper.New())
	if err := registry.Register(playerTCPCapability, fixedLoginBudget(stuckLoginBudget)); err != nil {
		t.Fatal(err)
	}
	controller := &Controller{registry: registry, players: players}
	var owners playerOwners = alwaysOwner{}
	controller.owners.Store(&owners)
	return &stuckLogin{controller: controller, players: players, projector: projector, store: store, loads: loads}
}

// enter runs EnterGame under the deadline the transport gives a request (the
// login budget is a share of it) and checks the answer: login_timeout, within
// the login budget.
func (login *stuckLogin) enter(t *testing.T) {
	t.Helper()
	dispatchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	response, err := login.controller.HandleEnterGame(&player_agent.Context{BaseCtx: dispatchCtx, PlayerID: stuckPlayerID, MsgID: msgid.MsgEnterGame, Seq: 1}, &pb.EnterGameRequest{})
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("enter game returned a transport error instead of an answer: %v", err)
	}
	t.Logf("answered after %v: code=%d reason=%q, projection attempts so far=%d", elapsed.Round(time.Millisecond), response.Code, response.Reason, login.store.attempts.Load())
	if elapsed > stuckLoginBudget+800*time.Millisecond {
		t.Errorf("a cold login stuck on its projection answered after %v; the login budget is %v", elapsed.Round(time.Millisecond), stuckLoginBudget)
	}
	if response.Code != apperrors.ErrLoginTimeout.Code() {
		t.Errorf("code=%d reason=%q, want login_timeout (%d): a login that ran out of budget must say so by name", response.Code, response.Reason, apperrors.ErrLoginTimeout.Code())
	}
}

func TestEnterGameColdLoadStuckOnProjectionAnswersWithinTheLoginBudget(t *testing.T) {
	t.Run("the login leads the load", func(t *testing.T) {
		login := newStuckLogin(t)
		login.enter(t)
		// The login gave up, but the load it started keeps running: the load
		// runs on a context of its own (RR-20260926-54), bounded by the
		// framework load timeout and the loader's shutdown, not by whoever
		// happened to start it. The next attempt joins that live load instead
		// of starting another one, and answers within its own budget again.
		login.enter(t)
		if got := len(login.loads); got != 1 {
			t.Errorf("two logins started %d loads, want 1: the second login must join the load still in flight", got)
		}
	})
	t.Run("the login joins another load", func(t *testing.T) {
		login := newStuckLogin(t)
		// Another caller is already loading this player, with all the time
		// it wants; the login joins that flight.
		leaderCtx, stopLeader := context.WithCancel(context.Background())
		defer stopLeader()
		leaderDone := make(chan error, 1)
		go func() {
			_, err := login.players.Get(leaderCtx, stuckPlayerID)
			leaderDone <- err
		}()
		select {
		case <-login.loads:
		case <-time.After(2 * time.Second):
			t.Fatal("the leading load never started")
		}
		login.enter(t)
		if got := len(login.loads); got != 0 {
			t.Fatalf("the login started its own load (%d) instead of joining the one in flight", got)
		}
		// The waiter leaving did not end the load it was sharing, and did not
		// stop the projector retrying the transaction that load waits for.
		attempts := login.store.attempts.Load()
		select {
		case err := <-leaderDone:
			t.Fatalf("the shared load ended when one of its waiters gave up: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if login.store.attempts.Load() <= attempts {
			t.Error("the projector stopped retrying after the login gave up")
		}
		// Closing the projector is what ends that load: the wait learns the
		// projector will not project this entity any more.
		_ = login.projector.Close(context.Background())
		select {
		case err := <-leaderDone:
			if err == nil {
				t.Fatal("the shared load succeeded against a store that never took the write")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the shared load did not end when the projector closed")
		}
	})
}
