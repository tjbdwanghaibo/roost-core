package player

import (
	"fmt"
	"log/slog"

	player_agent "example.com/planet/game/player_agent"
	"example.com/planet/protocol/pb"
	"github.com/tjbdwanghaibo/roost-core/errcode"
)

// HandleSceneReady releases this player's replication session. EnterGame
// joined the scene with the session HELD — subscribed, receiving nothing —
// because the client installs its state decoder only after the login answer;
// this is the client saying the decoder is in place, and the next tick sends
// its first snapshot.
func (controller *Controller) HandleSceneReady(context *player_agent.Context, request *pb.SceneReadyRequest) (*pb.SceneReadyResponse, error) {
	if context == nil || request == nil {
		return nil, fmt.Errorf("scene_ready endpoint: context and request are required")
	}
	scene, err := controller.Scene()
	if err != nil {
		code, reason := errcode.ClientError(err)
		return &pb.SceneReadyResponse{Code: code, Reason: reason}, nil
	}
	if err := scene.Ready(context.PlayerID); err != nil {
		code, reason := errcode.ClientError(err)
		slog.Warn("scene_ready: session not released", "player_id", context.PlayerID, "err", err)
		return &pb.SceneReadyResponse{Code: code, Reason: reason}, nil
	}
	return &pb.SceneReadyResponse{}, nil
}
