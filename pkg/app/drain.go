package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/aukilabs/hagall/pkg/admin"
	"github.com/aukilabs/hagall/pkg/admission"
	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/aukilabs/hagall/pkg/provider"
	"github.com/google/uuid"
)

const drainRelinquishConcurrency = 16

type applicationDrainOperation struct {
	request admin.DrainRequest
	context context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	result  admin.DrainResult
	err     error
}

// Drain executes the one immutable planned drain for this process
// incarnation. The private HTTP layer separately enforces exact request replay;
// this coordinator also serializes that operation with signal-driven shutdown.
func (a *Application) Drain(ctx context.Context, request admin.DrainRequest) (admin.DrainResult, error) {
	return a.startOrJoinDrain(ctx, request, false)
}

// DrainForSignal runs the same state machine as POST /drain. If an operations
// drain is already in flight, a process signal joins that immutable operation
// instead of attempting to change its intent mid-flight.
func (a *Application) DrainForSignal(ctx context.Context, intent admin.ShutdownIntent, deadline time.Duration) (admin.DrainResult, error) {
	if deadline < time.Second || deadline > 15*time.Minute || deadline%time.Second != 0 {
		return admin.DrainResult{}, errors.New("signal drain deadline must be whole seconds in [1,900]")
	}
	return a.startOrJoinDrain(ctx, admin.DrainRequest{
		ShutdownIntent:  intent,
		DeadlineSeconds: uint16(deadline / time.Second),
	}, true)
}

func (a *Application) startOrJoinDrain(ctx context.Context, request admin.DrainRequest, joinExisting bool) (admin.DrainResult, error) {
	if a == nil {
		return admin.DrainResult{}, errors.New("relay application is required")
	}
	if ctx == nil {
		return admin.DrainResult{}, errors.New("drain context is required")
	}
	if err := validateDrainRequest(request); err != nil {
		return admin.DrainResult{}, err
	}

	a.drainMu.Lock()
	operation := a.drainOperation
	if operation == nil {
		if a.closing {
			a.drainMu.Unlock()
			return admin.DrainResult{}, errors.New("relay application is closing")
		}
		operationContext, cancel := context.WithTimeout(context.Background(), time.Duration(request.DeadlineSeconds)*time.Second)
		operation = &applicationDrainOperation{
			request: request,
			context: operationContext,
			cancel:  cancel,
			done:    make(chan struct{}),
		}
		a.drainOperation = operation
		go a.runDrainOperation(operation)
	} else if !joinExisting && operation.request != request {
		a.drainMu.Unlock()
		return admin.DrainResult{}, admin.ErrDrainConflict
	}
	a.drainMu.Unlock()

	select {
	case <-ctx.Done():
		return admin.DrainResult{}, ctx.Err()
	case <-operation.done:
		return operation.result, operation.err
	}
}

func validateDrainRequest(request admin.DrainRequest) error {
	switch request.ShutdownIntent {
	case admin.ShutdownIntentRestartSameIdentity, admin.ShutdownIntentReassign:
	default:
		return errors.New("invalid relay shutdown intent")
	}
	if request.DeadlineSeconds < 1 || request.DeadlineSeconds > 900 {
		return errors.New("relay drain deadline must be in [1,900] seconds")
	}
	return nil
}

func (a *Application) runDrainOperation(operation *applicationDrainOperation) {
	defer operation.cancel()
	operation.result, operation.err = a.executeDrain(operation.context, operation.request)
	close(operation.done)
}

func (a *Application) executeDrain(ctx context.Context, request admin.DrainRequest) (admin.DrainResult, error) {
	result := admin.DrainResult{ShutdownIntent: request.ShutdownIntent}
	a.beginDrainGates(request.ShutdownIntent)

	initialIntent := drainIntent(request.ShutdownIntent == admin.ShutdownIntentRestartSameIdentity, request.ShutdownIntent)
	initialStatusErr := a.reportExactDrainStatus(ctx, dmsclient.ProviderStatus{
		AcceptingBookings: false,
		Draining:          true,
		ShutdownIntent:    initialIntent,
	})
	barrierErr := a.joinDrainBarrier(ctx)
	if ctx.Err() != nil && a.cancel != nil {
		// A forced deadline must also interrupt startup/control requests that
		// use the application lifecycle context rather than this drain context.
		a.cancel()
	}
	controlErr := errors.Join(initialStatusErr, barrierErr)

	total, countErr := a.authoritativeLeaseCount(ctx)
	if countErr != nil {
		controlErr = errors.Join(controlErr, fmt.Errorf("read active relay leases before drain: %w", countErr))
		if a.registry != nil {
			total, _ = a.registry.Counts()
		}
	}
	if total < 0 {
		total = 0
	}
	result.LeasesTotal = uint32(total)
	result.LeasesRemaining = result.LeasesTotal

	forcedCircuitClose := a.waitForCircuitGrace(ctx, request.ShutdownIntent, result.LeasesTotal)
	// Status refreshes and lease heartbeats stay live throughout the grace.
	// Stop only the periodic status loop at the release boundary; the final
	// typed status below is serialized against any request already in flight.
	a.stopControlLoop()

	remaining := total
	remoteCleared := false
	workerStopped := false
	if request.ShutdownIntent == admin.ShutdownIntentReassign && a.worker != nil {
		var relinquishErr error
		remaining, relinquishErr = a.worker.RelinquishAll(ctx)
		if remaining < 0 {
			remaining = 0
		}
		a.setDrainIntent(admin.ShutdownIntentReassign)
		finalStatusErr := a.reportExactDrainStatus(ctx, dmsclient.ProviderStatus{
			AcceptingBookings: false,
			Draining:          true,
			ShutdownIntent:    drainIntent(true, admin.ShutdownIntentReassign),
		})
		if finalStatusErr == nil {
			remoteCleared = true
			remaining = 0
			var detailed *provider.RelinquishAllError
			if errors.As(relinquishErr, &detailed) && detailed.Local != nil {
				controlErr = errors.Join(controlErr, fmt.Errorf("close relinquished relay targets: %w", detailed.Local))
			}
			// The final reassign status atomically requeues any residual
			// assignments. Revoke local authority before the following DELETE
			// can spend another network timeout.
			if err := a.worker.ShutdownPreserving(ctx); err != nil {
				controlErr = errors.Join(controlErr, fmt.Errorf("stop local relay booking authority: %w", err))
			}
			workerStopped = true
		} else {
			if relinquishErr != nil {
				controlErr = errors.Join(controlErr, fmt.Errorf("relinquish relay leases: %w", relinquishErr))
			}
			controlErr = errors.Join(controlErr, fmt.Errorf("install final reassign status: %w", finalStatusErr))
		}
	}

	mayRelease := request.ShutdownIntent != admin.ShutdownIntentRestartSameIdentity || initialStatusErr == nil
	if mayRelease {
		releaseErr := a.releaseExactSession(ctx)
		if releaseErr == nil {
			remoteCleared = true
			remaining = 0
		} else {
			controlErr = errors.Join(controlErr, fmt.Errorf("release relay provider session: %w", releaseErr))
		}
	}

	if a.worker != nil && !workerStopped {
		if err := a.worker.ShutdownPreserving(ctx); err != nil {
			controlErr = errors.Join(controlErr, fmt.Errorf("stop local relay booking authority: %w", err))
		}
	}
	a.setDataPlaneReady(false)

	if remoteCleared {
		remaining = 0
	}
	if remaining > int(result.LeasesTotal) {
		result.LeasesTotal = uint32(remaining)
	}
	result.LeasesRemaining = uint32(remaining)
	result.LeasesReleased = result.LeasesTotal - result.LeasesRemaining

	if errors.Is(ctx.Err(), context.DeadlineExceeded) || forcedCircuitClose {
		result.Result = admin.DrainOutcomeDeadlineExceeded
		return result, context.DeadlineExceeded
	}
	if controlErr != nil || result.LeasesRemaining != 0 {
		result.Result = admin.DrainOutcomeControlError
		if controlErr == nil {
			controlErr = errors.New("DMS retained relay leases after drain")
		}
		return result, controlErr
	}
	result.Result = admin.DrainOutcomeCompleted
	return result, nil
}

func (a *Application) beginDrainGates(intent admin.ShutdownIntent) {
	a.gateMu.Lock()
	a.draining = true
	a.shutdownIntent = drainIntent(intent == admin.ShutdownIntentRestartSameIdentity, intent)
	a.gateMu.Unlock()
	if a.state != nil {
		a.state.BeginDrain()
	}
	if a.node != nil && a.node.Host() != nil {
		a.node.Host().RemoveStreamHandler(admission.ProtocolID)
	}
	if a.worker != nil {
		a.worker.BeginDrainGates()
	}
	a.setProviderAccepting(false)
	a.publishSchedulingState()
}

func (a *Application) joinDrainBarrier(ctx context.Context) error {
	if a.worker == nil {
		return nil
	}
	return a.worker.JoinDrainBarrier(ctx)
}

func (a *Application) setDrainIntent(intent admin.ShutdownIntent) {
	a.gateMu.Lock()
	value := string(intent)
	a.shutdownIntent = &value
	a.gateMu.Unlock()
	a.publishSchedulingState()
}

func drainIntent(include bool, intent admin.ShutdownIntent) *string {
	if !include {
		return nil
	}
	value := string(intent)
	return &value
}

func (a *Application) reportExactDrainStatus(ctx context.Context, status dmsclient.ProviderStatus) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := a.lockStatus(ctx); err != nil {
		return err
	}
	defer a.statusMu.Unlock()
	if a.sessionWasReleased() {
		return errors.New("relay provider session is already released")
	}
	a.currentMu.RLock()
	access := a.access
	session := a.session
	a.currentMu.RUnlock()
	if access.Token == "" || session.ProviderSessionID == uuid.Nil {
		return errors.New("relay provider control authority is unavailable")
	}
	updated, actual, err := a.reportProviderStatusWithRetry(ctx, access, session, status)
	if err != nil {
		return err
	}
	if !sameProviderStatus(actual, status) {
		return errors.New("relay drain status changed while it was being reported")
	}
	if err := a.installProviderControl(access, *updated); err != nil {
		return err
	}
	return nil
}

func sameProviderStatus(left, right dmsclient.ProviderStatus) bool {
	if left.AcceptingBookings != right.AcceptingBookings || left.Draining != right.Draining {
		return false
	}
	if left.ShutdownIntent == nil || right.ShutdownIntent == nil {
		return left.ShutdownIntent == nil && right.ShutdownIntent == nil
	}
	return *left.ShutdownIntent == *right.ShutdownIntent
}

func (a *Application) authoritativeLeaseCount(ctx context.Context) (int, error) {
	if a.worker == nil {
		return 0, nil
	}
	return a.worker.ActiveCount(ctx)
}

func (a *Application) waitForCircuitGrace(ctx context.Context, intent admin.ShutdownIntent, total uint32) bool {
	if a.metrics == nil || a.metrics.ActiveCircuits() == 0 {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	graceEnd := time.Now().UTC().Add(a.config.ShutdownDrain)
	cleanupStart := deadline.Add(-a.drainCleanupReserve(intent, total))
	if cleanupStart.Before(graceEnd) {
		graceEnd = cleanupStart
	}
	for a.metrics.ActiveCircuits() > 0 {
		now := time.Now().UTC()
		if !now.Before(graceEnd) {
			return true
		}
		timer := time.NewTimer(time.Until(graceEnd))
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-a.metrics.ActivityChanged():
			timer.Stop()
		case <-timer.C:
			return a.metrics.ActiveCircuits() > 0
		}
	}
	return false
}

func (a *Application) drainCleanupReserve(intent admin.ShutdownIntent, total uint32) time.Duration {
	// stopControlLoop prevents a new periodic status but an already-started
	// status can still own statusMu through two HTTP attempts and one backoff.
	// Reserve that complete attempt plus the exact provider-session DELETE.
	requests := 3
	backoffs := 1
	if intent == admin.ShutdownIntentReassign {
		waves := (int(total) + drainRelinquishConcurrency - 1) / drainRelinquishConcurrency
		// Active before/after, bounded relinquish waves, and up to two final
		// status attempts.
		requests += 4 + waves
		backoffs++
	}
	return time.Duration(requests)*a.config.HTTP.RequestTimeout +
		time.Duration(backoffs)*a.config.Timing.StatusMaxBackoff
}

func (a *Application) stopControlLoop() {
	if a == nil || a.controlStop == nil {
		return
	}
	a.controlStopOnce.Do(func() { close(a.controlStop) })
}

func (a *Application) releaseExactSession(ctx context.Context) error {
	if err := a.lockStatus(ctx); err != nil {
		return err
	}
	defer a.statusMu.Unlock()
	if a.sessionWasReleased() {
		return nil
	}
	a.currentMu.RLock()
	access := a.access
	session := a.session
	a.currentMu.RUnlock()
	if access.Token == "" || session.ProviderSessionID == uuid.Nil {
		return errors.New("relay provider control authority is unavailable")
	}
	if err := a.dms.ReleaseSession(ctx, access.Token, session.ProviderSessionID); err != nil {
		var apiErr *dmsclient.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusGone {
			return err
		}
	}
	a.drainMu.Lock()
	a.sessionReleased = true
	a.drainMu.Unlock()
	return nil
}

func (a *Application) lockStatus(ctx context.Context) error {
	if ctx == nil {
		return errors.New("provider status context is required")
	}
	for {
		if a.statusMu.TryLock() {
			return nil
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (a *Application) sessionWasReleased() bool {
	if a == nil {
		return false
	}
	a.drainMu.Lock()
	released := a.sessionReleased
	a.drainMu.Unlock()
	return released
}

func (a *Application) relayInfo(context.Context) (admin.RelayInfo, error) {
	if a == nil || a.node == nil || a.node.Host() == nil {
		return admin.RelayInfo{}, errors.New("relay host is unavailable")
	}
	addresses := make([]string, 0, len(a.node.Host().Addrs()))
	for _, address := range a.node.Host().Addrs() {
		addresses = append(addresses, address.String())
	}
	sort.Strings(addresses)
	info := admin.RelayInfo{
		PeerID:    a.node.Host().ID().String(),
		Addresses: addresses,
		Limits: admin.RelayLimits{
			DurationSeconds:       a.metadata.Limits.DurationSeconds,
			DataBytesPerDirection: a.metadata.Limits.DataBytesPerDirection,
		},
		Timers: admin.TimerInfo{
			HTTPRequestTimeout:                    a.config.HTTP.RequestTimeout.String(),
			RetryInitialBackoff:                   a.config.Retry.InitialBackoff.String(),
			RetryMaximumBackoff:                   a.config.Retry.MaximumBackoff.String(),
			ProviderStatusInterval:                a.config.Timing.StatusInterval.String(),
			ProviderStatusMaximumBackoff:          a.config.Timing.StatusMaxBackoff.String(),
			ConfiguredProviderSessionTTL:          a.config.Timing.ProviderSessionTTL.String(),
			NodeTokenRefreshFraction:              a.config.Timing.NodeTokenRefreshFraction,
			ProviderLeaseHeartbeatMinimumFraction: a.config.Timing.LeaseHeartbeatMinFraction,
			ProviderLeaseHeartbeatMaximumFraction: a.config.Timing.LeaseHeartbeatMaxFraction,
			ReservationTTL:                        a.config.Relay.ReservationTTL.String(),
			ShutdownDrain:                         a.config.ShutdownDrain.String(),
		},
	}
	if a.sessionWasReleased() {
		return info, nil
	}
	a.currentMu.RLock()
	session := a.session
	a.currentMu.RUnlock()
	if session.ProviderSessionID == uuid.Nil {
		return info, nil
	}
	sessionExpiresAt := session.SessionExpiresAt.UTC()
	jwtExpiresAt := session.ProviderNodeJWTExpiresAt.UTC()
	info.DMS = admin.DMSInfo{
		SessionPresent:           true,
		AcceptingBookings:        session.Status.AcceptingBookings,
		Draining:                 session.Status.Draining,
		ShutdownIntent:           adminIntent(session.Status.ShutdownIntent),
		SessionExpiresAt:         &sessionExpiresAt,
		ProviderNodeJWTExpiresAt: &jwtExpiresAt,
		EffectiveCapacity:        session.EffectiveCapacity,
		SchedulingRevision:       session.SchedulingRevision,
		StatusTTLSeconds:         uint32(session.StatusTTL / time.Second),
		ProviderLeaseTTLSeconds:  uint32(session.ProviderLeaseTTL / time.Second),
		RecoveryGraceSeconds:     uint32(session.RecoveryGrace / time.Second),
	}
	return info, nil
}

func adminIntent(value *string) *admin.ShutdownIntent {
	if value == nil {
		return nil
	}
	intent := admin.ShutdownIntent(*value)
	return &intent
}
