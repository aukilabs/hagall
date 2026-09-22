package node

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/relayconfig"
	"github.com/google/uuid"
	libp2p "github.com/libp2p/go-libp2p"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

type allowAllACL struct{}

func (allowAllACL) AllowReserve(peer.ID, ma.Multiaddr) bool {
	return true
}

func (allowAllACL) AllowConnect(peer.ID, ma.Multiaddr, peer.ID) bool {
	return true
}

func testOptions(t *testing.T, capacity int) Options {
	t.Helper()
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)
	canonical, err := relayconfig.Canonicalize([]string{
		"/dns4/relay-a.interop.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay-a.interop.aukiverse.com/tcp/4443/wss/p2p/" + peerID.String(),
	}, peerID, relayconfig.Limits{DataBytesPerDirection: 10 << 30, Duration: 15 * time.Minute})
	require.NoError(t, err)
	listenAddress, err := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/0")
	require.NoError(t, err)
	webSocketListenAddress, err := ma.NewMultiaddr("/ip4/127.0.0.1/tcp/0/ws")
	require.NoError(t, err)
	return Options{
		Identity:           privateKey,
		ListenAddresses:    []ma.Multiaddr{listenAddress, webSocketListenAddress},
		Advertised:         canonical,
		PrometheusRegistry: prometheus.NewRegistry(),
		ACL:                DenyAllACL(),
		Resources: Resources{
			Capacity:                  capacity,
			MaxReservations:           capacity,
			ReservationTTL:            10 * time.Minute,
			MaxReservationsPerIP:      capacity,
			MaxReservationsPerASN:     capacity,
			MaxCircuitsPerPeer:        16,
			BufferBytes:               2048,
			MemoryBytes:               1 << 30,
			FileDescriptors:           4096,
			Connections:               max(2048, capacity+256),
			Streams:                   8192,
			ConnectionManagerLow:      max(capacity, 768),
			ConnectionManagerHigh:     max(1024, capacity+256),
			ConnectionManagerGrace:    time.Minute,
			OutboundTCPConnectTimeout: 10 * time.Second,
			RelayLimits:               canonical.Limits(),
		},
	}
}

func TestResourceManagerConnectionLimitMatchesReservationQuota(t *testing.T) {
	resources := testOptions(t, 16).Resources
	manager, err := newResourceManager(resources)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })

	remote := ma.StringCast("/ip4/203.0.113.10/tcp/4001")
	connections := make([]network.ConnManagementScope, 0, resources.MaxReservationsPerIP)
	for range resources.MaxReservationsPerIP {
		connection, err := manager.OpenConnection(network.DirInbound, true, remote)
		require.NoError(t, err)
		connections = append(connections, connection)
	}
	t.Cleanup(func() {
		for _, connection := range connections {
			connection.Done()
		}
	})

	_, err = manager.OpenConnection(network.DirInbound, true, remote)
	require.Error(t, err, "the configured per-IP reservation quota must remain the connection ceiling")

	otherIP, err := manager.OpenConnection(
		network.DirInbound,
		true,
		ma.StringCast("/ip4/203.0.113.11/tcp/4001"),
	)
	require.NoError(t, err, "one saturated IP must not consume another IP's connection quota")
	otherIP.Done()
}

func TestProductionServiceAdvertisesExactSetAndDeniesEveryReservation(t *testing.T) {
	options := testOptions(t, 3)
	service, err := New(options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	require.Equal(t, options.Advertised.Bases(), hostAddressStrings(service.Host()))
	clientHost, err := libp2p.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clientHost.Close()) })

	_, err = relayclient.Reserve(context.Background(), clientHost, internalRelayInfo(service))
	require.Error(t, err, "the production ACL must fail closed before DMS authority is installed")
}

func TestProductionServiceServesRelayOverWebSocket(t *testing.T) {
	options := testOptions(t, 1)
	options.ACL = allowAllACL{}

	service, err := New(options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	require.Equal(t, options.Advertised.Bases(), hostAddressStrings(service.Host()))

	var dialAddress ma.Multiaddr
	for _, address := range service.Host().Network().ListenAddresses() {
		if _, err := address.ValueForProtocol(ma.P_WS); err == nil {
			dialAddress = address
			break
		}
	}
	require.NotNil(t, dialAddress)
	clientHost, err := libp2p.New(libp2p.NoListenAddrs)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clientHost.Close()) })

	reservation, err := relayclient.Reserve(context.Background(), clientHost, peer.AddrInfo{
		ID:    service.Host().ID(),
		Addrs: []ma.Multiaddr{dialAddress},
	})
	require.NoError(t, err)
	require.Equal(t, network.Connected, clientHost.Network().Connectedness(service.Host().ID()))
	actual := make([]string, 0, len(reservation.Addrs))
	for _, address := range reservation.Addrs {
		actual = append(actual, address.String())
	}
	sort.Strings(actual)
	require.Equal(t, options.Advertised.Bases(), actual)
}

func TestPublicRelayTCPListenerServesNoAdminHTTP(t *testing.T) {
	service, err := New(testOptions(t, 1))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	var listenAddress ma.Multiaddr
	for _, address := range service.Host().Network().ListenAddresses() {
		if _, err := address.ValueForProtocol(ma.P_WS); err != nil {
			listenAddress = address
			break
		}
	}
	require.NotNil(t, listenAddress)
	networkAddress, err := manet.ToNetAddr(listenAddress)
	require.NoError(t, err)
	client := &http.Client{Timeout: 250 * time.Millisecond}
	response, err := client.Get("http://" + networkAddress.String() + "/livez")
	if response != nil {
		_ = response.Body.Close()
	}
	require.Error(t, err, "raw libp2p TCP listener must not serve admin HTTP")
}

func TestProductionServiceRejectsPartialListenerBinding(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, occupied.Close()) })
	_, occupiedPort, err := net.SplitHostPort(occupied.Addr().String())
	require.NoError(t, err)

	for _, testCase := range []struct {
		name      string
		listeners []string
	}{
		{
			name: "TCP listener",
			listeners: []string{
				"/ip4/127.0.0.1/tcp/" + occupiedPort,
				"/ip4/127.0.0.1/tcp/0/ws",
			},
		},
		{
			name: "WebSocket listener",
			listeners: []string{
				"/ip4/127.0.0.1/tcp/0",
				"/ip4/127.0.0.1/tcp/" + occupiedPort + "/ws",
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			options := testOptions(t, 1)
			options.ListenAddresses = nil
			for _, raw := range testCase.listeners {
				address, err := ma.NewMultiaddr(raw)
				require.NoError(t, err)
				options.ListenAddresses = append(options.ListenAddresses, address)
			}
			service, err := New(options)
			require.Nil(t, service)
			require.ErrorContains(t, err, "configured relay listener is not bound")
		})
	}
}

func TestCheckListenersDetectsListenerLoss(t *testing.T) {
	service, err := New(testOptions(t, 1))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	require.NoError(t, service.CheckListeners())

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
	require.ErrorContains(t, service.CheckListeners(), "configured relay listener is not bound")
}

func TestReservationResponseUsesExactConfiguredAddressesAndFiniteLimits(t *testing.T) {
	options := testOptions(t, 3)
	options.ACL = allowAllACL{}
	service, err := New(options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	clientHost, err := libp2p.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clientHost.Close()) })

	reservation, err := relayclient.Reserve(context.Background(), clientHost, internalRelayInfo(service))
	require.NoError(t, err)
	require.NotNil(t, reservation.Voucher)
	require.Equal(t, options.Resources.RelayLimits.Duration, reservation.LimitDuration)
	require.Equal(t, uint64(options.Resources.RelayLimits.DataBytesPerDirection), reservation.LimitData)
	actual := make([]string, 0, len(reservation.Addrs))
	for _, address := range reservation.Addrs {
		actual = append(actual, address.String())
	}
	sort.Strings(actual)
	require.Equal(t, options.Advertised.Bases(), actual)
}

func TestSameIPCapacityAndNextReservationAreBounded(t *testing.T) {
	const capacity = 3
	options := testOptions(t, capacity)
	options.ACL = allowAllACL{}
	service, err := New(options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	clients := make([]hostCloser, 0, capacity+1)
	for index := 0; index < capacity+1; index++ {
		clientHost, err := libp2p.New()
		require.NoError(t, err)
		clients = append(clients, clientHost)
		_, err = relayclient.Reserve(context.Background(), clientHost, internalRelayInfo(service))
		if index < capacity {
			require.NoError(t, err, "same-IP reservations through configured capacity must work")
		} else {
			require.Error(t, err, "the reservation after configured capacity must be rejected")
		}
	}
	for _, clientHost := range clients {
		require.NoError(t, clientHost.Close())
	}
}

func TestRelayedPayloadCreatesNoFilesAndShutdownIsIdempotent(t *testing.T) {
	tempDirectory := t.TempDir()
	before := directoryEntries(t, tempDirectory)
	options := testOptions(t, 2)
	options.ACL = allowAllACL{}
	service, err := New(options)
	require.NoError(t, err)

	target, err := libp2p.New()
	require.NoError(t, err)
	source, err := libp2p.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = source.Close()
		_ = target.Close()
		_ = service.Close()
	})

	const testProtocol protocol.ID = "/auki/relay/no-file-test/1"
	received := make(chan []byte, 1)
	target.SetStreamHandler(testProtocol, func(stream network.Stream) {
		defer stream.Close()
		payload, readErr := io.ReadAll(stream)
		if readErr != nil {
			received <- nil
			return
		}
		received <- payload
	})
	_, err = relayclient.Reserve(context.Background(), target, internalRelayInfo(service))
	require.NoError(t, err)
	require.NoError(t, source.Connect(context.Background(), internalRelayInfo(service)))

	circuitAddress, err := ma.NewMultiaddr("/p2p/" + service.Host().ID().String() + "/p2p-circuit/p2p/" + target.ID().String())
	require.NoError(t, err)
	require.NoError(t, source.Connect(context.Background(), peer.AddrInfo{ID: target.ID(), Addrs: []ma.Multiaddr{circuitAddress}}))
	stream, err := source.NewStream(network.WithAllowLimitedConn(context.Background(), "relay no-file test"), target.ID(), testProtocol)
	require.NoError(t, err)
	payload := []byte("encrypted application bytes are forwarded in memory")
	_, err = stream.Write(payload)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	select {
	case actual := <-received:
		require.Equal(t, payload, actual)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for relayed application bytes")
	}
	require.Equal(t, before, directoryEntries(t, tempDirectory))

	relayPeerID := service.Host().ID()
	require.NoError(t, service.Close())
	require.NoError(t, service.Close())
	require.Eventually(t, func() bool {
		return source.Network().Connectedness(relayPeerID) == network.NotConnected
	}, 5*time.Second, 10*time.Millisecond, "relay shutdown did not close the source connection")
	connectContext, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	require.Error(t, source.Connect(connectContext, internalRelayInfo(service)))
}

func TestBookedACLRequiresSourceAdmissionAndRevokesOnlyExactTarget(t *testing.T) {
	const capacity = 2
	now := time.Now().UTC()
	registry, err := booking.New(booking.Config{
		MaximumBookings: capacity, MaximumAdmissions: capacity,
		AdmissionTTL: 30 * time.Second,
	})
	require.NoError(t, err)
	sessionID := uuid.New()
	require.NoError(t, registry.SetSession(booking.Session{
		ProviderSessionID: sessionID, EffectiveCapacity: capacity,
		SessionExpiresAt: now.Add(2 * time.Minute), NodeJWTExpiresAt: now.Add(3 * time.Minute),
	}))
	options := testOptions(t, capacity)
	options.ACL = booking.NewACL(registry)
	service, err := New(options)
	require.NoError(t, err)
	t.Cleanup(func() { _ = service.Close() })
	unbooked, err := libp2p.New()
	require.NoError(t, err)
	_, err = relayclient.Reserve(context.Background(), unbooked, internalRelayInfo(service))
	require.Error(t, err, "an authenticated but unbooked target must not reserve")
	require.NoError(t, unbooked.Close())

	targets := make([]host.Host, 0, capacity)
	targetIDs := make([]peer.ID, 0, capacity)
	authorities := make([]booking.Authority, 0, capacity)
	domain := uuid.New()
	for index := 0; index < capacity; index++ {
		target, createErr := libp2p.New()
		require.NoError(t, createErr)
		targets = append(targets, target)
		targetID := target.ID()
		targetIDs = append(targetIDs, targetID)
		authority := booking.Authority{
			BookingID: uuid.New(), SlotID: uuid.New(), DomainID: domain, TargetPeerID: targetID,
			Fence:          booking.Fence{ProviderSessionID: sessionID, AssignmentID: uuid.New(), ReservationEpoch: uuid.New()},
			RequestedUntil: now.Add(time.Hour), AuthorityExpiresAt: now.Add(2 * time.Minute), ProviderLeaseExpiresAt: now.Add(time.Minute),
		}
		authorities = append(authorities, authority)
		require.NoError(t, registry.InstallStarting(authority))
		_, reserveErr := relayclient.Reserve(context.Background(), target, internalRelayInfo(service))
		require.NoError(t, reserveErr)
		if index == 0 {
			_, renewErr := relayclient.Reserve(context.Background(), target, internalRelayInfo(service))
			require.NoError(t, renewErr, "a still-authorized target must be able to renew its reservation")
		}
	}
	defer func() {
		for _, target := range targets {
			_ = target.Close()
		}
	}()

	unauthorizedSource, err := libp2p.New()
	require.NoError(t, err)
	require.NoError(t, unauthorizedSource.Connect(context.Background(), internalRelayInfo(service)))
	firstCircuit := circuitTargetAddress(t, service.Host().ID(), targetIDs[0])
	unauthorized, cancelUnauthorized := context.WithTimeout(context.Background(), time.Second)
	require.Error(t, unauthorizedSource.Connect(unauthorized, peer.AddrInfo{ID: targetIDs[0], Addrs: []ma.Multiaddr{firstCircuit}}))
	cancelUnauthorized()
	require.NoError(t, unauthorizedSource.Close())

	source, err := libp2p.New()
	require.NoError(t, err)
	defer source.Close()
	require.NoError(t, source.Connect(context.Background(), internalRelayInfo(service)))

	for index, authority := range authorities {
		require.NoError(t, registry.ActivateReady(authority.TargetPeerID, authority.Fence))
		snapshot, found := registry.SnapshotForAdmission(authority.TargetPeerID, domain)
		require.True(t, found)
		_, err = registry.Admit(booking.AdmissionRequest{
			SourcePeerID: source.ID(), DomainID: domain, TargetPeerID: authority.TargetPeerID,
			ExpectedFence: snapshot.Authority.Fence, LiteralJWTExpiresAt: now.Add(time.Minute),
		})
		require.NoError(t, err)
		circuit := circuitTargetAddress(t, service.Host().ID(), targetIDs[index])
		require.NoError(t, source.Connect(context.Background(), peer.AddrInfo{ID: targetIDs[index], Addrs: []ma.Multiaddr{circuit}}))
	}

	removed, ok := registry.RemoveExact(targetIDs[0], authorities[0].Fence)
	require.True(t, ok)
	require.Equal(t, targetIDs[0], removed)
	require.NoError(t, service.Host().Network().ClosePeer(removed))
	require.False(t, registry.AllowConnect(source.ID(), removed), "target admissions must be deleted before connection teardown")
	require.Eventually(t, func() bool {
		return source.Network().Connectedness(removed) == network.NotConnected
	}, time.Second, time.Millisecond, "provider target teardown must close the relayed source circuit")
	revokedSource, err := libp2p.New()
	require.NoError(t, err)
	defer revokedSource.Close()
	require.NoError(t, revokedSource.Connect(context.Background(), internalRelayInfo(service)))
	revoked, cancelRevoked := context.WithTimeout(context.Background(), time.Second)
	require.Error(t, revokedSource.Connect(revoked, peer.AddrInfo{ID: removed, Addrs: []ma.Multiaddr{firstCircuit}}))
	cancelRevoked()
	require.Equal(t, network.Limited, source.Network().Connectedness(targetIDs[1]), "healthy sibling circuit must remain connected")
}

func circuitTargetAddress(t *testing.T, relayID, targetID peer.ID) ma.Multiaddr {
	t.Helper()
	address, err := ma.NewMultiaddr("/p2p/" + relayID.String() + "/p2p-circuit/p2p/" + targetID.String())
	require.NoError(t, err)
	return address
}

func TestResourceAndConnectionManagerLimitsRejectUnsafeCapacity(t *testing.T) {
	options := testOptions(t, MaxProviderCapacity)
	require.NoError(t, options.Resources.Validate())
	require.NoError(t, options.Resources.ValidateEffectiveCapacity(MaxProviderCapacity))

	invalid := options.Resources
	invalid.ConnectionManagerLow = MaxProviderCapacity - 1
	require.ErrorContains(t, invalid.Validate(), "low watermark")
	invalid = options.Resources
	invalid.MaxReservationsPerIP = MaxProviderCapacity - 1
	require.ErrorContains(t, invalid.Validate(), "per-IP")
	invalid = options.Resources
	invalid.Connections = invalid.ConnectionManagerHigh - 1
	require.ErrorContains(t, invalid.Validate(), "high watermark")
	require.Error(t, options.Resources.ValidateEffectiveCapacity(MaxProviderCapacity+1))
}

type hostCloser interface {
	Close() error
}

func internalRelayInfo(service *Service) peer.AddrInfo {
	return peer.AddrInfo{
		ID:    service.Host().ID(),
		Addrs: service.Host().Network().ListenAddresses(),
	}
}

func hostAddressStrings(h interface{ Addrs() []ma.Multiaddr }) []string {
	addresses := h.Addrs()
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.String())
	}
	sort.Strings(result)
	return result
}

func directoryEntries(t *testing.T, root string) []string {
	t.Helper()
	entries := make([]string, 0)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			entries = append(entries, path)
		}
		return nil
	}))
	sort.Strings(entries)
	return entries
}

var _ relayv2.ACLFilter = allowAllACL{}
