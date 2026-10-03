package player

import (
	stdcontext "context"
	"errors"
	"fmt"
	"log/slog"

	"example.com/planet/game/chatroom"
	syncsender "example.com/planet/game/handler/syncsender"
	lifecycle "example.com/planet/game/lifecycle"
	player_agent "example.com/planet/game/player_agent"
	apperrors "example.com/planet/internal/errors"
	"example.com/planet/protocol/pb"
	"github.com/tjbdwanghaibo/roost-core/errcode"
	svcchat "github.com/tjbdwanghaibo/roost-core/kit/service/chat"
)

// HandleEnterGame makes sure the authenticated player has a Player Entity.
// A Nest handler addresses an Entity that exists; a first login has none, so
// this endpoint goes through the lifecycle instead of a Sender. GetOrCreate is
// safe to race: two logins of the same new player both end holding the one
// Entity, and only one of them sees Created.
func (controller *Controller) HandleEnterGame(context *player_agent.Context, request *pb.EnterGameRequest) (*pb.EnterGameResponse, error) {
	if context == nil || request == nil {
		return nil, fmt.Errorf("enter_game endpoint: context and request are required")
	}
	// Ownership FIRST, before the Entity is loaded, because loading it is the
	// thing being gated. A Player may be resident in exactly one process: two
	// copies means two version counters over one document, and the loser of
	// that race is a `fatal projection version conflict` that takes the whole
	// process down (GAME_DEMO_TEMPLATE §9.11).
	//
	// So a login that lands on a process which does not own this player is
	// REFUSED by name. It is fail-closed on purpose: the demo has no way to
	// move a resident Entity between processes, so the alternatives are a
	// named refusal the client can act on (reconnect to the owner's gateway,
	// or wait out a dead process's lease) and silent corruption. When the
	// owner is gone the lease lapses within playerroute.Lease and the next
	// attempt succeeds here.
	owners, ownersErr := controller.PlayerOwners()
	if ownersErr != nil {
		slog.Error("enter_game: ownership unavailable", "player_id", context.PlayerID, "err", ownersErr)
		code, reason := errcode.ClientError(ownersErr)
		return &pb.EnterGameResponse{Code: code, Reason: reason}, nil
	}
	// Taking the player into service — the claim and the load — runs on its
	// own budget, a share of this request's dispatch deadline
	// (player_access.tcp.login_timeout, RR-20260926-36). A cold load waits
	// for this player's projections to reach the store; if the store is down
	// that wait does not end on its own, and it must not hold the connection:
	// the client gets login_timeout and retries. What is left of the dispatch
	// budget covers the answer and the placement steps below.
	//
	// Running out of budget stops the WAITING, nothing else. A load another
	// login of the same player is sharing goes on for them; nothing admitted
	// is undone. EnterGame is safe to retry: GetOrCreate finds the Entity a
	// timed-out attempt may have created.
	loginCtx, cancelLogin := controller.loginContext(context.Context())
	defer cancelLogin()
	mine, claimErr := owners.Claim(loginCtx, context.PlayerID)
	if claimErr != nil {
		if answer, cutShort := loginCutShort(context.PlayerID, "claim", claimErr); cutShort {
			return answer, nil
		}
		slog.Error("enter_game: ownership not claimed", "player_id", context.PlayerID, "err", claimErr)
		code, reason := errcode.ClientError(claimErr)
		return &pb.EnterGameResponse{Code: code, Reason: reason}, nil
	}
	if !mine {
		sid, _ := owners.OwnerSID(context.Context(), context.PlayerID)
		slog.Warn("enter_game: player is owned by another process", "player_id", context.PlayerID, "owner_sid", sid)
		code, reason := errcode.ClientError(errcode.Wrap(apperrors.ErrPlayerElsewhere, nil, "player_id", context.PlayerID, "owner_sid", sid))
		return &pb.EnterGameResponse{Code: code, Reason: reason}, nil
	}
	subject, created, err := controller.Players().GetOrCreate(loginCtx, context.PlayerID)
	cancelLogin()
	if err != nil {
		if answer, cutShort := loginCutShort(context.PlayerID, "load", err); cutShort {
			return answer, nil
		}
		code, reason := errcode.ClientError(err)
		if code == errcode.CodeInternal {
			slog.Error("enter_game failed", "player_id", context.PlayerID, "err", err)
		}
		return &pb.EnterGameResponse{Code: code, Reason: reason}, nil
	}
	// The World counts the login in its own transaction: a different Entity,
	// a different lock, a separate Nest call after the Player one completed.
	if err := syncsender.NewRecordEnterSender(controller.NestClient()).Sync_RecordEnter(context.Context(), controller.WorldID()); err != nil {
		slog.Warn("enter_game: world did not record the login", "player_id", context.PlayerID, "err", err)
	}
	// On the map. The position is the player's own state, so it is written in
	// a transaction like everything else; the scene only answers "where can
	// this player stand".
	spawn := lifecycle.WorldSceneConfig().Spawn
	at, err := syncsender.NewEnterSceneSender(controller.NestClient()).MultiSync_EnterScene(context.Context(), subject.ID(), controller.WorldSceneID(), spawn.X, spawn.Y)
	if err != nil {
		slog.Warn("enter_game: player not placed on the map", "player_id", context.PlayerID, "err", err)
	} else if scene, sceneErr := controller.Scene(); sceneErr != nil {
		slog.Warn("enter_game: no scene to join", "player_id", context.PlayerID, "err", sceneErr)
	} else if joinErr := scene.Join(context.Context(), subject, at); joinErr != nil {
		// Placed on the map but not replicated: the player can play, they
		// just do not see the others until the next login.
		slog.Warn("enter_game: player did not join the replicated scene", "player_id", context.PlayerID, "err", joinErr)
	}
	// Anything paid for while this player was offline is settled now. The
	// platform service recorded the grant durably when the payment landed;
	// this is the moment the player is in hand, so it is the moment the goods
	// reach the bag. Best effort — a drain that fails leaves the grants where
	// they are and the next login takes them.
	if drain, drainErr := controller.Purchases(); drainErr != nil {
		slog.Warn("enter_game: paid orders not settled", "player_id", context.PlayerID, "err", drainErr)
	} else if settled, drainErr := drain.DrainPlayer(context.Context(), context.PlayerID); drainErr != nil {
		slog.Warn("enter_game: paid orders not settled", "player_id", context.PlayerID, "err", drainErr)
	} else if len(settled) > 0 {
		slog.Info("enter_game: settled paid orders on login", "player_id", context.PlayerID, "count", len(settled))
	}
	// From here on the player receives world-channel pushes.
	controller.Presence().Add(context.PlayerID)
	// The game announces the login on the world channel through the chat
	// service's privileged path (PublishSystem: no player could have sent
	// this). Best effort — a missed announcement is not a failed login — and
	// idempotent per frame, so a retried EnterGame announces once.
	requestID := fmt.Sprintf("enter:%d:%s:%d", context.PlayerID, context.Session.Principal().SessionID, context.Seq)
	message, err := controller.Messaging().PublishSystem(context.Context(), svcchat.SystemPublishRequest{
		Channel: chatroom.WorldChannel, Actor: chatroom.SystemActor, Type: chatroom.TypeText,
		Body: []byte(fmt.Sprintf("player %d entered the world", context.PlayerID)), RequestID: requestID,
	})
	if err != nil {
		slog.Warn("enter_game: login not announced on the world channel", "player_id", context.PlayerID, "err", err)
	} else {
		controller.deliver(context.Context(), message)
	}
	return &pb.EnterGameResponse{PlayerID: context.PlayerID, Created: created}, nil
}

// loginContext is the login step's share of the request: the configured
// login budget when the transport publishes one, otherwise the request's own
// deadline alone.
func (controller *Controller) loginContext(parent stdcontext.Context) (stdcontext.Context, stdcontext.CancelFunc) {
	if budget := controller.LoginTimeout(); budget > 0 {
		return stdcontext.WithTimeout(parent, budget)
	}
	return stdcontext.WithCancel(parent)
}

// loginCutShort answers a claim or load whose wait was cut short: the login
// budget ran out, or the connection closed under it (a newer login of the
// same session replaced it, and the answer goes nowhere). Both are named —
// login_timeout, retry — rather than reported as a server error, and logged
// as what they are, not in the error log. Any other failure is not its
// business.
func loginCutShort(playerID int64, step string, err error) (*pb.EnterGameResponse, bool) {
	switch {
	case errors.Is(err, stdcontext.DeadlineExceeded):
		slog.Warn("enter_game: login budget exhausted", "step", step, "player_id", playerID, "err", err)
	case errors.Is(err, stdcontext.Canceled):
		slog.Info("enter_game: login abandoned, its connection closed", "step", step, "player_id", playerID, "err", err)
	default:
		return nil, false
	}
	code, reason := errcode.ClientError(errcode.Wrap(apperrors.ErrLoginTimeout, err, "player_id", playerID))
	return &pb.EnterGameResponse{Code: code, Reason: reason}, true
}
