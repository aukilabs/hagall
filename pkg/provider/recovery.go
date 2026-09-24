package provider

import "time"

// Active reads the provider's whole assignment set. Bound event-driven scans
// during cancellation/conflict bursts, without polling healthy providers.
const recoveryReconcileInterval = 5 * time.Second

func (w *Worker) requestRecovery() {
	select {
	case w.recoveryNeeded <- struct{}{}:
	default:
	}
}

// recoveryLoop is independent of the claim gate: existing reservations must
// recover even when capacity is full or new bookings are disabled. Reconcile
// serializes with claims and the drain barrier, and installs only DMS authority.
func (w *Worker) recoveryLoop(interval time.Duration) {
	for {
		select {
		case <-w.context.Done():
			return
		case <-w.recoveryNeeded:
		}
		delay := interval
		for {
			// Neither new conflicts nor occupancy signals may bypass this delay.
			if wait(w.context, delay, nil) != nil {
				return
			}
			// Coalesce everything preceding this snapshot. A conflict arriving
			// during reconciliation remains queued for a subsequent pass.
			select {
			case <-w.recoveryNeeded:
			default:
			}
			if w.context.Err() != nil {
				return
			}
			if err := w.Reconcile(w.context); err == nil {
				break
			}
			w.mu.Lock()
			draining := w.draining
			w.mu.Unlock()
			if draining || w.registry.Draining() || w.context.Err() != nil {
				return
			}
			// Active/Recover/Ready may race cancellation or lose a response
			// after commit. Retry the authoritative snapshot, never the old
			// fence. Per-operation telemetry records failures; a transient
			// recovery error must not take down the whole relay.
			delay = nextBackoff(delay, interval, max(interval, w.config.MaximumBackoff))
		}
	}
}
