package player

import (
	"fmt"
	"log/slog"

	syncsender "example.com/planet/game/handler/syncsender"
	playeraccess "example.com/planet/game/playeraccess"
	"example.com/planet/protocol/pb"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

// HandleWorldStats reads the World's counters through a Nest read handler:
// the World is locked for the read, and only the value comes out.
func (controller *Controller) HandleWorldStats(context *playeraccess.Context, request *pb.WorldStatsRequest) (*pb.WorldStatsResponse, error) {
	if context == nil || request == nil {
		return nil, fmt.Errorf("world_stats endpoint: context and request are required")
	}
	stats, err := syncsender.NewWorldStatsSender(controller.NestClient()).Sync_WorldStats(context.Context(), controller.WorldID())
	if err != nil {
		code, reason := errcode.ClientError(err)
		if code == errcode.CodeInternal {
			slog.Error("world_stats failed", "player_id", context.PlayerID, "err", err)
		}
		return &pb.WorldStatsResponse{Code: code, Reason: reason}, nil
	}
	return &pb.WorldStatsResponse{PlayersEntered: stats.PlayersEntered, MatchesFormed: stats.MatchesFormed, ExpGranted: stats.ExpGranted}, nil
}
