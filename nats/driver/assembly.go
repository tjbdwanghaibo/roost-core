package driver

import (
	"context"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// rpcCallbackWorkers is the size of the RPC callback pool every process
// assembles; it was a literal in the kit Mod before P3b.
const rpcCallbackWorkers = 4

// Assembly is the complete NATS capability set a process publishes: the core
// connection, JetStream on that connection and the request/reply RPC client.
// The bus is assembled by the kit Mod from registry configuration on top of
// Client and RPC; everything that needs driver internals lives here.
type Assembly struct {
	Client    *Client
	JetStream *JetStreamClient
	RPC       *RPCClient
}

// Assemble connects and builds JetStream and RPC on the connection. When
// JetStream cannot be initialised the connection is closed again rather than
// left for the caller to find.
func Assemble(cfg *fnats.Config, extra ClientOptions) (*Assembly, error) {
	client, err := NewClient(cfg, extra)
	if err != nil {
		return nil, err
	}
	jetStream, err := NewJetStreamClient(client)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &Assembly{
		Client:    client,
		JetStream: jetStream,
		RPC:       NewRPCClient(client, fnats.DefaultRetryPolicy(), rpcCallbackWorkers),
	}, nil
}

// Connected reports whether the underlying connection is up (health check).
func (a *Assembly) Connected() bool {
	return a != nil && a.Client.Connected()
}

// Close stops the RPC client (failing every pending call with ErrCancelled)
// and drains the connection within ctx; when the drain does not finish in
// time the connection is closed hard and the ctx error is returned. The bus
// must already be stopped by the caller — it owns subscriptions on Client.
func (a *Assembly) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if a.RPC != nil {
		a.RPC.Stop()
	}
	if a.Client != nil {
		if err := a.Client.DrainWithContext(ctx); err != nil {
			a.Client.Close()
			return err
		}
	}
	return nil
}
