package redis

import (
	"sync"

	goredis "github.com/redis/go-redis/v9"
)

type pubSub struct {
	ps   *goredis.PubSub
	ch   <-chan *PubSubMessage
	done chan struct{}
	once sync.Once
}

func newPubSub(ps *goredis.PubSub) *pubSub {
	goCh := ps.Channel()
	// Convert go-redis channel to framework channel
	msgCh := make(chan *PubSubMessage, cap(goCh))
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
				case msgCh <- &PubSubMessage{
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

func (p *pubSub) Channel() <-chan *PubSubMessage {
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

var _ IPubSub = (*pubSub)(nil)
