package booking

import (
	"crypto/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

type atomicClock struct {
	nanos atomic.Int64
}

func newAtomicClock(now time.Time) *atomicClock {
	clock := &atomicClock{}
	clock.Set(now)
	return clock
}

func (c *atomicClock) Now() time.Time {
	return time.Unix(0, c.nanos.Load()).UTC()
}

func (c *atomicClock) Set(now time.Time) {
	c.nanos.Store(now.UTC().UnixNano())
}

func bookingPeer(t *testing.T) peer.ID {
	t.Helper()
	_, publicKey, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPublicKey(publicKey)
	require.NoError(t, err)
	return peerID
}

func testRegistry(t *testing.T, clock *atomicClock, bookings, admissions int, ttl time.Duration) *Registry {
	t.Helper()
	registry, err := New(Config{
		MaximumBookings:   bookings,
		MaximumAdmissions: admissions,
		AdmissionTTL:      ttl,
		Now:               clock.Now,
	})
	require.NoError(t, err)
	return registry
}

func testSession(now time.Time, capacity int) Session {
	return Session{
		ProviderSessionID: uuid.New(),
		EffectiveCapacity: capacity,
		SessionExpiresAt:  now.Add(2 * time.Minute),
		NodeJWTExpiresAt:  now.Add(3 * time.Minute),
	}
}

func testAuthority(target peer.ID, sessionID uuid.UUID, domain uuid.UUID, now time.Time) Authority {
	return Authority{
		BookingID:              uuid.New(),
		SlotID:                 uuid.New(),
		DomainID:               domain,
		TargetPeerID:           target,
		Fence:                  Fence{ProviderSessionID: sessionID, AssignmentID: uuid.New(), ReservationEpoch: uuid.New()},
		RequestedUntil:         now.Add(4 * time.Minute),
		AuthorityExpiresAt:     now.Add(90 * time.Second),
		ProviderLeaseExpiresAt: now.Add(time.Minute),
	}
}

func TestRegistryStartingReadyAdmissionAndLiteralDeadline(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := newAtomicClock(now)
	registry := testRegistry(t, clock, 2, 4, 30*time.Second)
	session := testSession(now, 2)
	require.NoError(t, registry.SetSession(session))
	target := bookingPeer(t)
	source := bookingPeer(t)
	domain := uuid.New()
	authority := testAuthority(target, session.ProviderSessionID, domain, now)
	authority.ProviderLeaseExpiresAt = now.Add(20 * time.Second)
	require.NoError(t, registry.InstallStarting(authority))

	acl := NewACL(registry)
	require.True(t, acl.AllowReserve(target, nil))
	require.False(t, acl.AllowReserve(bookingPeer(t), nil))
	require.False(t, acl.AllowConnect(source, nil, target))
	_, found := registry.SnapshotForAdmission(target, domain)
	require.False(t, found, "starting authority must not admit CONNECT sources")

	require.NoError(t, registry.ActivateReady(target, authority.Fence))
	snapshot, found := registry.SnapshotForAdmission(target, domain)
	require.True(t, found)
	require.Equal(t, authority.Fence, snapshot.Authority.Fence)
	require.Equal(t, authority.ProviderLeaseExpiresAt, snapshot.EffectiveUntil)

	acceptedUntil, err := registry.Admit(AdmissionRequest{
		SourcePeerID:        source,
		DomainID:            domain,
		TargetPeerID:        target,
		ExpectedFence:       snapshot.Authority.Fence,
		LiteralJWTExpiresAt: now.Add(25 * time.Second),
	})
	require.NoError(t, err)
	require.Equal(t, authority.ProviderLeaseExpiresAt, acceptedUntil)
	require.True(t, acl.AllowConnect(source, nil, target))

	clock.Set(acceptedUntil)
	require.False(t, acl.AllowReserve(target, nil), "authority is invalid at its literal deadline")
	require.False(t, acl.AllowConnect(source, nil, target))
	require.Equal(t, []ExpiredAuthority{{TargetPeerID: target, Fence: authority.Fence}}, registry.Expire())
	bookings, admissions := registry.Counts()
	require.Zero(t, bookings)
	require.Zero(t, admissions)
}

func TestRegistryAdmissionIsBoundToExactGenerationAcrossConcurrentRevoke(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := newAtomicClock(now)
	registry := testRegistry(t, clock, 1, 8, 30*time.Second)
	session := testSession(now, 1)
	require.NoError(t, registry.SetSession(session))
	target := bookingPeer(t)
	source := bookingPeer(t)
	domain := uuid.New()
	old := testAuthority(target, session.ProviderSessionID, domain, now)
	require.NoError(t, registry.InstallStarting(old))
	require.NoError(t, registry.ActivateReady(target, old.Fence))
	snapshot, found := registry.SnapshotForAdmission(target, domain)
	require.True(t, found)

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, _ = registry.Admit(AdmissionRequest{
			SourcePeerID:        source,
			DomainID:            domain,
			TargetPeerID:        target,
			ExpectedFence:       snapshot.Authority.Fence,
			LiteralJWTExpiresAt: now.Add(time.Minute),
		})
	}()
	go func() {
		defer wait.Done()
		<-start
		registry.RemoveExact(target, old.Fence)
	}()
	close(start)
	wait.Wait()
	require.False(t, registry.AllowReserve(target))
	require.False(t, registry.AllowConnect(source, target), "no admission may survive completed revoke")

	replacement := testAuthority(target, session.ProviderSessionID, domain, now)
	require.NoError(t, registry.InstallStarting(replacement))
	require.NoError(t, registry.ActivateReady(target, replacement.Fence))
	_, err := registry.Admit(AdmissionRequest{
		SourcePeerID:        source,
		DomainID:            domain,
		TargetPeerID:        target,
		ExpectedFence:       old.Fence,
		LiteralJWTExpiresAt: now.Add(time.Minute),
	})
	require.ErrorIs(t, err, ErrStaleFence)
	require.False(t, registry.AllowConnect(source, target), "replacement must not inherit old cached admission")
}

func TestRegistryBoundsBookingsAdmissionsAndReusesOnlyExpiredCacheSpace(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := newAtomicClock(now)
	registry := testRegistry(t, clock, 2, 1, 10*time.Second)
	session := testSession(now, 1)
	require.NoError(t, registry.SetSession(session))
	target := bookingPeer(t)
	domain := uuid.New()
	authority := testAuthority(target, session.ProviderSessionID, domain, now)
	require.NoError(t, registry.InstallStarting(authority))
	require.NoError(t, registry.ActivateReady(target, authority.Fence))
	require.ErrorIs(t, registry.InstallStarting(testAuthority(bookingPeer(t), session.ProviderSessionID, uuid.New(), now)), ErrBookingCapacity)

	firstSource := bookingPeer(t)
	secondSource := bookingPeer(t)
	request := AdmissionRequest{
		SourcePeerID:        firstSource,
		DomainID:            domain,
		TargetPeerID:        target,
		ExpectedFence:       authority.Fence,
		LiteralJWTExpiresAt: now.Add(time.Minute),
	}
	acceptedUntil, err := registry.Admit(request)
	require.NoError(t, err)
	request.SourcePeerID = secondSource
	_, err = registry.Admit(request)
	require.ErrorIs(t, err, ErrAdmissionCapacity)
	_, admissions := registry.Counts()
	require.Equal(t, 1, admissions)

	clock.Set(acceptedUntil)
	request.LiteralJWTExpiresAt = acceptedUntil.Add(time.Minute)
	_, err = registry.Admit(request)
	require.NoError(t, err, "an expired admission may be pruned and its slot reused")
	require.False(t, registry.AllowConnect(firstSource, target))
	require.True(t, registry.AllowConnect(secondSource, target))
}

func TestRegistryRejectsSessionInheritanceAndStaleMutations(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := newAtomicClock(now)
	registry := testRegistry(t, clock, 2, 2, 30*time.Second)
	session := testSession(now, 2)
	require.NoError(t, registry.SetSession(session))
	target := bookingPeer(t)
	authority := testAuthority(target, session.ProviderSessionID, uuid.New(), now)
	require.NoError(t, registry.InstallStarting(authority))

	replacementSession := testSession(now, 2)
	require.ErrorIs(t, registry.SetSession(replacementSession), ErrSessionConflict)
	require.ErrorIs(t, registry.ActivateReady(target, Fence{
		ProviderSessionID: authority.Fence.ProviderSessionID,
		AssignmentID:      authority.Fence.AssignmentID,
		ReservationEpoch:  uuid.New(),
	}), ErrStaleFence)
	require.ErrorIs(t, registry.UpdateLease(target, Fence{
		ProviderSessionID: authority.Fence.ProviderSessionID,
		AssignmentID:      authority.Fence.AssignmentID,
		ReservationEpoch:  uuid.New(),
	}, now.Add(time.Minute)), ErrStaleFence)
	require.False(t, func() bool {
		_, removed := registry.RemoveExact(target, Fence{
			ProviderSessionID: authority.Fence.ProviderSessionID,
			AssignmentID:      authority.Fence.AssignmentID,
			ReservationEpoch:  uuid.New(),
		})
		return removed
	}())
	require.Equal(t, []peer.ID{target}, registry.ClearSession(session.ProviderSessionID))
	require.NoError(t, registry.SetSession(replacementSession))
}

func TestRegistryLeaseRefreshCannotReviveExpiredAuthority(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := newAtomicClock(now)
	registry := testRegistry(t, clock, 1, 2, 30*time.Second)
	session := testSession(now, 1)
	require.NoError(t, registry.SetSession(session))
	target := bookingPeer(t)
	authority := testAuthority(target, session.ProviderSessionID, uuid.New(), now)
	authority.ProviderLeaseExpiresAt = now.Add(time.Second)
	require.NoError(t, registry.InstallStarting(authority))
	require.NoError(t, registry.ActivateReady(target, authority.Fence))

	clock.Set(authority.ProviderLeaseExpiresAt)
	require.ErrorIs(t, registry.UpdateLease(target, authority.Fence, now.Add(time.Minute)), ErrAuthorityExpired)
	refreshed := authority
	refreshed.ProviderLeaseExpiresAt = now.Add(time.Minute)
	require.ErrorIs(t, registry.RefreshReady(refreshed), ErrAuthorityExpired)
	require.False(t, registry.AllowReserve(target))
}

func TestRegistryDelayedSessionAndAssignmentResponsesCannotReviveAuthority(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	t.Run("session response", func(t *testing.T) {
		clock := newAtomicClock(now)
		registry := testRegistry(t, clock, 1, 2, 30*time.Second)
		session := testSession(now, 1)
		session.SessionExpiresAt = now.Add(time.Second)
		require.NoError(t, registry.SetSession(session))
		authority := testAuthority(bookingPeer(t), session.ProviderSessionID, uuid.New(), now)
		require.NoError(t, registry.InstallStarting(authority))
		clock.Set(session.SessionExpiresAt)

		refreshed := session
		refreshed.SessionExpiresAt = now.Add(time.Minute)
		require.ErrorIs(t, registry.SetSession(refreshed), ErrAuthorityExpired)
		require.False(t, registry.AllowReserve(authority.TargetPeerID))
	})

	t.Run("assignment response", func(t *testing.T) {
		clock := newAtomicClock(now)
		registry := testRegistry(t, clock, 1, 2, 30*time.Second)
		session := testSession(now, 1)
		require.NoError(t, registry.SetSession(session))
		authority := testAuthority(bookingPeer(t), session.ProviderSessionID, uuid.New(), now)
		authority.ProviderLeaseExpiresAt = now.Add(time.Second)
		require.NoError(t, registry.InstallStarting(authority))
		require.NoError(t, registry.ActivateReady(authority.TargetPeerID, authority.Fence))
		clock.Set(authority.ProviderLeaseExpiresAt)

		refreshed := authority
		refreshed.ProviderLeaseExpiresAt = now.Add(time.Minute)
		require.ErrorIs(t, registry.InstallStarting(refreshed), ErrAuthorityExpired)
		require.False(t, registry.AllowReserve(authority.TargetPeerID))
	})
}

func TestRegistryConcurrentReadersWritersRemainBoundedAndFailClosed(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := newAtomicClock(now)
	registry := testRegistry(t, clock, 1, 32, 30*time.Second)
	session := testSession(now, 1)
	require.NoError(t, registry.SetSession(session))
	target := bookingPeer(t)
	domain := uuid.New()
	authority := testAuthority(target, session.ProviderSessionID, domain, now)
	require.NoError(t, registry.InstallStarting(authority))
	require.NoError(t, registry.ActivateReady(target, authority.Fence))

	const goroutines = 64
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for index := 0; index < goroutines; index++ {
		source := bookingPeer(t)
		go func() {
			defer wait.Done()
			for attempt := 0; attempt < 20; attempt++ {
				registry.AllowReserve(target)
				registry.SnapshotForAdmission(target, domain)
				_, _ = registry.Admit(AdmissionRequest{
					SourcePeerID:        source,
					DomainID:            domain,
					TargetPeerID:        target,
					ExpectedFence:       authority.Fence,
					LiteralJWTExpiresAt: now.Add(time.Minute),
				})
				registry.AllowConnect(source, target)
			}
		}()
	}
	wait.Wait()
	bookings, admissions := registry.Counts()
	require.Equal(t, 1, bookings)
	require.LessOrEqual(t, admissions, 32)
	_, removed := registry.RemoveExact(target, authority.Fence)
	require.True(t, removed)
	require.False(t, registry.AllowReserve(target))
	bookings, admissions = registry.Counts()
	require.Zero(t, bookings)
	require.Zero(t, admissions)
}

func TestBeginDrainRetainsExpiringAuthorityButClosesEveryNewAuthorizationGate(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := newAtomicClock(now)
	registry := testRegistry(t, clock, 1, 8, 30*time.Second)
	session := testSession(now, 1)
	require.NoError(t, registry.SetSession(session))
	target := bookingPeer(t)
	source := bookingPeer(t)
	domain := uuid.New()
	authority := testAuthority(target, session.ProviderSessionID, domain, now)
	require.NoError(t, registry.InstallStarting(authority))
	require.NoError(t, registry.ActivateReady(target, authority.Fence))
	snapshot, found := registry.SnapshotForAdmission(target, domain)
	require.True(t, found)
	_, err := registry.Admit(AdmissionRequest{
		SourcePeerID: source, DomainID: domain, TargetPeerID: target,
		ExpectedFence: snapshot.Authority.Fence, LiteralJWTExpiresAt: now.Add(time.Minute),
	})
	require.NoError(t, err)
	require.True(t, registry.AllowReserve(target))
	require.True(t, registry.AllowConnect(source, target))

	registry.BeginDrain()
	require.True(t, registry.Draining())
	bookings, admissions := registry.Counts()
	require.Equal(t, 1, bookings, "drain retains the known lease until the release boundary")
	require.Zero(t, admissions, "drain invalidates every cached source admission")
	_, found = registry.SnapshotForAdmission(target, domain)
	require.False(t, found)
	_, err = registry.Admit(AdmissionRequest{
		SourcePeerID: source, DomainID: domain, TargetPeerID: target,
		ExpectedFence: authority.Fence, LiteralJWTExpiresAt: now.Add(time.Minute),
	})
	require.ErrorIs(t, err, ErrAuthorityNotReady)
	require.False(t, registry.AllowReserve(target))
	require.False(t, registry.AllowConnect(source, target))

	refreshed := authority
	refreshed.ProviderLeaseExpiresAt = now.Add(2 * time.Minute)
	require.NoError(t, registry.RefreshReady(refreshed), "typed heartbeats continue during pre-release drain")
	clock.Set(refreshed.ProviderLeaseExpiresAt)
	expired := registry.Expire()
	require.Len(t, expired, 1)
	require.Equal(t, authority.Fence, expired[0].Fence)
	bookings, admissions = registry.Counts()
	require.Zero(t, bookings)
	require.Zero(t, admissions)
}

func TestRegistryValidatesBounds(t *testing.T) {
	_, err := New(Config{MaximumBookings: 0, MaximumAdmissions: 1, AdmissionTTL: time.Second})
	require.Error(t, err)
	_, err = New(Config{MaximumBookings: 1, MaximumAdmissions: 0, AdmissionTTL: time.Second})
	require.Error(t, err)
	_, err = New(Config{MaximumBookings: 1, MaximumAdmissions: 1, AdmissionTTL: 31 * time.Second})
	require.Error(t, err)
}

func TestRegistrySupports10000BookingsAndRejectsOverflow(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	registry := testRegistry(t, newAtomicClock(now), 10000, 4096, 30*time.Second)
	session := testSession(now, 10000)
	require.NoError(t, registry.SetSession(session))
	domain := uuid.New()
	for range 10000 {
		authority := testAuthority(bookingPeer(t), session.ProviderSessionID, domain, now)
		require.NoError(t, registry.InstallStarting(authority))
		require.NoError(t, registry.ActivateReady(authority.TargetPeerID, authority.Fence))
		require.True(t, registry.AllowReserve(authority.TargetPeerID))
	}
	bookings, _ := registry.Counts()
	require.Equal(t, 10000, bookings)
	require.ErrorIs(t, registry.InstallStarting(testAuthority(bookingPeer(t), session.ProviderSessionID, domain, now)), ErrBookingCapacity)
	_, err := New(Config{MaximumBookings: 0, MaximumAdmissions: 4096, AdmissionTTL: 30 * time.Second})
	require.Error(t, err)
}
