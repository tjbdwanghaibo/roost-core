package effects

import (
	"encoding/json"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/nest"
)

// TopicActivityPhaseDue is the effect the World's timer records when an
// activity window reaches its close phase.
const TopicActivityPhaseDue = "activity.phase_due"

// ActivityPhaseDue is the payload: which window came due.
type ActivityPhaseDue struct {
	ActivityID string `json:"activity_id"`
}

// EmitActivityPhaseDue records the effect on the current Nest transaction.
//
// The timer handler could call the activity coordinator directly — it has the
// activity id and the client is a package away — and that is exactly what this
// avoids: the handler runs under the World's lock, and a bus call there makes
// one slow service into a stalled World. Riding the outbox instead means the
// notification is committed with the timer's own removal (so it cannot be
// lost) and made outside the lock (so it cannot block anything).
func EmitActivityPhaseDue(activityID string) error {
	payload, err := json.Marshal(ActivityPhaseDue{ActivityID: activityID})
	if err != nil {
		return fmt.Errorf("effects: encode %s: %w", TopicActivityPhaseDue, err)
	}
	return nest.Emit(nest.Effect{
		Topic:   TopicActivityPhaseDue,
		Key:     activityID,
		Payload: payload,
	})
}
