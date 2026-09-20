package split

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/app"

	"github.com/tjbdwanghaibo/roost-kit/service/mail"
)

// RewardFlow is business logic that uses mail.
//
// It is written ONCE and deployed either way. Note what is absent: no bus, no
// service type, no client, no knowledge of whether mail runs in this process.
// The only thing it knows is the interface.
type RewardFlow struct {
	mail mail.Mail
}

// NewRewardFlow takes the mail capability out of the registry.
//
// It looks up the INTERFACE. That is the one line that has to be right, and it
// is the same line in both deployments:
//
//   - in the mail process, the capability holds the local *mail.Service
//   - in every other process, it holds a *mail.BusClient
//
// Looking up mail.Mail resolves in both. Looking up *mail.Service would
// compile, pass its tests, and work right up until mail moved into its own
// process — so the capability is published wrapped, and that lookup fails
// everywhere instead of failing later.
func NewRewardFlow(r *app.Registry) (*RewardFlow, error) {
	service, ok := app.Lookup[mail.Mail](r, mail.CapabilityName)
	if !ok || service == nil {
		return nil, fmt.Errorf("reward flow: capability %q not found; add mail.NewMod() in the "+
			"mail process or mail.NewClientMod() here", mail.CapabilityName)
	}
	return &RewardFlow{mail: service}, nil
}

// GrantSeasonReward sends a reward and hands the attachment to the caller's
// inventory exactly once.
//
// The claim sequence is the same locally and remotely, including the property
// it exists for: the token that comes back is CONSTANT for this (player, mail)
// pair, so the inventory side can dedupe on it and a retry — a new connection,
// a restart, an hour later — presents the same key.
func (f *RewardFlow) GrantSeasonReward(ctx context.Context, playerID int64, season string) error {
	sent, err := f.mail.Send(ctx, mail.SendRequest{
		Audience:   mail.AudienceDirect,
		Recipients: []int64{playerID},
		Subject:    "season reward",
		Attachment: []byte(`{"gold":100}`),
		// Seconds, and the field says so. A time.Duration would be the better
		// type inside one process and the wrong one on a wire.
		ExpiresInSeconds: 7 * 24 * 3600,
		// Required: the transports this reaches mail over are at-least-once,
		// so a send without a key is a duplicate mail per redelivery. Derived
		// from what the send IS, not from a clock or a counter.
		RequestID: "season-reward:" + season + ":" + fmt.Sprint(playerID),
	})
	if err != nil {
		return fmt.Errorf("send season reward: %w", err)
	}

	claim, err := f.mail.ReserveClaim(ctx, playerID, sent.ID, "")
	if err != nil {
		return fmt.Errorf("reserve reward: %w", err)
	}
	// grantToInventory dedupes on claim.Token. Whatever happens after this
	// point — a crash, a timeout, a retry with a fresh connection — the token
	// for this mail does not change, so it cannot grant twice.
	if err := grantToInventory(ctx, playerID, claim.Token, claim.Attachment); err != nil {
		// The reservation is released so a retry does not have to wait out
		// the lease. Cancelling reports whether it released anything, which
		// is why the result is checked rather than discarded.
		if _, cancelErr := f.mail.CancelClaim(ctx, playerID, sent.ID, claim.Token); cancelErr != nil {
			return fmt.Errorf("grant reward: %w (releasing the claim also failed: %v)", err, cancelErr)
		}
		return fmt.Errorf("grant reward: %w", err)
	}
	if _, err := f.mail.CommitClaim(ctx, playerID, sent.ID, claim.Token); err != nil {
		// The goods are in the inventory and the claim is not committed. A
		// retry re-reserves under the SAME token, the inventory side dedupes,
		// and the commit lands. Reporting the error is what drives that retry.
		return fmt.Errorf("commit reward claim: %w", err)
	}
	return nil
}

// Unread is the kind of read a player-facing handler does.
//
// The caller's identity is an argument, and it stays an argument across the
// bus. A protocol handler must pass the player id it VERIFIED, not one it read
// out of the packet — that check belongs to the handler and is the same duty
// whether mail is local or remote.
func (f *RewardFlow) Unread(ctx context.Context, verifiedPlayerID int64) (int32, error) {
	summary, err := f.mail.Summary(ctx, verifiedPlayerID)
	if err != nil {
		return 0, err
	}
	return summary.Unread, nil
}

// grantToInventory stands in for the game's own inventory write. It must be
// idempotent on token; see GrantSeasonReward.
// grantToInventory is a package variable rather than a function so the
// example's failure paths can be exercised: the grant is the one step that
// belongs to the game, and the example has no game.
var grantToInventory = func(ctx context.Context, playerID int64, token string, payload []byte) error {
	_, _, _, _ = ctx, playerID, token, payload
	return nil
}
