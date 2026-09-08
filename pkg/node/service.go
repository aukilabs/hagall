package node

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/aukilabs/hagall/pkg/admission"
	"github.com/aukilabs/hagall/pkg/relayconfig"
	libp2p "github.com/libp2p/go-libp2p"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	relayproto "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
)

const MaxProviderCapacity = 256

type Resources struct {
	Capacity                  int
	MaxReservations           int
	ReservationTTL            time.Duration
	MaxReservationsPerIP      int
	MaxReservationsPerASN     int
	MaxCircuitsPerPeer        int
	BufferBytes               int
	MemoryBytes               int64
	FileDescriptors           int
	Connections               int
	Streams                   int
	ConnectionManagerLow      int
	ConnectionManagerHigh     int
	ConnectionManagerGrace    time.Duration
	OutboundTCPConnectTimeout time.Duration
	RelayLimits               relayconfig.Limits
}

func (r Resources) Validate() error {
	if r.Capacity < 1 || r.Capacity > MaxProviderCapacity {
		return fmt.Errorf("relay capacity must be between 1 and %d", MaxProviderCapacity)
	}
	if r.MaxReservations < r.Capacity || r.MaxReservations > MaxProviderCapacity {
		return errors.New("maximum reservations must cover local capacity and be at most 256")
	}
	if r.ReservationTTL != 10*time.Minute {
		return errors.New("Circuit Relay v2 reservation TTL must be exactly 10 minutes in v1")
	}
	if r.MaxReservationsPerIP < r.Capacity || r.MaxReservationsPerASN < r.Capacity {
		return errors.New("per-IP and per-ASN reservation limits must cover local capacity")
	}
	if r.MaxCircuitsPerPeer <= 0 || r.BufferBytes <= 0 {
		return errors.New("circuit and buffer limits must be positive")
	}
	if err := r.RelayLimits.Validate(); err != nil {
		return err
	}
	if r.MemoryBytes < 128<<20 {
		return errors.New("resource-manager memory must be at least 128 MiB")
	}
	if r.FileDescriptors <= 0 || r.Connections <= 0 || r.Streams <= 0 {
		return errors.New("resource-manager FD, connection, and stream limits must be positive")
	}
	if r.FileDescriptors < r.Connections {
		return errors.New("resource-manager FD limit must cover its connection limit")
	}
	if r.Streams < r.Connections {
		return errors.New("resource-manager stream limit must cover its connection limit")
	}
	if r.ConnectionManagerLow < r.Capacity || r.ConnectionManagerLow >= r.ConnectionManagerHigh {
		return errors.New("connection-manager low watermark must cover capacity and be below its high watermark")
	}
	if r.ConnectionManagerHigh > r.Connections {
		return errors.New("connection-manager high watermark must not exceed the resource-manager connection limit")
	}
	if r.ConnectionManagerGrace < 0 || r.OutboundTCPConnectTimeout <= 0 {
		return errors.New("connection-manager grace cannot be negative and TCP connect timeout must be positive")
	}
	return nil
}

func (r Resources) ValidateEffectiveCapacity(effective int) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if effective < 1 || effective > MaxProviderCapacity {
		return fmt.Errorf("DMS effective capacity must be between 1 and %d", MaxProviderCapacity)
	}
	if effective > r.Capacity || effective > r.MaxReservationsPerIP || effective > r.MaxReservationsPerASN {
		return errors.New("DMS effective capacity exceeds local total, per-IP, or per-ASN reservation capacity")
	}
	return nil
}

type Options struct {
	Identity           libp2pcrypto.PrivKey
	ListenAddresses    []ma.Multiaddr
	Advertised         relayconfig.Canonical
	Resources          Resources
	PrometheusRegistry *prometheus.Registry
	// RelayMetricsTracer may wrap go-libp2p's stock tracer to add private,
	// low-cardinality gauges. When omitted, the stock tracer is registered.
	RelayMetricsTracer relayv2.MetricsTracer
	// ACL is a mutable, local-only authorization view. A nil value is replaced
	// with DenyAllACL so the relay never inherits go-libp2p's nil/allow-all
	// behavior during bootstrap.
	ACL relayv2.ACLFilter
}

type Service struct {
	host            host.Host
	relay           *relayv2.Relay
	resources       Resources
	listenAddresses []ma.Multiaddr
	closeOnce       sync.Once
	closeErr        error
}

type denyAllACL struct{}

func (denyAllACL) AllowReserve(peer.ID, ma.Multiaddr) bool {
	return false
}

func (denyAllACL) AllowConnect(peer.ID, ma.Multiaddr, peer.ID) bool {
	return false
}

func DenyAllACL() relayv2.ACLFilter {
	return denyAllACL{}
}

func New(options Options) (*Service, error) {
	acl := options.ACL
	if acl == nil {
		acl = DenyAllACL()
	}
	return newWithACL(options, acl)
}

func newWithACL(options Options, acl relayv2.ACLFilter) (*Service, error) {
	if options.Identity == nil {
		return nil, errors.New("relay libp2p identity is required")
	}
	if acl == nil {
		return nil, errors.New("relay ACL must never be nil")
	}
	if len(options.ListenAddresses) == 0 {
		return nil, errors.New("at least one explicit relay listen address is required")
	}
	if options.PrometheusRegistry == nil {
		return nil, errors.New("private Prometheus registry is required")
	}
	if err := options.Resources.Validate(); err != nil {
		return nil, err
	}
	expectedPeerID, err := peer.IDFromPrivateKey(options.Identity)
	if err != nil {
		return nil, fmt.Errorf("derive relay Peer ID: %w", err)
	}
	canonical, err := relayconfig.Canonicalize(options.Advertised.Bases(), expectedPeerID, options.Advertised.Limits())
	if err != nil {
		return nil, fmt.Errorf("validate relay advertised snapshot: %w", err)
	}
	if canonical.Fingerprint() != options.Advertised.Fingerprint() {
		return nil, errors.New("relay advertised snapshot is not canonical")
	}
	advertisedMultiaddrs, err := canonical.Multiaddrs()
	if err != nil {
		return nil, err
	}

	resourceManager, err := newResourceManager(options.Resources)
	if err != nil {
		return nil, err
	}
	connectionManager, err := connmgr.NewConnManager(
		options.Resources.ConnectionManagerLow,
		options.Resources.ConnectionManagerHigh,
		connmgr.WithGracePeriod(options.Resources.ConnectionManagerGrace),
	)
	if err != nil {
		_ = resourceManager.Close()
		return nil, fmt.Errorf("create relay connection manager: %w", err)
	}

	h, err := libp2p.New(
		libp2p.NoTransports,
		libp2p.Identity(options.Identity),
		libp2p.ListenAddrs(options.ListenAddresses...),
		libp2p.Transport(tcp.NewTCPTransport, tcp.WithConnectionTimeout(options.Resources.OutboundTCPConnectTimeout)),
		libp2p.Transport(websocket.New),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport),
		libp2p.AddrsFactory(func([]ma.Multiaddr) []ma.Multiaddr {
			return slices.Clone(advertisedMultiaddrs)
		}),
		libp2p.ResourceManager(resourceManager),
		libp2p.ConnectionManager(connectionManager),
		libp2p.DisableRelay(),
		libp2p.Ping(false),
		libp2p.DisableIdentifyAddressDiscovery(),
		libp2p.ForceReachabilityPublic(),
		libp2p.WithDialTimeout(options.Resources.OutboundTCPConnectTimeout),
		libp2p.PrometheusRegisterer(options.PrometheusRegistry),
	)
	if err != nil {
		_ = resourceManager.Close()
		return nil, fmt.Errorf("create relay libp2p host: %w", err)
	}
	if h.ID() != expectedPeerID {
		_ = h.Close()
		return nil, errors.New("relay host Peer ID differs from loaded identity")
	}
	service := &Service{
		host:            h,
		resources:       options.Resources,
		listenAddresses: slices.Clone(options.ListenAddresses),
	}
	if err := service.CheckListeners(); err != nil {
		_ = h.Close()
		return nil, err
	}
	if err := requireExactHostAddresses(h, canonical.Bases()); err != nil {
		_ = h.Close()
		return nil, err
	}

	relayLimit := &relayv2.RelayLimit{
		Duration: options.Resources.RelayLimits.Duration,
		Data:     options.Resources.RelayLimits.DataBytesPerDirection,
	}
	relayResources := relayv2.Resources{
		Limit:                  relayLimit,
		ReservationTTL:         options.Resources.ReservationTTL,
		MaxReservations:        options.Resources.MaxReservations,
		MaxCircuits:            options.Resources.MaxCircuitsPerPeer,
		BufferSize:             options.Resources.BufferBytes,
		MaxReservationsPerPeer: 1,
		MaxReservationsPerIP:   options.Resources.MaxReservationsPerIP,
		MaxReservationsPerASN:  options.Resources.MaxReservationsPerASN,
	}
	metricsTracer := options.RelayMetricsTracer
	if metricsTracer == nil {
		metricsTracer = relayv2.NewMetricsTracer(relayv2.WithRegisterer(options.PrometheusRegistry))
	}
	relayService, err := relayv2.New(
		h,
		relayv2.WithResources(relayResources),
		relayv2.WithACL(acl),
		relayv2.WithMetricsTracer(metricsTracer),
	)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("create Circuit Relay v2 service: %w", err)
	}
	service.relay = relayService
	return service, nil
}

func (s *Service) Host() host.Host {
	if s == nil {
		return nil
	}
	return s.host
}

func (s *Service) Resources() Resources {
	if s == nil {
		return Resources{}
	}
	return s.resources
}

// CheckListeners verifies that every configured local listener is still owned
// by the live libp2p swarm. go-libp2p considers a multi-address Listen call
// successful when any one address binds, so the relay must enforce this
// stronger all-listeners invariant before advertising provider availability.
func (s *Service) CheckListeners() error {
	if s == nil || s.host == nil {
		return errors.New("relay host is unavailable")
	}
	actual := s.host.Network().ListenAddresses()
	used := make([]bool, len(actual))
	for _, expected := range s.listenAddresses {
		matched := false
		for index, candidate := range actual {
			if !used[index] && listenAddressMatches(expected, candidate) {
				used[index] = true
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("configured relay listener is not bound: %s (live listeners: %v)", expected, multiaddrStrings(actual))
		}
	}
	return nil
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = errors.Join(s.relay.Close(), s.host.Close())
	})
	return s.closeErr
}

func requireExactHostAddresses(h host.Host, expected []string) error {
	actual := make([]string, 0, len(h.Addrs()))
	for _, address := range h.Addrs() {
		actual = append(actual, address.String())
	}
	sort.Strings(actual)
	if !slices.Equal(actual, expected) {
		return fmt.Errorf("relay host advertised addresses differ: got %v, expected %v", actual, expected)
	}
	return nil
}

func listenAddressMatches(expected, actual ma.Multiaddr) bool {
	if expected.Equal(actual) {
		return true
	}
	if len(expected) != len(actual) {
		return false
	}
	for index := range expected {
		expectedComponent := expected[index]
		actualComponent := actual[index]
		if expectedComponent.Protocol().Code != actualComponent.Protocol().Code {
			return false
		}
		if expectedComponent.Protocol().Code == ma.P_TCP && expectedComponent.Value() == "0" {
			port, err := strconv.ParseUint(actualComponent.Value(), 10, 16)
			if err != nil || port == 0 {
				return false
			}
			continue
		}
		if !expectedComponent.Equal(&actualComponent) {
			return false
		}
	}
	return true
}

func multiaddrStrings(addresses []ma.Multiaddr) []string {
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.String())
	}
	sort.Strings(result)
	return result
}

func newResourceManager(resources Resources) (network.ResourceManager, error) {
	scaling := rcmgr.DefaultLimits
	libp2p.SetDefaultServiceLimits(&scaling)
	base := scaling.Scale(resources.MemoryBytes, resources.FileDescriptors)

	system := finiteLimits(resources.Streams, resources.Connections, resources.FileDescriptors, resources.MemoryBytes)
	transient := finiteLimits(
		max(resources.Capacity*4, resources.Streams/2),
		max(resources.Capacity*2, resources.Connections/2),
		max(resources.Capacity*2, resources.FileDescriptors/2),
		max64(64<<20, resources.MemoryBytes/2),
	)
	perPeerStreams := max(64, resources.MaxCircuitsPerPeer*4)
	perPeer := finiteLimits(perPeerStreams, 8, 8, max64(16<<20, int64(resources.BufferBytes*perPeerStreams*2)))
	service := finiteLimits(resources.Streams, resources.Connections, resources.FileDescriptors, resources.MemoryBytes)
	protocolLimit := finiteLimits(resources.Streams, resources.Connections, resources.FileDescriptors, resources.MemoryBytes)

	partial := rcmgr.PartialLimitConfig{
		System:              system,
		Transient:           transient,
		ServiceDefault:      service,
		ServicePeerDefault:  perPeer,
		ProtocolDefault:     protocolLimit,
		ProtocolPeerDefault: perPeer,
		PeerDefault:         perPeer,
		Service: map[string]rcmgr.ResourceLimits{
			relayv2.ServiceName:   service,
			admission.ServiceName: service,
		},
		Protocol: map[protocol.ID]rcmgr.ResourceLimits{
			relayproto.ProtoIDv2Hop:  protocolLimit,
			relayproto.ProtoIDv2Stop: protocolLimit,
			admission.ProtocolID:     protocolLimit,
		},
		ServicePeer: map[string]rcmgr.ResourceLimits{
			relayv2.ServiceName:   perPeer,
			admission.ServiceName: perPeer,
		},
		ProtocolPeer: map[protocol.ID]rcmgr.ResourceLimits{
			relayproto.ProtoIDv2Hop:  perPeer,
			relayproto.ProtoIDv2Stop: perPeer,
			admission.ProtocolID:     perPeer,
		},
		Conn: rcmgr.ResourceLimits{
			Streams:         rcmgr.LimitVal(perPeerStreams),
			StreamsInbound:  rcmgr.LimitVal(perPeerStreams),
			StreamsOutbound: rcmgr.LimitVal(perPeerStreams),
			Conns:           1,
			ConnsInbound:    1,
			ConnsOutbound:   1,
			FD:              1,
			Memory:          rcmgr.LimitVal64(max64(4<<20, int64(resources.BufferBytes*perPeerStreams*2))),
		},
		Stream: rcmgr.ResourceLimits{
			Streams:         1,
			StreamsInbound:  1,
			StreamsOutbound: 1,
			Conns:           1,
			ConnsInbound:    1,
			ConnsOutbound:   1,
			FD:              1,
			Memory:          rcmgr.LimitVal64(max64(1<<20, int64(resources.BufferBytes*4))),
		},
	}
	manager, err := rcmgr.NewResourceManager(
		rcmgr.NewFixedLimiter(partial.Build(base)),
		// go-libp2p otherwise applies a hidden default of eight inbound
		// connections per IPv4 address (and IPv6 /56). A public relay must let
		// its explicit reservation quotas, rather than that dial-oriented
		// default, bound peers sharing one NAT or network prefix.
		rcmgr.WithLimitPerSubnet(
			[]rcmgr.ConnLimitPerSubnet{{
				PrefixLength: 32,
				ConnCount:    resources.MaxReservationsPerIP,
			}},
			[]rcmgr.ConnLimitPerSubnet{
				{
					PrefixLength: 56,
					ConnCount:    resources.MaxReservationsPerIP,
				},
				{
					PrefixLength: 48,
					ConnCount:    resources.MaxReservationsPerASN,
				},
			},
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create relay resource manager: %w", err)
	}
	return manager, nil
}

func finiteLimits(streams, connections, fileDescriptors int, memory int64) rcmgr.ResourceLimits {
	return rcmgr.ResourceLimits{
		Streams:         rcmgr.LimitVal(streams),
		StreamsInbound:  rcmgr.LimitVal(streams),
		StreamsOutbound: rcmgr.LimitVal(streams),
		Conns:           rcmgr.LimitVal(connections),
		ConnsInbound:    rcmgr.LimitVal(connections),
		ConnsOutbound:   rcmgr.LimitVal(connections),
		FD:              rcmgr.LimitVal(fileDescriptors),
		Memory:          rcmgr.LimitVal64(memory),
	}
}

func max64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
