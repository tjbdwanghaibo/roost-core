package chat

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func TestReviewCustomSharedChannelCannotReadPrivatePair(t *testing.T) {
	bodies := NewBodyRegistry()
	bodies.MustRegister("text", BodySpec{Validator: TextValidator(100)})
	policy := PolicyFuncs{
		Publish: func(context.Context, Sender, Channel) error { return nil },
		Read: func(_ context.Context, viewer Sender, ch Channel) error {
			if ch.Kind == ChannelPrivate && viewer.RoleID != 1 && viewer.RoleID != 2 {
				return fmt.Errorf("not a participant")
			}
			return nil
		},
	}
	s, err := NewStore(versionstore.NewMemoryStore[string, channelState](), Config{
		Policy: policy, Bodies: bodies, Rules: []ChannelRule{{Kind: "private:1", Scope: ScopeShared, RequiresTarget: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = s.Append(ctx, Sender{RoleID: 1}, PublishRequest{Channel: Channel{Kind: ChannelPrivate, Target: 2}, Type: "text", Body: []byte("secret"), RequestID: "private-msg"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.History(ctx, Sender{RoleID: 3}, HistoryQuery{Channel: Channel{Kind: "private:1", Target: 2}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) > 0 {
		t.Fatalf("shared channel disclosed private message: from=%d body=%s", page.Messages[0].From.RoleID, page.Messages[0].Body)
	}
}
