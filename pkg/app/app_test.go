package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/aukilabs/service-lib/pkg/tokens"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

type scriptedDDS struct {
	mu      sync.Mutex
	access  ddsclient.Access
	err     error
	calls   int
	started chan struct{}
	release chan struct{}
}

type orderedProviderDMS struct {
	mu      sync.Mutex
	events  []string
	session dmsclient.Session
}

func (d *orderedProviderDMS) record(event string) {
	d.mu.Lock()
	d.events = append(d.events, event)
	d.mu.Unlock()
}

func (d *orderedProviderDMS) Active(context.Context, string, uuid.UUID) ([]dmsclient.Assignment, error) {
	d.record("active")
	return nil, nil
}

func (d *orderedProviderDMS) Claim(context.Context, string, uuid.UUID) (*dmsclient.ClaimResult, error) {
	d.record("claim")
	return &dmsclient.ClaimResult{RetryAfter: time.Second}, nil
}

func (*orderedProviderDMS) Recover(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected recover")
}

func (*orderedProviderDMS) Heartbeat(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected heartbeat")
}

func (*orderedProviderDMS) Ready(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID, dmsclient.Metadata) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected ready")
}

func (*orderedProviderDMS) Fail(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected fail")
}

func (*orderedProviderDMS) Relinquish(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error) {
	return nil, errors.New("unexpected relinquish")
}

func (d *orderedProviderDMS) ReportStatus(_ context.Context, _ string, _ uuid.UUID, _ dmsclient.Expectations, status dmsclient.ProviderStatus) (*dmsclient.Session, error) {
	if status.AcceptingBookings {
		d.record("status:true")
	} else {
		d.record("status:false")
	}
	result := d.session
	result.Status = status
	return &result, nil
}

func (d *orderedProviderDMS) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

func (s *scriptedDDS) Authenticate(ctx context.Context, _ ddsclient.AuthenticateInput) (*ddsclient.Access, error) {
	s.mu.Lock()
	s.calls++
	result := s.access
	err := s.err
	started := s.started
	release := s.release
	s.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *scriptedDDS) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type recordingDMS struct {
	mu       sync.Mutex
	tokens   []string
	reported chan struct{}
}

func (*recordingDMS) OpenSession(context.Context, dmsclient.OpenInput) (*dmsclient.OpenResult, error) {
	panic("not used by the control-loop test")
}

func (d *recordingDMS) ReportNotAccepting(_ context.Context, token string, sessionID uuid.UUID, expected dmsclient.Expectations) (*dmsclient.Session, error) {
	d.mu.Lock()
	d.tokens = append(d.tokens, token)
	d.mu.Unlock()
	select {
	case d.reported <- struct{}{}:
	default:
	}
	return &dmsclient.Session{
		ProviderSessionID:        sessionID,
		EffectiveCapacity:        32,
		SchedulingRevision:       expected.SchedulingRevision,
		SessionExpiresAt:         time.Now().UTC().Add(3 * time.Minute),
		ProviderNodeJWTExpiresAt: expected.NodeTokenExpiresAt,
		StatusTTL:                3 * time.Minute,
		ProviderLeaseTTL:         3 * time.Minute,
		RecoveryGrace:            5 * time.Minute,
		Metadata:                 expected.Metadata,
	}, nil
}

type scriptedOpenDMS struct {
	mu          sync.Mutex
	openInputs  []dmsclient.OpenInput
	openErrors  []error
	openResult  dmsclient.OpenResult
	statusCalls int
}

func (d *scriptedOpenDMS) OpenSession(_ context.Context, input dmsclient.OpenInput) (*dmsclient.OpenResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.openInputs = append(d.openInputs, input)
	index := len(d.openInputs) - 1
	if index < len(d.openErrors) && d.openErrors[index] != nil {
		return nil, d.openErrors[index]
	}
	result := d.openResult
	return &result, nil
}

func (d *scriptedOpenDMS) ReportNotAccepting(_ context.Context, _ string, _ uuid.UUID, expected dmsclient.Expectations) (*dmsclient.Session, error) {
	d.mu.Lock()
	d.statusCalls++
	d.mu.Unlock()
	result := d.openResult.Session
	result.SchedulingRevision = expected.SchedulingRevision
	result.ProviderNodeJWTExpiresAt = expected.NodeTokenExpiresAt
	return &result, nil
}

func (*scriptedOpenDMS) ReleaseSession(context.Context, string, uuid.UUID) error { return nil }

func (d *scriptedOpenDMS) snapshot() ([]dmsclient.OpenInput, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dmsclient.OpenInput(nil), d.openInputs...), d.statusCalls
}

func TestOpenControlPlaneRetriesLostOpenResponseWithSameBootNonce(t *testing.T) {
	now := time.Now().UTC()
	access := ddsclient.Access{
		Token:              "peer-bound-token",
		IssuedAt:           now,
		ExpiresAt:          now.Add(time.Hour),
		SchedulingRevision: 7,
	}
	sessionID := uuid.New()
	dms := &scriptedOpenDMS{
		openErrors: []error{errors.New("lost response after DMS commit")},
		openResult: dmsclient.OpenResult{
			Outcome: dmsclient.OpenReplayed,
			Session: dmsclient.Session{
				ProviderSessionID:        sessionID,
				EffectiveCapacity:        32,
				SchedulingRevision:       access.SchedulingRevision,
				SessionExpiresAt:         now.Add(3 * time.Minute),
				ProviderNodeJWTExpiresAt: access.ExpiresAt,
			},
		},
	}
	cfg := config.Defaults()
	cfg.Retry.InitialBackoff = time.Millisecond
	application := newControlPlaneTestApplication(cfg, access, dms)

	require.NoError(t, application.openControlPlane(context.Background()))
	inputs, statusCalls := dms.snapshot()
	require.Len(t, inputs, 2)
	require.Equal(t, application.bootNonce, inputs[0].BootNonce)
	require.Equal(t, inputs[0].BootNonce, inputs[1].BootNonce)
	require.Equal(t, inputs[0].Expected, inputs[1].Expected)
	require.Equal(t, 1, statusCalls)
}

func TestOpenControlPlaneDoesNotRetryTerminalDMSConflict(t *testing.T) {
	now := time.Now().UTC()
	access := ddsclient.Access{Token: "peer-bound-token", IssuedAt: now, ExpiresAt: now.Add(time.Hour), SchedulingRevision: 7}
	dms := &scriptedOpenDMS{
		openErrors: []error{&dmsclient.APIError{StatusCode: http.StatusConflict, Code: "session_conflict", Message: "different boot nonce"}},
	}
	application := newControlPlaneTestApplication(config.Defaults(), access, dms)

	err := application.openControlPlane(context.Background())
	require.ErrorContains(t, err, "session_conflict")
	inputs, statusCalls := dms.snapshot()
	require.Len(t, inputs, 1)
	require.Zero(t, statusCalls)
}

func TestEffectiveCapacityUsesSignedDDSOverrideDistinctFromLocalCapacity(t *testing.T) {
	cfg := config.Defaults()
	application := newControlPlaneTestApplication(cfg, ddsclient.Access{}, &scriptedOpenDMS{})
	signed := 24

	effective, err := application.expectedEffectiveCapacity(&signed)
	require.NoError(t, err)
	require.Equal(t, signed, effective)
	require.NoError(t, application.validateEffectiveCapacity(signed, &signed))
	require.NoError(t, application.validateEffectiveCapacity(1, &signed), "DMS may impose a lower ceiling")
	require.ErrorContains(t, application.validateEffectiveCapacity(0, &signed), "DDS-authorized range")
	require.ErrorContains(t, application.validateEffectiveCapacity(cfg.Relay.LocalCapacity, &signed), "DDS-authorized range")
}

func TestLiveRecoveryGraceMustStrictlyExceedConfiguredDrainBeforeAccepting(t *testing.T) {
	cfg := config.Defaults()
	cfg.AcceptBookings = true
	cfg.ShutdownDrain = 5 * time.Minute
	application := &Application{config: cfg}

	require.ErrorContains(t, application.validateLiveProviderTiming(dmsclient.Session{
		RecoveryGrace: 5 * time.Minute,
	}), "must exceed")
	require.NoError(t, application.validateLiveProviderTiming(dmsclient.Session{
		RecoveryGrace: 5*time.Minute + time.Second,
	}))
}

func TestProviderReconcilesBeforeAcceptingAndClaiming(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		gate       bool
		wantStatus bool
		wantClaim  bool
	}{
		{name: "operator gate enabled", gate: true, wantStatus: true, wantClaim: true},
		{name: "operator gate disabled", gate: false, wantStatus: false, wantClaim: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now().UTC()
			signedCapacity := 1
			metadata := dmsclient.Metadata{
				BaseAddresses: []string{"/dns4/relay-a.interop.aukiverse.com/tcp/443/p2p/12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP"},
				EndpointKeys:  []string{"/dns4/relay-a.interop.aukiverse.com/tcp/443"},
				Limits:        dmsclient.Limits{DurationSeconds: 900, DataBytesPerDirection: 1}, ConfigFingerprint: "relay-config-v1|test",
			}
			session := dmsclient.Session{
				ProviderSessionID: uuid.New(), EffectiveCapacity: 1, SchedulingRevision: 7,
				SessionExpiresAt: now.Add(2 * time.Minute), ProviderNodeJWTExpiresAt: now.Add(3 * time.Minute),
				RecoveryGrace: 30 * time.Minute, Metadata: metadata,
			}
			dms := &orderedProviderDMS{session: session}
			registry, err := booking.New(booking.Config{MaximumBookings: 1, MaximumAdmissions: 8, AdmissionTTL: 30 * time.Second})
			require.NoError(t, err)
			worker, err := provider.New(provider.Options{
				DMS: dms, Registry: registry, Metadata: metadata, ClosePeer: func(peer.ID) error { return nil },
				Config: provider.Config{
					InitialBackoff: time.Millisecond, MaximumBackoff: 10 * time.Millisecond,
					EmptyClaimMaxJitterFraction: 0.2, HeartbeatMinimumFraction: 0.25, HeartbeatMaximumFraction: 0.35,
					ExpirySweepInterval: 10 * time.Millisecond, Random: func() float64 { return 0 }, Now: time.Now,
				},
			})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			cfg := config.Defaults()
			cfg.AcceptBookings = testCase.gate
			application := &Application{
				config: cfg, state: admin.NewState(), providerDMS: dms, worker: worker,
				context: ctx, cancel: cancel, deadlineChanged: make(chan struct{}, 1), metadata: metadata,
				capacity:              dmsclient.LocalCapacity{Total: 1, PerIP: 1, PerASN: 1},
				controlReady:          true,
				keyReady:              true,
				keyExpiresAt:          now.Add(time.Hour),
				tokenExpiresAt:        session.ProviderNodeJWTExpiresAt,
				sessionExpiresAt:      session.SessionExpiresAt,
				sessionTokenExpiresAt: session.ProviderNodeJWTExpiresAt,
				access: ddsclient.Access{
					Token: "peer-bound-token", IssuedAt: now, ExpiresAt: session.ProviderNodeJWTExpiresAt,
					MaxConcurrency: &signedCapacity, SchedulingRevision: 7,
				},
				session: session,
			}
			require.NoError(t, application.startProvider(context.Background()))
			if testCase.wantClaim {
				require.Eventually(t, func() bool { return len(dms.snapshot()) >= 3 }, time.Second, time.Millisecond)
			}
			events := dms.snapshot()
			require.NotEmpty(t, events)
			require.Equal(t, "active", events[0])
			if testCase.wantStatus {
				require.Contains(t, events, "status:true")
				require.Less(t, slices.Index(events, "active"), slices.Index(events, "status:true"))
			}
			if testCase.wantClaim {
				require.Contains(t, events, "claim")
				require.Less(t, slices.Index(events, "status:true"), slices.Index(events, "claim"))
			} else {
				require.NotContains(t, events, "status:true")
				require.NotContains(t, events, "claim")
			}
			cancel()
			require.NoError(t, worker.Shutdown(context.Background()))
		})
	}
}

func newControlPlaneTestApplication(cfg config.Config, access ddsclient.Access, dms dmsAPI) *Application {
	return &Application{
		config:          cfg,
		state:           admin.NewState(),
		dds:             &scriptedDDS{access: access},
		dms:             dms,
		bootNonce:       uuid.New(),
		deadlineChanged: make(chan struct{}, 1),
		metadata: dmsclient.Metadata{
			BaseAddresses:     []string{"/dns4/relay-a.interop.aukiverse.com/tcp/443/p2p/12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP"},
			EndpointKeys:      []string{"/dns4/relay-a.interop.aukiverse.com/tcp/443"},
			Limits:            dmsclient.Limits{DurationSeconds: 900, DataBytesPerDirection: 1},
			ConfigFingerprint: "relay-config-v1|test",
		},
		capacity: dmsclient.LocalCapacity{Total: 32, PerIP: 32, PerASN: 32},
	}
}

type blockingStatusDMS struct {
	started chan struct{}
	release chan struct{}
	now     time.Time
}

func (*blockingStatusDMS) OpenSession(context.Context, dmsclient.OpenInput) (*dmsclient.OpenResult, error) {
	panic("not used")
}

func (d *blockingStatusDMS) ReportNotAccepting(_ context.Context, _ string, sessionID uuid.UUID, expected dmsclient.Expectations) (*dmsclient.Session, error) {
	close(d.started)
	<-d.release
	return &dmsclient.Session{
		ProviderSessionID:        sessionID,
		EffectiveCapacity:        32,
		SchedulingRevision:       expected.SchedulingRevision,
		SessionExpiresAt:         d.now.Add(time.Hour),
		ProviderNodeJWTExpiresAt: expected.NodeTokenExpiresAt,
	}, nil
}

func (*blockingStatusDMS) ReleaseSession(context.Context, string, uuid.UUID) error { return nil }

type cancelAwareStatusDMS struct {
	started chan struct{}
}

func (*cancelAwareStatusDMS) OpenSession(context.Context, dmsclient.OpenInput) (*dmsclient.OpenResult, error) {
	panic("not used")
}

func (d *cancelAwareStatusDMS) ReportNotAccepting(ctx context.Context, _ string, _ uuid.UUID, _ dmsclient.Expectations) (*dmsclient.Session, error) {
	close(d.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*cancelAwareStatusDMS) ReleaseSession(context.Context, string, uuid.UUID) error { return nil }

func TestControlLoopTreatsCancellationAsCleanShutdown(t *testing.T) {
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	dms := &cancelAwareStatusDMS{started: make(chan struct{})}
	application := newControlPlaneTestApplication(config.Defaults(), ddsclient.Access{}, dms)
	application.config.Timing.StatusInterval = time.Millisecond
	application.context = ctx
	application.cancel = cancel
	application.errors = make(chan error, 1)
	application.access = ddsclient.Access{
		Token:              "peer-bound-token",
		IssuedAt:           now,
		ExpiresAt:          now.Add(time.Hour),
		SchedulingRevision: 7,
	}
	application.session = dmsclient.Session{ProviderSessionID: uuid.New()}

	done := make(chan struct{})
	go func() {
		application.controlLoop()
		close(done)
	}()
	<-dms.started
	cancel()
	<-done
	select {
	case err := <-application.errors:
		t.Fatalf("normal cancellation was reported as fatal: %v", err)
	default:
	}
}

func TestFatalReadinessRemainsDownAfterLateSuccessfulStatus(t *testing.T) {
	now := time.Now().UTC()
	dms := &blockingStatusDMS{started: make(chan struct{}), release: make(chan struct{}), now: now}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	application := newControlPlaneTestApplication(config.Defaults(), ddsclient.Access{}, dms)
	application.context = ctx
	application.cancel = cancel
	application.errors = make(chan error, 1)
	application.access = ddsclient.Access{
		Token:              "peer-bound-token",
		IssuedAt:           now,
		ExpiresAt:          now.Add(time.Hour),
		SchedulingRevision: 7,
	}
	application.session = dmsclient.Session{ProviderSessionID: uuid.New()}
	application.setKeyReadyUntil(true, now.Add(time.Hour))
	application.setTokenExpiresAt(now.Add(time.Hour))
	application.setSessionDeadlines(dmsclient.Session{
		SessionExpiresAt:         now.Add(time.Hour),
		ProviderNodeJWTExpiresAt: now.Add(time.Hour),
	})
	application.setDataPlaneReady(true)
	application.setControlReady(true)
	require.True(t, application.state.Ready())

	done := make(chan error, 1)
	go func() {
		err := application.reportStatusWithRetry()
		if err == nil {
			application.setControlReady(true)
		}
		done <- err
	}()
	<-dms.started
	application.reportFatal(errors.New("fatal server failure"))
	close(dms.release)
	require.NoError(t, <-done)
	require.False(t, application.state.Ready())
}

func TestAuthorityDeadlinesDisableAcceptanceWithoutFlappingDataPlaneReadiness(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Application, time.Time)
	}{
		{name: "retained key", mutate: func(a *Application, deadline time.Time) { a.keyExpiresAt = deadline }},
		{name: "Node token", mutate: func(a *Application, deadline time.Time) { a.tokenExpiresAt = deadline }},
		{name: "DMS session", mutate: func(a *Application, deadline time.Time) { a.sessionExpiresAt = deadline }},
		{name: "DMS session token authority", mutate: func(a *Application, deadline time.Time) { a.sessionTokenExpiresAt = deadline }},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			ctx, cancel := context.WithCancel(context.Background())
			cfg := config.Defaults()
			cfg.AcceptBookings = true
			worker := newDeadlineTestWorker(t)
			worker.SetAccepting(true)
			application := &Application{
				config:                cfg,
				state:                 admin.NewState(),
				worker:                worker,
				context:               ctx,
				deadlineChanged:       make(chan struct{}, 1),
				dataPlaneReady:        true,
				startupComplete:       true,
				controlReady:          true,
				keyReady:              true,
				keyExpiresAt:          now.Add(time.Hour),
				tokenExpiresAt:        now.Add(time.Hour),
				sessionExpiresAt:      now.Add(time.Hour),
				sessionTokenExpiresAt: now.Add(time.Hour),
			}
			deadline := now.Add(40 * time.Millisecond)
			test.mutate(application, deadline)
			application.gateMu.Lock()
			application.updateReadinessLocked(now)
			application.gateMu.Unlock()
			require.True(t, application.state.Ready())

			done := make(chan struct{})
			go func() {
				application.deadlineLoop()
				close(done)
			}()
			require.Eventually(t, func() bool {
				return application.state.Ready() && !worker.Accepting() &&
					application.state.Snapshot().Scheduling.State == admin.AcceptingStateControlStale
			}, time.Second, time.Millisecond)
			application.gateMu.Lock()
			application.updateReadinessLocked(deadline)
			application.gateMu.Unlock()
			require.True(t, application.state.Ready(), "control authority expiry must not withdraw a healthy data plane")
			cancel()
			<-done
		})
	}
}

func newDeadlineTestWorker(t *testing.T) *provider.Worker {
	t.Helper()
	metadata := dmsclient.Metadata{
		BaseAddresses: []string{
			"/dns4/relay-a.interop.aukiverse.com/tcp/443/p2p/12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP",
		},
		EndpointKeys:      []string{"/dns4/relay-a.interop.aukiverse.com/tcp/443"},
		Limits:            dmsclient.Limits{DurationSeconds: 900, DataBytesPerDirection: 1},
		ConfigFingerprint: "relay-config-v1|test",
	}
	registry, err := booking.New(booking.Config{
		MaximumBookings: 1, MaximumAdmissions: 2, AdmissionTTL: 30 * time.Second,
	})
	require.NoError(t, err)
	worker, err := provider.New(provider.Options{
		DMS: &orderedProviderDMS{}, Registry: registry, Metadata: metadata,
		ClosePeer: func(peer.ID) error { return nil },
		Config: provider.Config{
			InitialBackoff: time.Millisecond, MaximumBackoff: 10 * time.Millisecond,
			EmptyClaimMaxJitterFraction: 0.20,
			HeartbeatMinimumFraction:    0.25, HeartbeatMaximumFraction: 0.35,
			ExpirySweepInterval: time.Second, Random: func() float64 { return 0 }, Now: time.Now,
		},
	})
	require.NoError(t, err)
	return worker
}

func TestDeadlineLoopClearsAcceptanceWhenDeadlineAlreadyExpiredAtEntry(t *testing.T) {
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	application := &Application{
		config:                func() config.Config { cfg := config.Defaults(); cfg.AcceptBookings = true; return cfg }(),
		state:                 admin.NewState(),
		context:               ctx,
		deadlineChanged:       make(chan struct{}, 1),
		dataPlaneReady:        true,
		startupComplete:       true,
		controlReady:          true,
		keyReady:              true,
		keyExpiresAt:          now.Add(-time.Millisecond),
		tokenExpiresAt:        now.Add(time.Hour),
		sessionExpiresAt:      now.Add(time.Hour),
		sessionTokenExpiresAt: now.Add(time.Hour),
	}
	application.state.SetReady(true) // Simulate the narrow setter-to-loop race.

	done := make(chan struct{})
	go func() {
		application.deadlineLoop()
		close(done)
	}()
	require.Eventually(t, func() bool {
		snapshot := application.state.Snapshot()
		return snapshot.Ready && snapshot.Scheduling.State == admin.AcceptingStateControlStale
	}, time.Second, time.Millisecond)
	cancel()
	<-done
}

func TestStartRejectsInvalidVersionBeforeDDSKeyNetworkIO(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	cfg := config.Defaults()
	cfg.AllowLocalTestHTTP = true
	cfg.DDSURL = server.URL
	cfg.DMSURL = server.URL
	cfg.DDSPublicKeyURL = server.URL
	cfg.Secrets = config.SecretFiles{
		RegistrationCredentials: "/missing/registration",
		WalletPrivateKey:        "/missing/wallet",
		Libp2pPrivateKey:        "/missing/libp2p",
	}
	cfg.PublicBaseMultiaddrs = []string{"configured-and-validated-after-version"}

	application, err := Start(context.Background(), cfg, "dev", nil)
	require.ErrorContains(t, err, "semantic version")
	require.Nil(t, application, "pre-construction failures transfer no cleanup ownership")
	require.Zero(t, requests)
}

type ownedStartupIdentity struct {
	secrets config.SecretFiles
	peerID  peer.ID
}

func newOwnedStartupIdentity(t *testing.T) ownedStartupIdentity {
	t.Helper()
	directory := t.TempDir()
	nodeID := uuid.NewString()
	credentials := base64.StdEncoding.EncodeToString([]byte(nodeID + ":registration-secret"))
	walletKey, err := ethcrypto.GenerateKey()
	require.NoError(t, err)
	peerKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(peerKey)
	require.NoError(t, err)
	encodedPeerKey, err := libp2pcrypto.MarshalPrivateKey(peerKey)
	require.NoError(t, err)

	write := func(name string, contents []byte) string {
		path := filepath.Join(directory, name)
		require.NoError(t, os.WriteFile(path, contents, 0o600))
		return path
	}
	return ownedStartupIdentity{
		secrets: config.SecretFiles{
			RegistrationCredentials: write("registration", []byte(credentials)),
			WalletPrivateKey:        write("wallet", []byte(hex.EncodeToString(ethcrypto.FromECDSA(walletKey)))),
			Libp2pPrivateKey:        write("libp2p", encodedPeerKey),
		},
		peerID: peerID,
	}
}

func unusedLoopbackAddresses(t *testing.T, count int) []string {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	addresses := make([]string, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		listeners = append(listeners, listener)
		addresses = append(addresses, listener.Addr().String())
	}
	for _, listener := range listeners {
		require.NoError(t, listener.Close())
	}
	return addresses
}

func TestStartTransfersPostConstructionFailureOwnershipToCaller(t *testing.T) {
	identity := newOwnedStartupIdentity(t)
	signingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	publicKeyPEM, err := tokens.CreatePublicKeyPEM(&signingKey.PublicKey)
	require.NoError(t, err)
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&signingKey.PublicKey)
	require.NoError(t, err)
	publicKeyFingerprint := sha256.Sum256(publicKeyDER)
	keyServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(response).Encode(map[string]any{
			"version": 1, "generation": 1, "previous_key_overlap_seconds": 1860,
			"keys": []map[string]any{{
				"id": hex.EncodeToString(publicKeyFingerprint[:]), "status": "current",
				"public_key": publicKeyPEM, "signing_method": "ES256",
			}},
		}))
	}))
	defer keyServer.Close()

	occupiedAdmin, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupiedAdmin.Close()
	addresses := unusedLoopbackAddresses(t, 3)
	tcpAddress, webSocketAddress, metricsAddress := addresses[0], addresses[1], addresses[2]
	_, tcpPort, err := net.SplitHostPort(tcpAddress)
	require.NoError(t, err)
	_, webSocketPort, err := net.SplitHostPort(webSocketAddress)
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.AllowLocalTestHTTP = true
	cfg.DDSURL = keyServer.URL
	cfg.DMSURL = keyServer.URL
	cfg.DDSPublicKeyURL = keyServer.URL
	cfg.Secrets = identity.secrets
	cfg.TCPListenMultiaddr = "/ip4/127.0.0.1/tcp/" + tcpPort
	cfg.WebSocketListenMultiaddr = "/ip4/127.0.0.1/tcp/" + webSocketPort + "/ws"
	cfg.PublicBaseMultiaddrs = []string{
		"/dns4/relay.startup.aukiverse.com/tcp/" + tcpPort + "/p2p/" + identity.peerID.String(),
		"/dns4/relay.startup.aukiverse.com/tcp/" + webSocketPort + "/wss/p2p/" + identity.peerID.String(),
	}
	cfg.AdminAddress = occupiedAdmin.Addr().String()
	cfg.MetricsAddress = metricsAddress
	cfg.HTTP.RequestTimeout = 500 * time.Millisecond

	startupContext, cancelStartup := context.WithCancel(context.Background())
	application, startErr := Start(startupContext, cfg, "v0.0.0", slog.Default())
	require.ErrorContains(t, startErr, "listen on relay admin address")
	require.NotNil(t, application)
	require.False(t, application.closeStarted, "Start must transfer ownership without choosing an unplanned cleanup")
	require.NotNil(t, application.node)
	t.Cleanup(func() { _ = application.Close(context.Background()) })
	cancelStartup()
	select {
	case <-application.context.Done():
		t.Fatal("startup context cancellation escaped into the owned Application lifecycle")
	default:
	}

	closeContext, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	require.NoError(t, application.Close(closeContext))
	cancelClose()
	require.True(t, application.closeStarted)
	listener, listenErr := net.Listen("tcp", tcpAddress)
	require.NoError(t, listenErr, "caller cleanup leaked the partially started libp2p listener")
	require.NoError(t, listener.Close())
	webSocketListener, listenErr := net.Listen("tcp", webSocketAddress)
	require.NoError(t, listenErr, "caller cleanup leaked the partially started WebSocket listener")
	require.NoError(t, webSocketListener.Close())
}
func (*recordingDMS) ReleaseSession(context.Context, string, uuid.UUID) error {
	return nil
}

func (d *recordingDMS) lastToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.tokens) == 0 {
		return ""
	}
	return d.tokens[len(d.tokens)-1]
}

func TestControlLoopRefreshesAtSeventyFivePercentThenReportsWithNewAuthority(t *testing.T) {
	now := time.Now().UTC()
	initial := ddsclient.Access{
		Token:              "initial-token",
		IssuedAt:           now.Add(-80 * time.Millisecond),
		ExpiresAt:          now.Add(20 * time.Millisecond),
		SchedulingRevision: 4,
	}
	refreshed := ddsclient.Access{
		Token:              "refreshed-token",
		IssuedAt:           now,
		ExpiresAt:          now.Add(time.Hour),
		SchedulingRevision: 4,
	}
	dds := &scriptedDDS{access: refreshed}
	dms := &recordingDMS{reported: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config.Defaults()
	cfg.Timing.StatusInterval = time.Hour
	cfg.Timing.StatusMaxBackoff = time.Millisecond
	application := &Application{
		config:  cfg,
		state:   admin.NewState(),
		dds:     dds,
		dms:     dms,
		context: ctx,
		cancel:  cancel,
		errors:  make(chan error, 1),
		access:  initial,
		session: dmsclient.Session{ProviderSessionID: uuid.New()},
		metadata: dmsclient.Metadata{
			BaseAddresses:     []string{"/dns4/relay-a.interop.aukiverse.com/tcp/443/p2p/12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP"},
			EndpointKeys:      []string{"/dns4/relay-a.interop.aukiverse.com/tcp/443"},
			Limits:            dmsclient.Limits{DurationSeconds: 900, DataBytesPerDirection: 1},
			ConfigFingerprint: "relay-config-v1|test",
		},
		capacity: dmsclient.LocalCapacity{Total: 32, PerIP: 32, PerASN: 32},
		keyReady: true,
	}

	done := make(chan struct{})
	go func() {
		application.controlLoop()
		close(done)
	}()
	select {
	case <-dms.reported:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refreshed DMS provider status")
	}
	cancel()
	<-done

	require.Equal(t, 1, dds.callCount())
	require.Equal(t, "refreshed-token", dms.lastToken())
	application.currentMu.RLock()
	require.Equal(t, "refreshed-token", application.access.Token)
	application.currentMu.RUnlock()
}

func TestControlLoopDoesNotRenewStatusAfterScheduledTokenRefreshFailure(t *testing.T) {
	now := time.Now().UTC()
	dds := &scriptedDDS{err: errors.New("DDS unavailable")}
	dms := &recordingDMS{reported: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Defaults()
	cfg.Timing.StatusInterval = time.Millisecond
	cfg.Timing.StatusMaxBackoff = time.Millisecond
	application := &Application{
		config: cfg, logger: slog.Default(), state: admin.NewState(), dds: dds, dms: dms,
		context: ctx, cancel: cancel, errors: make(chan error, 1),
		access: ddsclient.Access{
			Token: "old-token", IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Minute),
		},
		session: dmsclient.Session{ProviderSessionID: uuid.New()},
	}

	done := make(chan struct{})
	go func() {
		application.controlLoop()
		close(done)
	}()
	require.Eventually(t, func() bool { return dds.callCount() >= 2 }, time.Second, time.Millisecond)
	select {
	case <-dms.reported:
		t.Fatal("status was renewed after the scheduled Node-token refresh failed")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestTerminalDMSControlErrorsRequireProviderReplacement(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusGone} {
		require.True(t, terminalDMSControlError(&dmsclient.APIError{StatusCode: status}), "status %d", status)
	}
	for _, err := range []error{
		errors.New("transport unavailable"),
		&dmsclient.APIError{StatusCode: http.StatusRequestTimeout},
		&dmsclient.APIError{StatusCode: http.StatusTooManyRequests},
		&dmsclient.APIError{StatusCode: http.StatusServiceUnavailable},
	} {
		require.False(t, terminalDMSControlError(err), "%v", err)
	}
}
