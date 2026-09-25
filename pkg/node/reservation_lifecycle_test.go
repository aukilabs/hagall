package node

// Deterministic regression for stale disconnect cleanup in the pinned relay.
// a Network decorator pauses the real disconnect callback around
// its Connectedness observation. All protocol traffic uses loopback TCP.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	pb "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/pb"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

type disconnectTraceGate struct {
	target        peer.ID
	beforeRead    bool
	entered       chan network.Connectedness
	release       chan struct{}
	completed     chan struct{}
	once          sync.Once
	releaseOnce   sync.Once
	completedOnce sync.Once
}

type disconnectTraceMetrics struct {
	relayv2.MetricsTracer
	closed atomic.Int64
}

func (m *disconnectTraceMetrics) ReservationClosed(count int) {
	m.closed.Add(int64(count))
	m.MetricsTracer.ReservationClosed(count)
}

func (g *disconnectTraceGate) resume() { g.releaseOnce.Do(func() { close(g.release) }) }

// Each callback gets its own network view. Only the old target callback is
// paused; new connection notifications and reservation handlers run normally.
type disconnectTraceView struct {
	network.Network
	gate *disconnectTraceGate
}

func (n *disconnectTraceView) Connectedness(p peer.ID) network.Connectedness {
	if p != n.gate.target {
		return n.Network.Connectedness(p)
	}
	var state network.Connectedness
	intercepted := false
	n.gate.once.Do(func() {
		intercepted = true
		if n.gate.beforeRead {
			n.gate.entered <- network.NotConnected
			<-n.gate.release
			state = n.Network.Connectedness(p)
		} else {
			state = n.Network.Connectedness(p)
			n.gate.entered <- state
			<-n.gate.release
		}
	})
	if !intercepted {
		return n.Network.Connectedness(p)
	}
	return state
}

type disconnectTraceNotifiee struct {
	network.Notifiee
	gate *disconnectTraceGate
}

func (n *disconnectTraceNotifiee) Disconnected(net network.Network, conn network.Conn) {
	if conn.RemotePeer() != n.gate.target {
		n.Notifiee.Disconnected(net, conn)
		return
	}
	n.Notifiee.Disconnected(&disconnectTraceView{Network: net, gate: n.gate}, conn)
	n.gate.completedOnce.Do(func() { close(n.gate.completed) })
}

type disconnectTraceNetwork struct {
	network.Network
	gate     *disconnectTraceGate
	mu       sync.Mutex
	wrappers map[network.Notifiee]network.Notifiee
}

func (n *disconnectTraceNetwork) Notify(notifiee network.Notifiee) {
	wrapper := &disconnectTraceNotifiee{Notifiee: notifiee, gate: n.gate}
	n.mu.Lock()
	n.wrappers[notifiee] = wrapper
	n.mu.Unlock()
	n.Network.Notify(wrapper)
}

func (n *disconnectTraceNetwork) StopNotify(notifiee network.Notifiee) {
	n.mu.Lock()
	wrapper := n.wrappers[notifiee]
	delete(n.wrappers, notifiee)
	n.mu.Unlock()
	if wrapper != nil {
		n.Network.StopNotify(wrapper)
	}
}

type disconnectTraceHost struct {
	host.Host
	net *disconnectTraceNetwork
}

func (h *disconnectTraceHost) Network() network.Network { return h.net }

func traceHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.DisableMetrics(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return h
}

func waitTrace[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for reservation lifecycle event")
		var zero T
		return zero
	}
}

// Read the real CONNECT response so the assertion uses the protocol's exact
// NO_RESERVATION status instead of a generic dial error. Each successful probe
// is reset afterward; this test qualifies reservation state, not payload flow.
func traceCircuitStatus(t *testing.T, ctx context.Context, source host.Host, relay, target peer.ID) pb.Status {
	t.Helper()
	s, err := source.NewStream(ctx, relay, proto.ProtoIDv2Hop)
	require.NoError(t, err)
	defer s.Reset()
	require.NoError(t, s.SetDeadline(time.Now().Add(5*time.Second)))
	wr := util.NewDelimitedWriter(s)
	rd := util.NewDelimitedReader(s, 4096)
	defer rd.Close()
	require.NoError(t, wr.WriteMsg(&pb.HopMessage{
		Type: pb.HopMessage_CONNECT.Enum(),
		Peer: util.PeerInfoToPeerV2(peer.AddrInfo{ID: target}),
	}))
	var response pb.HopMessage
	require.NoError(t, rd.ReadMsg(&response))
	require.Equal(t, pb.HopMessage_STATUS, response.GetType())
	return response.GetStatus()
}

// An old disconnect callback must never delete a reservation accepted on a
// replacement connection, even if its first connectedness observation is stale.
func TestReservationDisconnectPreservesReplacement(t *testing.T) {
	for _, beforeRead := range []bool{false, true} {
		name := "stale_snapshot_preserves_new_reservation"
		if beforeRead {
			name = "fresh_snapshot_preserves_new_reservation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			relayHost := traceHost(t)
			target := traceHost(t)
			source := traceHost(t)
			gate := &disconnectTraceGate{
				target: target.ID(), beforeRead: beforeRead,
				entered: make(chan network.Connectedness, 1),
				release: make(chan struct{}), completed: make(chan struct{}),
			}
			// Resume before any host cleanup, including on failed assertions.
			defer gate.resume()
			wrapped := &disconnectTraceHost{Host: relayHost, net: &disconnectTraceNetwork{
				Network: relayHost.Network(), gate: gate,
				wrappers: make(map[network.Notifiee]network.Notifiee),
			}}
			resources := relayv2.DefaultResources()
			resources.ReservationTTL = 10 * time.Minute
			resources.MaxReservationsPerIP = 8
			metrics := &disconnectTraceMetrics{MetricsTracer: relayv2.NewMetricsTracer(relayv2.WithRegisterer(prometheus.NewRegistry()))}
			r, err := relayv2.New(wrapped, relayv2.WithResources(resources), relayv2.WithMetricsTracer(metrics))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			info := peer.AddrInfo{ID: relayHost.ID(), Addrs: relayHost.Addrs()}
			require.NoError(t, source.Connect(ctx, info))
			first, err := relayclient.Reserve(ctx, target, info)
			require.NoError(t, err)
			require.True(t, first.Expiration.After(time.Now().Add(9*time.Minute)))
			oldConnections := relayHost.Network().ConnsToPeer(target.ID())
			require.Len(t, oldConnections, 1)
			oldID := oldConnections[0].ID()
			t.Logf("TRACE event=initial_reservation_accepted peer=%s connection=%s", target.ID(), oldID)
			require.Equal(t, pb.Status_OK, traceCircuitStatus(t, ctx, source, relayHost.ID(), target.ID()))

			require.NoError(t, target.Network().ClosePeer(relayHost.ID()))
			observed := waitTrace(t, ctx, gate.entered)
			require.Equal(t, network.NotConnected, observed)
			t.Logf("TRACE event=old_disconnect_paused before_connectedness_read=%t old_connection=%s", beforeRead, oldID)

			fresh, err := relayclient.Reserve(ctx, target, info)
			require.NoError(t, err)
			require.True(t, fresh.Expiration.After(time.Now().Add(9*time.Minute)))
			newConnections := relayHost.Network().ConnsToPeer(target.ID())
			require.Len(t, newConnections, 1)
			newID := newConnections[0].ID()
			require.NotEqual(t, oldID, newID)
			t.Logf("TRACE event=replacement_reservation_accepted peer=%s connection=%s expires=%s", target.ID(), newID, fresh.Expiration.UTC().Format(time.RFC3339Nano))
			require.Equal(t, pb.Status_OK, traceCircuitStatus(t, ctx, source, relayHost.ID(), target.ID()))

			gate.resume()
			waitTrace(t, ctx, gate.completed)
			require.Zero(t, metrics.closed.Load(), "replacement reservation must not be counted as removed")
			require.Equal(t, network.Connected, relayHost.Network().Connectedness(target.ID()))
			require.Equal(t, network.Connected, target.Network().Connectedness(relayHost.ID()))
			require.False(t, newConnections[0].IsClosed())
			status := traceCircuitStatus(t, ctx, source, relayHost.ID(), target.ID())
			t.Logf("TRACE event=circuit_after_old_disconnect status=%s replacement_connected=true connection=%s", status, newID)
			require.Equal(t, pb.Status_OK, status,
				"old disconnect cleanup must preserve the accepted replacement reservation")
			for range 3 {
				require.Equal(t, pb.Status_OK, traceCircuitStatus(t, ctx, source, relayHost.ID(), target.ID()))
			}

			// Renewal remains available on the same replacement connection.
			_, err = relayclient.Reserve(ctx, target, info)
			require.NoError(t, err)
			require.Equal(t, newID, relayHost.Network().ConnsToPeer(target.ID())[0].ID())
			require.Equal(t, pb.Status_OK, traceCircuitStatus(t, ctx, source, relayHost.ID(), target.ID()))
			t.Logf("TRACE event=renewal_preserves_circuit connection=%s", newID)

			// Ordinary disconnect must still release capacity exactly once.
			require.NoError(t, target.Network().ClosePeer(relayHost.ID()))
			require.Eventually(t, func() bool { return metrics.closed.Load() == 1 }, 5*time.Second, time.Millisecond)
			require.Equal(t, pb.Status_NO_RESERVATION, traceCircuitStatus(t, ctx, source, relayHost.ID(), target.ID()))
			t.Log("TRACE event=final_disconnect_releases_reservation removed=1")
		})
	}
}
