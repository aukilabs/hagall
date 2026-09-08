// Package telemetry owns the standalone relay's private, low-cardinality
// Prometheus instrumentation. It deliberately accepts only closed enums so a
// Peer, Domain, booking, assignment, or token can never become a label value.
package telemetry

import (
	"errors"
	"sync/atomic"
	"time"

	relaypb "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/pb"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/prometheus/client_golang/prometheus"
)

type ACLAction string

const (
	ACLReserve ACLAction = "reserve"
	ACLConnect ACLAction = "connect"
)

type SourceAuthOutcome string

const (
	SourceAuthAccepted       SourceAuthOutcome = "accepted"
	SourceAuthInvalidPeer    SourceAuthOutcome = "invalid_peer"
	SourceAuthRateLimited    SourceAuthOutcome = "rate_limited"
	SourceAuthMalformed      SourceAuthOutcome = "malformed"
	SourceAuthUnbooked       SourceAuthOutcome = "unbooked"
	SourceAuthInvalidToken   SourceAuthOutcome = "invalid_token"
	SourceAuthIdentityDenied SourceAuthOutcome = "identity_denied"
	SourceAuthCapacityDenied SourceAuthOutcome = "capacity_denied"
	SourceAuthWriteFailed    SourceAuthOutcome = "write_failed"
)

type ProviderOperation string

const (
	ProviderClaim      ProviderOperation = "claim"
	ProviderActive     ProviderOperation = "active"
	ProviderRecover    ProviderOperation = "recover"
	ProviderReady      ProviderOperation = "ready"
	ProviderHeartbeat  ProviderOperation = "heartbeat"
	ProviderFail       ProviderOperation = "fail"
	ProviderRelinquish ProviderOperation = "relinquish"
	ProviderStatus     ProviderOperation = "status"
)

type OperationOutcome string

const (
	OperationSuccess   OperationOutcome = "success"
	OperationEmpty     OperationOutcome = "empty"
	OperationRetryable OperationOutcome = "retryable_error"
	OperationTerminal  OperationOutcome = "terminal_error"
	OperationRevoked   OperationOutcome = "revoked"
)

type BookingEvent string

const (
	BookingReady        BookingEvent = "ready"
	BookingRecovered    BookingEvent = "recovered"
	BookingRenewed      BookingEvent = "renewed"
	BookingCanceled     BookingEvent = "canceled"
	BookingExpired      BookingEvent = "expired"
	BookingFailed       BookingEvent = "failed"
	BookingRelinquished BookingEvent = "relinquished"
	BookingReconciled   BookingEvent = "removed_by_reconcile"
)

type Options struct {
	Registerer      prometheus.Registerer
	ConfiguredSlots int
	UsedSlots       func() int
}

// Metrics is safe for concurrent ACL, stream-handler, provider-worker, and
// go-libp2p tracer callbacks.
type Metrics struct {
	aclDecisions       *prometheus.CounterVec
	sourceAuth         *prometheus.CounterVec
	providerOperations *prometheus.CounterVec
	bookingEvents      *prometheus.CounterVec
	reservationsActive prometheus.Gauge
	circuitsActive     prometheus.Gauge

	activeReservations atomic.Int64
	activeCircuits     atomic.Int64
	activityChanged    chan struct{}
	delegate           relayv2.MetricsTracer
}

func New(options Options) (*Metrics, error) {
	if options.Registerer == nil || options.ConfiguredSlots < 1 || options.UsedSlots == nil {
		return nil, errors.New("telemetry registerer, configured slots, and used-slots reader are required")
	}
	configured := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "auki", Subsystem: "relay_node", Name: "booking_slots_configured",
		Help: "Configured maximum number of concurrent DMS relay booking slots.",
	})
	configured.Set(float64(options.ConfiguredSlots))
	used := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "auki", Subsystem: "relay_node", Name: "booking_slots_used",
		Help: "Number of DMS relay booking generations currently installed locally.",
	}, func() float64 { return float64(options.UsedSlots()) })
	metrics := &Metrics{
		aclDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "auki", Subsystem: "relay_node", Name: "acl_decisions_total",
			Help: "Circuit Relay v2 ACL decisions by bounded operation and decision.",
		}, []string{"operation", "decision"}),
		sourceAuth: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "auki", Subsystem: "relay_node", Name: "source_auth_total",
			Help: "Relay source-admission attempts by bounded outcome.",
		}, []string{"outcome"}),
		providerOperations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "auki", Subsystem: "relay_node", Name: "provider_operations_total",
			Help: "Typed DMS provider operations by bounded operation and outcome.",
		}, []string{"operation", "outcome"}),
		bookingEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "auki", Subsystem: "relay_node", Name: "booking_events_total",
			Help: "Local relay booking lifecycle events by bounded event.",
		}, []string{"event"}),
		reservationsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "auki", Subsystem: "relay_node", Name: "reservations_active",
			Help: "Current Circuit Relay v2 reservations observed by go-libp2p.",
		}),
		circuitsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "auki", Subsystem: "relay_node", Name: "circuits_active",
			Help: "Current Circuit Relay v2 circuits observed by go-libp2p.",
		}),
		activityChanged: make(chan struct{}, 1),
	}
	collectors := []prometheus.Collector{
		configured, used, metrics.aclDecisions, metrics.sourceAuth,
		metrics.providerOperations, metrics.bookingEvents,
		metrics.reservationsActive, metrics.circuitsActive,
	}
	var result error
	for _, collector := range collectors {
		if err := options.Registerer.Register(collector); err != nil {
			result = errors.Join(result, err)
		}
	}
	if result != nil {
		return nil, result
	}
	return metrics, nil
}

func (m *Metrics) ObserveACL(action ACLAction, allowed bool) {
	if m == nil || !validACLAction(action) {
		return
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	m.aclDecisions.WithLabelValues(string(action), decision).Inc()
}

func (m *Metrics) ObserveSourceAuth(outcome SourceAuthOutcome) {
	if m == nil || !validSourceAuthOutcome(outcome) {
		return
	}
	m.sourceAuth.WithLabelValues(string(outcome)).Inc()
}

func (m *Metrics) ObserveProviderOperation(operation ProviderOperation, outcome OperationOutcome) {
	if m == nil || !validProviderOperation(operation) || !validOperationOutcome(outcome) {
		return
	}
	m.providerOperations.WithLabelValues(string(operation), string(outcome)).Inc()
}

func (m *Metrics) ObserveBookingEvent(event BookingEvent) {
	if m == nil || !validBookingEvent(event) {
		return
	}
	m.bookingEvents.WithLabelValues(string(event)).Inc()
}

func (m *Metrics) ActiveCircuits() int {
	if m == nil {
		return 0
	}
	return int(m.activeCircuits.Load())
}

func (m *Metrics) ActivityChanged() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.activityChanged
}

// RelayTracer wraps go-libp2p's standard tracer, retaining its stock byte,
// duration, status, and total counters while adding the active gauges needed
// by bounded drain decisions.
func (m *Metrics) RelayTracer(delegate relayv2.MetricsTracer) relayv2.MetricsTracer {
	if m == nil {
		return delegate
	}
	m.delegate = delegate
	return m
}

func (m *Metrics) RelayStatus(enabled bool) {
	if m.delegate != nil {
		m.delegate.RelayStatus(enabled)
	}
}

func (m *Metrics) ConnectionOpened() {
	m.circuitsActive.Set(float64(m.activeCircuits.Add(1)))
	m.signalActivity()
	if m.delegate != nil {
		m.delegate.ConnectionOpened()
	}
}

func (m *Metrics) ConnectionClosed(duration time.Duration) {
	m.circuitsActive.Set(float64(decrementNonnegative(&m.activeCircuits, 1)))
	m.signalActivity()
	if m.delegate != nil {
		m.delegate.ConnectionClosed(duration)
	}
}

func (m *Metrics) ConnectionRequestHandled(status relaypb.Status) {
	if m.delegate != nil {
		m.delegate.ConnectionRequestHandled(status)
	}
}

func (m *Metrics) ReservationAllowed(isRenewal bool) {
	if !isRenewal {
		m.reservationsActive.Set(float64(m.activeReservations.Add(1)))
	}
	if m.delegate != nil {
		m.delegate.ReservationAllowed(isRenewal)
	}
}

func (m *Metrics) ReservationClosed(count int) {
	if count > 0 {
		m.reservationsActive.Set(float64(decrementNonnegative(&m.activeReservations, int64(count))))
	}
	if m.delegate != nil {
		m.delegate.ReservationClosed(count)
	}
}

func (m *Metrics) ReservationRequestHandled(status relaypb.Status) {
	if m.delegate != nil {
		m.delegate.ReservationRequestHandled(status)
	}
}

func (m *Metrics) BytesTransferred(count int) {
	if m.delegate != nil {
		m.delegate.BytesTransferred(count)
	}
}

func (m *Metrics) signalActivity() {
	select {
	case m.activityChanged <- struct{}{}:
	default:
	}
}

func decrementNonnegative(value *atomic.Int64, count int64) int64 {
	for {
		current := value.Load()
		next := current - count
		if next < 0 {
			next = 0
		}
		if value.CompareAndSwap(current, next) {
			return next
		}
	}
}

func validACLAction(value ACLAction) bool {
	return value == ACLReserve || value == ACLConnect
}

func validSourceAuthOutcome(value SourceAuthOutcome) bool {
	switch value {
	case SourceAuthAccepted, SourceAuthInvalidPeer, SourceAuthRateLimited,
		SourceAuthMalformed, SourceAuthUnbooked, SourceAuthInvalidToken,
		SourceAuthIdentityDenied, SourceAuthCapacityDenied, SourceAuthWriteFailed:
		return true
	default:
		return false
	}
}

func validProviderOperation(value ProviderOperation) bool {
	switch value {
	case ProviderClaim, ProviderActive, ProviderRecover, ProviderReady,
		ProviderHeartbeat, ProviderFail, ProviderRelinquish, ProviderStatus:
		return true
	default:
		return false
	}
}

func validOperationOutcome(value OperationOutcome) bool {
	switch value {
	case OperationSuccess, OperationEmpty, OperationRetryable, OperationTerminal, OperationRevoked:
		return true
	default:
		return false
	}
}

func validBookingEvent(value BookingEvent) bool {
	switch value {
	case BookingReady, BookingRecovered, BookingRenewed, BookingCanceled,
		BookingExpired, BookingFailed, BookingRelinquished, BookingReconciled:
		return true
	default:
		return false
	}
}

var _ relayv2.MetricsTracer = (*Metrics)(nil)
