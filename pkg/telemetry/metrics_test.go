package telemetry

import (
	"testing"
	"time"

	relaypb "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/pb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestMetricsExposeOnlyBoundedLabelsAndActiveCounts(t *testing.T) {
	registry := prometheus.NewRegistry()
	used := 2
	metrics, err := New(Options{Registerer: registry, ConfiguredSlots: 8, UsedSlots: func() int { return used }})
	require.NoError(t, err)

	metrics.ObserveACL(ACLReserve, true)
	metrics.ObserveACL(ACLConnect, false)
	metrics.ObserveACL(ACLAction("peer-id-must-not-be-a-label"), true)
	metrics.ObserveSourceAuth(SourceAuthAccepted)
	for _, outcome := range []SourceAuthOutcome{SourceAuthRateLimited, SourceAuthBusy, SourceAuthCacheFull, SourceAuthContextDone} {
		metrics.ObserveSourceAuth(outcome)
		require.Equal(t, float64(1), metricValue(t, registry, "auki_relay_node_source_auth_total", map[string]string{"outcome": string(outcome)}))
	}
	metrics.ObserveSourceAuth(SourceAuthOutcome("domain-must-not-be-a-label"))
	metrics.ObserveProviderOperation(ProviderClaim, OperationSuccess)
	metrics.ObserveBookingEvent(BookingExpired)
	metrics.ConnectionOpened()
	metrics.ConnectionOpened()
	metrics.ConnectionClosed(time.Second)
	metrics.ConnectionClosed(time.Second)
	metrics.ConnectionClosed(time.Second)
	metrics.ReservationAllowed(false)
	metrics.ReservationAllowed(true)
	metrics.ReservationClosed(5)

	require.Equal(t, 0, metrics.ActiveCircuits())
	require.Equal(t, float64(8), metricValue(t, registry, "auki_relay_node_booking_slots_configured", nil))
	require.Equal(t, float64(2), metricValue(t, registry, "auki_relay_node_booking_slots_used", nil))
	require.Equal(t, float64(0), metricValue(t, registry, "auki_relay_node_circuits_active", nil))
	require.Equal(t, float64(0), metricValue(t, registry, "auki_relay_node_reservations_active", nil))
	require.Equal(t, float64(1), metricValue(t, registry, "auki_relay_node_acl_decisions_total", map[string]string{"operation": "reserve", "decision": "allow"}))
	require.Equal(t, float64(1), metricValue(t, registry, "auki_relay_node_acl_decisions_total", map[string]string{"operation": "connect", "decision": "deny"}))
	require.Equal(t, float64(1), metricValue(t, registry, "auki_relay_node_source_auth_total", map[string]string{"outcome": "accepted"}))
	require.Equal(t, float64(1), metricValue(t, registry, "auki_relay_node_provider_operations_total", map[string]string{"operation": "claim", "outcome": "success"}))
	require.Equal(t, float64(1), metricValue(t, registry, "auki_relay_node_booking_events_total", map[string]string{"event": "expired"}))
	requireMetricAbsent(t, registry, "auki_relay_node_booking_events_total", map[string]string{"event": "renewed"})
}

func TestRelayTracerDelegatesStockMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := New(Options{Registerer: registry, ConfiguredSlots: 1, UsedSlots: func() int { return 0 }})
	require.NoError(t, err)
	delegate := &recordingTracer{}
	tracer := metrics.RelayTracer(delegate)

	tracer.RelayStatus(true)
	tracer.ConnectionOpened()
	tracer.ConnectionRequestHandled(relaypb.Status_OK)
	tracer.ReservationAllowed(false)
	tracer.ReservationRequestHandled(relaypb.Status_OK)
	tracer.BytesTransferred(42)
	tracer.ReservationClosed(1)
	tracer.ConnectionClosed(time.Second)

	require.Equal(t, 8, delegate.calls)
}

func metricValue(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			actual := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				actual[label.GetName()] = label.GetValue()
			}
			if !equalLabels(actual, labels) {
				continue
			}
			if metric.Gauge != nil {
				return metric.GetGauge().GetValue()
			}
			return metric.GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s with labels %v was not registered", name, labels)
	return 0
}

func requireMetricAbsent(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			actual := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				actual[label.GetName()] = label.GetValue()
			}
			if equalLabels(actual, labels) {
				t.Fatalf("metric %s with labels %v was unexpectedly registered", name, labels)
			}
		}
	}
}

func equalLabels(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

type recordingTracer struct{ calls int }

func (r *recordingTracer) RelayStatus(bool)                         { r.calls++ }
func (r *recordingTracer) ConnectionOpened()                        { r.calls++ }
func (r *recordingTracer) ConnectionClosed(time.Duration)           { r.calls++ }
func (r *recordingTracer) ConnectionRequestHandled(relaypb.Status)  { r.calls++ }
func (r *recordingTracer) ReservationAllowed(bool)                  { r.calls++ }
func (r *recordingTracer) ReservationClosed(int)                    { r.calls++ }
func (r *recordingTracer) ReservationRequestHandled(relaypb.Status) { r.calls++ }
func (r *recordingTracer) BytesTransferred(int)                     { r.calls++ }
