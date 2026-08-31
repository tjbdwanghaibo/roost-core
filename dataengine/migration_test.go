package dataengine

import (
	"context"
	"errors"
	"testing"
)

type migrationTicket struct {
	done chan struct{}
	err  error
}

func (ticket migrationTicket) Done() <-chan struct{} { return ticket.done }
func (ticket migrationTicket) Err() error            { return ticket.err }

func TestWaitProjectionHonorsTicketAndContext(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if err := WaitProjection(context.Background(), migrationTicket{done: done}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("projection failed")
	if err := WaitProjection(context.Background(), migrationTicket{done: done, err: want}); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitProjection(ctx, migrationTicket{done: make(chan struct{})}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
