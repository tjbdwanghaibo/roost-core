package player

import (
	"errors"
	"fmt"
	"log/slog"

	"example.com/planet/game/dungeon"
	player "example.com/planet/game/entities/player"
	syncsender "example.com/planet/game/handler/syncsender"
	playeraccess "example.com/planet/game/playeraccess"
	"example.com/planet/game/ranking"
	apperrors "example.com/planet/internal/errors"
	"example.com/planet/protocol/pb"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	svcrank "github.com/tjbdwanghaibo/roost-core/service/rank"
	svcsession "github.com/tjbdwanghaibo/roost-core/service/session"
)

// DungeonClearExp is what a cleared dungeon is worth; the value and the
// exactly-once rules live in game/dungeon.
const DungeonClearExp = dungeon.ClearExp

// HandleFinishDungeon resolves the run with the session service and pays for
// it through the Player + World claim transaction.
//
// Two things decide whether this call pays, and neither is the request:
//
//   - **The run state the service returned.** Finish is idempotent: a run
//     that is already terminal comes back as it stands with no error, and a
//     run past its deadline comes back expired whatever outcome the caller
//     asked for. So `request.Success` says only what the client wanted;
//     `run.State == StateSucceeded` is the fact. Judging on the request paid
//     for failed runs, expired runs and every replay.
//   - **The claim ledger, and the window it is bounded by.** Two replays can
//     both see a succeeded run, so the
//     reward transaction records the run id on the Player and pays only when
//     it is the transaction that recorded it. Record and payment share one
//     WAL record, so neither can happen without the other. The ledger is
//     bounded by the run's own resolution time, and a run past that window is
//     refused rather than paid — a succeeded run stays succeeded in the
//     session service forever, so "it can no longer be replayed" was never
//     something this endpoint could assume.
//
// The two calls are still not one transaction: a crash between Finish and
// the claim leaves a resolved run unpaid. That direction is recoverable — a
// retry of FinishDungeon returns the same succeeded run and the claim is
// still unmade, so the client (or an operator) gets the reward on the next
// attempt. This is the shape a production game wants; what it must add is a
// retry that is not a client's goodwill.
func (controller *Controller) HandleFinishDungeon(context *playeraccess.Context, request *pb.FinishDungeonRequest) (*pb.FinishDungeonResponse, error) {
	if context == nil || request == nil {
		return nil, fmt.Errorf("finish_dungeon endpoint: context and request are required")
	}
	state, outcome := svcsession.StateFailed, "wiped"
	if request.Success {
		state, outcome = svcsession.StateSucceeded, "cleared"
	}
	run, err := controller.Sessions().Finish(context.Context(), context.PlayerID, request.RunID, state, outcome)
	if err != nil {
		code, reason := errcode.ClientError(err)
		if code == errcode.CodeInternal {
			slog.Error("finish_dungeon failed", "player_id", context.PlayerID, "run_id", request.RunID, "err", err)
		}
		return &pb.FinishDungeonResponse{Code: code, Reason: reason}, nil
	}
	response := &pb.FinishDungeonResponse{State: string(run.State), Outcome: run.Outcome}
	// Only the run the service says succeeded earns a reward — not the one
	// the request hoped for.
	if run.State != svcsession.StateSucceeded {
		return response, nil
	}
	entityID, err := entity.BuildEntityID(context.PlayerID, player.EntityKindPlayer)
	if err != nil {
		return nil, fmt.Errorf("finish_dungeon endpoint: %w", err)
	}
	// When the run resolved, as the session service recorded it — the number
	// the reward's deadline is computed from. It is deliberately not
	// the clock's now and not anything the client sent: a claim window anchored to
	// a time the game invents is a window the game can be talked out of.
	resolvedAt := run.FinishedAtUnix
	if resolvedAt <= 0 {
		resolvedAt = run.StartedAtUnix
	}
	board := ranking.ClearsBoard()
	result, err := syncsender.NewClaimDungeonSender(controller.NestClient()).MultiSync_ClaimDungeon(context.Context(), entityID, controller.WorldID(), run.ID, DungeonClearExp, resolvedAt, controller.BusinessNow().Unix(), board.Season)
	if err != nil {
		// The run is resolved either way; the reward failing is its own
		// coded answer so the client can tell the two apart.
		code, reason := errcode.ClientError(err)
		switch {
		case code == errcode.CodeInternal:
			slog.Error("finish_dungeon: clear reward not granted", "player_id", context.PlayerID, "run_id", request.RunID, "err", err)
		case errors.Is(err, apperrors.ErrDungeonClaimWindow):
			// A run this old can no longer be paid, and that is a decision
			// somebody has to be able to see: it is either a replay that
			// would have been paid twice, or a player who really never
			// collected. Compensation is an operator's call, by mail.
			slog.Warn("finish_dungeon: clear reward window closed", "player_id", context.PlayerID, "run_id", request.RunID, "resolved_at", resolvedAt)
		}
		response.Code, response.Reason = code, reason
		return response, nil
	}
	// A replay answers with the run's terminal state and no reward: it was
	// already paid, and saying so is more useful than repeating the numbers.
	if result.Claimed {
		response.ExpGranted, response.LevelsGained = result.ExpGranted, result.LevelsGained
	}
	// The leaderboard point. It is OUTSIDE the transaction — the board lives
	// in another service — so the two are not atomic: a crash here leaves a
	// paid clear that was never counted. That direction is the recoverable
	// one: the transaction counted the clear on the Player, and the board is
	// brought UP TO that count (UpdateMax, no request id) rather than
	// incremented, so this submit, a retry of it, a replay of any older run
	// and two of them racing all leave the same number. The submit is
	// deliberately not gated on result.Claimed for that reason: a replay that
	// finds the reward already paid is exactly the case where the point may
	// still be missing — and the next clear's snapshot carries it anyway.
	if _, err := controller.Ranking().Submit(context.Context(), board, ranking.ClearsScore(context.PlayerID, result.Clears), svcrank.UpdateMax, ""); err != nil {
		slog.Warn("finish_dungeon: clear not counted on the board", "player_id", context.PlayerID, "run_id", run.ID, "clears", result.Clears, "err", err)
	}
	// The same clear also contributes to the current server-wide window —
	// but only from the call that PAID it (result.Claimed), unlike the board
	// above. The board takes a snapshot of a count the transaction keeps, so
	// resubmitting it is harmless; a contribution is an increment into
	// whichever window the clock is in when it is made, and the coordinator
	// deduplicates per (window, player, run). A replay inside the 4-hour claim
	// window but past a 5-minute activity boundary lands in a new window under
	// a new key and would count again — one run could score in up to 48
	// windows. So the fact that drives the contribution is the payment, the
	// same fact that drives the clear count (maintainer decision 2026-10-18).
	//
	// The cost, deliberately accepted: a crash or a failed call between the
	// payment and this contribution loses the point for good — no replay
	// brings it back. The activity is best effort, and a point refilled later
	// would have been booked in the wrong window anyway.
	if !result.Claimed {
		return response, nil
	}
	if runner, runnerErr := controller.Activities(); runnerErr != nil {
		slog.Warn("finish_dungeon: activity runner unavailable; the clear's point is not contributed and will not be retried", "player_id", context.PlayerID, "run_id", run.ID, "err", runnerErr)
	} else if activityID, _, err := runner.Contribute(context.Context(), context.PlayerID, run.ID); err != nil {
		slog.Warn("finish_dungeon: clear not contributed to the activity; a replay will not retry it",
			"player_id", context.PlayerID, "run_id", run.ID, "activity_id", activityID, "err", err)
	} else {
		// Which window took the point. A caller that reads its standing a
		// moment later may well be looking at the NEXT window; this is what
		// lets it tell that apart from a point that did not count.
		response.ActivityID = activityID
	}
	return response, nil
}
