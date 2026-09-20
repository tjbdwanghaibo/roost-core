package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RetryInterval is how often the platform process retries deliveries that
// failed.
//
// This cadence is what makes a failed delivery recoverable. Orders carry
// Attempts, MaxAttempts and NextAttemptAtUnix, and nothing advanced them
// unless a caller happened to call AttemptDelivery — so a delivery that failed
// once stayed failed, which for this package means a player paid and got
// nothing.
const RetryInterval = 30 * time.Second

// run retries pending deliveries until the process is shutting down.
//
// It retries the orders retryOrders reports. There is no way to enumerate
// pending orders without an unbounded scan of the keyspace, and putting such a
// scan on a timer is the shape this repository removes — so a deployment
// supplies them, from whatever pending-delivery index it keeps.
//
// A failed attempt is logged and left for the next tick rather than returned:
// returning would take the process down, turning one undeliverable order into
// an outage for every other payment. ErrDeliveryHeld is expected and not an
// error — it means another attempt is already in flight — and ErrDeliveryExpired
// is logged at error level because it is the terminal state that needs a human:
// the money was taken and the goods will not be granted by any retry.
func (s *Server) run(ctx context.Context) error {
	ticker := time.NewTicker(RetryInterval)
	defer ticker.Stop()
	service, ok := s.Service().(*Service)
	if !ok {
		// The Server only starts on the local implementation, so this cannot
		// happen — and if it ever does, retrying nothing silently would mean
		// paid orders quietly stop being delivered.
		return fmt.Errorf("platform server: the local capability is not a *Service, so no paid order is being retried")
	}
	// Say once, at start, which mode this process is in. A deployment that
	// expects recovery and sees "off" has forgotten to wire its index; the
	// old loop called a private method that always returned nil, so the
	// difference was invisible (RR-20260917-04).
	if service.BackgroundRetryEnabled() {
		slog.Info("platform server: retrying paid orders from the configured pending index",
			"interval", RetryInterval, "batch", RetryBatch)
	} else {
		slog.Info("platform server: background delivery retry is off; no pending-order source is configured",
			"how", "set platform.Config.Pending (Mod.WithPendingOrders)")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			service.retryPendingOnce(ctx)
		}
	}
}

// retryPendingOnce is one pass of the retry loop: read a page of the pending
// index and attempt each order on it. It is a method so a test can drive one
// pass without a ticker.
func (service *Service) retryPendingOnce(ctx context.Context) {
	{
		{
			orderIDs, err := service.pendingOrderIDs(ctx)
			if err != nil {
				// The index is the deployment's to fix; one failing read must
				// not end the loop for every other order.
				slog.Error("platform server: pending order index failed; retrying next tick", "err", err)
				return
			}
			for _, orderID := range orderIDs {
				receipt, err := service.AttemptDelivery(ctx, orderID)
				switch {
				case errors.Is(err, ErrDeliveryHeld):
					// Another attempt holds the reservation. Not an error.
				case errors.Is(err, ErrOrderSettled):
					// A human resolved this order out of band. Logged at info
					// rather than skipped silently: the order is still in
					// retryOrders, so whatever supplies that list has not been
					// told, and a settled order reappearing every tick is the
					// signal that it needs to be.
					slog.Info("platform server: skipping an order settled out of band",
						"order_id", orderID)
				case errors.Is(err, ErrDeliveryExpired):
					slog.Error("platform server: delivery attempts exhausted; a paid order will not be delivered",
						"order_id", orderID)
				case errors.Is(err, ErrOrderInvalid):
					// The index names an order that is not there. It would be
					// read on every page and refused every time, and with a
					// small batch it would be the whole batch, forever
					// (RR-20260919-07). Retiring it is safe exactly because
					// there is nothing to deliver; an index that cannot retire
					// keeps it, and the log is then the operator's signal.
					slog.Error("platform server: an indexed order has no record; retiring the index entry",
						"order_id", orderID, "err", err)
					if retirer, ok := service.cfg.Pending.(PendingRetirer); ok {
						if retireErr := retirer.RetirePending(ctx, orderID); retireErr != nil {
							slog.Error("platform server: index entry not retired",
								"order_id", orderID, "err", retireErr)
						}
					}
				case errors.Is(err, versionstore.ErrMalformedRecord):
					// The order's bytes do not decode, and they will not decode
					// on the next tick either. Left alone it keeps its place at
					// the head of the page: with a batch of 128, 128 such
					// records are the whole batch, forever, and the healthy
					// orders behind them are never even read (RR-20260920-05).
					//
					// It is NOT retired — unlike an index entry with no order,
					// this one names a real record that somebody has to look
					// at. It is pushed back instead, so it keeps its place in
					// the queue without keeping its place at the front, and it
					// stays visible to whoever goes looking.
					slog.Error("platform server: an indexed order cannot be decoded; deferring it so it stops blocking the page",
						"order_id", orderID, "defer", PoisonedRetryDelay, "err", err)
					if deferrer, ok := service.cfg.Pending.(PendingDeferrer); ok {
						if deferErr := deferrer.DeferPending(ctx, orderID, service.cfg.Now().Add(PoisonedRetryDelay)); deferErr != nil {
							slog.Error("platform server: index entry not deferred; it will be read again next tick",
								"order_id", orderID, "err", deferErr)
						}
					}
				case err != nil:
					slog.Error("platform server: delivery attempt failed",
						"order_id", orderID, "err", err)
				case receipt.Delivered && !receipt.Replayed:
					slog.Info("platform server: retried delivery succeeded", "order_id", orderID)
				}
			}
		}
	}
}

// PoisonedRetryDelay is how far a record that cannot be decoded is pushed
// back. Long enough that it stops crowding the page, short enough that a
// deployment which fixes the record (a codec rollback, a manual repair) sees
// it retried without a restart.
const PoisonedRetryDelay = 15 * time.Minute

// PendingDeferrer is implemented by an index that can push an entry's next
// attempt time forward. The Server uses it for records that cannot be
// decoded: they must not be retired — something has to look at them — but
// they must not hold the front of the queue either. An index that cannot
// defer keeps the entry where it is, and the log is then the only signal.
type PendingDeferrer interface {
	DeferPending(ctx context.Context, orderID string, notBefore time.Time) error
}

// The orders this process retries come from the deployment's own pending
// index (platform.Config.Pending, wired through Mod.WithPendingOrders): see
// Service.pendingOrderIDs. There is deliberately no default — enumerating
// pending orders would be the unbounded scan this package refuses — but
// "none configured" is now stated at start rather than mimed by a private
// method that returned nil (RR-20260917-04).
