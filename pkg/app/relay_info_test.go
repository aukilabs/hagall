package app

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/admin"
	"github.com/aukilabs/hagall/pkg/config"
	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/aukilabs/hagall/pkg/node"
	"github.com/aukilabs/hagall/pkg/relayconfig"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestRelayInfoMapsLiveProviderStateAndRedactsItAfterExactRelease(t *testing.T) {
	service, canonical, peerID := newAppTestRelayService(t)

	now := time.Now().UTC()
	shutdownIntent := dmsclient.ShutdownIntentRestartSameIdentity
	application := &Application{
		config: config.Defaults(), node: service,
		metadata: dmsclient.Metadata{
			BaseAddresses: canonical.Bases(), EndpointKeys: canonical.EndpointKeys(),
			Limits: dmsclient.Limits{
				DurationSeconds:       canonical.Limits().DurationSeconds(),
				DataBytesPerDirection: canonical.Limits().DataBytesPerDirection,
			},
			ConfigFingerprint: canonical.Fingerprint(),
		},
		session: dmsclient.Session{
			ProviderSessionID: uuid.New(), Status: dmsclient.ProviderStatus{
				AcceptingBookings: false, Draining: true, ShutdownIntent: &shutdownIntent,
			},
			EffectiveCapacity: 1, SchedulingRevision: 9,
			SessionExpiresAt: now.Add(2 * time.Minute), ProviderNodeJWTExpiresAt: now.Add(5 * time.Minute),
			StatusTTL: 3 * time.Minute, ProviderLeaseTTL: 3 * time.Minute, RecoveryGrace: 30 * time.Minute,
		},
	}

	before, err := application.relayInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, peerID.String(), before.PeerID)
	require.Equal(t, canonical.Bases(), before.Addresses)
	require.True(t, before.DMS.SessionPresent)
	require.True(t, before.DMS.Draining)
	require.Equal(t, uint32(1), before.DMS.EffectiveCapacity)
	require.Equal(t, uint64(9), before.DMS.SchedulingRevision)
	require.NotNil(t, before.DMS.ShutdownIntent)

	application.drainMu.Lock()
	application.sessionReleased = true
	application.drainMu.Unlock()
	after, err := application.relayInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, before.PeerID, after.PeerID)
	require.Equal(t, before.Addresses, after.Addresses)
	require.False(t, after.DMS.SessionPresent)
	require.Zero(t, after.DMS.EffectiveCapacity)
	require.Nil(t, after.DMS.SessionExpiresAt)
	require.Nil(t, after.DMS.ProviderNodeJWTExpiresAt)
}

func TestListenerHealthLossClearsReadinessAndReportsFatal(t *testing.T) {
	service, _, _ := newAppTestRelayService(t)
	appContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	application := &Application{
		config:  config.Defaults(),
		state:   admin.NewState(),
		node:    service,
		context: appContext,
		cancel:  cancel,
		errors:  make(chan error, 1),
	}
	application.setDataPlaneReady(true)

	loopDone := make(chan struct{})
	go func() {
		application.listenerHealthLoop()
		close(loopDone)
	}()

	closeAppTestWebSocketListener(t, service)

	select {
	case err := <-application.errors:
		require.ErrorContains(t, err, "listener health check failed")
	case <-time.After(2 * listenerHealthInterval):
		t.Fatal("listener loss did not report a fatal error")
	}
	require.False(t, application.state.Ready())
	<-loopDone
}

func TestListenerHealthLoopIgnoresPlannedListenerTeardown(t *testing.T) {
	service, _, _ := newAppTestRelayService(t)
	appContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	application := &Application{
		config:  config.Defaults(),
		state:   admin.NewState(),
		node:    service,
		context: appContext,
		cancel:  cancel,
		errors:  make(chan error, 1),
	}
	application.markTerminal()
	closeAppTestWebSocketListener(t, service)

	loopDone := make(chan struct{})
	go func() {
		application.listenerHealthLoop()
		close(loopDone)
	}()
	select {
	case <-loopDone:
	case <-time.After(2 * listenerHealthInterval):
		t.Fatal("listener monitor did not stop during planned teardown")
	}
	select {
	case err := <-application.errors:
		t.Fatalf("planned listener teardown reported a fatal error: %v", err)
	default:
	}
}

func newAppTestRelayService(t *testing.T) (*node.Service, relayconfig.Canonical, peer.ID) {
	t.Helper()
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)
	limits := relayconfig.Limits{Duration: 15 * time.Minute, DataBytesPerDirection: 1 << 20}
	canonical, err := relayconfig.Canonicalize([]string{
		"/dns4/relay-a.interop.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay-a.interop.aukiverse.com/tcp/4443/wss/p2p/" + peerID.String(),
	}, peerID, limits)
	require.NoError(t, err)
	listenAddress, err := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/0")
	require.NoError(t, err)
	webSocketListenAddress, err := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/0/ws")
	require.NoError(t, err)
	service, err := node.New(node.Options{
		Identity: privateKey, ListenAddresses: []ma.Multiaddr{listenAddress, webSocketListenAddress}, Advertised: canonical,
		PrometheusRegistry: prometheus.NewRegistry(), ACL: node.DenyAllACL(),
		Resources: node.Resources{
			Capacity: 1, MaxReservations: 1, ReservationTTL: 10 * time.Minute,
			MaxReservationsPerIP: 1, MaxReservationsPerASN: 1, MaxCircuitsPerPeer: 16,
			BufferBytes: 2048, MemoryBytes: 1 << 30, FileDescriptors: 4096,
			Connections: 2048, Streams: 8192, ConnectionManagerLow: 768,
			ConnectionManagerHigh: 1024, ConnectionManagerGrace: time.Minute,
			OutboundTCPConnectTimeout: 10 * time.Second, RelayLimits: canonical.Limits(),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	return service, canonical, peerID
}

func closeAppTestWebSocketListener(t *testing.T, service *node.Service) {
	t.Helper()
	var webSocketAddress ma.Multiaddr
	for _, address := range service.Host().Network().ListenAddresses() {
		if _, err := address.ValueForProtocol(ma.P_WS); err == nil {
			webSocketAddress = address
			break
		}
	}
	require.NotNil(t, webSocketAddress)
	listenerCloser, ok := service.Host().Network().(interface{ ListenClose(...ma.Multiaddr) })
	require.True(t, ok)
	listenerCloser.ListenClose(webSocketAddress)
}
