package mail

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReviewExpiredMailboxMakesRoom(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < MaxMailboxEntries; i++ {
		req := directTo(1)
		req.RequestID = fmt.Sprintf("expired-%d", i)
		req.ExpiresInSeconds = 1
		if _, err := h.service.Send(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.advance(2 * time.Second)
	page, err := h.service.List(ctx, 1, "", MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("visible=%d unread=%d", len(page.Items), page.Unread)
	req := directTo(1)
	req.RequestID = "fresh"
	if _, err := h.service.Send(ctx, req); err != nil {
		t.Fatalf("expired invisible mails still block delivery: %v", err)
	}
}
