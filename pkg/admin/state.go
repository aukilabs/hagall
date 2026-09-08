// Package admin owns the relay process's private lifecycle and observation
// HTTP surfaces. It deliberately has no access to provider credentials or
// booking identities.
package admin

import (
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// AcceptingState is the bounded operator-visible reason for the current
// scheduling state.
type AcceptingState string

const (
	AcceptingStateNotReady        AcceptingState = "not_ready"
	AcceptingStateBootstrapClosed AcceptingState = "bootstrap_closed"
	AcceptingStateControlStale    AcceptingState = "control_stale"
	AcceptingStateSaturated       AcceptingState = "saturated"
	AcceptingStateDraining        AcceptingState = "draining"
	AcceptingStateAccepting       AcceptingState = "accepting"
	AcceptingStateTerminal        AcceptingState = "terminal"
)

// SchedulingSnapshot is the exact /accepting response. It contains only
// low-cardinality state and aggregate capacity; no booking identities appear.
type SchedulingSnapshot struct {
	State             AcceptingState `json:"state"`
	AcceptingBookings bool           `json:"accepting_bookings"`
	Draining          bool           `json:"draining"`
	EffectiveCapacity uint32         `json:"effective_capacity"`
	UsedBookingSlots  uint32         `json:"used_booking_slots"`
	FreeBookingSlots  uint32         `json:"free_booking_slots"`
}

// StateSnapshot atomically captures liveness, data-plane readiness, and the
// current scheduling observation.
type StateSnapshot struct {
	Alive      bool
	Ready      bool
	Scheduling SchedulingSnapshot
}

type State struct {
	mu           sync.RWMutex
	snapshot     StateSnapshot
	drainStarted bool
}

func NewState() *State {
	return &State{snapshot: StateSnapshot{
		Alive: true,
		Scheduling: SchedulingSnapshot{
			State: AcceptingStateNotReady,
		},
	}}
}

func (s *State) SetReady(ready bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.snapshot.Ready = ready && s.snapshot.Alive
	if !s.snapshot.Ready && !s.drainStarted && s.snapshot.Scheduling.State != AcceptingStateTerminal {
		s.setSchedulingLocked(AcceptingStateNotReady, s.snapshot.Scheduling.EffectiveCapacity, s.snapshot.Scheduling.UsedBookingSlots)
	}
	s.mu.Unlock()
}

func (s *State) SetAlive(alive bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.snapshot.Alive = alive
	if !alive {
		s.snapshot.Ready = false
		s.setSchedulingLocked(AcceptingStateTerminal, s.snapshot.Scheduling.EffectiveCapacity, s.snapshot.Scheduling.UsedBookingSlots)
	} else if s.snapshot.Scheduling.State == AcceptingStateTerminal {
		s.setSchedulingLocked(AcceptingStateNotReady, s.snapshot.Scheduling.EffectiveCapacity, s.snapshot.Scheduling.UsedBookingSlots)
	}
	s.mu.Unlock()
}

// SetScheduling atomically publishes a bounded reason and aggregate slot
// counts. Accepting/draining/free values are derived rather than trusted from
// callers. An accepting update fails closed unless the process is live, ready,
// and has at least one free slot.
func (s *State) SetScheduling(state AcceptingState, effectiveCapacity, usedBookingSlots uint32) error {
	if s == nil {
		return errors.New("admin state is required")
	}
	if err := validateSchedulingInput(state, effectiveCapacity, usedBookingSlots); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snapshot.Alive {
		state = AcceptingStateTerminal
	} else if s.drainStarted {
		state = AcceptingStateDraining
	} else if !s.snapshot.Ready && state != AcceptingStateDraining && state != AcceptingStateTerminal {
		state = AcceptingStateNotReady
	}
	s.setSchedulingLocked(state, effectiveCapacity, usedBookingSlots)
	return nil
}

// BeginDrain synchronously closes the scheduling gate while preserving the
// most recent aggregate capacity snapshot.
func (s *State) BeginDrain() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.snapshot.Alive {
		s.mu.Unlock()
		return
	}
	s.drainStarted = true
	s.setSchedulingLocked(AcceptingStateDraining, s.snapshot.Scheduling.EffectiveCapacity, s.snapshot.Scheduling.UsedBookingSlots)
	s.mu.Unlock()
}

func (s *State) setSchedulingLocked(state AcceptingState, effectiveCapacity, usedBookingSlots uint32) {
	freeBookingSlots := effectiveCapacity - usedBookingSlots
	s.snapshot.Scheduling = SchedulingSnapshot{
		State:             state,
		AcceptingBookings: state == AcceptingStateAccepting,
		Draining:          state == AcceptingStateDraining,
		EffectiveCapacity: effectiveCapacity,
		UsedBookingSlots:  usedBookingSlots,
		FreeBookingSlots:  freeBookingSlots,
	}
}

func validateSchedulingInput(state AcceptingState, effectiveCapacity, usedBookingSlots uint32) error {
	switch state {
	case AcceptingStateNotReady,
		AcceptingStateBootstrapClosed,
		AcceptingStateControlStale,
		AcceptingStateSaturated,
		AcceptingStateDraining,
		AcceptingStateAccepting,
		AcceptingStateTerminal:
	default:
		return fmt.Errorf("invalid relay accepting state %q", state)
	}
	if usedBookingSlots > effectiveCapacity {
		return errors.New("used booking slots exceed effective capacity")
	}
	freeBookingSlots := effectiveCapacity - usedBookingSlots
	if state == AcceptingStateAccepting && freeBookingSlots == 0 {
		return errors.New("accepting relay must have free booking capacity")
	}
	if state == AcceptingStateSaturated && (effectiveCapacity == 0 || freeBookingSlots != 0) {
		return errors.New("saturated relay must have a positive fully used capacity")
	}
	return nil
}

func (s *State) Snapshot() StateSnapshot {
	if s == nil {
		return StateSnapshot{Scheduling: SchedulingSnapshot{State: AcceptingStateTerminal}}
	}
	s.mu.RLock()
	snapshot := s.snapshot
	s.mu.RUnlock()
	return snapshot
}

func (s *State) Ready() bool {
	return s.Snapshot().Ready
}

func (s *State) Alive() bool {
	return s.Snapshot().Alive
}

func (s *State) Accepting() bool {
	return s.Snapshot().Scheduling.AcceptingBookings
}

func (s *State) Draining() bool {
	return s.Snapshot().Scheduling.Draining
}

func (s *State) RegisterMetrics(registry *prometheus.Registry) error {
	if s == nil || registry == nil {
		return errors.New("admin state and Prometheus registry are required")
	}
	collectors := []prometheus.Collector{
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "auki",
			Subsystem: "relay_node",
			Name:      "data_plane_ready",
			Help:      "Whether the relay data plane has passed every startup readiness gate.",
		}, func() float64 {
			return boolGauge(s.Snapshot().Ready)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "auki",
			Subsystem: "relay_node",
			Name:      "accepting_bookings",
			Help:      "Whether the relay is currently eligible to accept new bookings.",
		}, func() float64 {
			return boolGauge(s.Snapshot().Scheduling.AcceptingBookings)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: "auki",
			Subsystem: "relay_node",
			Name:      "draining",
			Help:      "Whether the relay is executing a bounded shutdown drain.",
		}, func() float64 {
			return boolGauge(s.Snapshot().Scheduling.Draining)
		}),
	}
	for _, collector := range collectors {
		if err := registry.Register(collector); err != nil {
			return err
		}
	}
	return nil
}

func boolGauge(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
