package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
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
			orderIDs, err := service.pendingOrderIDs(ctx)
			if err != nil {
				// The index is the deployment's to fix; one failing read must
				// not end the loop for every other order.
				slog.Error("platform server: pending order index failed; retrying next tick", "err", err)
				continue
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

// The orders this process retries come from the deployment's own pending
// index (platform.Config.Pending, wired through Mod.WithPendingOrders): see
// Service.pendingOrderIDs. There is deliberately no default — enumerating
// pending orders would be the unbounded scan this package refuses — but
// "none configured" is now stated at start rather than mimed by a private
// method that returned nil (RR-20260917-04).
