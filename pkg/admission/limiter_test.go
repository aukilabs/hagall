package admission

import (
	"context"
	"crypto/rand"
	"net/netip"
	"testing"
	"time"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

func testPeer(t *testing.T) peer.ID {
	t.Helper()
	_, publicKey, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPublicKey(publicKey)
	require.NoError(t, err)
	return peerID
}

func TestLimiterBoundsConcurrencyAttemptsCacheAndLifetime(t *testing.T) {
	limiter, err := New(Config{
		TTL:             20 * time.Millisecond,
		MaximumEntries:  4,
		Concurrency:     2,
		AttemptsPerPeer: 2,
		AttemptsPerIP:   2,
		AttemptWindow:   time.Second,
	})
	require.NoError(t, err)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	peerA := testPeer(t)
	peerB := testPeer(t)
	ipA := netip.MustParseAddr("192.0.2.1")
	ipB := netip.MustParseAddr("192.0.2.2")

	ctxA, releaseA, err := limiter.Acquire(context.Background(), peerA, ipA, now)
	require.NoError(t, err)
	_, releaseB, err := limiter.Acquire(context.Background(), peerB, ipB, now)
	require.NoError(t, err)
	_, _, err = limiter.Acquire(context.Background(), testPeer(t), netip.MustParseAddr("192.0.2.3"), now)
	require.ErrorIs(t, err, ErrBusy)
	require.Equal(t, 4, limiter.CachedEntries())

	select {
	case <-ctxA.Done():
	case <-time.After(time.Second):
		t.Fatal("bounded authentication context did not expire")
	}
	releaseA()
	releaseA()
	releaseB()

	_, release, err := limiter.Acquire(context.Background(), peerA, ipA, now.Add(time.Millisecond))
	require.NoError(t, err)
	release()
	_, _, err = limiter.Acquire(context.Background(), peerA, ipA, now.Add(2*time.Millisecond))
	require.ErrorIs(t, err, ErrRateLimited)

	_, release, err = limiter.Acquire(context.Background(), peerA, ipA, now.Add(2*time.Second))
	require.NoError(t, err, "expired attempt buckets must be pruned and reusable")
	release()
}

func TestLimiterRejectsNewIdentitiesInsteadOfGrowingPastItsBound(t *testing.T) {
	limiter, err := New(Config{
		TTL:             time.Second,
		MaximumEntries:  2,
		Concurrency:     2,
		AttemptsPerPeer: 2,
		AttemptsPerIP:   2,
		AttemptWindow:   time.Minute,
	})
	require.NoError(t, err)
	firstContext, firstRelease, err := limiter.Acquire(context.Background(), testPeer(t), netip.MustParseAddr("192.0.2.1"), time.Now())
	require.NoError(t, err)
	require.NotNil(t, firstContext)
	firstRelease()

	_, _, err = limiter.Acquire(context.Background(), testPeer(t), netip.MustParseAddr("192.0.2.2"), time.Now())
	require.ErrorIs(t, err, ErrCacheFull)
	require.Equal(t, 2, limiter.CachedEntries())
}
