package redis

import (
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"sync"

	goredis "github.com/redis/go-redis/v9"
)

type pubSub struct {
	ps   *goredis.PubSub
	ch   <-chan *fredis.PubSubMessage
	done chan struct{}
	once sync.Once
}

func newPubSub(ps *goredis.PubSub) *pubSub {
	goCh := ps.Channel()
	// Convert go-redis channel to framework channel
	msgCh := make(chan *fredis.PubSubMessage, cap(goCh))
	done := make(chan struct{})
	go func() {
		defer close(msgCh)
		for {
			select {
			case <-done:
				return
			case msg, ok := <-goCh:
				if !ok {
					return
				}
				select {
				case msgCh <- &fredis.PubSubMessage{
					Channel: msg.Channel,
					Payload: msg.Payload,
				}:
				case <-done:
					return
				}
			}
		}
	}()
	return &pubSub{ps: ps, ch: msgCh, done: done}
}

func (p *pubSub) Channel() <-chan *fredis.PubSubMessage {
	return p.ch
}

func (p *pubSub) Close() error {
	var err error
	p.once.Do(func() {
		close(p.done)
		err = p.ps.Close()
	})
	return err
}

var _ fredis.IPubSub = (*pubSub)(nil)
