package driver

import (
	"context"
	"errors"
	"strings"
	"sync"

	gonats "github.com/nats-io/nats.go"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

const maxInboxRequests = 65536
const maxInboxPrefixes = 16

// ForInbox 复用同一 NATS 连接，每个角色前缀只有一个有界回信订阅。
// 请求只登记一个 waiter，回调只做表查找/非阻塞交付；取消移除 waiter，迟到回信丢弃。
// 命名句柄归共享 Client 所有，连接关闭同时唤醒全部 waiter，不另建总线或重投。
func (client *Client) ForInbox(prefix string) (fnats.RawClient, error) {
	if err := client.admit(); err != nil {
		return nil, err
	}
	if prefix == "" || len(prefix) > 384 || strings.ContainsAny(prefix, " *>\t\r\n") {
		return nil, errors.New("nats: invalid raw inbox prefix")
	}
	client.state.inboxMu.Lock()
	defer client.state.inboxMu.Unlock()
	if client.closed() {
		return nil, fnats.ErrClosed
	}
	if existing := client.state.inboxes[prefix]; existing != nil {
		return existing, nil
	}
	if len(client.state.inboxes) >= maxInboxPrefixes {
		return nil, errors.New("nats: raw inbox prefix capacity")
	}
	inbox := &inboxClient{Client: client, prefix: prefix, done: make(chan struct{}), pending: make(map[string]chan inboxResult)}
	sub, err := client.SubscribeBounded(prefix+".*", fnats.PendingLimits{Messages: 4096, Bytes: 16 << 20, OnError: func(err error) { inbox.fail(err) }}, inbox.receive)
	if err != nil {
		return nil, err
	}
	inbox.sub = sub
	if client.state.inboxes == nil {
		client.state.inboxes = make(map[string]*inboxClient)
	}
	client.state.inboxes[prefix] = inbox
	return inbox, nil
}

type inboxResult struct {
	data []byte
	err  error
}
type inboxClient struct {
	*Client
	prefix  string
	sub     fnats.DrainSubscription
	mu      sync.Mutex
	pending map[string]chan inboxResult
	done    chan struct{}
	failure error
}

func (inbox *inboxClient) fail(err error) {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if inbox.failure == nil {
		inbox.failure = err
		close(inbox.done)
	}
}
func (inbox *inboxClient) receive(msg *fnats.Msg) {
	inbox.mu.Lock()
	response := inbox.pending[msg.Subject]
	delete(inbox.pending, msg.Subject)
	inbox.mu.Unlock()
	if response == nil {
		return
	}
	result := inboxResult{data: msg.Data}
	if msg.Status == "503" && len(msg.Data) == 0 {
		result.err = gonats.ErrNoResponders
	}
	response <- result // 专属容量 1 的 waiter，只由第一条回信取得；不阻塞 callback。
}
func (inbox *inboxClient) RequestContext(ctx context.Context, subject string, data []byte) ([]byte, error) {
	if err := inbox.validateSubject(subject); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, inbox.wrapError(errors.New("nats: request context required"))
	}
	if err := ctx.Err(); err != nil {
		return nil, inbox.wrapError(err)
	}
	if int64(len(data)) > inbox.conn.MaxPayload() {
		return nil, inbox.wrapError(gonats.ErrMaxPayload)
	}
	if inbox.closed() {
		return nil, fnats.ErrClosed
	}
	reply := inbox.prefix + "." + strings.TrimPrefix(gonats.NewInbox(), "_INBOX.")
	response := make(chan inboxResult, 1)
	inbox.mu.Lock()
	if inbox.failure != nil {
		err := inbox.failure
		inbox.mu.Unlock()
		return nil, inbox.wrapError(err)
	}
	if len(inbox.pending) >= maxInboxRequests {
		inbox.mu.Unlock()
		return nil, inbox.wrapError(errors.New("nats: raw inbox request capacity"))
	}
	inbox.pending[reply] = response
	inbox.mu.Unlock()
	defer func() { inbox.mu.Lock(); delete(inbox.pending, reply); inbox.mu.Unlock() }()
	if err := inbox.conn.PublishRequest(subject, reply, data); err != nil {
		return nil, inbox.wrapError(err)
	}
	select {
	case result := <-response:
		if inbox.closed() {
			return nil, fnats.ErrClosed
		}
		return result.data, inbox.wrapError(result.err)
	case <-ctx.Done():
		return nil, inbox.wrapError(ctx.Err())
	case <-inbox.done:
		inbox.mu.Lock()
		err := inbox.failure
		inbox.mu.Unlock()
		return nil, inbox.wrapError(err)
	}
}
