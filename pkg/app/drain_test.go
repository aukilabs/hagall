package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/admin"
	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/config"
	"github.com/aukilabs/hagall/pkg/ddsclient"
	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/aukilabs/hagall/pkg/provider"
	"github.com/aukilabs/hagall/pkg/telemetry"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

type drainTestDMS struct {
	mu sync.Mutex

	session       dmsclient.Session
	active        []dmsclient.Assignment
	events        []string
	statusErr     error
	relinquishErr error
	releaseErr    error

	acceptingStatusStarted chan struct{}
	acceptingStatusRelease chan struct{}
	drainStatusStarted     chan struct{}
	drainStatusRelease     chan struct{}
	claimStarted           chan struct{}
	claimRelease           chan struct{}
	activeStarted          chan struct{}
	activeRelease          chan struct{}
	heartbeatCalls         int
	claimCalls             int
	statusCalls            int
}

func (*drainTestDMS) OpenSession(context.Context, dmsclient.OpenInput) (*dmsclient.OpenResult, error) {
	return nil, errors.New("unexpected session open")
}

func (d *drainTestDMS) ReportNotAccepting(ctx context.Context, token string, sessionID uuid.UUID, expected dmsclient.Expectations) (*dmsclient.Session, error) {
	return d.ReportStatus(ctx, token, sessionID, expected, dmsclient.ProviderStatus{})
}

func (d *drainTestDMS) ReportStatus(ctx context.Context, _ string, _ uuid.UUID, _ dmsclient.Expectations, status dmsclient.ProviderStatus) (*dmsclient.Session, error) {
	if status.AcceptingBookings && d.acceptingStatusRelease != nil {
		select {
		case d.acceptingStatusStarted <- struct{}{}:
		default:
		}
		select {
		case <-d.acceptingStatusRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if status.Draining && d.drainStatusRelease != nil {
		select {
		case d.drainStatusStarted <- struct{}{}:
		default:
		}
		select {
		case <-d.drainStatusRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statusCalls++
	d.events = append(d.events, "status:"+statusEvent(status))
	if d.statusErr != nil {
		return nil, d.statusErr
	}
	d.session.Status = status
	if status.ShutdownIntent != nil && *status.ShutdownIntent == dmsclient.ShutdownIntentReassign {
		d.active = nil
	}
	result := d.session
	return &result, nil
}

func (d *drainTestDMS) ReleaseSession(context.Context, string, uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, "delete")
	if d.releaseErr != nil {
		return d.releaseErr
	}
	d.active = nil
	return nil
}

func (d *drainTestDMS) Active(ctx context.Context, _ string, _ uuid.UUID) ([]dmsclient.Assignment, error) {
	if d.activeRelease != nil {
		select {
		case d.activeStarted <- struct{}{}:
		default:
		}
		select {
		case <-d.activeRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, "active")
	return append([]dmsclient.Assignment(nil), d.active...), nil
}

func (d *drainTestDMS) Claim(ctx context.Context, _ string, _ uuid.UUID) (*dmsclient.ClaimResult, error) {
	d.mu.Lock()
	d.claimCalls++
	d.mu.Unlock()
	if d.claimRelease != nil {
		select {
		case d.claimStarted <- struct{}{}:
		default:
		}
		select {
		case <-d.claimRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &dmsclient.ClaimResult{RetryAfter: time.Second}, nil
}

func (*drainTestDMS) Recover(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected recover")
}

func (d *drainTestDMS) Heartbeat(_ context.Context, _ string, _ uuid.UUID, assignmentID uuid.UUID, epoch uuid.UUID) (*dmsclient.Assignment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for index := range d.active {
		assignment := d.active[index]
		if assignment.AssignmentID == nil || assignment.ReservationEpoch == nil ||
			*assignment.AssignmentID != assignmentID || *assignment.ReservationEpoch != epoch {
			continue
		}
		lease := time.Now().UTC().Add(400 * time.Millisecond)
		assignment.ProviderLeaseExpiresAt = &lease
		d.active[index] = assignment
		d.heartbeatCalls++
		d.events = append(d.events, "heartbeat")
		return &assignment, nil
	}
	return nil, errors.New("unexpected heartbeat assignment")
}

func (*drainTestDMS) Ready(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID, dmsclient.Metadata) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected ready")
}

func (*drainTestDMS) Fail(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected fail")
}

func (d *drainTestDMS) Relinquish(_ context.Context, _ string, _ uuid.UUID, assignmentID uuid.UUID, _ uuid.UUID) (*dmsclient.Assignment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, "relinquish")
	if d.relinquishErr != nil {
		return nil, d.relinquishErr
	}
	for index := range d.active {
		if d.active[index].AssignmentID != nil && *d.active[index].AssignmentID == assignmentID {
			d.active = append(d.active[:index], d.active[index+1:]...)
			break
		}
	}
	return &dmsclient.Assignment{Revoke: true}, nil
}

func (d *drainTestDMS) record(event string) {
	d.mu.Lock()
	d.events = append(d.events, event)
	d.mu.Unlock()
}

func (d *drainTestDMS) resetEvents() {
	d.mu.Lock()
	d.events = nil
	d.mu.Unlock()
}

func (d *drainTestDMS) snapshot() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	deletes := 0
	for _, event := range d.events {
		if event == "delete" {
			deletes++
		}
	}
	return append([]string(nil), d.events...), deletes
}

func (d *drainTestDMS) lifecycleCounts() (status, heartbeat, claim int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusCalls, d.heartbeatCalls, d.claimCalls
}

func statusEvent(status dmsclient.ProviderStatus) string {
	if status.AcceptingBookings {
		return "accepting"
	}
	if status.ShutdownIntent == nil {
		return "draining"
	}
	return *status.ShutdownIntent
}

func TestRestartDrainInstallsIntentBeforeDeleteAndNeverRelinquishes(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	request := admin.DrainRequest{ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1}

	result, err := application.Drain(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, admin.DrainOutcomeCompleted, result.Result)
	require.Equal(t, uint32(1), result.LeasesTotal)
	require.Equal(t, uint32(1), result.LeasesReleased)
	require.Zero(t, result.LeasesRemaining)
	events, deletes := dms.snapshot()
	require.Equal(t, []string{"status:restart_same_identity", "active", "delete", "close"}, events)
	require.Equal(t, 1, deletes)
	require.NotContains(t, events, "relinquish")
	require.False(t, application.state.Ready())

	replayed, replayErr := application.Drain(context.Background(), request)
	require.NoError(t, replayErr)
	require.Equal(t, result, replayed)
	_, deletes = dms.snapshot()
	require.Equal(t, 1, deletes, "exact replay must not repeat session DELETE")
	require.NoError(t, application.Close(context.Background()))
	_, deletes = dms.snapshot()
	require.Equal(t, 1, deletes, "Close must not repeat a completed planned drain")
}

func TestRestartDrainStatusFailureNeverFallsThroughToDefaultDelete(t *testing.T) {
	application, dms := newDrainTestApplication(t, errors.New("DMS status unavailable"))
	result, err := application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
	})

	require.Error(t, err)
	require.Equal(t, admin.DrainOutcomeControlError, result.Result)
	require.Equal(t, uint32(1), result.LeasesTotal)
	require.Zero(t, result.LeasesReleased)
	require.Equal(t, uint32(1), result.LeasesRemaining)
	events, deletes := dms.snapshot()
	require.Zero(t, deletes, "DELETE without a persisted restart intent would reassign the child")
	require.NotContains(t, events, "relinquish")
	require.Contains(t, events, "close", "local authority must still fail closed")
	require.NoError(t, application.Close(context.Background()))
	_, deletes = dms.snapshot()
	require.Zero(t, deletes, "Close must preserve the failed planned restart intent")
}

func TestCloseRetriesFailedExactSessionReleaseWithoutRepeatingLocalTeardown(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	dms.releaseErr = errors.New("DMS release unavailable")

	require.ErrorContains(t, application.Close(context.Background()), "DMS release unavailable")
	dms.releaseErr = nil
	require.NoError(t, application.Close(context.Background()))
	events, deletes := dms.snapshot()
	require.Equal(t, 2, deletes, "the unfinished exact session release must be retried")
	closeCount := 0
	for _, event := range events {
		if event == "close" {
			closeCount++
		}
	}
	require.Equal(t, 1, closeCount, "retry must not repeat local target teardown")
}

func TestReassignDrainRelinquishesBeforeFinalCatchallAndDelete(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	result, err := application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentReassign, DeadlineSeconds: 1,
	})

	require.NoError(t, err)
	require.Equal(t, admin.DrainOutcomeCompleted, result.Result)
	events, deletes := dms.snapshot()
	require.Equal(t, []string{
		"status:draining", "active", "active", "relinquish", "close", "active",
		"status:reassign", "delete",
	}, events)
	require.Equal(t, 1, deletes)
}

func TestReassignFinalStatusResolvesFailedChildRelinquish(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	dms.relinquishErr = errors.New("relinquish response unavailable")
	result, err := application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentReassign, DeadlineSeconds: 1,
	})

	require.NoError(t, err, "the final reassign status is the authoritative catchall")
	require.Equal(t, admin.DrainOutcomeCompleted, result.Result)
	events, _ := dms.snapshot()
	require.Equal(t, []string{
		"status:draining", "active", "active", "relinquish", "close", "active",
		"status:reassign", "delete",
	}, events)
	dms.mu.Lock()
	require.Empty(t, dms.active)
	dms.mu.Unlock()
}

func TestDrainReplayConflictAndSignalJoinShareOneOperation(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	restart := admin.DrainRequest{ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1}
	first, err := application.Drain(context.Background(), restart)
	require.NoError(t, err)

	_, err = application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentReassign, DeadlineSeconds: 1,
	})
	require.ErrorIs(t, err, admin.ErrDrainConflict)
	joined, err := application.DrainForSignal(context.Background(), admin.ShutdownIntentReassign, time.Second)
	require.NoError(t, err)
	require.Equal(t, first, joined, "a signal must join the already chosen immutable intent")
	_, deletes := dms.snapshot()
	require.Equal(t, 1, deletes)
}

func TestActiveCircuitForcesDeadlineOutcomeButReadinessStaysUpDuringGrace(t *testing.T) {
	metrics, err := telemetry.New(telemetry.Options{
		Registerer: prometheus.NewRegistry(), ConfiguredSlots: 1, UsedSlots: func() int { return 1 },
	})
	require.NoError(t, err)
	metrics.ConnectionOpened()
	application, _ := newDrainTestApplication(t, nil)
	application.metrics = metrics
	application.config.ShutdownDrain = 30 * time.Millisecond
	application.config.HTTP.RequestTimeout = time.Millisecond
	application.config.Timing.StatusMaxBackoff = time.Millisecond

	done := make(chan struct{})
	var result admin.DrainResult
	var drainErr error
	go func() {
		result, drainErr = application.Drain(context.Background(), admin.DrainRequest{
			ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
		})
		close(done)
	}()
	require.Eventually(t, application.state.Draining, time.Second, time.Millisecond)
	require.True(t, application.state.Ready(), "data-plane readiness remains true during pre-release grace")
	select {
	case <-done:
		t.Fatal("drain skipped the configured circuit grace")
	case <-time.After(10 * time.Millisecond):
	}
	<-done
	require.ErrorIs(t, drainErr, context.DeadlineExceeded)
	require.Equal(t, admin.DrainOutcomeDeadlineExceeded, result.Result)
	require.False(t, application.state.Ready())
}

func TestCloseTimeoutCancelsAndJoinsInFlightDrainWithoutDefaultDelete(t *testing.T) {
	metrics, err := telemetry.New(telemetry.Options{
		Registerer: prometheus.NewRegistry(), ConfiguredSlots: 1, UsedSlots: func() int { return 1 },
	})
	require.NoError(t, err)
	metrics.ConnectionOpened()
	application, dms := newDrainTestApplication(t, nil)
	application.metrics = metrics
	application.config.ShutdownDrain = 5 * time.Second
	application.config.HTTP.RequestTimeout = time.Millisecond
	application.config.Timing.StatusMaxBackoff = time.Millisecond
	drainDone := make(chan error, 1)
	go func() {
		_, drainErr := application.Drain(context.Background(), admin.DrainRequest{
			ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 10,
		})
		drainDone <- drainErr
	}()
	require.Eventually(t, application.state.Draining, time.Second, time.Millisecond)

	closeContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	require.NoError(t, application.Close(closeContext))
	require.Less(t, time.Since(started), time.Second)
	require.Error(t, <-drainDone)
	_, deletes := dms.snapshot()
	require.LessOrEqual(t, deletes, 1)
	require.NoError(t, application.Close(context.Background()))
	_, deletesAfterRetry := dms.snapshot()
	require.Equal(t, deletes, deletesAfterRetry, "Close must not fall through to a default session DELETE")
}

func TestPeriodicStatusDoesNothingAfterExactSessionDelete(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	_, err := application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
	})
	require.NoError(t, err)
	eventsBefore, _ := dms.snapshot()
	require.NoError(t, application.reportStatusWithRetry())
	eventsAfter, _ := dms.snapshot()
	require.Equal(t, eventsBefore, eventsAfter)
}

func TestDrainCleanupReserveIncludesInFlightStatusAndReleaseBoundary(t *testing.T) {
	application := &Application{config: config.Defaults()}
	application.config.HTTP.RequestTimeout = 10 * time.Second
	application.config.Timing.StatusMaxBackoff = 5 * time.Second

	require.Equal(t, 35*time.Second, application.drainCleanupReserve(
		admin.ShutdownIntentRestartSameIdentity,
		1,
	))
	// 17 leases need two bounded relinquish waves: the reassign reserve covers
	// the in-flight status, two Active calls, two final-status attempts, those
	// waves, exact DELETE, and both possible status backoffs.
	require.Equal(t, 100*time.Second, application.drainCleanupReserve(
		admin.ShutdownIntentReassign,
		17,
	))
}

func TestDrainSerializesBehindInFlightStartupAcceptingStatusAndWritesLast(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	dms.acceptingStatusStarted = make(chan struct{}, 1)
	dms.acceptingStatusRelease = make(chan struct{})

	startupDone := make(chan error, 1)
	go func() { startupDone <- application.startProvider(context.Background()) }()
	<-dms.acceptingStatusStarted
	drainDone := make(chan error, 1)
	go func() {
		_, err := application.Drain(context.Background(), admin.DrainRequest{
			ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
		})
		drainDone <- err
	}()
	require.Eventually(t, application.state.Draining, time.Second, time.Millisecond)
	close(dms.acceptingStatusRelease)

	require.ErrorContains(t, <-startupDone, "interrupted by drain")
	require.NoError(t, <-drainDone)
	events, _ := dms.snapshot()
	acceptingIndex := -1
	restartIndex := -1
	for index, event := range events {
		switch event {
		case "status:accepting":
			acceptingIndex = index
		case "status:restart_same_identity":
			restartIndex = index
		}
	}
	require.NotEqual(t, -1, acceptingIndex)
	require.Greater(t, restartIndex, acceptingIndex, "drain status must be the last DMS scheduling write")
	require.False(t, application.worker.Accepting())
	require.NoError(t, application.Close(context.Background()))
}

func TestCanceledStartupKeepsOwnedMaintenanceAliveUntilDrainAndClose(t *testing.T) {
	application, dms := newDrainTestApplicationWithLease(t, nil, 400*time.Millisecond, 2)
	application.setDataPlaneReady(false)
	application.setStartupComplete(false)
	application.config.Timing.StatusInterval = 10 * time.Millisecond
	dms.acceptingStatusStarted = make(chan struct{}, 1)
	dms.acceptingStatusRelease = make(chan struct{})

	startupContext, cancelStartup := context.WithCancel(context.Background())
	require.NoError(t, application.activateProviderLifecycle())
	application.startLoops()
	application.startLoops() // Lifecycle supervision is one-shot even if setup retries its call site.
	startupDone := make(chan error, 1)
	go func() { startupDone <- application.completeProviderStartup(startupContext) }()
	select {
	case <-dms.acceptingStatusStarted:
	case <-time.After(time.Second):
		t.Fatal("startup did not reach the cancelable initial accepting status")
	}
	statusBefore, heartbeatBefore, _ := dms.lifecycleCounts()
	cancelStartup()
	select {
	case err := <-startupDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("startup did not return after its context was canceled")
	}

	select {
	case <-application.context.Done():
		t.Fatal("startup cancellation killed the Application-owned lifecycle")
	default:
	}
	application.gateMu.Lock()
	reconciledReady := application.dataPlaneReady
	startupComplete := application.startupComplete
	application.gateMu.Unlock()
	require.True(t, reconciledReady, "readiness may latch only after the successful Active reconciliation")
	require.False(t, startupComplete, "a canceled accepting transition must not open the background acceptance gate")
	require.False(t, application.worker.Accepting())

	require.Eventually(t, func() bool {
		status, heartbeat, claims := dms.lifecycleCounts()
		return status > statusBefore && heartbeat > heartbeatBefore && claims == 0
	}, 2*time.Second, 5*time.Millisecond,
		"owned status and heartbeat maintenance must survive startup cancellation without claiming")

	result, err := application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
	})
	require.NoError(t, err)
	require.Equal(t, admin.DrainOutcomeCompleted, result.Result)
	_, _, claims := dms.lifecycleCounts()
	require.Zero(t, claims)
	select {
	case <-application.context.Done():
		t.Fatal("planned drain must not cancel the Application lifecycle before Close")
	default:
	}

	require.NoError(t, application.Close(context.Background()))
	select {
	case <-application.context.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the owned Application lifecycle")
	}
}

func TestDrainStatusCannotBeRolledBackByDelayedDDSRefresh(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	now := time.Now().UTC()
	signedCapacity := 1
	application.currentMu.Lock()
	application.access.IssuedAt = now.Add(-80 * time.Millisecond)
	application.access.ExpiresAt = now.Add(20 * time.Millisecond)
	application.currentMu.Unlock()
	dds := &scriptedDDS{
		access: ddsclient.Access{
			Token: "newer-token", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
			SchedulingRevision: 8, MaxConcurrency: &signedCapacity,
		},
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	application.dds = dds
	dms.drainStatusStarted = make(chan struct{}, 1)
	dms.drainStatusRelease = make(chan struct{})
	controlDone := make(chan struct{})
	go func() {
		application.controlLoop()
		close(controlDone)
	}()
	<-dds.started

	drainDone := make(chan error, 1)
	go func() {
		_, err := application.Drain(context.Background(), admin.DrainRequest{
			ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
		})
		drainDone <- err
	}()
	<-dms.drainStatusStarted
	close(dds.release)
	// Let the refresh return and queue behind the drain's status serialization.
	time.Sleep(10 * time.Millisecond)
	close(dms.drainStatusRelease)
	require.NoError(t, <-drainDone)
	select {
	case <-controlDone:
	case <-time.After(time.Second):
		t.Fatal("delayed DDS refresh did not stop after drain")
	}

	application.currentMu.RLock()
	access := application.access
	application.currentMu.RUnlock()
	require.Equal(t, "node-token", access.Token)
	require.Equal(t, uint64(7), access.SchedulingRevision)
	require.NoError(t, application.Close(context.Background()))
}

func TestDrainDeadlineCancelsBlockedClaimAndCompletesLocalTeardown(t *testing.T) {
	for _, intent := range []admin.ShutdownIntent{
		admin.ShutdownIntentRestartSameIdentity,
		admin.ShutdownIntentReassign,
	} {
		t.Run(string(intent), func(t *testing.T) {
			application, dms := newDrainTestApplication(t, nil)
			// Make startup reconciliation remove the fixture assignment so the
			// first accepting claim can cross the barrier and block in a
			// context-aware DMS request.
			dms.mu.Lock()
			dms.active = nil
			dms.claimStarted = make(chan struct{}, 1)
			dms.claimRelease = make(chan struct{})
			dms.mu.Unlock()
			require.NoError(t, application.startProvider(context.Background()))
			<-dms.claimStarted

			started := time.Now()
			type drainCallResult struct {
				result admin.DrainResult
				err    error
			}
			drainDone := make(chan drainCallResult, 1)
			go func() {
				result, err := application.Drain(context.Background(), admin.DrainRequest{
					ShutdownIntent: intent, DeadlineSeconds: 1,
				})
				drainDone <- drainCallResult{result: result, err: err}
			}()
			expectedStatus := "status:draining"
			if intent == admin.ShutdownIntentRestartSameIdentity {
				expectedStatus = "status:restart_same_identity"
			}
			require.Eventually(t, func() bool {
				events, _ := dms.snapshot()
				return slices.Contains(events, expectedStatus)
			}, 500*time.Millisecond, time.Millisecond,
				"DMS draining status must be attempted before joining the blocked claim")
			select {
			case <-drainDone:
				t.Fatal("drain returned before the blocked pre-gate claim reached its deadline")
			default:
			}
			call := <-drainDone
			result, err := call.result, call.err
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, admin.DrainOutcomeDeadlineExceeded, result.Result)
			require.Less(t, time.Since(started), 1500*time.Millisecond)
			require.False(t, application.state.Ready())
			require.True(t, application.registry.Draining())
			require.NoError(t, application.Close(context.Background()))
		})
	}
}

func TestDrainDeadlineCancelsBlockedPreStartReconciliation(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	dms.activeStarted = make(chan struct{}, 1)
	dms.activeRelease = make(chan struct{})

	startupDone := make(chan error, 1)
	go func() { startupDone <- application.startProvider(application.context) }()
	<-dms.activeStarted
	started := time.Now()
	result, err := application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, admin.DrainOutcomeDeadlineExceeded, result.Result)
	require.Less(t, time.Since(started), 1500*time.Millisecond)
	require.Error(t, <-startupDone)
	require.False(t, application.state.Ready())
	require.NoError(t, application.Close(context.Background()))
}

func TestDrainDeadlineDoesNotWaitIndefinitelyForStatusSerialization(t *testing.T) {
	application, dms := newDrainTestApplication(t, nil)
	application.statusMu.Lock()
	started := time.Now()
	result, err := application.Drain(context.Background(), admin.DrainRequest{
		ShutdownIntent: admin.ShutdownIntentRestartSameIdentity, DeadlineSeconds: 1,
	})
	application.statusMu.Unlock()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, admin.DrainOutcomeDeadlineExceeded, result.Result)
	require.Less(t, time.Since(started), 1500*time.Millisecond)
	_, deletes := dms.snapshot()
	require.Zero(t, deletes, "restart must not DELETE without a persisted intent")
	require.NoError(t, application.Close(context.Background()))
}

func newDrainTestApplication(t *testing.T, statusErr error) (*Application, *drainTestDMS) {
	return newDrainTestApplicationWithLease(t, statusErr, time.Minute, 1)
}

func newDrainTestApplicationWithLease(t *testing.T, statusErr error, leaseDuration time.Duration, effectiveCapacity int) (*Application, *drainTestDMS) {
	t.Helper()
	now := time.Now().UTC()
	metadata := dmsclient.Metadata{
		BaseAddresses:     []string{"/dns4/relay-a.dev.aukiverse.com/tcp/443/p2p/12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP"},
		EndpointKeys:      []string{"/dns4/relay-a.dev.aukiverse.com/tcp/443"},
		Limits:            dmsclient.Limits{DurationSeconds: 900, DataBytesPerDirection: 1 << 20},
		ConfigFingerprint: "relay-config-v1|test",
	}
	session := dmsclient.Session{
		ProviderSessionID: uuid.New(), EffectiveCapacity: uint32(effectiveCapacity), SchedulingRevision: 7,
		SessionExpiresAt: now.Add(2 * time.Minute), ProviderNodeJWTExpiresAt: now.Add(5 * time.Minute),
		StatusTTL: 3 * time.Minute, ProviderLeaseTTL: 3 * time.Minute, RecoveryGrace: 30 * time.Minute,
		Metadata: metadata,
	}
	assignment := drainTestAssignment(t, now, session, metadata, leaseDuration)
	dms := &drainTestDMS{session: session, active: []dmsclient.Assignment{assignment}, statusErr: statusErr}
	registry, err := booking.New(booking.Config{MaximumBookings: effectiveCapacity, MaximumAdmissions: 8, AdmissionTTL: 30 * time.Second})
	require.NoError(t, err)
	worker, err := provider.New(provider.Options{
		DMS: dms, Registry: registry, Metadata: metadata,
		ClosePeer: func(peer.ID) error { dms.record("close"); return nil },
		Config: provider.Config{
			InitialBackoff: time.Millisecond, MaximumBackoff: 10 * time.Millisecond,
			EmptyClaimMaxJitterFraction: 0.2,
			HeartbeatMinimumFraction:    0.25, HeartbeatMaximumFraction: 0.35,
			ExpirySweepInterval: time.Second, Random: func() float64 { return 0 }, Now: time.Now,
		},
	})
	require.NoError(t, err)
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))
	dms.resetEvents()

	cfg := config.Defaults()
	cfg.AcceptBookings = true
	cfg.Timing.StatusMaxBackoff = time.Millisecond
	signedCapacity := effectiveCapacity
	appContext, cancel := context.WithCancel(context.Background())
	application := &Application{
		config: cfg, state: admin.NewState(), dms: dms, providerDMS: dms,
		registry: registry, worker: worker, context: appContext, cancel: cancel,
		controlStop: make(chan struct{}), deadlineChanged: make(chan struct{}, 1),
		access: ddsclient.Access{
			Token: "node-token", IssuedAt: now, ExpiresAt: session.ProviderNodeJWTExpiresAt,
			SchedulingRevision: session.SchedulingRevision, MaxConcurrency: &signedCapacity,
		},
		session: session, metadata: metadata,
		capacity:       dmsclient.LocalCapacity{Total: effectiveCapacity, PerIP: effectiveCapacity, PerASN: effectiveCapacity},
		dataPlaneReady: true, controlReady: true, keyReady: true, startupComplete: true,
		keyExpiresAt: now.Add(time.Hour), tokenExpiresAt: session.ProviderNodeJWTExpiresAt,
		sessionExpiresAt: session.SessionExpiresAt, sessionTokenExpiresAt: session.ProviderNodeJWTExpiresAt,
	}
	application.state.SetReady(true)
	application.publishSchedulingState()
	return application, dms
}

func drainTestAssignment(t *testing.T, now time.Time, session dmsclient.Session, metadata dmsclient.Metadata, leaseDuration time.Duration) dmsclient.Assignment {
	t.Helper()
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(nil)
	require.NoError(t, err)
	target, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)
	assignmentID := uuid.New()
	epoch := uuid.New()
	lease := now.Add(leaseDuration)
	metadataCopy := metadata
	return dmsclient.Assignment{
		BookingID: uuid.New(), SlotID: uuid.New(), SlotIndex: 0, RelayCount: 1,
		DomainID: uuid.New(), TargetPeerID: target.String(), BookingState: dmsclient.BookingStateActive,
		SlotState: dmsclient.SlotStateReady, ProviderSessionID: &session.ProviderSessionID,
		AssignmentID: &assignmentID, ReservationEpoch: &epoch,
		RequestedUntil: now.Add(time.Hour), AuthorityExpiresAt: now.Add(2 * time.Minute),
		ProviderLeaseExpiresAt: &lease, EffectiveCapacity: session.EffectiveCapacity,
		SchedulingRevision: session.SchedulingRevision, Metadata: &metadataCopy,
	}
}
