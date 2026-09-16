package mail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// vanishingSends models the ledger race the guard exists for: our claim loses
// to a racer, and by the time we read the racer's row back it is gone.
type vanishingSends struct {
	versionstore.Store[string, SentRecord]
}

func (vanishingSends) Create(context.Context, string, SentRecord) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{}, false, nil
}
func (vanishingSends) Get(context.Context, string) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{}, false, nil
}

// danglingSends loses the claim to a racer whose ledger row names an envelope
// that does not exist.
type danglingSends struct {
	versionstore.Store[string, SentRecord]
}

func (danglingSends) Create(context.Context, string, SentRecord) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{}, false, nil
}
func (danglingSends) Get(_ context.Context, requestID string) (versionstore.Versioned[SentRecord], bool, error) {
	return versionstore.Versioned[SentRecord]{Value: SentRecord{RequestID: requestID, MailID: "mail-missing"}, Version: 1}, true, nil
}

// collidingEnvelopes reports every mail id as already taken.
type collidingEnvelopes struct{ *fakeEnvelopes }

func (c collidingEnvelopes) Create(context.Context, Envelope) (bool, error) { return false, nil }

// U-0096 (C2): the three "the world moved under us" branches of Send are
// each a conflict the caller must see, never a silent success — and none of
// them may leave an envelope behind that the ledger does not name.
func TestSendReportsEachLedgerRaceAsConflict(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"claim lost and racer row vanished", func(cfg *Config) { cfg.Sends = vanishingSends{Store: cfg.Sends} }, `send "send-1" vanished during create`},
		{"claim lost and racer names a missing mail", func(cfg *Config) { cfg.Sends = danglingSends{Store: cfg.Sends} }, `send "send-1" names missing mail mail-missing`},
		{"mail id already in use", func(cfg *Config) { cfg.Envelopes = collidingEnvelopes{fakeEnvelopes: cfg.Envelopes.(*fakeEnvelopes)} }, "mail id mail-1 is already in use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mutate)
			_, err := h.service.Send(ctx, directTo(7))
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Send = %v; want ErrConflict containing %q", err, tc.want)
			}
			h.envelopes.mu.Lock()
			stored := len(h.envelopes.items)
			h.envelopes.mu.Unlock()
			if stored != 0 {
				t.Fatalf("a refused send left %d envelope(s) behind", stored)
			}
			if got := h.metrics.Count("accepted:send"); got != 0 {
				t.Fatalf("a refused send was counted as accepted (%d)", got)
			}
		})
	}
}
