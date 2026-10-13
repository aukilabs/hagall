// Package app wires the standalone relay process. It deliberately depends on
// relay-local packages and neutral shared authentication models; no Domain
// Server controller, repository, database state, storage, or data-access path
// is used by this process.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aukilabs/hagall/pkg/admin"
	"github.com/aukilabs/hagall/pkg/admission"
	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/config"
	"github.com/aukilabs/hagall/pkg/ddsclient"
	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/aukilabs/hagall/pkg/identity"
	"github.com/aukilabs/hagall/pkg/node"
	"github.com/aukilabs/hagall/pkg/provider"
	"github.com/aukilabs/hagall/pkg/relayconfig"
	"github.com/aukilabs/hagall/pkg/telemetry"
	"github.com/aukilabs/hagall/pkg/verification"
	"github.com/aukilabs/service-lib/pkg/tokenissuer"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
)

const listenerHealthInterval = time.Second

type ddsAPI interface {
	Authenticate(context.Context, ddsclient.AuthenticateInput) (*ddsclient.Access, error)
}

type dmsAPI interface {
	OpenSession(context.Context, dmsclient.OpenInput) (*dmsclient.OpenResult, error)
	ReportNotAccepting(context.Context, string, uuid.UUID, dmsclient.Expectations) (*dmsclient.Session, error)
	ReleaseSession(context.Context, string, uuid.UUID) error
}

type providerDMSAPI interface {
	provider.DMS
	ReportStatus(context.Context, string, uuid.UUID, dmsclient.Expectations, dmsclient.ProviderStatus) (*dmsclient.Session, error)
}

type providerContractError struct{ err error }

func (e *providerContractError) Error() string {
	return "invalid DMS provider contract: " + e.err.Error()
}
func (e *providerContractError) Unwrap() error { return e.err }

// Application is one process incarnation. Its boot nonce, relay identity,
// immutable address snapshot, and DMS session remain fixed until Close.
type Application struct {
	config      config.Config
	logger      *slog.Logger
	state       *admin.State
	servers     *admin.Servers
	node        *node.Service
	ring        *verification.Ring
	dds         ddsAPI
	dms         dmsAPI
	providerDMS providerDMSAPI
	registry    *booking.Registry
	worker      *provider.Worker
	metrics     *telemetry.Metrics

	authInput ddsclient.AuthenticateInput
	metadata  dmsclient.Metadata
	capacity  dmsclient.LocalCapacity
	bootNonce uuid.UUID

	context         context.Context
	cancel          context.CancelFunc
	wait            sync.WaitGroup
	loopsOnce       sync.Once
	errors          chan error
	errOnce         sync.Once
	deadlineChanged chan struct{}
	controlStop     chan struct{}
	controlStopOnce sync.Once

	currentMu sync.RWMutex
	access    ddsclient.Access
	session   dmsclient.Session
	statusMu  sync.Mutex

	gateMu                sync.Mutex
	dataPlaneReady        bool
	controlReady          bool
	keyReady              bool
	terminal              bool
	draining              bool
	startupComplete       bool
	shutdownIntent        *string
	keyExpiresAt          time.Time
	tokenExpiresAt        time.Time
	sessionExpiresAt      time.Time
	sessionTokenExpiresAt time.Time

	closeMu       sync.Mutex
	closeStarted  bool
	localCloseErr error

	drainMu         sync.Mutex
	drainOperation  *applicationDrainOperation
	sessionReleased bool
	closing         bool
}

// Start validates and constructs every startup gate before reporting ready.
// The dynamic ACL starts empty and remains fail closed until DMS reconciliation
// installs exact provider-session/assignment/epoch authority.
//
// Ownership transfers as soon as a non-nil Application is returned. Errors
// before construction return a nil Application. Errors after construction
// return the partially started Application without closing it so the caller
// can select the planned signal-drain intent before cleanup. Every caller must
// therefore Close a non-nil Application even when err is non-nil.
func Start(parent context.Context, cfg config.Config, version string, logger *slog.Logger) (*Application, error) {
	if parent == nil {
		return nil, errors.New("relay application context is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := ddsclient.ValidateVersion(version); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	loadedIdentity, err := identity.Load(identity.Files{
		RegistrationCredentials: cfg.Secrets.RegistrationCredentials,
		WalletPrivateKey:        cfg.Secrets.WalletPrivateKey,
		Libp2pPrivateKey:        cfg.Secrets.Libp2pPrivateKey,
	})
	if err != nil {
		return nil, fmt.Errorf("load relay identity: %w", err)
	}
	canonical, err := relayconfig.Canonicalize(cfg.PublicBaseMultiaddrs, loadedIdentity.PeerID, cfg.Relay.Limits)
	if err != nil {
		return nil, fmt.Errorf("canonicalize relay addresses: %w", err)
	}
	listenAddress, err := ma.NewMultiaddr(cfg.TCPListenMultiaddr)
	if err != nil {
		return nil, fmt.Errorf("parse relay listen address: %w", err)
	}
	webSocketListenAddress, err := ma.NewMultiaddr(cfg.WebSocketListenMultiaddr)
	if err != nil {
		return nil, fmt.Errorf("parse relay WebSocket listen address: %w", err)
	}
	listenAddresses := []ma.Multiaddr{listenAddress, webSocketListenAddress}

	httpClient := &http.Client{Timeout: cfg.HTTP.RequestTimeout}
	ring, err := verification.NewRing(&verification.HTTPFetcher{
		URL:    cfg.DDSPublicKeyURL,
		Client: httpClient,
	}, verification.Options{
		ExpectedMethod:       cfg.VerificationKeys.SigningMethod,
		PreviousKeyOverlap:   cfg.VerificationKeys.PreviousKeyOverlap,
		MaxKeyStaleness:      cfg.VerificationKeys.MaxStaleness,
		UnknownRefreshPeriod: cfg.VerificationKeys.UnknownRefreshInterval,
	})
	if err != nil {
		return nil, err
	}
	keyRefreshStarted := time.Now().UTC()
	if err := ring.Preload(parent); err != nil {
		return nil, fmt.Errorf("preload DDS verification key: %w", err)
	}
	audience := strings.TrimRight(cfg.DDSURL, "/")
	nodeTokenVerifier, err := verification.NewNodeTokenVerifier(
		ring,
		tokenissuer.DDS,
		[]string{audience},
		cfg.VerificationKeys.ClockSkew,
		time.Now,
	)
	if err != nil {
		return nil, err
	}
	dds, err := ddsclient.New(ddsclient.Options{
		BaseURL:             cfg.DDSURL,
		HTTPClient:          httpClient,
		AllowHTTPForTesting: cfg.AllowLocalTestHTTP,
		Verifier:            nodeTokenVerifier,
	})
	if err != nil {
		return nil, err
	}
	dms, err := dmsclient.New(dmsclient.Options{
		Capacity:            cfg.Relay.LocalCapacity,
		BaseURL:             cfg.DMSURL,
		HTTPClient:          httpClient,
		AllowHTTPForTesting: cfg.AllowLocalTestHTTP,
		StatusInterval:      cfg.Timing.StatusInterval,
		MaxStatusBackoff:    cfg.Timing.StatusMaxBackoff,
	})
	if err != nil {
		return nil, err
	}

	// The parent bounds startup only. Once resources are constructed, their
	// lifecycle is owned by Application.Close so a signal can cancel a blocked
	// reconciliation/status request without killing recovered assignment
	// heartbeats, provider status maintenance, admin handlers, or drain itself.
	applicationContext, cancel := context.WithCancel(context.WithoutCancel(parent))
	bookingRegistry, err := booking.New(booking.Config{
		MaximumBookings:   cfg.Relay.LocalCapacity,
		MaximumAdmissions: cfg.Admission.MaximumEntries,
		AdmissionTTL:      cfg.Admission.TTL,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	authLimiter, err := admission.New(admission.Config{
		TTL:             admission.StreamIOTimeout,
		MaximumEntries:  max(2, 2*cfg.Admission.AuthConcurrency),
		Concurrency:     cfg.Admission.AuthConcurrency,
		AttemptsPerPeer: cfg.Admission.AttemptsPerPeer,
		AttemptsPerIP:   cfg.Admission.AttemptsPerIP,
		AttemptWindow:   cfg.Admission.AttemptWindow,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	authHandler, err := admission.NewHandler(admission.HandlerOptions{
		Context:   applicationContext,
		Limiter:   authLimiter,
		Registry:  bookingRegistry,
		Verifier:  ring,
		ClockSkew: cfg.VerificationKeys.ClockSkew,
	})
	if err != nil {
		cancel()
		return nil, err
	}

	metricsRegistry := prometheus.NewRegistry()
	state := admin.NewState()
	if err := state.RegisterMetrics(metricsRegistry); err != nil {
		cancel()
		return nil, err
	}
	relayMetrics, err := telemetry.New(telemetry.Options{
		Registerer:      metricsRegistry,
		ConfiguredSlots: cfg.Relay.LocalCapacity,
		UsedSlots: func() int {
			bookings, _ := bookingRegistry.Counts()
			return bookings
		},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	authHandler.SetObserver(relayMetrics)
	stockRelayMetrics := relayv2.NewMetricsTracer(relayv2.WithRegisterer(metricsRegistry))
	nodeService, err := node.New(node.Options{
		Identity:           loadedIdentity.Libp2pPrivateKey,
		ListenAddresses:    listenAddresses,
		Advertised:         canonical,
		PrometheusRegistry: metricsRegistry,
		ACL:                booking.NewACL(bookingRegistry, relayMetrics),
		RelayMetricsTracer: relayMetrics.RelayTracer(stockRelayMetrics),
		Resources: node.Resources{
			Capacity:                  cfg.Relay.LocalCapacity,
			MaxReservations:           cfg.Relay.MaxReservations,
			ReservationTTL:            cfg.Relay.ReservationTTL,
			MaxReservationsPerIP:      cfg.Relay.MaxReservationsPerIP,
			MaxReservationsPerASN:     cfg.Relay.MaxReservationsPerASN,
			MaxCircuitsPerPeer:        cfg.Relay.MaxCircuitsPerPeer,
			BufferBytes:               cfg.Relay.BufferSize,
			MemoryBytes:               cfg.ResourceManager.MemoryBytes,
			FileDescriptors:           cfg.ResourceManager.FileDescriptors,
			Connections:               cfg.ResourceManager.Connections,
			Streams:                   cfg.ResourceManager.Streams,
			ConnectionManagerLow:      cfg.ConnectionManager.LowWater,
			ConnectionManagerHigh:     cfg.ConnectionManager.HighWater,
			ConnectionManagerGrace:    time.Minute,
			OutboundTCPConnectTimeout: cfg.HTTP.RequestTimeout,
			RelayLimits:               cfg.Relay.Limits,
		},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	application := &Application{
		config:          cfg,
		logger:          logger,
		state:           state,
		node:            nodeService,
		ring:            ring,
		dds:             dds,
		dms:             dms,
		providerDMS:     dms,
		registry:        bookingRegistry,
		metrics:         relayMetrics,
		bootNonce:       uuid.New(),
		context:         applicationContext,
		cancel:          cancel,
		errors:          make(chan error, 1),
		deadlineChanged: make(chan struct{}, 1),
		controlStop:     make(chan struct{}),
		metadata: dmsclient.Metadata{
			BaseAddresses: canonical.Bases(),
			EndpointKeys:  canonical.EndpointKeys(),
			Limits: dmsclient.Limits{
				DurationSeconds:       canonical.Limits().DurationSeconds(),
				DataBytesPerDirection: canonical.Limits().DataBytesPerDirection,
			},
			ConfigFingerprint: canonical.Fingerprint(),
		},
		capacity: dmsclient.LocalCapacity{
			Total:  cfg.Relay.LocalCapacity,
			PerIP:  cfg.Relay.MaxReservationsPerIP,
			PerASN: cfg.Relay.MaxReservationsPerASN,
		},
	}
	var maxConcurrency *int
	if cfg.Relay.DDSMaxConcurrency != nil {
		value := *cfg.Relay.DDSMaxConcurrency
		maxConcurrency = &value
	}
	application.authInput = ddsclient.AuthenticateInput{
		RegistrationCredentials: loadedIdentity.RegistrationCredentials,
		Version:                 version,
		MaxConcurrency:          maxConcurrency,
		WalletPrivateKey:        loadedIdentity.WalletPrivateKey,
		PeerPrivateKey:          loadedIdentity.Libp2pPrivateKey,
	}
	application.worker, err = provider.New(provider.Options{
		DMS:      dms,
		Registry: bookingRegistry,
		Metadata: application.metadata,
		ClosePeer: func(target peer.ID) error {
			return nodeService.Host().Network().ClosePeer(target)
		},
		Config: provider.Config{
			InitialBackoff:              cfg.Retry.InitialBackoff,
			MaximumBackoff:              cfg.Retry.MaximumBackoff,
			EmptyClaimMaxJitterFraction: cfg.Retry.EmptyClaimMaxJitterFraction,
			HeartbeatMinimumFraction:    cfg.Timing.LeaseHeartbeatMinFraction,
			HeartbeatMaximumFraction:    cfg.Timing.LeaseHeartbeatMaxFraction,
			ExpirySweepInterval:         time.Second,
			Random:                      rand.Float64,
			Now:                         time.Now,
		},
		Observer: relayMetrics,
	})
	if err != nil {
		return application, err
	}
	nodeService.Host().SetStreamHandler(admission.ProtocolID, authHandler.HandleStream)
	servers, err := admin.Start(admin.Options{
		Context:        applicationContext,
		AdminAddress:   cfg.AdminAddress,
		MetricsAddress: cfg.MetricsAddress,
		State:          state,
		Registry:       metricsRegistry,
		RelayInfo:      application.relayInfo,
		Drain:          application.Drain,
	})
	if err != nil {
		return application, err
	}
	application.servers = servers

	if err := application.openControlPlane(parent); err != nil {
		return application, err
	}
	application.setKeyReadyUntil(ring.Ready(time.Now().UTC()), keyRefreshStarted.Add(cfg.VerificationKeys.MaxStaleness))
	application.setControlReady(true)
	if err := application.activateProviderLifecycle(); err != nil {
		return application, err
	}
	// From worker activation onward, partially recovered authority must retain
	// its heartbeat/status safety net even if the cancelable startup work below
	// is interrupted. The worker remains non-accepting until completion.
	application.startLoops()
	if err := application.completeProviderStartup(parent); err != nil {
		return application, err
	}
	return application, nil
}

func (a *Application) openControlPlane(ctx context.Context) error {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	access, err := a.dds.Authenticate(ctx, a.authInput)
	if err != nil {
		return fmt.Errorf("authenticate relay Node with DDS: %w", err)
	}
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	if _, err := a.expectedEffectiveCapacity(access.MaxConcurrency); err != nil {
		return err
	}
	if err := a.checkDataPlaneListeners(); err != nil {
		return fmt.Errorf("verify relay listeners before opening DMS provider session: %w", err)
	}
	expected := a.expectations(*access)
	opened, err := a.openSessionWithRetry(ctx, dmsclient.OpenInput{
		AccessToken: access.Token,
		BootNonce:   a.bootNonce,
		Expected:    expected,
	})
	if err != nil {
		return fmt.Errorf("open DMS relay provider session: %w", err)
	}
	a.currentMu.Lock()
	a.access = *access
	a.session = opened.Session
	a.currentMu.Unlock()
	a.setTokenExpiresAt(access.ExpiresAt)
	a.setSessionDeadlines(opened.Session)
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	if err := a.validateEffectiveCapacity(int(opened.Session.EffectiveCapacity), access.MaxConcurrency); err != nil {
		return err
	}
	status, err := a.dms.ReportNotAccepting(ctx, access.Token, opened.Session.ProviderSessionID, expected)
	if err != nil {
		return fmt.Errorf("report initial DMS relay provider status: %w", err)
	}
	if err := a.validateEffectiveCapacity(int(status.EffectiveCapacity), access.MaxConcurrency); err != nil {
		return err
	}
	a.currentMu.Lock()
	a.session = *status
	a.currentMu.Unlock()
	a.setSessionDeadlines(*status)
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	return nil
}

func (a *Application) startProvider(ctx context.Context) error {
	if err := a.activateProviderLifecycle(); err != nil {
		return err
	}
	return a.completeProviderStartup(ctx)
}

func (a *Application) activateProviderLifecycle() error {
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	a.currentMu.RLock()
	access := a.access
	session := a.session
	a.currentMu.RUnlock()
	// Starting fail closed is what lets the worker's expiry/heartbeat machinery
	// run before reconciliation without permitting a Claim.
	a.worker.SetAccepting(false)
	if err := a.worker.SetControl(access.Token, session); err != nil {
		return fmt.Errorf("install DMS provider authority: %w", err)
	}
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	if err := a.worker.Start(a.context); err != nil {
		return fmt.Errorf("start DMS relay booking worker: %w", err)
	}
	return nil
}

func (a *Application) completeProviderStartup(ctx context.Context) error {
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	if err := a.reconcileProviderWithRetry(ctx); err != nil {
		return fmt.Errorf("reconcile DMS relay assignments: %w", err)
	}
	if a.lifecycleStopping() {
		return errors.New("relay startup was interrupted by drain")
	}
	if err := a.checkDataPlaneListeners(); err != nil {
		return fmt.Errorf("verify relay listeners before accepting bookings: %w", err)
	}
	// Identity, both listeners, and authoritative child recovery are complete.
	// Readiness does not flap on transient DDS/DMS control-plane outages, but a
	// lost listener clears it and terminates this process incarnation.
	a.setDataPlaneReady(true)
	if !a.config.AcceptBookings {
		a.setStartupComplete(true)
		a.worker.SetAccepting(false)
		return nil
	}
	a.currentMu.RLock()
	session := a.session
	a.currentMu.RUnlock()
	if err := a.validateLiveProviderTiming(session); err != nil {
		return err
	}
	// Serialize the initial accepting transition with explicit drain status and
	// exact session release. beginDrain closes the local gates before waiting
	// here, so a drain that starts during this request always writes the final
	// DMS state and startup never reopens it afterward.
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if a.lifecycleStopping() || a.sessionWasReleased() {
		return errors.New("relay startup was interrupted by drain")
	}
	if err := a.checkDataPlaneListeners(); err != nil {
		a.setDataPlaneReady(false)
		return fmt.Errorf("verify relay listeners before reporting accepting status: %w", err)
	}
	a.currentMu.RLock()
	access := a.access
	session = a.session
	a.currentMu.RUnlock()
	status, reported, err := a.reportProviderStatusWithRetry(ctx, access, session, a.providerStatus(true))
	if err != nil {
		return fmt.Errorf("report accepting DMS relay provider status: %w", err)
	}
	if a.lifecycleStopping() || a.sessionWasReleased() {
		return errors.New("relay startup was interrupted by drain")
	}
	if !reported.AcceptingBookings {
		return errors.New("provider authority expired before accepting status could be installed")
	}
	if err := a.installProviderControl(access, *status); err != nil {
		return err
	}
	// Keep this gate under status serialization. The background status loop is
	// already live, but it cannot independently enable acceptance until this
	// cancelable initial transition has succeeded.
	a.setStartupComplete(true)
	a.setProviderAccepting(true)
	if a.lifecycleStopping() || a.sessionWasReleased() {
		return errors.New("relay startup was interrupted by drain")
	}
	return nil
}

func (a *Application) lifecycleStopping() bool {
	if a == nil {
		return true
	}
	a.gateMu.Lock()
	stopping := a.draining || a.terminal
	a.gateMu.Unlock()
	return stopping
}

func (a *Application) reportProviderStatus(ctx context.Context, access ddsclient.Access, session dmsclient.Session, status dmsclient.ProviderStatus) (*dmsclient.Session, error) {
	var updated *dmsclient.Session
	var err error
	if a.providerDMS != nil {
		updated, err = a.providerDMS.ReportStatus(ctx, access.Token, session.ProviderSessionID, a.expectations(access), status)
	} else if status.AcceptingBookings || status.Draining || status.ShutdownIntent != nil {
		err = errors.New("typed DMS provider status API is unavailable")
	} else {
		updated, err = a.dms.ReportNotAccepting(ctx, access.Token, session.ProviderSessionID, a.expectations(access))
	}
	if a.metrics != nil {
		outcome := telemetry.OperationSuccess
		if err != nil {
			outcome = telemetry.OperationRetryable
			if terminalDMSControlError(err) {
				outcome = telemetry.OperationTerminal
			}
		}
		a.metrics.ObserveProviderOperation(telemetry.ProviderStatus, outcome)
	}
	return updated, err
}

func (a *Application) installProviderControl(access ddsclient.Access, session dmsclient.Session) error {
	if err := a.validateEffectiveCapacity(int(session.EffectiveCapacity), access.MaxConcurrency); err != nil {
		return &providerContractError{err: err}
	}
	if a.worker != nil {
		if err := a.worker.SetControl(access.Token, session); err != nil {
			return &providerContractError{err: err}
		}
	}
	a.currentMu.Lock()
	a.access = access
	a.session = session
	a.currentMu.Unlock()
	a.setTokenExpiresAt(access.ExpiresAt)
	a.setSessionDeadlines(session)
	return nil
}

func (a *Application) reconcileProviderWithRetry(ctx context.Context) error {
	err := a.worker.Reconcile(ctx)
	if err == nil || terminalDMSControlError(err) || ctx.Err() != nil {
		return err
	}
	if waitContext(ctx, a.config.Retry.InitialBackoff) != nil {
		return ctx.Err()
	}
	return a.worker.Reconcile(ctx)
}

func (a *Application) openSessionWithRetry(ctx context.Context, input dmsclient.OpenInput) (*dmsclient.OpenResult, error) {
	opened, err := a.dms.OpenSession(ctx, input)
	if err == nil || !retryableOpenError(ctx, err) {
		return opened, err
	}
	if waitContext(ctx, a.config.Retry.InitialBackoff) != nil {
		return nil, ctx.Err()
	}
	return a.dms.OpenSession(ctx, input)
}

func retryableOpenError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var apiErr *dmsclient.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode >= http.StatusBadRequest && apiErr.StatusCode < http.StatusInternalServerError {
		return false
	}
	return true
}

func (a *Application) expectedEffectiveCapacity(signedMaxConcurrency *int) (int, error) {
	effective, err := a.config.EffectiveCapacityForDDSClaim(signedMaxConcurrency)
	if err != nil {
		return 0, err
	}
	if a.node != nil {
		if err := a.node.Resources().ValidateEffectiveCapacity(effective); err != nil {
			return 0, err
		}
	}
	return effective, nil
}

func (a *Application) validateEffectiveCapacity(effective int, signedMaxConcurrency *int) error {
	expected, err := a.expectedEffectiveCapacity(signedMaxConcurrency)
	if err != nil {
		return err
	}
	// DMS may apply a lower configured scheduling ceiling, but cannot grant
	// more authority than the signed DDS capacity or the local resource budget.
	if effective < 1 || effective > expected {
		return fmt.Errorf("DMS effective relay capacity %d is outside DDS-authorized range [1,%d]", effective, expected)
	}
	return nil
}

// validateLiveProviderTiming enforces the part of the rollout inequality the
// process can prove from authenticated live DMS state. Deployment validation
// must additionally budget measured startup, load-balancer convergence, Robot
// retry, and safety margin, but accepting can never be safe when recovery grace
// does not even outlast this process's configured pre-release drain.
func (a *Application) validateLiveProviderTiming(session dmsclient.Session) error {
	if session.RecoveryGrace <= a.config.ShutdownDrain {
		return fmt.Errorf(
			"DMS recovery grace %s must exceed configured relay shutdown drain %s before accepting bookings",
			session.RecoveryGrace,
			a.config.ShutdownDrain,
		)
	}
	return nil
}

func (a *Application) expectations(access ddsclient.Access) dmsclient.Expectations {
	return dmsclient.Expectations{
		Metadata:           a.metadata,
		LocalCapacity:      a.capacity,
		SchedulingRevision: access.SchedulingRevision,
		NodeTokenExpiresAt: access.ExpiresAt,
	}
}

func (a *Application) startLoops() {
	a.loopsOnce.Do(func() {
		a.wait.Add(5)
		go func() {
			defer a.wait.Done()
			a.controlLoop()
		}()
		go func() {
			defer a.wait.Done()
			a.keyLoop()
		}()
		go func() {
			defer a.wait.Done()
			a.deadlineLoop()
		}()
		go func() {
			defer a.wait.Done()
			a.listenerHealthLoop()
		}()
		go func() {
			defer a.wait.Done()
			select {
			case <-a.context.Done():
			case err := <-a.servers.Errors():
				a.reportFatal(err)
			}
		}()
		if a.worker != nil {
			a.wait.Add(1)
			go func() {
				defer a.wait.Done()
				select {
				case <-a.context.Done():
				case err := <-a.worker.Errors():
					a.worker.SetAccepting(false)
					a.setControlReady(false)
					a.reportFatal(fmt.Errorf("DMS relay booking worker: %w", err))
				}
			}()
		}
	})
}

func (a *Application) listenerHealthLoop() {
	ticker := time.NewTicker(listenerHealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.context.Done():
			return
		case <-ticker.C:
			if err := a.checkDataPlaneListeners(); err != nil {
				// Close marks the lifecycle terminal and cancels the context before
				// closing the node. Do not turn that planned listener teardown into a
				// second fatal outcome if the ticker was selected concurrently.
				if a.context.Err() != nil || a.lifecycleStopping() {
					return
				}
				a.setDataPlaneReady(false)
				a.reportFatal(fmt.Errorf("relay listener health check failed: %w", err))
				return
			}
		}
	}
}

func (a *Application) checkDataPlaneListeners() error {
	if a == nil {
		return errors.New("relay application is unavailable")
	}
	// Application values without a node exist only in focused lifecycle unit
	// tests. Start always installs the concrete node before any control-plane
	// operation or background loop.
	if a.node == nil {
		return nil
	}
	return a.node.CheckListeners()
}

func (a *Application) controlLoop() {
	a.currentMu.RLock()
	access := a.access
	a.currentMu.RUnlock()
	nextRefresh, err := ddsclient.RefreshAt(access.IssuedAt, access.ExpiresAt)
	if err != nil {
		a.reportFatal(err)
		return
	}
	nextStatus := time.Now().UTC().Add(a.config.Timing.StatusInterval)
	var availabilityChanged <-chan struct{}
	if a.worker != nil {
		availabilityChanged = a.worker.AvailabilityChanged()
	}
	for {
		next := nextStatus
		if nextRefresh.Before(next) {
			next = nextRefresh
		}
		delay := time.Until(next)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-a.context.Done():
			timer.Stop()
			return
		case <-a.controlStop:
			timer.Stop()
			return
		case <-availabilityChanged:
			timer.Stop()
			if a.availabilityStatusChanged(time.Now().UTC()) {
				nextStatus = time.Now().UTC()
			}
		case <-timer.C:
		}

		now := time.Now().UTC()
		if !now.Before(nextRefresh) {
			refreshed, refreshErr := a.authenticateWithRetry()
			if refreshErr != nil {
				if a.context.Err() != nil {
					return
				}
				a.logger.Warn("DDS relay Node token refresh failed; new claims are disabled", "error", refreshErr)
				a.setProviderAccepting(false)
				nextRefresh = time.Now().UTC().Add(a.config.Timing.StatusMaxBackoff)
				// Do not renew the session or re-enable claims with the old token
				// while a scheduled refresh remains unhealthy.
				nextStatus = nextRefresh
				continue
			}
			if _, capacityErr := a.expectedEffectiveCapacity(refreshed.MaxConcurrency); capacityErr != nil {
				a.reportFatal(capacityErr)
				return
			}
			// A refresh can complete while a planned drain is installing its
			// final status. Serialize the access snapshot with status/session
			// mutations so a delayed old snapshot can never overwrite a newer
			// token or scheduling revision after drain has begun.
			a.statusMu.Lock()
			if a.lifecycleStopping() || a.sessionWasReleased() {
				a.statusMu.Unlock()
				return
			}
			a.currentMu.Lock()
			a.access = *refreshed
			a.currentMu.Unlock()
			a.setTokenExpiresAt(refreshed.ExpiresAt)
			a.statusMu.Unlock()
			access = *refreshed
			nextRefresh, err = ddsclient.RefreshAt(access.IssuedAt, access.ExpiresAt)
			if err != nil {
				a.reportFatal(err)
				return
			}
			nextStatus = now
		}
		if !now.Before(nextStatus) {
			if err := a.reportStatusWithRetry(); err != nil {
				if a.context.Err() != nil {
					return
				}
				if terminalDMSControlError(err) {
					a.reportFatal(fmt.Errorf("DMS relay provider status requires session replacement: %w", err))
					return
				}
				a.logger.Warn("DMS relay provider status refresh failed; new claims are disabled", "error", err)
				a.setProviderAccepting(false)
				nextStatus = time.Now().UTC().Add(a.config.Timing.StatusMaxBackoff)
				continue
			}
			a.setControlReady(true)
			nextStatus = time.Now().UTC().Add(a.config.Timing.StatusInterval)
		}
	}
}

// Occupancy changes still update local metrics immediately. Only a change in
// advertised eligibility (or a required reconciliation) advances the periodic
// DMS status deadline; ordinary claims must not renew an unchanged status.
func (a *Application) availabilityStatusChanged(now time.Time) bool {
	eligible := a.providerAcceptanceEligible(now)
	if !eligible {
		a.setProviderAccepting(false)
	} else {
		a.publishSchedulingState()
	}
	desired := a.providerStatus(eligible)
	a.currentMu.RLock()
	reported := a.session.Status
	a.currentMu.RUnlock()
	return desired.AcceptingBookings != reported.AcceptingBookings ||
		desired.Draining != reported.Draining ||
		!sameShutdownIntent(desired.ShutdownIntent, reported.ShutdownIntent) ||
		(eligible && a.worker != nil && !a.worker.Accepting())
}

func sameShutdownIntent(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func terminalDMSControlError(err error) bool {
	var contractErr *providerContractError
	if errors.As(err, &contractErr) {
		return true
	}
	var apiErr *dmsclient.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode >= http.StatusBadRequest && apiErr.StatusCode < http.StatusInternalServerError &&
		apiErr.StatusCode != http.StatusRequestTimeout && apiErr.StatusCode != http.StatusTooManyRequests
}

func (a *Application) authenticateWithRetry() (*ddsclient.Access, error) {
	access, err := a.dds.Authenticate(a.context, a.authInput)
	if err == nil {
		return access, nil
	}
	a.setControlReady(false)
	a.setProviderAccepting(false)
	if waitContext(a.context, a.config.Timing.StatusMaxBackoff) != nil {
		return nil, a.context.Err()
	}
	return a.dds.Authenticate(a.context, a.authInput)
}

func (a *Application) reportStatusWithRetry() error {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if a.sessionWasReleased() {
		return nil
	}

	desiredAccepting := a.providerAcceptanceEligible(time.Now().UTC())
	a.currentMu.RLock()
	access := a.access
	session := a.session
	a.currentMu.RUnlock()
	needsReconcile := desiredAccepting && a.worker != nil && !a.worker.Accepting()
	requestedAccepting := desiredAccepting && !needsReconcile
	requestedStatus := a.providerStatus(requestedAccepting)
	updated, requestedStatus, err := a.reportProviderStatusWithRetry(a.context, access, session, requestedStatus)
	if err != nil {
		return err
	}
	if err := a.installProviderControl(access, *updated); err != nil {
		a.setControlReady(false)
		a.setProviderAccepting(false)
		return err
	}

	if needsReconcile {
		if err := a.reconcileProviderWithRetry(a.context); err != nil {
			a.setControlReady(false)
			a.setProviderAccepting(false)
			return err
		}
		if a.providerAcceptanceEligible(time.Now().UTC()) {
			a.currentMu.RLock()
			access = a.access
			session = a.session
			a.currentMu.RUnlock()
			requestedStatus = a.providerStatus(true)
			updated, requestedStatus, err = a.reportProviderStatusWithRetry(a.context, access, session, requestedStatus)
			if err != nil {
				return err
			}
			if err := a.installProviderControl(access, *updated); err != nil {
				a.setControlReady(false)
				a.setProviderAccepting(false)
				return err
			}
		}
	}

	// A key/token/session deadline or terminal transition can race a successful
	// accepting response. Correct DMS back to non-accepting before returning.
	if requestedStatus.AcceptingBookings && !a.providerAcceptanceEligible(time.Now().UTC()) {
		a.setProviderAccepting(false)
		a.currentMu.RLock()
		access = a.access
		session = a.session
		a.currentMu.RUnlock()
		requestedStatus = a.providerStatus(false)
		updated, requestedStatus, err = a.reportProviderStatusWithRetry(a.context, access, session, requestedStatus)
		if err != nil {
			return err
		}
		if err := a.installProviderControl(access, *updated); err != nil {
			a.setControlReady(false)
			return err
		}
	}
	a.setProviderAccepting(requestedStatus.AcceptingBookings && a.providerAcceptanceEligible(time.Now().UTC()))
	return nil
}

func (a *Application) reportProviderStatusWithRetry(ctx context.Context, access ddsclient.Access, session dmsclient.Session, status dmsclient.ProviderStatus) (*dmsclient.Session, dmsclient.ProviderStatus, error) {
	updated, err := a.reportProviderStatus(ctx, access, session, status)
	if err != nil {
		a.setControlReady(false)
		a.setProviderAccepting(false)
		if terminalDMSControlError(err) {
			return nil, dmsclient.ProviderStatus{}, err
		}
		if waitContext(ctx, a.config.Timing.StatusMaxBackoff) != nil {
			return nil, dmsclient.ProviderStatus{}, ctx.Err()
		}
		if status.AcceptingBookings && !a.providerAcceptanceEligible(time.Now().UTC()) {
			status = a.providerStatus(false)
		}
		updated, err = a.reportProviderStatus(ctx, access, session, status)
	}
	if err != nil {
		return nil, dmsclient.ProviderStatus{}, err
	}
	return updated, status, nil
}

func (a *Application) providerStatus(accepting bool) dmsclient.ProviderStatus {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	status := dmsclient.ProviderStatus{
		AcceptingBookings: accepting && !a.draining && !a.terminal,
		Draining:          a.draining,
	}
	if a.shutdownIntent != nil {
		intent := *a.shutdownIntent
		status.ShutdownIntent = &intent
	}
	return status
}

func (a *Application) setProviderAccepting(accepting bool) {
	if accepting {
		accepting = a.providerAcceptanceEligible(time.Now().UTC())
	}
	if a.worker != nil {
		a.worker.SetAccepting(accepting)
	}
	a.publishSchedulingState()
}

func (a *Application) publishSchedulingState() {
	if a == nil || a.state == nil {
		return
	}
	a.gateMu.Lock()
	terminal := a.terminal
	draining := a.draining
	startupComplete := a.startupComplete
	dataPlaneReady := a.dataPlaneReady
	controlReady := a.controlReady
	keyReady := a.keyReady
	keyExpiresAt := a.keyExpiresAt
	tokenExpiresAt := a.tokenExpiresAt
	sessionExpiresAt := a.sessionExpiresAt
	sessionTokenExpiresAt := a.sessionTokenExpiresAt
	a.gateMu.Unlock()
	a.currentMu.RLock()
	effectiveCapacity := a.session.EffectiveCapacity
	a.currentMu.RUnlock()
	used := 0
	if a.registry != nil {
		used, _ = a.registry.Counts()
	}
	if used > int(effectiveCapacity) {
		used = int(effectiveCapacity)
	}
	now := time.Now().UTC()
	reason := admin.AcceptingStateControlStale
	switch {
	case terminal:
		reason = admin.AcceptingStateTerminal
	case draining:
		reason = admin.AcceptingStateDraining
	case !dataPlaneReady || !startupComplete:
		reason = admin.AcceptingStateNotReady
	case !a.config.AcceptBookings:
		reason = admin.AcceptingStateBootstrapClosed
	case !controlReady || !keyReady || !deadlineCurrent(now, keyExpiresAt) ||
		!deadlineCurrent(now, tokenExpiresAt) || !deadlineCurrent(now, sessionExpiresAt) ||
		!deadlineCurrent(now, sessionTokenExpiresAt):
		reason = admin.AcceptingStateControlStale
	case effectiveCapacity > 0 && used >= int(effectiveCapacity):
		reason = admin.AcceptingStateSaturated
	case a.worker != nil && a.worker.Accepting():
		reason = admin.AcceptingStateAccepting
	}
	_ = a.state.SetScheduling(reason, effectiveCapacity, uint32(used))
}

func (a *Application) providerAcceptanceEligible(now time.Time) bool {
	if !a.config.AcceptBookings {
		return false
	}
	a.currentMu.RLock()
	recoveryGrace := a.session.RecoveryGrace
	a.currentMu.RUnlock()
	if recoveryGrace <= a.config.ShutdownDrain {
		return false
	}
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	eligible := a.startupComplete && !a.terminal && !a.draining && a.dataPlaneReady && a.controlReady && a.keyReady &&
		deadlineCurrent(now, a.keyExpiresAt) && deadlineCurrent(now, a.tokenExpiresAt) &&
		deadlineCurrent(now, a.sessionExpiresAt) && deadlineCurrent(now, a.sessionTokenExpiresAt)
	if !eligible {
		return false
	}
	return a.worker == nil || a.worker.CapacityAvailable()
}

func (a *Application) keyLoop() {
	ticker := time.NewTicker(a.config.VerificationKeys.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.context.Done():
			return
		case <-ticker.C:
			refreshStarted := time.Now().UTC()
			if err := a.ring.Preload(a.context); err != nil {
				a.logger.Warn("DDS verification-key refresh failed", "error", err)
				ready := a.ring.Ready(time.Now().UTC())
				a.setKeyReady(ready)
				if !ready {
					a.setProviderAccepting(false)
				}
				continue
			}
			a.setKeyReadyUntil(a.ring.Ready(time.Now().UTC()), refreshStarted.Add(a.config.VerificationKeys.MaxStaleness))
		}
	}
}

func (a *Application) setControlReady(ready bool) {
	a.gateMu.Lock()
	a.controlReady = ready
	a.updateReadinessLocked(time.Now().UTC())
	a.gateMu.Unlock()
	a.publishSchedulingState()
	a.notifyDeadlineChanged()
}

func (a *Application) setDataPlaneReady(ready bool) {
	a.gateMu.Lock()
	a.dataPlaneReady = ready
	a.updateReadinessLocked(time.Now().UTC())
	a.gateMu.Unlock()
	a.publishSchedulingState()
}

func (a *Application) setStartupComplete(complete bool) {
	a.gateMu.Lock()
	a.startupComplete = complete
	a.gateMu.Unlock()
	a.publishSchedulingState()
}

func (a *Application) setKeyReady(ready bool) {
	a.gateMu.Lock()
	a.keyReady = ready
	a.updateReadinessLocked(time.Now().UTC())
	a.gateMu.Unlock()
	a.publishSchedulingState()
	a.notifyDeadlineChanged()
}

func (a *Application) setKeyReadyUntil(ready bool, expiresAt time.Time) {
	a.gateMu.Lock()
	a.keyReady = ready
	a.keyExpiresAt = expiresAt.UTC()
	a.updateReadinessLocked(time.Now().UTC())
	a.gateMu.Unlock()
	a.publishSchedulingState()
	a.notifyDeadlineChanged()
}

func (a *Application) setTokenExpiresAt(expiresAt time.Time) {
	a.gateMu.Lock()
	a.tokenExpiresAt = expiresAt.UTC()
	a.updateReadinessLocked(time.Now().UTC())
	a.gateMu.Unlock()
	a.publishSchedulingState()
	a.notifyDeadlineChanged()
}

func (a *Application) setSessionDeadlines(session dmsclient.Session) {
	a.gateMu.Lock()
	a.sessionExpiresAt = session.SessionExpiresAt.UTC()
	a.sessionTokenExpiresAt = session.ProviderNodeJWTExpiresAt.UTC()
	a.updateReadinessLocked(time.Now().UTC())
	a.gateMu.Unlock()
	a.publishSchedulingState()
	a.notifyDeadlineChanged()
}

func (a *Application) markTerminal() {
	a.gateMu.Lock()
	a.terminal = true
	a.updateReadinessLocked(time.Now().UTC())
	a.gateMu.Unlock()
	a.publishSchedulingState()
	a.notifyDeadlineChanged()
}

func (a *Application) updateReadinessLocked(now time.Time) {
	_ = now
	ready := !a.terminal && a.dataPlaneReady
	if a.state != nil {
		a.state.SetReady(ready)
	}
}

func deadlineCurrent(now, expiresAt time.Time) bool {
	return !expiresAt.IsZero() && now.Before(expiresAt)
}

func (a *Application) notifyDeadlineChanged() {
	if a.deadlineChanged == nil {
		return
	}
	select {
	case a.deadlineChanged <- struct{}{}:
	default:
	}
}

func (a *Application) deadlineLoop() {
	for {
		now := time.Now().UTC()
		deadline := a.nextReadinessDeadline(now)
		if !a.providerAcceptanceEligible(now) {
			a.setProviderAccepting(false)
		}
		var timer *time.Timer
		var timerC <-chan time.Time
		if !deadline.IsZero() {
			delay := time.Until(deadline)
			if delay < 0 {
				delay = 0
			}
			timer = time.NewTimer(delay)
			timerC = timer.C
		}
		select {
		case <-a.context.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-a.deadlineChanged:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
			a.gateMu.Lock()
			a.updateReadinessLocked(time.Now().UTC())
			a.gateMu.Unlock()
		}
	}
}

func (a *Application) nextReadinessDeadline(now time.Time) time.Time {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	// Recompute before deciding whether a timer is needed. A deadline can pass
	// between its setter's readiness update and this loop iteration; returning
	// no timer must never leave the last probe value latched true.
	a.updateReadinessLocked(now)
	if a.terminal || !a.controlReady || !a.keyReady {
		return time.Time{}
	}
	deadlines := [...]time.Time{a.keyExpiresAt, a.tokenExpiresAt, a.sessionExpiresAt, a.sessionTokenExpiresAt}
	var earliest time.Time
	for _, deadline := range deadlines {
		if !deadlineCurrent(now, deadline) {
			return time.Time{}
		}
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	return earliest
}

func (a *Application) reportFatal(err error) {
	if err == nil {
		return
	}
	a.markTerminal()
	a.setProviderAccepting(false)
	a.errOnce.Do(func() { a.errors <- err })
}

// Wait blocks until Close cancels the owned lifecycle or a bounded background
// component fails. Startup cancellation is deliberately separate so callers
// can run the planned drain before invoking Close.
func (a *Application) Wait() error {
	if a == nil {
		return errors.New("relay application is nil")
	}
	select {
	case <-a.context.Done():
		return nil
	case err := <-a.errors:
		return err
	}
}

// Close denies every local gate and releases process resources. When a planned
// drain has started, Close never changes its DMS intent or repeats its exact
// session deletion; it performs preserving local cleanup only.
func (a *Application) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("relay close context is required")
	}
	a.closeMu.Lock()
	defer a.closeMu.Unlock()

	a.drainMu.Lock()
	drainOperation := a.drainOperation
	a.closing = true
	a.drainMu.Unlock()
	plannedDrain := drainOperation != nil
	if !a.closeStarted {
		if drainOperation != nil {
			select {
			case <-drainOperation.done:
			case <-ctx.Done():
				drainOperation.cancel()
				// Cancel the worker/control context before joining the drain. An
				// in-flight Claim or Ready request may be the operation currently
				// holding the provider barrier; its HTTP context must be canceled
				// before that barrier can complete.
				if a.cancel != nil {
					a.cancel()
				}
				<-drainOperation.done
			}
		}
		a.markTerminal()
		a.setProviderAccepting(false)
		if a.node != nil && a.node.Host() != nil {
			a.node.Host().RemoveStreamHandler(admission.ProtocolID)
		}
		a.stopControlLoop()
		if a.cancel != nil {
			a.cancel()
		}
		a.wait.Wait()
		var workerErr error
		if a.worker != nil {
			// Exact provider-session DELETE below is the authoritative remote
			// catch-all for an unplanned stop. Keep worker shutdown local so a
			// retryable child mutation cannot poison all later Close attempts.
			workerErr = a.worker.ShutdownPreserving(ctx)
		}
		var nodeErr error
		if a.node != nil {
			nodeErr = a.node.Close()
		}
		a.localCloseErr = errors.Join(workerErr, nodeErr)
		a.closeStarted = true
	}

	var releaseErr error
	if !plannedDrain && !a.sessionWasReleased() && a.dms != nil {
		a.currentMu.RLock()
		access := a.access
		session := a.session
		a.currentMu.RUnlock()
		if access.Token != "" && session.ProviderSessionID != uuid.Nil {
			releaseErr = a.releaseExactSession(ctx)
		}
	}
	var serverErr error
	if a.servers != nil {
		serverErr = a.servers.Close(ctx)
	}
	return errors.Join(a.localCloseErr, releaseErr, serverErr)
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
