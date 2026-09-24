package provider

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

// Model DMS's requester-failure contract: rotate the epoch, retain the slot
// as recovering, exclude it from claims, and reject the old heartbeat fence.
type recoveryDMS struct {
	*fakeDMS
	activeCalls     atomic.Int64
	conflicts       atomic.Int64
	activeHook      func(context.Context, int64, []dmsclient.Assignment) ([]dmsclient.Assignment, error)
	loseReady       atomic.Bool
	cancelOnRecover atomic.Bool
}

func (d *recoveryDMS) Active(ctx context.Context, token string, session uuid.UUID) ([]dmsclient.Assignment, error) {
	assignments, err := d.fakeDMS.Active(ctx, token, session)
	call := d.activeCalls.Add(1)
	if d.activeHook != nil {
		return d.activeHook(ctx, call, assignments)
	}
	return assignments, err
}

func (d *recoveryDMS) Heartbeat(ctx context.Context, token string, session, assignment, epoch uuid.UUID) (*dmsclient.Assignment, error) {
	d.mu.Lock()
	current := false
	for _, item := range d.active {
		if item.AssignmentID != nil && *item.AssignmentID == assignment && item.ReservationEpoch != nil && *item.ReservationEpoch == epoch {
			current = true
		}
	}
	d.mu.Unlock()
	if !current {
		d.conflicts.Add(1)
		return nil, &dmsclient.APIError{StatusCode: http.StatusConflict}
	}
	return d.fakeDMS.Heartbeat(ctx, token, session, assignment, epoch)
}

func (d *recoveryDMS) Recover(ctx context.Context, token string, session, assignment, epoch uuid.UUID) (*dmsclient.Assignment, error) {
	if d.cancelOnRecover.Swap(false) {
		d.mu.Lock()
		d.removeActiveLocked(assignment)
		d.mu.Unlock()
		return nil, &dmsclient.APIError{StatusCode: http.StatusConflict}
	}
	return d.fakeDMS.Recover(ctx, token, session, assignment, epoch)
}

func (d *recoveryDMS) Ready(ctx context.Context, token string, session, assignment, epoch uuid.UUID, metadata dmsclient.Metadata) (*dmsclient.Assignment, error) {
	ready, err := d.fakeDMS.Ready(ctx, token, session, assignment, epoch, metadata)
	if err == nil && d.loseReady.Swap(false) {
		return nil, errors.New("ready committed but response was lost")
	}
	return ready, err
}

func recoveringReplacement(assignment dmsclient.Assignment) dmsclient.Assignment {
	assignment.SlotState = dmsclient.SlotStateRecovering
	assignment.ReservationEpoch = uuidPointer(uuid.New())
	assignment.ProviderLeaseExpiresAt = nil
	deadline := time.Now().UTC().Add(time.Minute)
	assignment.RecoveryExpiresAt = &deadline
	return assignment
}

func TestHeartbeatConflictDiscoversRecovery(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "accepting_with_spare_capacity"
		capacity := uint32(2)
		if full {
			name = "full_and_not_accepting"
			capacity = 1
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			metadata := testMetadata()
			session := testSession(now, metadata, capacity)
			old := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
			lease := now.Add(2 * time.Second)
			old.ProviderLeaseExpiresAt = &lease
			dms := &recoveryDMS{fakeDMS: &fakeDMS{active: []dmsclient.Assignment{old}, metadata: metadata}}
			registry := testRegistry(t, int(capacity))
			var closed atomic.Int64
			worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { closed.Add(1); return nil })
			require.NoError(t, worker.SetControl("node-token", session))
			require.NoError(t, worker.Reconcile(context.Background()))
			replacement := recoveringReplacement(old)
			dms.mu.Lock()
			dms.active = []dmsclient.Assignment{replacement}
			dms.mu.Unlock()
			require.NoError(t, worker.Start(context.Background()))
			t.Cleanup(func() { require.NoError(t, worker.Shutdown(context.Background())) })
			worker.SetAccepting(!full)
			target := mustPeer(t, old.TargetPeerID)
			require.Eventually(t, func() bool { return dms.conflicts.Load() == 1 && !registry.AllowReserve(target) }, time.Second, time.Millisecond)
			require.Eventually(t, func() bool {
				snapshot, ok := registry.SnapshotForAdmission(target, old.DomainID)
				return ok && snapshot.Authority.Fence.ReservationEpoch == *replacement.ReservationEpoch
			}, recoveryReconcileInterval+2*time.Second, 5*time.Millisecond)
			require.EqualValues(t, 2, dms.activeCalls.Load())
			dms.mu.Lock()
			require.Equal(t, 1, dms.recoverCalls)
			require.Equal(t, 1, dms.readyCalls)
			if !full {
				require.Greater(t, dms.claimCalls, 0, "successful empty claims must not prevent recovery")
			} else {
				require.Zero(t, dms.claimCalls)
			}
			dms.mu.Unlock()
			// A late old-fence teardown must not close the recovered peer.
			require.NoError(t, worker.revoke(old))
			require.True(t, registry.AllowReserve(target))
			require.EqualValues(t, 1, closed.Load())
		})
	}
}

// Exercise the production loop with shorter delays, without starting claims or
// heartbeats. The end-to-end worker test above uses the production interval.
func startRecoveryTestLoop(t *testing.T, worker *Worker, interval time.Duration) {
	t.Helper()
	worker.context, worker.cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.recoveryLoop(interval) }()
	t.Cleanup(func() {
		worker.cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("recovery loop did not stop")
		}
	})
}

func TestRecoveryCoalescesBurstsAndPreservesConflictsDuringSnapshot(t *testing.T) {
	metadata := testMetadata()
	session := testSession(time.Now().UTC(), metadata, 1)
	started, release := make(chan struct{}), make(chan struct{})
	snapshots := make(chan time.Time, 2)
	dms := &recoveryDMS{fakeDMS: &fakeDMS{metadata: metadata}}
	dms.activeHook = func(ctx context.Context, call int64, assignments []dmsclient.Assignment) ([]dmsclient.Assignment, error) {
		select {
		case snapshots <- time.Now():
		default:
		}
		if call == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return assignments, nil
	}
	worker := testWorker(t, dms, testRegistry(t, 1), metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	const interval = 20 * time.Millisecond
	startRecoveryTestLoop(t, worker, interval)
	for range 1000 {
		worker.requestRecovery()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("recovery snapshot did not start")
	}
	for range 1000 {
		worker.requestRecovery()
	}
	close(release)
	require.Eventually(t, func() bool { return dms.activeCalls.Load() == 2 }, time.Second, time.Millisecond)
	first, second := <-snapshots, <-snapshots
	require.GreaterOrEqual(t, second.Sub(first), interval, "conflicts must not bypass the scan delay")
	require.Never(t, func() bool { return dms.activeCalls.Load() > 2 }, 3*interval, time.Millisecond)
}

func TestRecoveryRetriesAmbiguousResponsesAndConcurrentCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "lost_ready_response"
		if cancel {
			name = "canceled_between_active_and_recover"
		}
		t.Run(name, func(t *testing.T) {
			metadata := testMetadata()
			session := testSession(time.Now().UTC(), metadata, 1)
			assignment := recoveringReplacement(testAssignment(t, time.Now().UTC(), session, 0, dmsclient.SlotStateReady, metadata))
			dms := &recoveryDMS{fakeDMS: &fakeDMS{metadata: metadata, active: []dmsclient.Assignment{assignment}}}
			dms.loseReady.Store(!cancel)
			dms.cancelOnRecover.Store(cancel)
			dms.activeHook = func(_ context.Context, call int64, assignments []dmsclient.Assignment) ([]dmsclient.Assignment, error) {
				if call == 1 {
					return nil, &dmsclient.APIError{StatusCode: http.StatusServiceUnavailable}
				}
				return assignments, nil
			}
			registry := testRegistry(t, 1)
			worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
			require.NoError(t, worker.SetControl("node-token", session))
			startRecoveryTestLoop(t, worker, 10*time.Millisecond)
			worker.requestRecovery()
			require.Eventually(t, func() bool { return dms.activeCalls.Load() >= 3 }, time.Second, time.Millisecond)
			require.Never(t, func() bool { return dms.activeCalls.Load() > 3 }, 50*time.Millisecond, time.Millisecond)
			require.Equal(t, !cancel, registry.AllowReserve(mustPeer(t, assignment.TargetPeerID)))
			dms.mu.Lock()
			if cancel {
				require.Zero(t, dms.readyCalls)
			} else {
				require.Equal(t, 1, dms.readyCalls, "lost Ready must converge from Active, not mutate the stale lease again")
			}
			dms.mu.Unlock()
			select {
			case err := <-worker.Errors():
				t.Fatalf("transient recovery failure became fatal: %v", err)
			default:
			}
		})
	}
}

func TestDrainSuppressesPendingRecovery(t *testing.T) {
	metadata := testMetadata()
	session := testSession(time.Now().UTC(), metadata, 1)
	dms := &recoveryDMS{fakeDMS: &fakeDMS{metadata: metadata}}
	worker := testWorker(t, dms, testRegistry(t, 1), metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	startRecoveryTestLoop(t, worker, 20*time.Millisecond)
	worker.requestRecovery()
	require.NoError(t, worker.BeginDrainContext(context.Background()))
	require.Never(t, func() bool { return dms.activeCalls.Load() != 0 }, 60*time.Millisecond, time.Millisecond)
}
