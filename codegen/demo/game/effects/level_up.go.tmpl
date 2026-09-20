// Package effects names the side effects this game emits from Nest
// transactions. An effect is the transactional outbox: it is admitted to the
// WAL in the same record as the state change, published to JetStream after
// commit, and consumed with a Mongo inbox that makes the consumer's side
// effect happen once. A level-up that reached the store therefore always
// produces exactly one reward mail — and a level-up that rolled back never
// does — without the handler knowing anything about mail.
package effects

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tjbdwanghaibo/roost-core/nest"
)

// TopicPlayerLevelUp is the effect topic; the subject on the wire is
// <dataengine.effects.subject_prefix>.<topic>.
const TopicPlayerLevelUp = "player.level_up"

// PlayerLevelUp is the payload. JSON, because the consumer may not be Go.
type PlayerLevelUp struct {
	PlayerID int64 `json:"player_id"`
	Level    int32 `json:"level"`
}

// EmitPlayerLevelUp records the effect on the current Nest transaction. It
// fails outside one: an effect with no transaction to ride on would be a
// message nobody committed to.
func EmitPlayerLevelUp(playerID int64, level int32) error {
	payload, err := json.Marshal(PlayerLevelUp{PlayerID: playerID, Level: level})
	if err != nil {
		return fmt.Errorf("effects: encode %s: %w", TopicPlayerLevelUp, err)
	}
	return nest.Emit(nest.Effect{
		Topic:   TopicPlayerLevelUp,
		Key:     strconv.FormatInt(playerID, 10),
		Payload: payload,
	})
}
