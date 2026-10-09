package account

import (
	"context"
	"testing"
)

func TestUpsertServerRejectsNegativeIDAndUnknownStatus(t *testing.T) {
	s, _, _ := newService(t)
	for _, server := range []GameServer{{ID: -1, Status: ServerOpen}, {ID: 2, Status: "typo"}} {
		if _, err := s.UpsertServer(context.Background(), server); err == nil {
			t.Errorf("invalid server accepted: %+v", server)
		}
	}
}
