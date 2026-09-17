package match

import (
	"context"
	"testing"
)

// U-0223 · C2 · RR-20260917-03：内存 Store 的输入与返回值不得与存储共享可变切片。
// 旧实现按值存取，Ticket.Subject.Payload、Match.Members、Match.TicketIDs 都是浅复制：
// 改写 Enqueue 的输入 Payload、任何返回的 Ticket / Match 里的切片，再读存储就是改过的值。
func TestReturnedAndInputSlicesDoNotAliasTheStore(t *testing.T) {
	ctx := context.Background()
	queue := Queue{Mode: "ranked", GroupSize: 2}
	fresh := func(t *testing.T) (Store, Ticket, Ticket) {
		t.Helper()
		s, err := NewMemoryStore(Config{})
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.Enqueue(ctx, queue, Subject{Kind: "player", ID: 1, Payload: []byte("old")}, "")
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Enqueue(ctx, queue, Subject{Kind: "player", ID: 2, Payload: []byte("old")}, "")
		if err != nil {
			t.Fatal(err)
		}
		return s, a, b
	}
	payloadOf := func(t *testing.T, s Store, id string) string {
		t.Helper()
		got, found, err := s.Ticket(ctx, queue, id, Subject{Kind: "player", ID: 1})
		if err != nil || !found {
			t.Fatalf("ticket %s: found=%v err=%v", id, found, err)
		}
		return string(got.Subject.Payload)
	}

	t.Run("enqueue input payload", func(t *testing.T) {
		s, err := NewMemoryStore(Config{})
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte("old")
		a, err := s.Enqueue(ctx, queue, Subject{Kind: "player", ID: 1, Payload: payload}, "")
		if err != nil {
			t.Fatal(err)
		}
		payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("the caller's input slice rewrote the stored payload: %q", got)
		}
	})
	t.Run("enqueue result payload", func(t *testing.T) {
		s, a, _ := fresh(t)
		a.Subject.Payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("Enqueue's returned ticket aliases the store: %q", got)
		}
	})
	t.Run("ticket query payload", func(t *testing.T) {
		s, a, _ := fresh(t)
		got, _, _ := s.Ticket(ctx, queue, a.ID, Subject{Kind: "player", ID: 1})
		got.Subject.Payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("Ticket's result aliases the store: %q", got)
		}
	})
	t.Run("candidates payload", func(t *testing.T) {
		s, a, _ := fresh(t)
		candidates, err := s.Candidates(ctx, queue, 10)
		if err != nil {
			t.Fatal(err)
		}
		candidates[0].Subject.Payload[0] = 'X'
		if got := payloadOf(t, s, a.ID); got != "old" {
			t.Fatalf("Candidates' result aliases the store: %q", got)
		}
	})
	t.Run("commit result members and ticket ids", func(t *testing.T) {
		s, a, b := fresh(t)
		m, err := s.Commit(ctx, queue, []string{a.ID, b.ID})
		if err != nil {
			t.Fatal(err)
		}
		m.Members[0].ID = 999
		m.Members[0].Payload[0] = 'X'
		m.TicketIDs[0] = "forged"
		again, found, err := s.Match(ctx, queue, m.ID)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		if again.Members[0].ID != 1 || string(again.Members[0].Payload) != "old" || again.TicketIDs[0] != a.ID {
			t.Fatalf("Commit's returned match aliases the store: %+v", again)
		}
	})
	t.Run("match query members and ticket ids", func(t *testing.T) {
		s, a, b := fresh(t)
		m, err := s.Commit(ctx, queue, []string{a.ID, b.ID})
		if err != nil {
			t.Fatal(err)
		}
		first, _, _ := s.Match(ctx, queue, m.ID)
		first.Members[0].ID = 999
		first.TicketIDs[0] = "forged"
		again, _, _ := s.Match(ctx, queue, m.ID)
		if again.Members[0].ID != 1 || again.TicketIDs[0] != a.ID {
			t.Fatalf("Match's result aliases the store: %+v", again)
		}
	})
}
