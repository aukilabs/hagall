package verification

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/authpkg"
	"github.com/aukilabs/service-lib/pkg/tokenclaims"
	"github.com/aukilabs/service-lib/pkg/tokens"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

type fetchResult struct {
	keySet KeySet
	err    error
}

type queuedFetcher struct {
	mu      sync.Mutex
	results []fetchResult
	calls   int
}

func (f *queuedFetcher) Fetch(context.Context) (KeySet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.results) == 0 {
		return KeySet{}, errors.New("scripted key outage")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.keySet, result.err
}

func (f *queuedFetcher) push(result fetchResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, result)
}

func (f *queuedFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type signingIdentity struct {
	material Material
	config   *tokens.Config
}

func newSigningIdentity(t *testing.T) signingIdentity {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return signingIdentity{
		material: Material{PublicKey: publicKey, Method: jwt.SigningMethodEdDSA},
		config: &tokens.Config{
			PrivateKey:    privateKey,
			PublicKey:     publicKey,
			SigningMethod: jwt.SigningMethodEdDSA,
		},
	}
}

func newRingForTest(t *testing.T, fetcher Fetcher, now *time.Time) *Ring {
	t.Helper()
	ring, err := NewRing(fetcher, Options{
		ExpectedMethod:       jwt.SigningMethodEdDSA.Alg(),
		PreviousKeyOverlap:   31 * time.Minute,
		MaxKeyStaleness:      time.Hour,
		UnknownRefreshPeriod: 30 * time.Second,
		Now:                  func() time.Time { return *now },
	})
	require.NoError(t, err)
	return ring
}

func testKeySet(generation uint64, current Material, previous *Material) KeySet {
	return KeySet{
		Generation:         generation,
		PreviousKeyOverlap: 31 * time.Minute,
		Current:            current,
		Previous:           previous,
	}
}

func makeP2PToken(t *testing.T, identity signingIdentity, issuedAt time.Time) (string, time.Time) {
	t.Helper()
	libp2pKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(libp2pKey)
	require.NoError(t, err)
	expiresAt := issuedAt.Add(authpkg.P2PAccessTokenTTL)
	token, returnedExpiresAt, err := authpkg.GenerateP2PAccessTokenAt(
		identity.config,
		uuid.NewString(),
		uuid.NewString(),
		authpkg.P2PPeerTypeCompute,
		peerID.String(),
		[]string{uuid.NewString()},
		issuedAt,
	)
	require.NoError(t, err)
	require.True(t, returnedExpiresAt.Equal(expiresAt))
	return token, expiresAt
}

func makeNodeToken(t *testing.T, identity signingIdentity, issuedAt time.Time, audience string, boundPeer *peer.ID) string {
	t.Helper()
	revision := int64(7)
	var peerID *string
	if boundPeer != nil {
		value := boundPeer.String()
		peerID = &value
	}
	claims := authpkg.NodeAccessClaims{
		NodeID:             uuid.NewString(),
		OrganizationID:     uuid.NewString(),
		NodeMode:           authpkg.NodeModePublic,
		Capabilities:       []string{"/p2p/circuit-relay/v1"},
		SchedulingRevision: &revision,
		PeerID:             peerID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "dds",
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(issuedAt.Add(time.Hour)),
		},
	}
	claims.Subject = claims.NodeID
	token, err := jwt.NewWithClaims(identity.config.SigningMethod, claims).SignedString(identity.config.PrivateKey)
	require.NoError(t, err)
	return token
}

func TestRingFollowsAuthoritativeRotationLineage(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	one := newSigningIdentity(t)
	two := newSigningIdentity(t)
	three := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{keySet: testKeySet(1, one.material, nil)}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))

	tokenOne, _ := makeP2PToken(t, one, now.Add(-time.Minute))
	_, _, err := ring.VerifyP2P(context.Background(), tokenOne, now, time.Minute)
	require.NoError(t, err)

	fetcher.push(fetchResult{keySet: testKeySet(2, two.material, &one.material)})
	require.NoError(t, ring.Preload(context.Background()))
	tokenTwo, _ := makeP2PToken(t, two, now.Add(-time.Minute))
	_, _, err = ring.VerifyP2P(context.Background(), tokenOne, now, time.Minute)
	require.NoError(t, err, "the prior key must remain valid for live tokens")
	_, _, err = ring.VerifyP2P(context.Background(), tokenTwo, now, time.Minute)
	require.NoError(t, err)

	fetcher.push(fetchResult{keySet: testKeySet(3, three.material, &two.material)})
	require.NoError(t, ring.Preload(context.Background()))
	tokenThree, _ := makeP2PToken(t, three, now.Add(-time.Minute))
	_, _, err = ring.VerifyP2P(context.Background(), tokenTwo, now, time.Minute)
	require.NoError(t, err, "DDS still publishes the prior current key")
	_, _, err = ring.VerifyP2P(context.Background(), tokenThree, now, time.Minute)
	require.NoError(t, err)
	require.True(t, ring.Ready(now))
}

func TestRingPreloadRestoresPublishedPreviousKeyAfterRestart(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	previous := newSigningIdentity(t)
	current := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{
		keySet: testKeySet(7, current.material, &previous.material),
	}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))

	previousToken, _ := makeP2PToken(t, previous, now.Add(-time.Minute))
	_, _, err := ring.VerifyP2P(context.Background(), previousToken, now, time.Minute)
	require.NoError(t, err, "a restarted relay must retain the DDS-published previous key")
	currentToken, _ := makeP2PToken(t, current, now.Add(-time.Minute))
	_, _, err = ring.VerifyP2P(context.Background(), currentToken, now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, fetcher.callCount())
}

func TestRingRestartNearNextRotationAcceptsThePublishedSuccessorImmediately(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	one := newSigningIdentity(t)
	two := newSigningIdentity(t)
	three := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{
		{keySet: testKeySet(2, two.material, &one.material)},
		{keySet: testKeySet(3, three.material, &two.material)},
	}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()), "simulate restart while DDS publishes B+A")
	require.NoError(t, ring.Preload(context.Background()), "C+B must not wait on a client-local overlap timer")

	for name, identity := range map[string]signingIdentity{"previous": two, "current": three} {
		t.Run(name, func(t *testing.T) {
			token, _ := makeP2PToken(t, identity, now.Add(-time.Minute))
			_, _, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
			require.NoError(t, err)
		})
	}
}

func TestRingRejectsInvalidHigherGenerationLineageWithoutReplacingLiveKeys(t *testing.T) {
	for _, test := range []struct {
		name       string
		nextKeySet func(one, two, three signingIdentity) KeySet
	}{
		{
			name: "changed current does not publish old current as previous",
			nextKeySet: func(one, _ signingIdentity, three signingIdentity) KeySet {
				return testKeySet(3, three.material, &one.material)
			},
		},
		{
			name: "unchanged current introduces unrelated previous",
			nextKeySet: func(_ signingIdentity, two signingIdentity, three signingIdentity) KeySet {
				return testKeySet(3, two.material, &three.material)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
			one := newSigningIdentity(t)
			two := newSigningIdentity(t)
			three := newSigningIdentity(t)
			fetcher := &queuedFetcher{results: []fetchResult{
				{keySet: testKeySet(2, two.material, &one.material)},
				{keySet: test.nextKeySet(one, two, three)},
			}}
			ring := newRingForTest(t, fetcher, &now)
			require.NoError(t, ring.Preload(context.Background()))
			require.ErrorIs(t, ring.Preload(context.Background()), ErrInvalidKeySetLineage)

			for name, identity := range map[string]signingIdentity{"previous": one, "current": two} {
				t.Run(name, func(t *testing.T) {
					token, _ := makeP2PToken(t, identity, now.Add(-time.Minute))
					_, _, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
					require.NoError(t, err, "rejected publication must leave the installed set untouched")
				})
			}
		})
	}
}

func TestRingAllowsHigherGenerationToRetainOrRetireTheSamePreviousKey(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	one := newSigningIdentity(t)
	two := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{
		{keySet: testKeySet(2, two.material, &one.material)},
		{keySet: testKeySet(3, two.material, &one.material)},
		{keySet: testKeySet(4, two.material, nil)},
	}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))
	require.NoError(t, ring.Preload(context.Background()))
	require.Len(t, ring.usableKeys(now), 2)
	require.NoError(t, ring.Preload(context.Background()))
	require.Len(t, ring.usableKeys(now), 1)

	token, _ := makeP2PToken(t, two, now.Add(-time.Minute))
	_, _, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
	require.NoError(t, err)
}

func TestRingRejectsPublishedOverlapBelowTheConfiguredMinimum(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	one := newSigningIdentity(t)
	two := newSigningIdentity(t)
	shortOverlap := testKeySet(2, two.material, &one.material)
	shortOverlap.PreviousKeyOverlap = 30 * time.Minute
	fetcher := &queuedFetcher{results: []fetchResult{
		{keySet: testKeySet(1, one.material, nil)},
		{keySet: shortOverlap},
	}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))
	require.ErrorIs(t, ring.Preload(context.Background()), ErrRotationTooFast)

	token, _ := makeP2PToken(t, one, now.Add(-time.Minute))
	_, _, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
	require.NoError(t, err, "rejected publication must leave the installed set untouched")
}

func TestRingRejectsStaleAndSameGenerationKeySetsWithoutReplacingLiveKeys(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	one := newSigningIdentity(t)
	two := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{
		{keySet: testKeySet(2, two.material, &one.material)},
		{keySet: testKeySet(1, one.material, nil)},
		{keySet: testKeySet(2, one.material, nil)},
	}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))
	require.ErrorIs(t, ring.Preload(context.Background()), ErrStaleKeySet)
	require.ErrorIs(t, ring.Preload(context.Background()), ErrConflictingKeySet)

	token, _ := makeP2PToken(t, two, now.Add(-time.Minute))
	_, _, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
	require.NoError(t, err, "rejected publications must leave the live key set untouched")
}

func TestUnknownSignatureRefreshIsRateLimitedAndCanInstallOneNewKey(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	one := newSigningIdentity(t)
	two := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{
		{keySet: testKeySet(1, one.material, nil)},
		{keySet: testKeySet(2, two.material, &one.material)},
	}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))

	tokenTwo, literalExpiry := makeP2PToken(t, two, now.Add(-time.Minute))
	claims, returnedExpiry, err := ring.VerifyP2P(context.Background(), tokenTwo, now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claims)
	require.Equal(t, literalExpiry, returnedExpiry)
	require.Equal(t, 2, fetcher.callCount())

	unknown := newSigningIdentity(t)
	unknownToken, _ := makeP2PToken(t, unknown, now.Add(-time.Minute))
	_, _, err = ring.VerifyP2P(context.Background(), unknownToken, now, time.Minute)
	require.Error(t, err)
	require.Equal(t, 2, fetcher.callCount(), "a second unknown signature inside the window must not fetch")
}

func TestRingFailsClosedAfterOutageMakesEveryKeyStale(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	previous := newSigningIdentity(t)
	current := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{keySet: testKeySet(2, current.material, &previous.material)}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))

	now = now.Add(time.Hour + time.Second)
	require.False(t, ring.Ready(now))
	for name, identity := range map[string]signingIdentity{"previous": previous, "current": current} {
		t.Run(name, func(t *testing.T) {
			token, _ := makeP2PToken(t, identity, now.Add(-time.Minute))
			_, _, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
			require.ErrorIs(t, err, ErrNoTrustedKey)
		})
	}
}

func TestVerificationSkewNeverChangesLiteralExpiration(t *testing.T) {
	issuedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	now := issuedAt
	one := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{keySet: testKeySet(1, one.material, nil)}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))
	token, literalExpiry := makeP2PToken(t, one, issuedAt)

	now = literalExpiry.Add(30 * time.Second)
	_, returnedExpiry, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
	require.NoError(t, err, "configured verifier skew may accept the request")
	require.Equal(t, literalExpiry, returnedExpiry, "authority must use the signed literal exp")

	now = literalExpiry.Add(time.Minute)
	_, _, err = ring.VerifyP2P(context.Background(), token, now, time.Minute)
	require.ErrorContains(t, err, "expired")
	require.Equal(t, 1, fetcher.callCount(), "an expired but correctly signed token must not trigger a key refresh")
}

func TestP2PVerificationTreatsPeerTypeAsMetadataAndAcceptsMultipleDomains(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	identity := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{keySet: testKeySet(1, identity.material, nil)}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))

	libp2pKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(libp2pKey)
	require.NoError(t, err)
	domains := []string{uuid.NewString(), uuid.NewString()}
	for _, peerType := range []string{
		authpkg.P2PPeerTypeRobot,
		authpkg.P2PPeerTypeCompute,
		authpkg.P2PPeerTypeUser,
		authpkg.P2PPeerTypeApp,
		authpkg.P2PPeerTypeDomainServer,
	} {
		organizationID := uuid.NewString()
		if peerType == authpkg.P2PPeerTypeDomainServer {
			organizationID = ""
		}
		claims := &authpkg.P2PAccessTokenClaims{
			PeerType:       peerType,
			PeerID:         peerID.String(),
			OrganizationID: organizationID,
			DomainIDs:      domains,
			Scopes:         []string{authpkg.P2PAccessTokenScope},
			TypedClaims: tokenclaims.TypedClaims{
				Type: authpkg.P2PAccessTokenType,
				RegisteredClaims: jwt.RegisteredClaims{
					Issuer:    authpkg.P2PAccessTokenIssuer,
					Subject:   uuid.NewString(),
					Audience:  jwt.ClaimStrings{authpkg.P2PAccessTokenAudience},
					IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
					ExpiresAt: jwt.NewNumericDate(now.Add(-time.Minute).Add(authpkg.P2PAccessTokenTTL)),
				},
			},
		}
		token, err := jwt.NewWithClaims(identity.config.SigningMethod, claims).SignedString(identity.config.PrivateKey)
		require.NoError(t, err)

		verified, _, err := ring.VerifyP2P(context.Background(), token, now, time.Minute)
		require.NoError(t, err)
		require.Equal(t, peerType, verified.PeerType)
		require.Equal(t, organizationID, verified.OrganizationID)
		require.Equal(t, domains, verified.DomainIDs)
	}
}

func TestP2PVerificationAcceptsLegacyMissingOrganizationAndRejectsNoncanonicalAuthority(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	identity := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{keySet: testKeySet(1, identity.material, nil)}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))

	libp2pKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(libp2pKey)
	require.NoError(t, err)
	legacyToken, _, err := authpkg.GenerateP2PAccessTokenAt(
		identity.config,
		uuid.NewString(),
		"",
		authpkg.P2PPeerTypeCompute,
		peerID.String(),
		[]string{uuid.NewString()},
		now.Add(-time.Minute),
	)
	require.NoError(t, err)
	legacyClaims, _, err := ring.VerifyP2P(context.Background(), legacyToken, now, time.Minute)
	require.NoError(t, err)
	require.Empty(t, legacyClaims.OrganizationID)

	validClaims := func() *authpkg.P2PAccessTokenClaims {
		issuedAt := jwt.NewNumericDate(now.Add(-time.Minute))
		return &authpkg.P2PAccessTokenClaims{
			PeerType:       authpkg.P2PPeerTypeCompute,
			PeerID:         peerID.String(),
			OrganizationID: uuid.NewString(),
			DomainIDs:      []string{uuid.NewString()},
			Scopes:         []string{authpkg.P2PAccessTokenScope},
			TypedClaims: tokenclaims.TypedClaims{
				Type: authpkg.P2PAccessTokenType,
				RegisteredClaims: jwt.RegisteredClaims{
					Issuer:    authpkg.P2PAccessTokenIssuer,
					Subject:   uuid.NewString(),
					Audience:  jwt.ClaimStrings{authpkg.P2PAccessTokenAudience},
					IssuedAt:  issuedAt,
					ExpiresAt: jwt.NewNumericDate(issuedAt.Add(authpkg.P2PAccessTokenTTL)),
				},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*authpkg.P2PAccessTokenClaims)
	}{
		{name: "noncanonical requester organization", mutate: func(claims *authpkg.P2PAccessTokenClaims) {
			claims.OrganizationID = strings.ToUpper(claims.OrganizationID)
		}},
		{name: "noncanonical subject", mutate: func(claims *authpkg.P2PAccessTokenClaims) {
			claims.Subject = strings.ToUpper(claims.Subject)
		}},
		{name: "noncanonical Domain", mutate: func(claims *authpkg.P2PAccessTokenClaims) {
			claims.DomainIDs[0] = strings.ToUpper(claims.DomainIDs[0])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := validClaims()
			test.mutate(claims)
			token, err := jwt.NewWithClaims(identity.config.SigningMethod, claims).SignedString(identity.config.PrivateKey)
			require.NoError(t, err)
			_, _, err = ring.VerifyP2P(context.Background(), token, now, time.Minute)
			require.Error(t, err)
		})
	}
	require.Equal(t, 1, fetcher.callCount(), "signed profile failures must not refresh keys")
}

func TestP2PVerificationPreservesOpaqueUsersAndRejectsUnboundedSignedSubjects(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	identity := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{keySet: testKeySet(1, identity.material, nil)}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))
	token, _ := makeP2PToken(t, identity, now)
	claims, err := authpkg.VerifyP2PAccessToken(identity.config, token)
	require.NoError(t, err)
	claims.PeerType = authpkg.P2PPeerTypeUser
	for _, subject := range []string{
		" User|Case-敏感 ", "550E8400-E29B-41D4-A716-446655440000", strings.Repeat("界", 85),
	} {
		claims.Subject = subject
		signed, err := jwt.NewWithClaims(identity.config.SigningMethod, claims).SignedString(identity.config.PrivateKey)
		require.NoError(t, err)
		verified, _, err := ring.VerifyP2P(context.Background(), signed, now, time.Minute)
		require.NoError(t, err)
		require.Equal(t, subject, verified.Subject)
	}
	for _, subject := range []string{"", strings.Repeat("x", 256), strings.Repeat("é", 128)} {
		claims.Subject = subject
		signed, err := jwt.NewWithClaims(identity.config.SigningMethod, claims).SignedString(identity.config.PrivateKey)
		require.NoError(t, err)
		_, _, err = ring.VerifyP2P(context.Background(), signed, now, time.Minute)
		require.Error(t, err)
	}
	require.Equal(t, 1, fetcher.callCount(), "invalid subjects must not refresh trusted keys")
}

func TestNodeTokenVerifierUsesTheSameBoundedRingAndPinnedProfile(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	one := newSigningIdentity(t)
	fetcher := &queuedFetcher{results: []fetchResult{{keySet: testKeySet(1, one.material, nil)}}}
	ring := newRingForTest(t, fetcher, &now)
	require.NoError(t, ring.Preload(context.Background()))
	verifier, err := NewNodeTokenVerifier(ring, "dds", []string{"https://dds.example.com"}, time.Minute, func() time.Time { return now })
	require.NoError(t, err)

	libp2pKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(libp2pKey)
	require.NoError(t, err)
	token := makeNodeToken(t, one, now.Add(-time.Minute), "https://dds.example.com", &peerID)
	claims, err := verifier.VerifyNodeAccessToken(context.Background(), token)
	require.NoError(t, err)
	require.NotNil(t, claims.PeerID)
	require.Equal(t, peerID.String(), *claims.PeerID)

	wrongAudience := makeNodeToken(t, one, now.Add(-time.Minute), "https://other.example.com", nil)
	_, err = verifier.VerifyNodeAccessToken(context.Background(), wrongAudience)
	require.ErrorContains(t, err, "audience")
	require.Equal(t, 1, fetcher.callCount(), "a signed profile failure must not refresh keys")
}

func TestHTTPFetcherRequiresStrictBoundedKeyResponse(t *testing.T) {
	current := newSigningIdentity(t)
	previous := newSigningIdentity(t)
	currentPEM, err := tokens.CreatePublicKeyPEM(current.material.PublicKey)
	require.NoError(t, err)
	previousPEM, err := tokens.CreatePublicKeyPEM(previous.material.PublicKey)
	require.NoError(t, err)
	currentID, err := publishedKeyFingerprint(current.material.PublicKey)
	require.NoError(t, err)
	previousID, err := publishedKeyFingerprint(previous.material.PublicKey)
	require.NoError(t, err)
	validResponse := verificationKeySetResponse{
		Version:                   verificationKeySetVersion,
		Generation:                7,
		PreviousKeyOverlapSeconds: uint64((31 * time.Minute) / time.Second),
		Keys: []verificationKeyResponse{
			{ID: currentID, Status: verificationKeyCurrent, PublicKey: currentPEM, SigningMethod: jwt.SigningMethodEdDSA.Alg()},
			{ID: previousID, Status: verificationKeyPrevious, PublicKey: previousPEM, SigningMethod: jwt.SigningMethodEdDSA.Alg()},
		},
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "application/json", request.Header.Get("Accept"))
		require.Equal(t, "no-cache", request.Header.Get("Cache-Control"))
		response.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(response).Encode(validResponse))
	}))
	defer server.Close()

	fetcher := &HTTPFetcher{URL: server.URL, Client: server.Client()}
	keySet, err := fetcher.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(7), keySet.Generation)
	require.Equal(t, 31*time.Minute, keySet.PreviousKeyOverlap)
	require.Equal(t, jwt.SigningMethodEdDSA.Alg(), keySet.Current.Method.Alg())
	require.NotNil(t, keySet.Previous)
	require.Equal(t, jwt.SigningMethodEdDSA.Alg(), keySet.Previous.Method.Alg())

	badServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"version": 1, "generation": 7, "previous_key_overlap_seconds": 1860,
			"keys": validResponse.Keys, "unexpected": true,
		})
	}))
	defer badServer.Close()
	_, err = (&HTTPFetcher{URL: badServer.URL, Client: badServer.Client()}).Fetch(context.Background())
	require.ErrorContains(t, err, "unknown field")

	nonJSONServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain")
		_, _ = response.Write([]byte("not a trusted key document"))
	}))
	defer nonJSONServer.Close()
	_, err = (&HTTPFetcher{URL: nonJSONServer.URL, Client: nonJSONServer.Client()}).Fetch(context.Background())
	require.ErrorContains(t, err, "not JSON")

	oversizedServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(make([]byte, maxPublicKeyResponseBytes+1))
	}))
	defer oversizedServer.Close()
	_, err = (&HTTPFetcher{URL: oversizedServer.URL, Client: oversizedServer.Client()}).Fetch(context.Background())
	require.ErrorContains(t, err, "too large")

	redirectServer := httptest.NewServer(http.RedirectHandler(server.URL, http.StatusFound))
	defer redirectServer.Close()
	_, err = (&HTTPFetcher{URL: redirectServer.URL, Client: redirectServer.Client()}).Fetch(context.Background())
	require.ErrorContains(t, err, "unexpected HTTP status 302")
}
