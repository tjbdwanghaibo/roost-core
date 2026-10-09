//go:build integration

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

func rawTestClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	client, err := NewClient(fnats.DefaultConfig(url), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func rawTestFlush(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRealRawRequestAndSubscriptionDrain(t *testing.T) {
	client := rawTestClient(t)
	subject := fmt.Sprintf("roost.raw.%d", time.Now().UnixNano())
	sub, err := client.SubscribeBounded(subject, fnats.PendingLimits{Messages: 8, Bytes: 4096}, func(msg *fnats.Msg) {
		if err := client.PublishOnce(msg.Reply, append([]byte("reply:"), msg.Data...)); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.RequestContext(ctx, subject, []byte("packet"))
	if err != nil || string(response) != "reply:packet" {
		t.Fatalf("response=%q err=%v", response, err)
	}
	maxPayload, err := client.MaxPayload()
	if err != nil || maxPayload <= 0 {
		t.Fatalf("max payload=%d err=%v", maxPayload, err)
	}
	if err := sub.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	if sub.IsValid() {
		t.Fatal("successful drain left a valid subscription")
	}
	if err := client.PublishOnce("invalid subject", nil); err == nil {
		t.Fatal("invalid subject admitted")
	}
}

func TestRealRawSlowConsumerAndCloseWaitForActualCallback(t *testing.T) {
	client := rawTestClient(t)
	subject := fmt.Sprintf("roost.raw.slow.%d", time.Now().UnixNano())
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	faults := make(chan error, 1)
	sub, err := client.SubscribeBounded(subject, fnats.PendingLimits{Messages: 1, Bytes: 32, OnError: func(err error) {
		select {
		case faults <- err:
		default:
		}
	}}, func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
		select {
		case <-returned:
		default:
			close(returned)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	if err := client.PublishOnce(subject, []byte("first")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback not entered")
	}
	for range 16 {
		if err := client.PublishOnce(subject, []byte("queue overflow")); err != nil {
			t.Fatal(err)
		}
	}
	rawTestFlush(t, client)
	select {
	case err := <-faults:
		if !errors.Is(err, gonats.ErrSlowConsumer) {
			t.Fatalf("fault=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow consumer was not reported")
	}
	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := sub.DrainContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first drain=%v", err)
	}
	cancel()
	client.Close()
	budget, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := sub.DrainContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close must not invent callback completion: %v", err)
	}
	cancel()
	releaseHandler()
	budget, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sub.DrainContext(budget); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("drain returned before callback")
	}
	if err := client.PublishOnce(subject, nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("publish after close=%v", err)
	}
}

func TestRealRawCancellationDoesNotReplayAnAdmittedRequest(t *testing.T) {
	client := rawTestClient(t)
	subject := fmt.Sprintf("roost.raw.cancel.%d", time.Now().UnixNano())
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	sub, err := client.SubscribeBounded(subject, fnats.PendingLimits{Messages: 4, Bytes: 4096}, func(msg *fnats.Msg) { entered <- struct{}{}; <-release; _ = client.PublishOnce(msg.Reply, nil) })
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.RequestContext(ctx, subject, nil); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not arrive")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release waiter")
	}
	unblock()
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err := sub.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(entered) != 0 {
		t.Fatal("canceled request was replayed")
	}
}

func TestRealScopedInboxReusesSubscriptionAndCancelsOnlyItsWaiter(t *testing.T) {
	client := rawTestClient(t)
	prefix := fmt.Sprintf("roost.raw.scoped.n%d", time.Now().UnixNano())
	scoped, err := client.ForInbox(prefix)
	if err != nil {
		t.Fatal(err)
	}
	again, err := client.ForInbox(prefix)
	if err != nil || scoped != again {
		t.Fatalf("inbox not reused: %v", err)
	}
	responder, err := client.SubscribeBounded(prefix+".request", fnats.PendingLimits{Messages: 4096, Bytes: 1 << 20}, func(msg *fnats.Msg) { _ = client.PublishOnce(msg.Reply, msg.Data) })
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	var wait sync.WaitGroup
	for index := range 256 {
		wait.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			payload := []byte(fmt.Sprintf("request-%d", index))
			data, err := scoped.RequestContext(ctx, prefix+".request", payload)
			if err != nil || string(data) != string(payload) {
				t.Errorf("data=%q err=%v", data, err)
			}
		})
	}
	wait.Wait()
	inbox := scoped.(*inboxClient)
	inbox.mu.Lock()
	pending := len(inbox.pending)
	inbox.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending=%d", pending)
	}
	// 取消一个已发出请求只移除它的 waiter；迟到回信不能串到后面的请求。
	requests := make(chan *fnats.Msg, 2)
	delayed, err := client.SubscribeBounded(prefix+".service.delayed", fnats.PendingLimits{Messages: 8, Bytes: 4096}, func(msg *fnats.Msg) { requests <- msg })
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	cancelCtx, cancelWait := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		_, err := scoped.RequestContext(cancelCtx, prefix+".service.delayed", []byte("cancel"))
		canceled <- err
	}()
	var late *fnats.Msg
	select {
	case late = <-requests:
	case <-time.After(time.Second):
		t.Fatal("delayed request not sent")
	}
	cancelWait()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release scoped waiter")
	}
	if err := client.PublishOnce(late.Reply, []byte("late")); err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := scoped.RequestContext(ctx, prefix+".request", []byte("after cancel"))
	if err != nil || string(data) != "after cancel" {
		t.Fatalf("late ACK contaminated next request: %q %v", data, err)
	}
	if err := delayed.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.RequestContext(ctx, prefix+".service.absent", nil); !errors.Is(err, fnats.ErrNoResponders) {
		t.Fatalf("no responder=%v", err)
	}
	if err := responder.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	client.Close()
	if _, err := scoped.RequestContext(ctx, prefix+".request", nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("closed=%v", err)
	}
}
