package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

type fakeDMS struct {
	mu sync.Mutex

	active                 []dmsclient.Assignment
	claims                 []*dmsclient.ClaimResult
	claimErrors            []error
	readyCalls             int
	recoverCalls           int
	heartbeats             int
	failCalls              int
	relinquishes           int
	relinquishErrors       []error
	claimCalls             int
	metadata               dmsclient.Metadata
	claimStarted           chan struct{}
	claimRelease           chan struct{}
	heartbeatStarted       chan struct{}
	heartbeatRelease       chan struct{}
	readyStarted           chan struct{}
	readyRelease           chan struct{}
	commitRelinquishErrors bool
}

type blockingActiveDMS struct {
	*fakeDMS
	started chan struct{}
	release chan struct{}
}

func (d *blockingActiveDMS) Active(ctx context.Context, token string, sessionID uuid.UUID) ([]dmsclient.Assignment, error) {
	select {
	case d.started <- struct{}{}:
	default:
	}
	select {
	case <-d.release:
		return d.fakeDMS.Active(ctx, token, sessionID)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type blockingRelinquishDMS struct {
	*fakeDMS
	slowAssignment uuid.UUID
	started        chan struct{}
	release        chan struct{}
}

func (d *blockingRelinquishDMS) Relinquish(ctx context.Context, _ string, _ uuid.UUID, assignmentID uuid.UUID, _ uuid.UUID) (*dmsclient.Assignment, error) {
	if assignmentID == d.slowAssignment {
		select {
		case d.started <- struct{}{}:
		default:
		}
		select {
		case <-d.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	d.relinquishes++
	d.removeActiveLocked(assignmentID)
	d.mu.Unlock()
	return &dmsclient.Assignment{Revoke: true}, nil
}

func (f *fakeDMS) Active(context.Context, string, uuid.UUID) ([]dmsclient.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dmsclient.Assignment(nil), f.active...), nil
}

func (f *fakeDMS) Claim(context.Context, string, uuid.UUID) (*dmsclient.ClaimResult, error) {
	f.mu.Lock()
	f.claimCalls++
	started := f.claimStarted
	release := f.claimRelease
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	f.mu.Unlock()
	if release != nil {
		<-release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.claimErrors) + len(f.claims)
	_ = index
	if len(f.claimErrors) > 0 {
		err := f.claimErrors[0]
		f.claimErrors = f.claimErrors[1:]
		return nil, err
	}
	if len(f.claims) == 0 {
		return &dmsclient.ClaimResult{RetryAfter: time.Second}, nil
	}
	result := f.claims[0]
	f.claims = f.claims[1:]
	return result, nil
}

func (f *fakeDMS) Recover(_ context.Context, _ string, sessionID, _ uuid.UUID, epoch uuid.UUID) (*dmsclient.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recoverCalls++
	for _, assignment := range f.active {
		if assignment.ReservationEpoch != nil && *assignment.ReservationEpoch == epoch {
			result := assignment
			result.SlotState = dmsclient.SlotStateStarting
			result.ProviderSessionID = uuidPointer(sessionID)
			lease := time.Now().UTC().Add(time.Minute)
			result.ProviderLeaseExpiresAt = &lease
			result.RecoveryExpiresAt = nil
			return &result, nil
		}
	}
	return nil, errors.New("unknown recovery")
}

func (f *fakeDMS) Heartbeat(_ context.Context, _ string, _ uuid.UUID, _ uuid.UUID, epoch uuid.UUID) (*dmsclient.Assignment, error) {
	f.mu.Lock()
	f.heartbeats++
	for _, assignment := range f.active {
		if assignment.ReservationEpoch != nil && *assignment.ReservationEpoch == epoch {
			result := assignment
			result.SlotState = dmsclient.SlotStateReady
			result.Metadata = metadataPointer(f.metadata)
			lease := time.Now().UTC().Add(time.Minute)
			result.ProviderLeaseExpiresAt = &lease
			started := f.heartbeatStarted
			release := f.heartbeatRelease
			f.mu.Unlock()
			if started != nil {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			if release != nil {
				<-release
			}
			return &result, nil
		}
	}
	f.mu.Unlock()
	return nil, errors.New("unknown heartbeat")
}

func (f *fakeDMS) Ready(_ context.Context, _ string, _ uuid.UUID, _ uuid.UUID, epoch uuid.UUID, metadata dmsclient.Metadata) (*dmsclient.Assignment, error) {
	f.mu.Lock()
	f.readyCalls++
	for index, assignment := range f.active {
		if assignment.ReservationEpoch != nil && *assignment.ReservationEpoch == epoch {
			result := assignment
			result.SlotState = dmsclient.SlotStateReady
			result.Metadata = metadataPointer(metadata)
			if result.ProviderLeaseExpiresAt == nil {
				lease := time.Now().UTC().Add(time.Minute)
				result.ProviderLeaseExpiresAt = &lease
				result.RecoveryExpiresAt = nil
			}
			f.active[index] = result
			started := f.readyStarted
			release := f.readyRelease
			f.mu.Unlock()
			if started != nil {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			if release != nil {
				<-release
			}
			return &result, nil
		}
	}
	// Claimed assignments are not necessarily in the scripted active list.
	for _, result := range f.claims {
		if result.Assignment != nil && result.Assignment.ReservationEpoch != nil && *result.Assignment.ReservationEpoch == epoch {
			assignment := *result.Assignment
			assignment.SlotState = dmsclient.SlotStateReady
			assignment.Metadata = metadataPointer(metadata)
			f.mu.Unlock()
			return &assignment, nil
		}
	}
	f.mu.Unlock()
	return nil, errors.New("unknown ready assignment")
}

func (f *fakeDMS) Fail(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCalls++
	return &dmsclient.Assignment{Revoke: true}, nil
}

func (f *fakeDMS) Relinquish(_ context.Context, _ string, _ uuid.UUID, assignmentID uuid.UUID, _ uuid.UUID) (*dmsclient.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relinquishes++
	if len(f.relinquishErrors) > 0 {
		err := f.relinquishErrors[0]
		f.relinquishErrors = f.relinquishErrors[1:]
		if f.commitRelinquishErrors {
			f.removeActiveLocked(assignmentID)
		}
		return nil, err
	}
	f.removeActiveLocked(assignmentID)
	return &dmsclient.Assignment{Revoke: true}, nil
}

func (f *fakeDMS) removeActiveLocked(assignmentID uuid.UUID) {
	for index := range f.active {
		if f.active[index].AssignmentID != nil && *f.active[index].AssignmentID == assignmentID {
			f.active = append(f.active[:index], f.active[index+1:]...)
			return
		}
	}
}

func TestReconcileStartingAndRecoveringBeforeReservationAuthority(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 2)
	starting := testAssignment(t, now, session, 0, dmsclient.SlotStateStarting, metadata)
	recovering := testAssignment(t, now, session, 1, dmsclient.SlotStateRecovering, metadata)
	recovering.ProviderLeaseExpiresAt = nil
	recovery := now.Add(time.Minute)
	recovering.RecoveryExpiresAt = &recovery
	dms := &fakeDMS{active: []dmsclient.Assignment{starting, recovering}, metadata: metadata}
	registry := testRegistry(t, 2)
	closed := make(chan peer.ID, 4)
	worker := testWorker(t, dms, registry, metadata, func(target peer.ID) error { closed <- target; return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))

	require.True(t, registry.AllowReserve(mustPeer(t, starting.TargetPeerID)))
	require.True(t, registry.AllowReserve(mustPeer(t, recovering.TargetPeerID)))
	dms.mu.Lock()
	require.Equal(t, 1, dms.recoverCalls)
	require.Equal(t, 2, dms.readyCalls)
	dms.mu.Unlock()
	select {
	case target := <-closed:
		t.Fatalf("reconciliation unexpectedly revoked %s", target)
	default:
	}
}

func TestReconcileRejectsStartingAndRecoveringMetadataMismatch(t *testing.T) {
	for _, state := range []dmsclient.SlotState{dmsclient.SlotStateStarting, dmsclient.SlotStateRecovering} {
		t.Run(string(state), func(t *testing.T) {
			now := time.Now().UTC()
			metadata := testMetadata()
			session := testSession(now, metadata, 1)
			assignment := testAssignment(t, now, session, 0, state, metadata)
			mismatched := metadata
			mismatched.ConfigFingerprint += "-changed"
			assignment.Metadata = metadataPointer(mismatched)
			if state == dmsclient.SlotStateRecovering {
				assignment.ProviderLeaseExpiresAt = nil
				recovery := now.Add(time.Minute)
				assignment.RecoveryExpiresAt = &recovery
			}
			dms := &fakeDMS{active: []dmsclient.Assignment{assignment}, metadata: metadata}
			registry := testRegistry(t, 1)
			worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
			require.NoError(t, worker.SetControl("node-token", session))
			require.NoError(t, worker.Reconcile(context.Background()))
			bookings, _ := registry.Counts()
			require.Zero(t, bookings)
			dms.mu.Lock()
			require.Equal(t, 1, dms.failCalls)
			dms.mu.Unlock()
		})
	}
}

func TestHeartbeatMetadataMismatchRevokesAndFailsOnlyThatChild(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	assignment := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
	lease := now.Add(100 * time.Millisecond)
	assignment.ProviderLeaseExpiresAt = &lease
	dms := &fakeDMS{active: []dmsclient.Assignment{assignment}, metadata: metadata}
	registry := testRegistry(t, 1)
	closed := make(chan peer.ID, 1)
	worker := testWorker(t, dms, registry, metadata, func(target peer.ID) error { closed <- target; return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))
	dms.mu.Lock()
	dms.metadata.ConfigFingerprint += "-changed"
	dms.mu.Unlock()
	require.NoError(t, worker.Start(context.Background()))
	select {
	case target := <-closed:
		require.Equal(t, assignment.TargetPeerID, target.String())
	case <-time.After(time.Second):
		t.Fatal("metadata mismatch did not revoke the child")
	}
	dms.mu.Lock()
	require.Equal(t, 1, dms.failCalls)
	dms.mu.Unlock()
	bookings, _ := registry.Counts()
	require.Zero(t, bookings)
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestClaimLoopFillsOnlyEffectiveCapacity(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 2)
	first := testAssignment(t, now, session, 0, dmsclient.SlotStateStarting, metadata)
	first.Metadata = nil
	second := testAssignment(t, now, session, 1, dmsclient.SlotStateStarting, metadata)
	second.Metadata = nil
	dms := &fakeDMS{metadata: metadata}
	// Ready resolves from active; mirror committed claims there as a DMS would.
	dms.active = []dmsclient.Assignment{first, second}
	dms.claims = []*dmsclient.ClaimResult{{Assignment: &first}, {Assignment: &second}}
	registry := testRegistry(t, 2)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	// The startup active view is empty in this scenario; claims arrive later.
	dms.mu.Lock()
	dms.active = nil
	dms.mu.Unlock()
	require.NoError(t, worker.Reconcile(context.Background()))
	dms.mu.Lock()
	dms.active = []dmsclient.Assignment{first, second}
	dms.mu.Unlock()
	require.NoError(t, worker.Start(context.Background()))
	worker.SetAccepting(true)
	require.Eventually(t, func() bool {
		bookings, _ := registry.Counts()
		return bookings == 2
	}, time.Second, time.Millisecond)
	require.True(t, registry.AllowReserve(mustPeer(t, first.TargetPeerID)))
	require.True(t, registry.AllowReserve(mustPeer(t, second.TargetPeerID)))
	require.NoError(t, worker.Shutdown(context.Background()))
	dms.mu.Lock()
	require.Equal(t, 2, dms.relinquishes, "shutdown must relinquish every tracked child")
	dms.mu.Unlock()
}

func TestAmbiguousClaimReconcilesBeforeAnotherClaim(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	committed := testAssignment(t, now, session, 0, dmsclient.SlotStateStarting, metadata)
	dms := &fakeDMS{metadata: metadata, active: []dmsclient.Assignment{committed}, claimErrors: []error{errors.New("response lost after commit")}}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	// Seed no active work for startup, then expose the committed claim when the
	// worker reconciles the ambiguous POST.
	dms.mu.Lock()
	dms.active = nil
	dms.mu.Unlock()
	require.NoError(t, worker.Reconcile(context.Background()))
	dms.mu.Lock()
	dms.active = []dmsclient.Assignment{committed}
	dms.mu.Unlock()
	require.NoError(t, worker.Start(context.Background()))
	worker.SetAccepting(true)
	require.Eventually(t, func() bool {
		bookings, _ := registry.Counts()
		return bookings == 1
	}, time.Second, time.Millisecond)
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestReconcileRevokesOldEpochBeforeInstallingSameTargetReplacement(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	old := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
	dms := &fakeDMS{metadata: metadata, active: []dmsclient.Assignment{old}}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))

	replacement := old
	replacementID := uuid.New()
	replacementEpoch := uuid.New()
	replacement.AssignmentID = &replacementID
	replacement.ReservationEpoch = &replacementEpoch
	replacement.SlotState = dmsclient.SlotStateStarting
	dms.mu.Lock()
	dms.active = []dmsclient.Assignment{replacement}
	dms.mu.Unlock()

	require.NoError(t, worker.Reconcile(context.Background()))
	snapshot, ok := registry.SnapshotForAdmission(mustPeer(t, replacement.TargetPeerID), replacement.DomainID)
	require.True(t, ok)
	require.Equal(t, replacementID, snapshot.Authority.Fence.AssignmentID)
	require.Equal(t, replacementEpoch, snapshot.Authority.Fence.ReservationEpoch)
}

func TestIdleExpiryTicksDoNotShortenEmptyClaimRetryAfter(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	dms := &fakeDMS{metadata: metadata}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Start(context.Background()))
	worker.SetAccepting(true)
	require.Eventually(t, func() bool {
		dms.mu.Lock()
		defer dms.mu.Unlock()
		return dms.claimCalls == 1
	}, time.Second, time.Millisecond)
	renewed := session
	renewed.SessionExpiresAt = now.Add(3 * time.Minute)
	renewed.ProviderNodeJWTExpiresAt = now.Add(6 * time.Minute)
	require.NoError(t, worker.SetControl("renewed-node-token", renewed))
	worker.SetAccepting(true)
	time.Sleep(75 * time.Millisecond)
	dms.mu.Lock()
	require.Equal(t, 1, dms.claimCalls, "idle expiry, status refresh, and an unchanged gate must not interrupt Retry-After")
	dms.mu.Unlock()
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestGateCloseRelinquishesClaimCommittedAcrossInFlightRequest(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	claimed := testAssignment(t, now, session, 0, dmsclient.SlotStateStarting, metadata)
	claimed.Metadata = nil
	dms := &fakeDMS{
		metadata: metadata, claims: []*dmsclient.ClaimResult{{Assignment: &claimed}},
		claimStarted: make(chan struct{}, 1), claimRelease: make(chan struct{}),
	}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Start(context.Background()))
	worker.SetAccepting(true)
	<-dms.claimStarted
	worker.SetAccepting(false)
	close(dms.claimRelease)
	require.Eventually(t, func() bool {
		dms.mu.Lock()
		defer dms.mu.Unlock()
		return dms.relinquishes == 1
	}, time.Second, time.Millisecond)
	bookings, _ := registry.Counts()
	require.Zero(t, bookings)
	dms.mu.Lock()
	require.Zero(t, dms.readyCalls)
	dms.mu.Unlock()
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestAmbiguousGateCloseRelinquishReconcilesBeforeAnotherClaim(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	claimed := testAssignment(t, now, session, 0, dmsclient.SlotStateStarting, metadata)
	claimed.Metadata = nil
	dms := &fakeDMS{
		metadata: metadata, claims: []*dmsclient.ClaimResult{{Assignment: &claimed}},
		claimStarted: make(chan struct{}, 1), claimRelease: make(chan struct{}),
		relinquishErrors: []error{errors.New("response lost after relinquish")},
	}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Start(context.Background()))
	worker.SetAccepting(true)
	<-dms.claimStarted

	worker.SetAccepting(false)
	active := claimed
	active.Metadata = metadataPointer(metadata)
	dms.mu.Lock()
	dms.active = []dmsclient.Assignment{active}
	dms.mu.Unlock()
	close(dms.claimRelease)
	require.Eventually(t, func() bool {
		dms.mu.Lock()
		defer dms.mu.Unlock()
		return dms.relinquishes == 1
	}, time.Second, time.Millisecond)

	worker.SetAccepting(true)
	require.Eventually(t, func() bool {
		bookings, _ := registry.Counts()
		return bookings == 1
	}, time.Second, time.Millisecond)
	dms.mu.Lock()
	require.Equal(t, 1, dms.claimCalls, "an ambiguous relinquish must reconcile before another claim")
	dms.mu.Unlock()
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestDelayedOldHeartbeatCannotReinstallRevokedGeneration(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	old := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
	lease := now.Add(120 * time.Millisecond)
	old.ProviderLeaseExpiresAt = &lease
	dms := &fakeDMS{
		metadata: metadata, active: []dmsclient.Assignment{old},
		heartbeatStarted: make(chan struct{}, 1), heartbeatRelease: make(chan struct{}),
	}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))
	require.NoError(t, worker.Start(context.Background()))
	<-dms.heartbeatStarted

	require.NoError(t, worker.revoke(old))
	replacement := old
	replacementID := uuid.New()
	replacementEpoch := uuid.New()
	replacement.AssignmentID = &replacementID
	replacement.ReservationEpoch = &replacementEpoch
	replacement.SlotState = dmsclient.SlotStateStarting
	replacementLease := time.Now().UTC().Add(time.Minute)
	replacement.ProviderLeaseExpiresAt = &replacementLease
	dms.mu.Lock()
	dms.active = []dmsclient.Assignment{replacement}
	dms.mu.Unlock()
	require.NoError(t, worker.Reconcile(context.Background()))
	close(dms.heartbeatRelease)
	time.Sleep(20 * time.Millisecond)

	snapshot, found := registry.SnapshotForAdmission(mustPeer(t, replacement.TargetPeerID), replacement.DomainID)
	require.True(t, found)
	require.Equal(t, replacementID, snapshot.Authority.Fence.AssignmentID)
	require.Equal(t, replacementEpoch, snapshot.Authority.Fence.ReservationEpoch)
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestKnownAuthorityDeadlineClosesTargetWithoutWaitingForSweepInterval(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	assignment := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
	lease := now.Add(150 * time.Millisecond)
	assignment.ProviderLeaseExpiresAt = &lease
	dms := &fakeDMS{
		metadata: metadata, active: []dmsclient.Assignment{assignment},
		heartbeatStarted: make(chan struct{}, 1), heartbeatRelease: make(chan struct{}),
	}
	registry := testRegistry(t, 1)
	closed := make(chan peer.ID, 1)
	worker, err := New(Options{
		DMS: dms, Registry: registry, Metadata: metadata,
		ClosePeer: func(target peer.ID) error { closed <- target; return nil },
		Config: Config{
			InitialBackoff: time.Millisecond, MaximumBackoff: 10 * time.Millisecond,
			EmptyClaimMaxJitterFraction: 0.20,
			HeartbeatMinimumFraction:    0.25, HeartbeatMaximumFraction: 0.35,
			ExpirySweepInterval: time.Second,
			Random:              func() float64 { return 0 }, Now: time.Now,
		},
	})
	require.NoError(t, err)
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))
	require.NoError(t, worker.Start(context.Background()))
	<-dms.heartbeatStarted

	select {
	case target := <-closed:
		require.Equal(t, assignment.TargetPeerID, target.String())
		require.Less(t, time.Since(now), time.Second, "known authority must not wait for the fallback sweep")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("target connection was not closed at its known authority deadline")
	}
	bookings, _ := registry.Counts()
	require.Zero(t, bookings)
	close(dms.heartbeatRelease)
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestExpiredGenerationClosesBeforeSameTargetReplacementInstalls(t *testing.T) {
	fixedNow := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return fixedNow
	}
	setClock := func(value time.Time) {
		clockMu.Lock()
		fixedNow = value
		clockMu.Unlock()
	}
	metadata := testMetadata()
	session := testSession(fixedNow, metadata, 1)
	old := testAssignment(t, fixedNow, session, 0, dmsclient.SlotStateReady, metadata)
	oldLease := fixedNow.Add(time.Second)
	old.ProviderLeaseExpiresAt = &oldLease
	dms := &fakeDMS{metadata: metadata, active: []dmsclient.Assignment{old}}
	registry, err := booking.New(booking.Config{
		MaximumBookings: 1, MaximumAdmissions: 16, AdmissionTTL: 30 * time.Second, Now: clock,
	})
	require.NoError(t, err)
	closeStarted := make(chan struct{}, 1)
	closeRelease := make(chan struct{})
	worker, err := New(Options{
		DMS: dms, Registry: registry, Metadata: metadata,
		ClosePeer: func(peer.ID) error {
			closeStarted <- struct{}{}
			<-closeRelease
			return nil
		},
		Config: Config{
			InitialBackoff: time.Millisecond, MaximumBackoff: 10 * time.Millisecond,
			EmptyClaimMaxJitterFraction: 0.20,
			HeartbeatMinimumFraction:    0.25, HeartbeatMaximumFraction: 0.35,
			ExpirySweepInterval: time.Second, Random: func() float64 { return 0 }, Now: clock,
		},
	})
	require.NoError(t, err)
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))

	setClock(oldLease)
	expireDone := make(chan struct{})
	go func() {
		worker.expireDue()
		close(expireDone)
	}()
	<-closeStarted

	replacement := old
	replacementID := uuid.New()
	replacementEpoch := uuid.New()
	replacement.AssignmentID = &replacementID
	replacement.ReservationEpoch = &replacementEpoch
	replacementLease := oldLease.Add(time.Minute)
	replacement.ProviderLeaseExpiresAt = &replacementLease
	dms.mu.Lock()
	dms.active = []dmsclient.Assignment{replacement}
	dms.mu.Unlock()
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- worker.Reconcile(context.Background()) }()
	select {
	case err := <-reconcileDone:
		t.Fatalf("replacement installed before old target close completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(closeRelease)
	<-expireDone
	require.NoError(t, <-reconcileDone)
	snapshot, found := registry.SnapshotForAdmission(mustPeer(t, replacement.TargetPeerID), replacement.DomainID)
	require.True(t, found)
	require.Equal(t, replacementID, snapshot.Authority.Fence.AssignmentID)
}

func TestRelinquishAllUsesAuthoritativeActiveForUntrackedLease(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	assignment := testAssignment(t, now, session, 0, dmsclient.SlotStateStarting, metadata)
	dms := &fakeDMS{metadata: metadata, active: []dmsclient.Assignment{assignment}}
	worker := testWorker(t, dms, testRegistry(t, 1), metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	worker.BeginDrain()

	remaining, err := worker.RelinquishAll(context.Background())
	require.NoError(t, err)
	require.Zero(t, remaining)
	dms.mu.Lock()
	require.Equal(t, 1, dms.relinquishes)
	require.Empty(t, dms.active)
	dms.mu.Unlock()
}

func TestRelinquishAllResolvesLostCommittedResponseWithActive(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	assignment := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
	dms := &fakeDMS{
		metadata: metadata, active: []dmsclient.Assignment{assignment},
		relinquishErrors: []error{errors.New("response lost after commit")}, commitRelinquishErrors: true,
	}
	registry := testRegistry(t, 1)
	closed := make(chan peer.ID, 1)
	worker := testWorker(t, dms, registry, metadata, func(target peer.ID) error { closed <- target; return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))
	worker.BeginDrain()

	remaining, err := worker.RelinquishAll(context.Background())
	require.NoError(t, err, "the final Active snapshot proves the ambiguous mutation committed")
	require.Zero(t, remaining)
	select {
	case target := <-closed:
		require.Equal(t, assignment.TargetPeerID, target.String())
	case <-time.After(time.Second):
		t.Fatal("resolved relinquish did not close the released target")
	}
	require.Zero(t, worker.ActiveAssignments())
}

func TestStartingCapacitySignalsBeforeReadyResponseCompletes(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	assignment := testAssignment(t, now, session, 0, dmsclient.SlotStateStarting, metadata)
	assignment.Metadata = nil
	dms := &fakeDMS{
		metadata: metadata, active: []dmsclient.Assignment{assignment},
		claims:       []*dmsclient.ClaimResult{{Assignment: &assignment}},
		readyStarted: make(chan struct{}, 1), readyRelease: make(chan struct{}),
	}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	select {
	case <-worker.AvailabilityChanged():
	default:
	}
	require.NoError(t, worker.Start(context.Background()))
	worker.SetAccepting(true)
	<-dms.readyStarted
	select {
	case <-worker.AvailabilityChanged():
	case <-time.After(time.Second):
		t.Fatal("starting occupancy did not signal saturation before Ready completed")
	}
	require.False(t, worker.CapacityAvailable())
	close(dms.readyRelease)
	require.Eventually(t, func() bool { return worker.ActiveAssignments() == 1 }, time.Second, time.Millisecond)
	require.NoError(t, worker.Shutdown(context.Background()))
}

func TestBeginDrainKeepsHeartbeatAliveUntilReleaseBoundary(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	assignment := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
	lease := now.Add(120 * time.Millisecond)
	assignment.ProviderLeaseExpiresAt = &lease
	dms := &fakeDMS{metadata: metadata, active: []dmsclient.Assignment{assignment}}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))
	require.NoError(t, worker.Start(context.Background()))
	worker.BeginDrain()
	require.Eventually(t, func() bool {
		dms.mu.Lock()
		defer dms.mu.Unlock()
		return dms.heartbeats > 0
	}, time.Second, time.Millisecond)
	require.NoError(t, worker.ShutdownPreserving(context.Background()))
	dms.mu.Lock()
	require.Zero(t, dms.relinquishes, "preserving shutdown must not duplicate remote lease release")
	dms.mu.Unlock()
}

func TestBeginDrainIsStickyAgainstStaleAcceptingUpdate(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	dms := &fakeDMS{metadata: metadata}
	worker := testWorker(t, dms, testRegistry(t, 1), metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	worker.BeginDrain()
	worker.SetAccepting(true)
	require.False(t, worker.Accepting())
	require.ErrorContains(t, worker.Start(context.Background()), "draining")
	worker.SetAccepting(true)
	time.Sleep(20 * time.Millisecond)
	dms.mu.Lock()
	require.Zero(t, dms.claimCalls)
	dms.mu.Unlock()
	require.NoError(t, worker.ShutdownPreserving(context.Background()))
}

func TestBeginDrainContextBoundsBlockedClaimBarrier(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	dms := &fakeDMS{
		metadata: metadata, claimStarted: make(chan struct{}, 1), claimRelease: make(chan struct{}),
	}
	registry := testRegistry(t, 1)
	worker := testWorker(t, dms, registry, metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Start(context.Background()))
	worker.SetAccepting(true)
	<-dms.claimStarted

	drainContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := worker.BeginDrainContext(drainContext)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 250*time.Millisecond)
	require.False(t, worker.Accepting())
	require.True(t, registry.Draining())

	close(dms.claimRelease)
	require.NoError(t, worker.ShutdownPreserving(context.Background()))
}

func TestBeginDrainJoinsInFlightReconcile(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 1)
	dms := &blockingActiveDMS{
		fakeDMS: &fakeDMS{metadata: metadata},
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	worker := testWorker(t, dms, testRegistry(t, 1), metadata, func(peer.ID) error { return nil })
	require.NoError(t, worker.SetControl("node-token", session))
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- worker.Reconcile(context.Background()) }()
	<-dms.started
	drainDone := make(chan struct{})
	go func() {
		worker.BeginDrain()
		close(drainDone)
	}()
	select {
	case <-drainDone:
		t.Fatal("drain returned before the in-flight reconciliation joined")
	case <-time.After(20 * time.Millisecond):
	}
	close(dms.release)
	require.NoError(t, <-reconcileDone)
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after reconciliation completed")
	}
	require.ErrorContains(t, worker.Reconcile(context.Background()), "draining")
}

func TestRelinquishAllClosesEachReleasedTargetWithoutWaitingForSlowSibling(t *testing.T) {
	now := time.Now().UTC()
	metadata := testMetadata()
	session := testSession(now, metadata, 2)
	fast := testAssignment(t, now, session, 0, dmsclient.SlotStateReady, metadata)
	slow := testAssignment(t, now, session, 1, dmsclient.SlotStateReady, metadata)
	dms := &blockingRelinquishDMS{
		fakeDMS:        &fakeDMS{metadata: metadata, active: []dmsclient.Assignment{fast, slow}},
		slowAssignment: *slow.AssignmentID,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
	}
	closed := make(chan peer.ID, 2)
	worker := testWorker(t, dms, testRegistry(t, 2), metadata, func(target peer.ID) error {
		closed <- target
		return nil
	})
	require.NoError(t, worker.SetControl("node-token", session))
	require.NoError(t, worker.Reconcile(context.Background()))
	worker.BeginDrain()
	done := make(chan error, 1)
	go func() {
		_, err := worker.RelinquishAll(context.Background())
		done <- err
	}()
	<-dms.started
	select {
	case target := <-closed:
		require.Equal(t, fast.TargetPeerID, target.String())
	case <-time.After(time.Second):
		t.Fatal("fast released target remained open behind a slow sibling")
	}
	close(dms.release)
	require.NoError(t, <-done)
	select {
	case target := <-closed:
		require.Equal(t, slow.TargetPeerID, target.String())
	case <-time.After(time.Second):
		t.Fatal("slow released target was not closed")
	}
}

func testWorker(t *testing.T, dms DMS, registry *booking.Registry, metadata dmsclient.Metadata, closer func(peer.ID) error) *Worker {
	t.Helper()
	worker, err := New(Options{
		DMS: dms, Registry: registry, Metadata: metadata, ClosePeer: closer,
		Config: Config{
			InitialBackoff: time.Millisecond, MaximumBackoff: 10 * time.Millisecond,
			EmptyClaimMaxJitterFraction: 0.20,
			HeartbeatMinimumFraction:    0.25, HeartbeatMaximumFraction: 0.35,
			ExpirySweepInterval: 10 * time.Millisecond,
			Random:              func() float64 { return 0 }, Now: time.Now,
		},
	})
	require.NoError(t, err)
	return worker
}

func testRegistry(t *testing.T, capacity int) *booking.Registry {
	t.Helper()
	registry, err := booking.New(booking.Config{MaximumBookings: capacity, MaximumAdmissions: 16, AdmissionTTL: 30 * time.Second})
	require.NoError(t, err)
	return registry
}

func testMetadata() dmsclient.Metadata {
	return dmsclient.Metadata{
		BaseAddresses:     []string{"/dns4/relay-a.dev.aukiverse.com/tcp/443/p2p/12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP"},
		EndpointKeys:      []string{"/dns4/relay-a.dev.aukiverse.com/tcp/443"},
		Limits:            dmsclient.Limits{DurationSeconds: 900, DataBytesPerDirection: 1 << 20},
		ConfigFingerprint: "relay-config-v1|test",
	}
}

func testSession(now time.Time, metadata dmsclient.Metadata, capacity uint32) dmsclient.Session {
	return dmsclient.Session{
		ProviderSessionID: uuid.New(), EffectiveCapacity: capacity, SchedulingRevision: 7,
		SessionExpiresAt: now.Add(2 * time.Minute), ProviderNodeJWTExpiresAt: now.Add(5 * time.Minute), Metadata: metadata,
	}
}

func testAssignment(t *testing.T, now time.Time, session dmsclient.Session, slot uint8, state dmsclient.SlotState, metadata dmsclient.Metadata) dmsclient.Assignment {
	t.Helper()
	key, _, err := libp2pcrypto.GenerateEd25519Key(nil)
	require.NoError(t, err)
	target, err := peer.IDFromPrivateKey(key)
	require.NoError(t, err)
	assignmentID := uuid.New()
	epoch := uuid.New()
	lease := now.Add(time.Minute)
	metadataCopy := metadata
	return dmsclient.Assignment{
		BookingID: uuid.New(), SlotID: uuid.New(), SlotIndex: slot, RelayCount: 2,
		DomainID: uuid.New(), TargetPeerID: target.String(), BookingState: dmsclient.BookingStateActive, SlotState: state,
		ProviderSessionID: uuidPointer(session.ProviderSessionID), AssignmentID: &assignmentID, ReservationEpoch: &epoch,
		RequestedUntil: now.Add(time.Hour), AuthorityExpiresAt: now.Add(2 * time.Minute), ProviderLeaseExpiresAt: &lease,
		EffectiveCapacity: session.EffectiveCapacity, SchedulingRevision: session.SchedulingRevision, Metadata: &metadataCopy,
	}
}

func uuidPointer(value uuid.UUID) *uuid.UUID { return &value }

func metadataPointer(value dmsclient.Metadata) *dmsclient.Metadata {
	copyValue := value
	return &copyValue
}

func mustPeer(t *testing.T, value string) peer.ID {
	t.Helper()
	result, err := peer.Decode(value)
	require.NoError(t, err)
	return result
}
