package physics

import (
	"encoding/json"
	"testing"
)

// Unity 读取的场景人口/版本必须与服务器 ID 绑定规则一致，避免各自合法但彼此无法通信。
func TestTerrainManifestMatchesServerPopulation(t *testing.T) {
	var metadata struct {
		Version  int    `json:"version"`
		Players  int    `json:"players"`
		Capsules int    `json:"capsules"`
		Ball     uint64 `json:"big_ball_id"`
	}
	if err := json.Unmarshal(terrainJSON, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Version != 1 || metadata.Players != CrowdPlayers || metadata.Capsules != CrowdCapsules || metadata.Ball != CrowdBallID {
		t.Fatalf("terrain/client metadata disagrees with server: %+v", metadata)
	}
}
