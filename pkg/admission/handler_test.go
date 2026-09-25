package admission

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/authpkg"
	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/telemetry"
	"github.com/aukilabs/hagall/pkg/verification"
	"github.com/aukilabs/service-lib/pkg/tokenclaims"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	libp2p "github.com/libp2p/go-libp2p"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/stretchr/testify/require"
)

type verifierFunc func(context.Context, string, time.Time, time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error)

type authObserver chan telemetry.SourceAuthOutcome

func (o authObserver) ObserveSourceAuth(outcome telemetry.SourceAuthOutcome) { o <- outcome }

func TestHandlerDistinguishesLimiterFailuresWithTheSameWireDenial(t *testing.T) {
	for _, outcome := range []telemetry.SourceAuthOutcome{
		telemetry.SourceAuthRateLimited, telemetry.SourceAuthBusy,
		telemetry.SourceAuthCacheFull, telemetry.SourceAuthContextDone,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			fixture := newHandlerFixture(t)
			limiter, err := New(Config{
				TTL: time.Second, MaximumEntries: 2, Concurrency: 1,
				AttemptsPerPeer: 2, AttemptsPerIP: 2, AttemptWindow: time.Minute,
			})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ip := netip.MustParseAddr("127.0.0.1")
			switch outcome {
			case telemetry.SourceAuthRateLimited:
				for range 2 {
					_, release, err := limiter.Acquire(ctx, fixture.client.ID(), ip, fixture.now)
					require.NoError(t, err)
					release()
				}
			case telemetry.SourceAuthBusy:
				_, release, err := limiter.Acquire(ctx, fixture.client.ID(), ip, fixture.now)
				require.NoError(t, err)
				defer release()
			case telemetry.SourceAuthCacheFull:
				_, release, err := limiter.Acquire(ctx, fixture.target, netip.MustParseAddr("192.0.2.1"), fixture.now)
				require.NoError(t, err)
				release()
			case telemetry.SourceAuthContextDone:
				cancel()
			}
			observed := make(authObserver, 1)
			handler, err := NewHandler(HandlerOptions{
				Context: ctx, Limiter: limiter, Registry: fixture.registry,
				Verifier: verifierFunc(func(context.Context, string, time.Time, time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
					t.Error("rejected admission must not reach verification")
					return nil, time.Time{}, errors.New("unexpected verification")
				}),
				Now: func() time.Time { return fixture.now }, Observer: observed,
			})
			require.NoError(t, err)
			fixture.server.SetStreamHandler(ProtocolID, handler.HandleStream)
			payload := encodeRequest(t, fixture.domain, fixture.target, "token")
			require.Equal(t, deniedResponse, exchangePayload(t, fixture.client, fixture.server.ID(), payload))
			select {
			case got := <-observed:
				require.Equal(t, outcome, got)
			case <-time.After(time.Second):
				t.Fatal("missing admission outcome")
			}
		})
	}
}

const rustSourceAdmissionRequestVector = `{"version":1,"domain_id":"11111111-2222-3333-4444-555555555555","target_peer_id":"12D3KooWBMyph6PCuP6GUJkwFdR7bLUPZ3exLvgEPpR93J52GaJg","p2p_access_token":"header.payload.signature"}`

func (f verifierFunc) VerifyP2P(ctx context.Context, token string, now time.Time, skew time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
	return f(ctx, token, now, skew)
}

func TestSourceAdmissionRequestVectorMatchesRust(t *testing.T) {
	payload := []byte(rustSourceAdmissionRequestVector)
	require.Len(t, payload, 182)
	require.Equal(t, []byte{0, 0, 0, 182}, framed(payload)[:4])

	request, err := decodeRequest(payload)
	require.NoError(t, err)
	require.Equal(t, 1, request.Version)
	require.Equal(t, uuid.MustParse("11111111-2222-3333-4444-555555555555"), request.DomainID)
	require.Equal(t, "12D3KooWBMyph6PCuP6GUJkwFdR7bLUPZ3exLvgEPpR93J52GaJg", request.TargetPeerID.String())
	require.Equal(t, "header.payload.signature", request.P2PAccessToken)
}

type staticKeyFetcher struct {
	material verification.Material
}

func (f staticKeyFetcher) Fetch(context.Context) (verification.KeySet, error) {
	return verification.KeySet{
		Generation:         1,
		PreviousKeyOverlap: 31 * time.Minute,
		Current:            f.material,
	}, nil
}

type handlerFixture struct {
	now       time.Time
	server    host.Host
	client    host.Host
	registry  *booking.Registry
	target    peer.ID
	domain    uuid.UUID
	authority booking.Authority
}

func newHandlerPeer(t *testing.T) peer.ID {
	t.Helper()
	_, publicKey, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPublicKey(publicKey)
	require.NoError(t, err)
	return peerID
}

func newHandlerFixture(t *testing.T) handlerFixture {
	t.Helper()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	hostOptions := []libp2p.Option{
		libp2p.NoTransports,
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport),
	}
	server, err := libp2p.New(hostOptions...)
	require.NoError(t, err)
	client, err := libp2p.New(hostOptions...)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		require.NoError(t, server.Close())
	})
	require.NoError(t, client.Connect(context.Background(), peer.AddrInfo{ID: server.ID(), Addrs: server.Addrs()}))
	registry, err := booking.New(booking.Config{
		MaximumBookings:   2,
		MaximumAdmissions: 16,
		AdmissionTTL:      30 * time.Second,
		Now:               func() time.Time { return now },
	})
	require.NoError(t, err)
	sessionID := uuid.New()
	require.NoError(t, registry.SetSession(booking.Session{
		ProviderSessionID: sessionID,
		EffectiveCapacity: 2,
		SessionExpiresAt:  now.Add(2 * time.Minute),
		NodeJWTExpiresAt:  now.Add(3 * time.Minute),
	}))
	target := newHandlerPeer(t)
	domain := uuid.New()
	authority := booking.Authority{
		BookingID:              uuid.New(),
		SlotID:                 uuid.New(),
		DomainID:               domain,
		TargetPeerID:           target,
		Fence:                  booking.Fence{ProviderSessionID: sessionID, AssignmentID: uuid.New(), ReservationEpoch: uuid.New()},
		RequestedUntil:         now.Add(5 * time.Minute),
		AuthorityExpiresAt:     now.Add(90 * time.Second),
		ProviderLeaseExpiresAt: now.Add(time.Minute),
	}
	require.NoError(t, registry.InstallStarting(authority))
	require.NoError(t, registry.ActivateReady(target, authority.Fence))
	return handlerFixture{now: now, server: server, client: client, registry: registry, target: target, domain: domain, authority: authority}
}

func (f handlerFixture) installHandler(t *testing.T, verifier P2PVerifier, concurrency, attempts int) *Handler {
	t.Helper()
	limiter, err := New(Config{
		TTL:             30 * time.Second,
		MaximumEntries:  64,
		Concurrency:     concurrency,
		AttemptsPerPeer: min(attempts, concurrency),
		AttemptsPerIP:   min(attempts, concurrency),
		AttemptWindow:   time.Minute,
	})
	require.NoError(t, err)
	handler, err := NewHandler(HandlerOptions{
		Limiter:   limiter,
		Registry:  f.registry,
		Verifier:  verifier,
		ClockSkew: time.Minute,
		Now:       func() time.Time { return f.now },
	})
	require.NoError(t, err)
	f.server.SetStreamHandler(ProtocolID, handler.HandleStream)
	return handler
}

func TestHandlerAdmitsExactNoiseSourceAndReturnsLiteralMinimum(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.installHandler(t, verifierFunc(func(_ context.Context, token string, now time.Time, skew time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
		require.Equal(t, "opaque-secret-token", token)
		require.Equal(t, fixture.now, now)
		require.Equal(t, time.Minute, skew)
		return &authpkg.P2PAccessTokenClaims{
			PeerID:    fixture.client.ID().String(),
			DomainIDs: []string{uuid.New().String(), fixture.domain.String()},
		}, fixture.now.Add(45 * time.Second), nil
	}), 4, 16)

	payload := encodeRequest(t, fixture.domain, fixture.target, "opaque-secret-token")
	actual := exchangePayload(t, fixture.client, fixture.server.ID(), payload)
	require.NotContains(t, string(actual), "opaque-secret-token")
	var result response
	require.NoError(t, json.Unmarshal(actual, &result))
	require.True(t, result.Accepted)
	require.Equal(t, fixture.now.Add(30*time.Second).Format(time.RFC3339Nano), result.AcceptedUntil)
	require.True(t, fixture.registry.AllowConnect(fixture.client.ID(), fixture.target))
}

func TestHandlerUsesFullKeyringProfileButNeverExtendsLiteralExpirationBySkew(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		expiresAt func(time.Time) time.Time
		peerType  string
		multiple  bool
		accepted  bool
	}{
		{name: "live Compute token", expiresAt: func(now time.Time) time.Time { return now.Add(10 * time.Second) }, peerType: authpkg.P2PPeerTypeCompute, accepted: true},
		{name: "live User token", expiresAt: func(now time.Time) time.Time { return now.Add(10 * time.Second) }, peerType: authpkg.P2PPeerTypeUser, accepted: true},
		{name: "live App token", expiresAt: func(now time.Time) time.Time { return now.Add(10 * time.Second) }, peerType: authpkg.P2PPeerTypeApp, accepted: true},
		{name: "live Robot token with multiple signed Domains", expiresAt: func(now time.Time) time.Time { return now.Add(10 * time.Second) }, peerType: authpkg.P2PPeerTypeRobot, multiple: true, accepted: true},
		{name: "live Domain Server token with multiple signed Domains", expiresAt: func(now time.Time) time.Time { return now.Add(10 * time.Second) }, peerType: authpkg.P2PPeerTypeDomainServer, multiple: true, accepted: true},
		{name: "literal expiration wins over verifier skew", expiresAt: func(now time.Time) time.Time { return now }, peerType: authpkg.P2PPeerTypeCompute, accepted: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newHandlerFixture(t)
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			ring, err := verification.NewRing(staticKeyFetcher{material: verification.Material{
				PublicKey: publicKey,
				Method:    jwt.SigningMethodEdDSA,
			}}, verification.Options{
				ExpectedMethod:       jwt.SigningMethodEdDSA.Alg(),
				PreviousKeyOverlap:   31 * time.Minute,
				MaxKeyStaleness:      time.Hour,
				UnknownRefreshPeriod: time.Minute,
				Now:                  func() time.Time { return fixture.now },
			})
			require.NoError(t, err)
			require.NoError(t, ring.Preload(context.Background()))
			fixture.installHandler(t, ring, 4, 4)

			expiresAt := testCase.expiresAt(fixture.now)
			issuedAt := expiresAt.Add(-authpkg.P2PAccessTokenTTL)
			domains := []string{fixture.domain.String()}
			if testCase.multiple {
				domains = []string{uuid.NewString(), fixture.domain.String()}
			}
			claims := authpkg.P2PAccessTokenClaims{
				PeerType:  testCase.peerType,
				PeerID:    fixture.client.ID().String(),
				DomainIDs: domains,
				Scopes:    []string{authpkg.P2PAccessTokenScope},
				TypedClaims: tokenclaims.TypedClaims{
					Type: authpkg.P2PAccessTokenType,
					RegisteredClaims: jwt.RegisteredClaims{
						Issuer:    authpkg.P2PAccessTokenIssuer,
						Subject:   uuid.New().String(),
						Audience:  jwt.ClaimStrings{authpkg.P2PAccessTokenAudience},
						IssuedAt:  jwt.NewNumericDate(issuedAt),
						ExpiresAt: jwt.NewNumericDate(expiresAt),
					},
				},
			}
			token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(privateKey)
			require.NoError(t, err)
			actual := exchangePayload(t, fixture.client, fixture.server.ID(), encodeRequest(t, fixture.domain, fixture.target, token))
			if !testCase.accepted {
				require.Equal(t, deniedResponse, actual)
				return
			}
			var result response
			require.NoError(t, json.Unmarshal(actual, &result))
			require.True(t, result.Accepted)
			require.Equal(t, expiresAt.Format(time.RFC3339Nano), result.AcceptedUntil)
		})
	}
}

func TestHandlerUsesOneGenericDenialForAuthorizationFailures(t *testing.T) {
	fixture := newHandlerFixture(t)
	var literalExpiry atomic.Int64
	literalExpiry.Store(fixture.now.Add(time.Minute).UnixNano())
	claimedPeer := atomic.Value{}
	claimedPeer.Store(fixture.client.ID().String())
	claimedDomain := atomic.Value{}
	claimedDomain.Store(fixture.domain.String())
	fixture.installHandler(t, verifierFunc(func(_ context.Context, _ string, _ time.Time, _ time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
		return &authpkg.P2PAccessTokenClaims{
			PeerID:    claimedPeer.Load().(string),
			DomainIDs: []string{claimedDomain.Load().(string)},
		}, time.Unix(0, literalExpiry.Load()).UTC(), nil
	}), 4, 16)

	wrongDomain := uuid.New()
	cases := []struct {
		name    string
		prepare func()
		payload []byte
	}{
		{name: "unbooked target", payload: encodeRequest(t, fixture.domain, newHandlerPeer(t), "token")},
		{name: "wrong requested Domain", payload: encodeRequest(t, wrongDomain, fixture.target, "token")},
		{
			name: "copied token Peer ID",
			prepare: func() {
				claimedPeer.Store(newHandlerPeer(t).String())
			},
			payload: encodeRequest(t, fixture.domain, fixture.target, "token"),
		},
		{
			name: "signed Domain mismatch",
			prepare: func() {
				claimedPeer.Store(fixture.client.ID().String())
				claimedDomain.Store(wrongDomain.String())
			},
			payload: encodeRequest(t, fixture.domain, fixture.target, "token"),
		},
		{
			name: "literal expiration despite verifier leeway",
			prepare: func() {
				claimedDomain.Store(fixture.domain.String())
				literalExpiry.Store(fixture.now.UnixNano())
			},
			payload: encodeRequest(t, fixture.domain, fixture.target, "token"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.prepare != nil {
				testCase.prepare()
			}
			actual := exchangePayload(t, fixture.client, fixture.server.ID(), testCase.payload)
			require.Equal(t, deniedResponse, actual)
		})
	}
}

func TestHandlerRejectsInvalidFrameAndJSONVectorsGenerically(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.installHandler(t, verifierFunc(func(context.Context, string, time.Time, time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
		return nil, time.Time{}, errors.New("must not reach verifier")
	}), 4, 32)
	valid := string(encodeRequest(t, fixture.domain, fixture.target, "token"))
	vectors := []struct {
		name  string
		frame []byte
	}{
		{name: "zero length", frame: []byte{0, 0, 0, 0}},
		{name: "oversized", frame: lengthHeader(RequestMaximumBytes + 1)},
		{name: "truncated", frame: append(lengthHeader(20), []byte(`{"version":1}`)...)},
		{name: "invalid UTF8", frame: framed([]byte{0xff})},
		{name: "unknown field", frame: framed([]byte(strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1)))},
		{name: "missing field", frame: framed([]byte(`{"version":1}`))},
		{name: "duplicate field", frame: framed([]byte(strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1)))},
		{name: "unknown version", frame: framed([]byte(strings.Replace(valid, `"version":1`, `"version":2`, 1)))},
		{name: "trailing JSON", frame: framed(append([]byte(valid), []byte(`{}`)...))},
		{name: "trailing whitespace", frame: framed(append([]byte(valid), ' '))},
	}
	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			actual := exchangeRawFrame(t, fixture.client, fixture.server.ID(), vector.frame)
			require.Equal(t, deniedResponse, actual)
			require.LessOrEqual(t, len(actual), ResponseMaximumBytes)
		})
	}
}

func TestHandlerBoundsConcurrentVerificationWithoutWaitingForSlot(t *testing.T) {
	fixture := newHandlerFixture(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	fixture.installHandler(t, verifierFunc(func(ctx context.Context, _ string, _ time.Time, _ time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, time.Time{}, ctx.Err()
		}
		return &authpkg.P2PAccessTokenClaims{PeerID: fixture.client.ID().String(), DomainIDs: []string{fixture.domain.String()}}, fixture.now.Add(time.Minute), nil
	}), 1, 8)
	payload := encodeRequest(t, fixture.domain, fixture.target, "token")
	firstResponse := make(chan []byte, 1)
	go func() {
		firstResponse <- exchangePayload(t, fixture.client, fixture.server.ID(), payload)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first verification did not start")
	}
	require.Equal(t, deniedResponse, exchangePayload(t, fixture.client, fixture.server.ID(), payload))
	require.EqualValues(t, 1, calls.Load(), "busy streams must not enter JWT verification")
	close(release)
	select {
	case actual := <-firstResponse:
		var result response
		require.NoError(t, json.Unmarshal(actual, &result))
		require.True(t, result.Accepted)
	case <-time.After(5 * time.Second):
		t.Fatal("first verification did not complete")
	}
}

func TestDecodeRequestRequiresExactlyOneCanonicalObject(t *testing.T) {
	target := newHandlerPeer(t)
	domain := uuid.New()
	payload := encodeRequest(t, domain, target, "token")
	request, err := decodeRequest(payload)
	require.NoError(t, err)
	require.Equal(t, 1, request.Version)
	require.Equal(t, domain, request.DomainID)
	require.Equal(t, target, request.TargetPeerID)
	require.Equal(t, "token", request.P2PAccessToken)

	_, err = decodeRequest(append(payload, '\n'))
	require.ErrorContains(t, err, "trailing")
	_, err = decodeRequest([]byte(`[]`))
	require.Error(t, err)
}

func encodeRequest(t *testing.T, domain uuid.UUID, target peer.ID, token string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"version":          1,
		"domain_id":        domain.String(),
		"target_peer_id":   target.String(),
		"p2p_access_token": token,
	})
	require.NoError(t, err)
	return payload
}

func exchangePayload(t *testing.T, client host.Host, server peer.ID, payload []byte) []byte {
	t.Helper()
	return exchangeRawFrame(t, client, server, framed(payload))
}

func exchangeRawFrame(t *testing.T, client host.Host, server peer.ID, frame []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.NewStream(ctx, server, ProtocolID)
	require.NoError(t, err)
	defer stream.Close()
	require.NoError(t, stream.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = io.Copy(stream, bytes.NewReader(frame))
	require.NoError(t, err)
	require.NoError(t, stream.CloseWrite())
	var header [4]byte
	_, err = io.ReadFull(stream, header[:])
	require.NoError(t, err)
	length := binary.BigEndian.Uint32(header[:])
	require.NotZero(t, length)
	require.LessOrEqual(t, length, uint32(ResponseMaximumBytes))
	payload := make([]byte, int(length))
	_, err = io.ReadFull(stream, payload)
	require.NoError(t, err)
	var trailing [1]byte
	count, err := stream.Read(trailing[:])
	require.Zero(t, count)
	require.ErrorIs(t, err, io.EOF, "relay auth must close after exactly one response")
	return payload
}

func framed(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}

func lengthHeader(length int) []byte {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(length))
	return header[:]
}
