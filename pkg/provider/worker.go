package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/aukilabs/hagall/pkg/telemetry"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
)

type DMS interface {
	Active(context.Context, string, uuid.UUID) ([]dmsclient.Assignment, error)
	Claim(context.Context, string, uuid.UUID) (*dmsclient.ClaimResult, error)
	Recover(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error)
	Heartbeat(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error)
	Ready(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID, dmsclient.Metadata) (*dmsclient.Assignment, error)
	Fail(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error)
	Relinquish(context.Context, string, uuid.UUID, uuid.UUID, uuid.UUID) (*dmsclient.Assignment, error)
}

const maximumHeartbeatBackoff = 10 * time.Second
const maximumDrainRelinquishConcurrency = 16

type Config struct {
	InitialBackoff              time.Duration
	MaximumBackoff              time.Duration
	EmptyClaimMaxJitterFraction float64
	HeartbeatMinimumFraction    float64
	HeartbeatMaximumFraction    float64
	ExpirySweepInterval         time.Duration
	Random                      RandomSource
	Now                         func() time.Time
}

func (c Config) validate() error {
	if c.InitialBackoff <= 0 || c.MaximumBackoff < c.InitialBackoff || c.MaximumBackoff > 30*time.Second {
		return errors.New("provider retry backoff is invalid")
	}
	if c.EmptyClaimMaxJitterFraction < 0 || c.EmptyClaimMaxJitterFraction > 0.20 {
		return errors.New("provider empty-claim jitter is invalid")
	}
	if c.HeartbeatMinimumFraction != 0.25 || c.HeartbeatMaximumFraction != 0.35 {
		return errors.New("provider heartbeat fractions must equal 0.25 and 0.35")
	}
	if c.ExpirySweepInterval <= 0 || c.ExpirySweepInterval > time.Second {
		return errors.New("provider expiry sweep interval must be in (0s,1s]")
	}
	if c.Random == nil || c.Now == nil {
		return errors.New("provider random source and clock are required")
	}
	return nil
}

type Options struct {
	DMS       DMS
	Registry  *booking.Registry
	Metadata  dmsclient.Metadata
	ClosePeer func(peer.ID) error
	Config    Config
	Observer  interface {
		ObserveProviderOperation(telemetry.ProviderOperation, telemetry.OperationOutcome)
		ObserveBookingEvent(telemetry.BookingEvent)
	}
}

type assignmentKey struct {
	assignment uuid.UUID
	epoch      uuid.UUID
}

type trackedAssignment struct {
	assignment dmsclient.Assignment
	cancel     context.CancelFunc
}

// RelinquishAllError separates local teardown failures from remote mutations
// that a later authoritative reassign status can safely supersede.
type RelinquishAllError struct {
	Local  error
	Remote error
}

func (e *RelinquishAllError) Error() string {
	return errors.Join(e.Local, e.Remote).Error()
}

func (e *RelinquishAllError) Unwrap() error {
	return errors.Join(e.Local, e.Remote)
}

// Worker owns every DMS child assigned to this provider incarnation. All
// network authority lives in Registry; the worker only performs bounded HTTP
// synchronization and closes target connections after registry revocation.
type Worker struct {
	dms       DMS
	registry  *booking.Registry
	metadata  dmsclient.Metadata
	closePeer func(peer.ID) error
	config    Config
	observer  interface {
		ObserveProviderOperation(telemetry.ProviderOperation, telemetry.OperationOutcome)
		ObserveBookingEvent(telemetry.BookingEvent)
	}
	authorityMu sync.Mutex
	claimMu     sync.Mutex

	mu             sync.Mutex
	token          string
	session        dmsclient.Session
	accepting      bool
	draining       bool
	gateGeneration uint64
	started        bool
	context        context.Context
	cancel         context.CancelFunc
	tracked        map[assignmentKey]*trackedAssignment

	wakeup          chan struct{}
	deadlineChanged chan struct{}
	availability    chan struct{}
	errors          chan error
	wait            sync.WaitGroup
	close           sync.Once
	closeErr        error
}

func New(options Options) (*Worker, error) {
	if options.DMS == nil || options.Registry == nil || options.ClosePeer == nil {
		return nil, errors.New("provider DMS client, registry, and peer closer are required")
	}
	if err := options.Config.validate(); err != nil {
		return nil, err
	}
	if len(options.Metadata.BaseAddresses) == 0 || len(options.Metadata.EndpointKeys) == 0 || options.Metadata.ConfigFingerprint == "" || options.Metadata.Limits.DurationSeconds == 0 || options.Metadata.Limits.DataBytesPerDirection <= 0 {
		return nil, errors.New("provider immutable relay metadata is incomplete")
	}
	return &Worker{
		dms: options.DMS, registry: options.Registry, metadata: cloneMetadata(options.Metadata),
		closePeer: options.ClosePeer, config: options.Config, observer: options.Observer,
		tracked: make(map[assignmentKey]*trackedAssignment), wakeup: make(chan struct{}, 1), deadlineChanged: make(chan struct{}, 1), availability: make(chan struct{}, 1), errors: make(chan error, 1),
	}, nil
}

// SetControl installs the latest peer-bound Node token and exact DMS session.
// It refreshes only the global session cap; assignment leases remain fenced by
// their own last authoritative mutation response.
func (w *Worker) SetControl(token string, session dmsclient.Session) error {
	if token == "" || session.ProviderSessionID == uuid.Nil {
		return errors.New("provider token and session are required")
	}
	if !sameMetadata(session.Metadata, w.metadata) {
		return errors.New("provider session metadata differs from the local immutable relay configuration")
	}
	w.authorityMu.Lock()
	err := w.registry.SetSession(booking.Session{
		ProviderSessionID: session.ProviderSessionID,
		EffectiveCapacity: int(session.EffectiveCapacity),
		SessionExpiresAt:  session.SessionExpiresAt,
		NodeJWTExpiresAt:  session.ProviderNodeJWTExpiresAt,
	})
	w.authorityMu.Unlock()
	if err != nil {
		return err
	}
	w.signalDeadline()
	w.mu.Lock()
	capacityChanged := w.session.EffectiveCapacity != session.EffectiveCapacity
	w.token = token
	w.session = session
	w.mu.Unlock()
	if capacityChanged {
		w.signalAvailability()
	}
	return nil
}

func (w *Worker) SetAccepting(accepting bool) {
	w.mu.Lock()
	if w.draining {
		accepting = false
	}
	changed := w.accepting != accepting
	if w.accepting != accepting {
		w.accepting = accepting
		w.gateGeneration++
	}
	w.mu.Unlock()
	if changed {
		w.signal()
	}
}

func (w *Worker) Accepting() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.accepting
}

func (w *Worker) AvailabilityChanged() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.availability
}

func (w *Worker) CapacityAvailable() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	capacity := int(w.session.EffectiveCapacity)
	w.mu.Unlock()
	used, _ := w.registry.Counts()
	return !w.registry.Draining() && capacity > 0 && used < capacity
}

func (w *Worker) ActiveAssignments() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.tracked)
}

// ActiveCount returns DMS's authoritative provider-assignment count without
// recovering, readying, or otherwise mutating any child. It is used only by
// the planned drain coordinator after the local claim barrier is closed.
func (w *Worker) ActiveCount(ctx context.Context) (int, error) {
	if w == nil {
		return 0, nil
	}
	token, session, err := w.control()
	if err != nil {
		return 0, err
	}
	assignments, err := w.dms.Active(ctx, token, session.ProviderSessionID)
	if err != nil {
		w.observeOperation(telemetry.ProviderActive, operationErrorOutcome(err))
		return 0, err
	}
	w.observeOperation(telemetry.ProviderActive, telemetry.OperationSuccess)
	return len(assignments), nil
}

// beginDrainGates is local and immediate: claims, RESERVE, source admission,
// and new CONNECT decisions stop, while heartbeat goroutines and already-open
// go-libp2p circuits continue until the bounded drain completes.
func (w *Worker) beginDrainGates() bool {
	if w == nil {
		return false
	}
	w.registry.BeginDrain()
	w.mu.Lock()
	w.draining = true
	changed := w.accepting
	if w.accepting {
		w.accepting = false
		w.gateGeneration++
	}
	w.mu.Unlock()
	if changed {
		w.signal()
	}
	return true
}

// BeginDrainContext closes every local admission/claim gate immediately, then
// joins a Claim or reconciliation that crossed the gate before it closed. The
// join is context-bounded so an unavailable DMS cannot make the process drain
// exceed its operator-provided deadline.
func (w *Worker) BeginDrainContext(ctx context.Context) error {
	w.BeginDrainGates()
	return w.JoinDrainBarrier(ctx)
}

// BeginDrainGates synchronously stops fresh local claims, relay-auth,
// reservations, and CONNECT authorization without waiting for an in-flight
// provider request. Application drain reports DMS status immediately after
// this step, then calls JoinDrainBarrier before reading authoritative work.
func (w *Worker) BeginDrainGates() {
	w.beginDrainGates()
}

// JoinDrainBarrier waits for a Claim or reconciliation that crossed the gate
// before it closed. DMS serializes its status update against that operation;
// after the join, an authoritative Active read contains the final result.
func (w *Worker) JoinDrainBarrier(ctx context.Context) error {
	if w == nil {
		return nil
	}
	// Join any Claim that crossed the gate before it closed. The claim path
	// retains this mutex until it has either installed or relinquished the
	// returned fence, so the drain's subsequent Active read is complete.
	if err := w.lockClaims(ctx); err != nil {
		return err
	}
	w.claimMu.Unlock()
	return nil
}

func (w *Worker) lockClaims(ctx context.Context) error {
	if ctx == nil {
		return errors.New("provider operation context is required")
	}
	for {
		if w.claimMu.TryLock() {
			return nil
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// BeginDrain preserves the existing synchronous worker API for callers that
// already own a bounded lifecycle context. Application drains use
// BeginDrainContext directly.
func (w *Worker) BeginDrain() {
	_ = w.BeginDrainContext(context.Background())
}

// Reconcile reconstructs local state exclusively from DMS. It must complete
// before the provider advertises accepting_bookings=true.
func (w *Worker) Reconcile(ctx context.Context) error {
	if w == nil {
		return errors.New("provider worker is required")
	}
	w.claimMu.Lock()
	defer w.claimMu.Unlock()
	w.mu.Lock()
	draining := w.draining
	w.mu.Unlock()
	if draining || w.registry.Draining() {
		return errors.New("provider worker is draining")
	}
	return w.reconcileLocked(ctx)
}

func (w *Worker) reconcileLocked(ctx context.Context) error {
	token, session, err := w.control()
	if err != nil {
		return err
	}
	assignments, err := w.dms.Active(ctx, token, session.ProviderSessionID)
	if err != nil {
		w.observeOperation(telemetry.ProviderActive, operationErrorOutcome(err))
		return fmt.Errorf("list active relay assignments: %w", err)
	}
	w.observeOperation(telemetry.ProviderActive, telemetry.OperationSuccess)
	seen := make(map[assignmentKey]struct{}, len(assignments))
	for index := range assignments {
		key, keyErr := requiredKey(assignments[index])
		if keyErr != nil {
			return keyErr
		}
		seen[key] = struct{}{}
	}
	// Remove generations no longer present in the authoritative Active view
	// before installing replacements. This lets an epoch rotation for the same
	// target converge without inheriting the old registry entry or admissions.
	w.mu.Lock()
	stale := make([]dmsclient.Assignment, 0)
	for key, tracked := range w.tracked {
		if _, exists := seen[key]; exists {
			continue
		}
		stale = append(stale, tracked.assignment)
	}
	w.mu.Unlock()
	for _, assignment := range stale {
		w.observeBooking(telemetry.BookingReconciled)
		w.revoke(assignment)
	}
	for index := range assignments {
		if err := w.reconcileAssignment(ctx, assignments[index]); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) reconcileAssignment(ctx context.Context, assignment dmsclient.Assignment) error {
	if assignment.Revoke || assignment.BookingState != dmsclient.BookingStateActive {
		w.observeAssignmentEnd(assignment)
		w.revoke(assignment)
		return nil
	}
	switch assignment.SlotState {
	case dmsclient.SlotStateRecovering:
		if !assignmentMetadataMatches(assignment, w.metadata) {
			return w.failRemote(ctx, assignment, "recovery metadata differs from live relay configuration")
		}
		key, err := requiredKey(assignment)
		if err != nil {
			return err
		}
		token, session, err := w.control()
		if err != nil {
			return err
		}
		recovered, err := w.dms.Recover(ctx, token, session.ProviderSessionID, key.assignment, key.epoch)
		if err != nil {
			w.observeOperation(telemetry.ProviderRecover, operationErrorOutcome(err))
			return fmt.Errorf("recover relay assignment %s: %w", key.assignment, err)
		}
		w.observeOperation(telemetry.ProviderRecover, telemetry.OperationSuccess)
		if recovered.SlotState != dmsclient.SlotStateStarting || recovered.Revoke {
			w.revoke(assignment)
			return errors.New("DMS recovery did not return a live starting lease")
		}
		if !assignmentMetadataMatches(*recovered, w.metadata) {
			return w.failRemote(ctx, *recovered, "recovered assignment metadata differs from live relay configuration")
		}
		if err := w.installAndReady(ctx, *recovered); err != nil {
			return err
		}
		w.observeBooking(telemetry.BookingRecovered)
		return nil
	case dmsclient.SlotStateStarting:
		if !assignmentMetadataMatches(assignment, w.metadata) {
			return w.failRemote(ctx, assignment, "starting assignment metadata differs from live relay configuration")
		}
		return w.installAndReady(ctx, assignment)
	case dmsclient.SlotStateReady:
		if !assignmentMetadataMatches(assignment, w.metadata) {
			return w.failRemote(ctx, assignment, "ready assignment metadata differs from live relay configuration")
		}
		return w.installReady(assignment)
	default:
		w.revoke(assignment)
		return nil
	}
}

func (w *Worker) installAndReady(ctx context.Context, assignment dmsclient.Assignment) error {
	authority, err := w.authority(assignment)
	if err != nil {
		return w.failRemote(ctx, assignment, err.Error())
	}
	w.authorityMu.Lock()
	err = w.registry.InstallStarting(authority)
	w.authorityMu.Unlock()
	if err != nil {
		return w.failRemote(ctx, assignment, err.Error())
	}
	w.signalDeadline()
	w.signalAvailability()
	key := assignmentKey{assignment: authority.Fence.AssignmentID, epoch: authority.Fence.ReservationEpoch}
	token, session, err := w.control()
	if err != nil {
		w.revoke(assignment)
		return err
	}
	ready, err := w.dms.Ready(ctx, token, session.ProviderSessionID, key.assignment, key.epoch, cloneMetadata(w.metadata))
	if err != nil {
		w.observeOperation(telemetry.ProviderReady, operationErrorOutcome(err))
		// The mutation may have committed. Fail closed locally; the next Active
		// reconciliation is the only source of truth.
		w.revoke(assignment)
		return fmt.Errorf("mark relay assignment %s ready: %w", key.assignment, err)
	}
	w.observeOperation(telemetry.ProviderReady, telemetry.OperationSuccess)
	if ready.Revoke || ready.SlotState != dmsclient.SlotStateReady {
		w.revoke(assignment)
		return errors.New("DMS ready response revoked or failed to ready the assignment")
	}
	return w.installReady(*ready)
}

func (w *Worker) installReady(assignment dmsclient.Assignment) error {
	authority, err := w.authority(assignment)
	if err != nil {
		return err
	}
	w.authorityMu.Lock()
	if err := w.registry.InstallStarting(authority); err != nil {
		w.authorityMu.Unlock()
		return err
	}
	if err := w.registry.ActivateReady(authority.TargetPeerID, authority.Fence); err != nil {
		w.authorityMu.Unlock()
		w.revoke(assignment)
		return err
	}
	newGeneration := w.track(assignment)
	w.authorityMu.Unlock()
	w.signalDeadline()
	if newGeneration {
		w.observeBooking(telemetry.BookingReady)
	}
	return nil
}

func (w *Worker) failRemote(ctx context.Context, assignment dmsclient.Assignment, reason string) error {
	key, keyErr := requiredKey(assignment)
	token, session, controlErr := w.control()
	reportContext := ctx
	w.mu.Lock()
	if w.context != nil {
		reportContext = w.context
	}
	w.mu.Unlock()
	revokeErr := w.revoke(assignment)
	if keyErr != nil {
		return errors.Join(revokeErr, fmt.Errorf("invalid DMS assignment: %s: %w", reason, keyErr))
	}
	if controlErr != nil {
		return errors.Join(revokeErr, errors.New(reason), controlErr)
	}
	_, failErr := w.dms.Fail(reportContext, token, session.ProviderSessionID, key.assignment, key.epoch)
	if failErr != nil {
		w.observeOperation(telemetry.ProviderFail, operationErrorOutcome(failErr))
		return errors.Join(revokeErr, errors.New(reason), fmt.Errorf("report retryable provider failure: %w", failErr))
	}
	w.observeOperation(telemetry.ProviderFail, telemetry.OperationSuccess)
	w.observeBooking(telemetry.BookingFailed)
	return revokeErr
}

func (w *Worker) authority(assignment dmsclient.Assignment) (booking.Authority, error) {
	key, err := requiredKey(assignment)
	if err != nil {
		return booking.Authority{}, err
	}
	token, session, err := w.control()
	_ = token
	if err != nil {
		return booking.Authority{}, err
	}
	if assignment.BookingState != dmsclient.BookingStateActive || assignment.Revoke || assignment.ProviderSessionID == nil || *assignment.ProviderSessionID != session.ProviderSessionID {
		return booking.Authority{}, errors.New("assignment does not retain current provider authority")
	}
	if assignment.EffectiveCapacity != session.EffectiveCapacity || assignment.SchedulingRevision != session.SchedulingRevision {
		return booking.Authority{}, errors.New("assignment scheduling principal differs from the current session")
	}
	if assignment.RelayCount < 1 || assignment.RelayCount > 3 || assignment.SlotIndex >= assignment.RelayCount {
		return booking.Authority{}, errors.New("assignment slot index or relay count is invalid")
	}
	if assignment.BookingID == uuid.Nil || assignment.SlotID == uuid.Nil || assignment.DomainID == uuid.Nil {
		return booking.Authority{}, errors.New("assignment booking, slot, and Domain IDs are required")
	}
	target, err := peer.Decode(assignment.TargetPeerID)
	if err != nil {
		return booking.Authority{}, errors.New("assignment target Peer ID is invalid")
	}
	if assignment.ProviderLeaseExpiresAt == nil {
		return booking.Authority{}, errors.New("assignment provider lease deadline is required")
	}
	deadlines := []time.Time{assignment.RequestedUntil, assignment.AuthorityExpiresAt, *assignment.ProviderLeaseExpiresAt, session.SessionExpiresAt, session.ProviderNodeJWTExpiresAt}
	now := w.config.Now().UTC()
	for _, deadline := range deadlines {
		if deadline.IsZero() || !now.Before(deadline) {
			return booking.Authority{}, errors.New("assignment contains expired authority")
		}
	}
	lease := assignment.ProviderLeaseExpiresAt.UTC()
	for _, capDeadline := range []time.Time{assignment.RequestedUntil, assignment.AuthorityExpiresAt, session.SessionExpiresAt, session.ProviderNodeJWTExpiresAt} {
		if lease.After(capDeadline.UTC()) {
			return booking.Authority{}, errors.New("assignment lease exceeds a controlling authority deadline")
		}
	}
	return booking.Authority{
		BookingID: assignment.BookingID, SlotID: assignment.SlotID, DomainID: assignment.DomainID,
		TargetPeerID:   target,
		Fence:          booking.Fence{ProviderSessionID: session.ProviderSessionID, AssignmentID: key.assignment, ReservationEpoch: key.epoch},
		RequestedUntil: assignment.RequestedUntil, AuthorityExpiresAt: assignment.AuthorityExpiresAt, ProviderLeaseExpiresAt: lease,
	}, nil
}

func (w *Worker) Start(parent context.Context) error {
	if parent == nil {
		return errors.New("provider worker context is required")
	}
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return errors.New("provider worker is already started")
	}
	if w.draining {
		w.mu.Unlock()
		return errors.New("provider worker is draining")
	}
	w.context, w.cancel = context.WithCancel(parent)
	w.started = true
	tracked := make([]*trackedAssignment, 0, len(w.tracked))
	for _, assignment := range w.tracked {
		tracked = append(tracked, assignment)
	}
	w.mu.Unlock()
	for _, assignment := range tracked {
		w.startHeartbeat(assignment)
	}
	w.wait.Add(2)
	go func() { defer w.wait.Done(); w.claimLoop() }()
	go func() { defer w.wait.Done(); w.expiryLoop() }()
	return nil
}

func (w *Worker) Errors() <-chan error { return w.errors }

func (w *Worker) claimLoop() {
	backoff := time.Duration(0)
	reconcileRequired := false
	for {
		generation, err := w.waitUntilClaimable()
		if err != nil {
			return
		}
		if !w.claimGenerationActive(generation) {
			continue
		}
		if reconcileRequired {
			w.claimMu.Lock()
			if !w.claimGenerationActive(generation) {
				w.claimMu.Unlock()
				continue
			}
			if reconcileErr := w.reconcileLocked(w.context); reconcileErr != nil {
				w.claimMu.Unlock()
				if isTerminalSessionError(reconcileErr) {
					w.SetAccepting(false)
					w.report(reconcileErr)
					return
				}
				backoff = nextBackoff(backoff, w.config.InitialBackoff, w.config.MaximumBackoff)
				if wait(w.context, backoff, w.wakeup) != nil {
					return
				}
				continue
			}
			w.claimMu.Unlock()
			reconcileRequired = false
			backoff = 0
			continue
		}
		w.claimMu.Lock()
		if !w.claimGenerationActive(generation) {
			w.claimMu.Unlock()
			continue
		}
		token, session, err := w.control()
		if err != nil {
			w.claimMu.Unlock()
			w.report(err)
			return
		}
		result, err := w.dms.Claim(w.context, token, session.ProviderSessionID)
		if err != nil {
			w.observeOperation(telemetry.ProviderClaim, operationErrorOutcome(err))
			if isTerminalSessionError(err) {
				w.SetAccepting(false)
				w.claimMu.Unlock()
				w.report(fmt.Errorf("claim relay booking: %w", err))
				return
			}
			// A response may have been lost after commit. Require a successful
			// Active reconciliation before any later claim.
			reconcileRequired = true
			backoff = nextBackoff(backoff, w.config.InitialBackoff, w.config.MaximumBackoff)
			w.claimMu.Unlock()
			if wait(w.context, backoff, w.wakeup) != nil {
				return
			}
			continue
		}
		backoff = 0
		if result == nil {
			w.claimMu.Unlock()
			w.report(errors.New("DMS claim returned a nil result"))
			return
		}
		if result.Assignment == nil {
			w.observeOperation(telemetry.ProviderClaim, telemetry.OperationEmpty)
			delay, delayErr := emptyClaimDelay(result.RetryAfter, w.config.EmptyClaimMaxJitterFraction, w.config.Random)
			if delayErr != nil {
				w.claimMu.Unlock()
				w.report(delayErr)
				return
			}
			w.claimMu.Unlock()
			if wait(w.context, delay, w.wakeup) != nil {
				return
			}
			continue
		}
		w.observeOperation(telemetry.ProviderClaim, telemetry.OperationSuccess)
		if !w.claimGenerationActive(generation) {
			if relinquishErr := w.relinquishRemote(w.context, *result.Assignment); relinquishErr != nil {
				reconcileRequired = true
			}
			w.claimMu.Unlock()
			continue
		}
		if err := w.installAndReady(w.context, *result.Assignment); err != nil {
			w.claimMu.Unlock()
			if w.context.Err() != nil {
				return
			}
			// Ready/fail may have committed even when their response was lost.
			// Reconcile before another claim so unseen DMS occupancy can never
			// let this provider exceed its authenticated capacity.
			reconcileRequired = true
			continue
		}
		w.claimMu.Unlock()
	}
}

func (w *Worker) waitUntilClaimable() (uint64, error) {
	for {
		w.mu.Lock()
		accepting := w.accepting
		draining := w.draining
		generation := w.gateGeneration
		capacity := int(w.session.EffectiveCapacity)
		ctx := w.context
		w.mu.Unlock()
		bookings, _ := w.registry.Counts()
		if accepting && !draining && !w.registry.Draining() && capacity > 0 && bookings < capacity {
			// Consume the state-change signal that made this iteration
			// claimable. A signal arriving after this point is preserved to
			// wake the subsequent Retry-After/backoff wait.
			select {
			case <-w.wakeup:
			default:
			}
			return generation, nil
		}
		if wait(ctx, time.Hour, w.wakeup) != nil {
			return 0, ctx.Err()
		}
	}
}

func (w *Worker) claimGenerationActive(generation uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.accepting && !w.draining && !w.registry.Draining() && w.gateGeneration == generation
}

func (w *Worker) relinquishRemote(ctx context.Context, assignment dmsclient.Assignment) error {
	key, err := requiredKey(assignment)
	if err != nil {
		return err
	}
	token, session, err := w.control()
	if err != nil {
		return err
	}
	_, err = w.dms.Relinquish(ctx, token, session.ProviderSessionID, key.assignment, key.epoch)
	if err != nil {
		w.observeOperation(telemetry.ProviderRelinquish, operationErrorOutcome(err))
	} else {
		w.observeOperation(telemetry.ProviderRelinquish, telemetry.OperationSuccess)
		w.observeBooking(telemetry.BookingRelinquished)
	}
	return err
}

func (w *Worker) expiryLoop() {
	for {
		delay := w.config.ExpirySweepInterval
		if deadline := w.registry.NextDeadline(); !deadline.IsZero() {
			untilDeadline := deadline.Sub(w.config.Now().UTC())
			if untilDeadline < 0 {
				untilDeadline = 0
			}
			if untilDeadline < delay {
				delay = untilDeadline
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-w.context.Done():
			stopTimer(timer)
			return
		case <-w.deadlineChanged:
			stopTimer(timer)
			continue
		case <-timer.C:
		}

		w.expireDue()
	}
}

func (w *Worker) expireDue() {
	w.authorityMu.Lock()
	expired := w.registry.Expire()
	for _, authority := range expired {
		w.observeBooking(telemetry.BookingExpired)
		w.removeTrackedFence(authority.Fence)
		if closeErr := w.closePeer(authority.TargetPeerID); closeErr != nil {
			w.report(fmt.Errorf("close expired relay target %s: %w", authority.TargetPeerID, closeErr))
		}
	}
	w.authorityMu.Unlock()
	if len(expired) > 0 {
		w.signal()
		w.signalAvailability()
	}
}

func (w *Worker) track(assignment dmsclient.Assignment) bool {
	key, err := requiredKey(assignment)
	if err != nil {
		return false
	}
	w.mu.Lock()
	if existing, exists := w.tracked[key]; exists {
		existing.assignment = assignment
		w.mu.Unlock()
		return false
	}
	tracked := &trackedAssignment{assignment: assignment}
	w.tracked[key] = tracked
	started := w.started
	w.mu.Unlock()
	if started {
		w.startHeartbeat(tracked)
	}
	w.signalAvailability()
	return true
}

func (w *Worker) startHeartbeat(tracked *trackedAssignment) {
	w.mu.Lock()
	if tracked.cancel != nil || w.context == nil {
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(w.context)
	tracked.cancel = cancel
	w.wait.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wait.Done()
		w.heartbeatLoop(ctx, tracked)
	}()
}

func (w *Worker) heartbeatLoop(ctx context.Context, tracked *trackedAssignment) {
	for {
		w.mu.Lock()
		assignment := tracked.assignment
		w.mu.Unlock()
		if assignment.ProviderLeaseExpiresAt == nil {
			w.revoke(assignment)
			return
		}
		delay, err := heartbeatDelay(w.config.Now().UTC(), *assignment.ProviderLeaseExpiresAt, w.config.HeartbeatMinimumFraction, w.config.HeartbeatMaximumFraction, w.config.Random)
		if err != nil {
			w.revoke(assignment)
			return
		}
		if wait(ctx, delay, nil) != nil {
			return
		}
		backoff := time.Duration(0)
		for {
			key, keyErr := requiredKey(assignment)
			if keyErr != nil {
				w.revoke(assignment)
				return
			}
			token, session, controlErr := w.control()
			if controlErr != nil {
				w.revoke(assignment)
				return
			}
			updated, heartbeatErr := w.dms.Heartbeat(ctx, token, session.ProviderSessionID, key.assignment, key.epoch)
			if heartbeatErr != nil {
				w.observeOperation(telemetry.ProviderHeartbeat, operationErrorOutcome(heartbeatErr))
				if isTerminalAssignmentError(heartbeatErr) {
					w.revoke(assignment)
					return
				}
				backoff = nextBackoff(
					backoff,
					min(w.config.InitialBackoff, maximumHeartbeatBackoff),
					min(w.config.MaximumBackoff, maximumHeartbeatBackoff),
				)
				remaining := assignment.ProviderLeaseExpiresAt.Sub(w.config.Now().UTC())
				if backoff <= 0 || backoff >= remaining || wait(ctx, backoff, nil) != nil {
					w.revoke(assignment)
					return
				}
				continue
			}
			if updated.Revoke {
				w.observeOperation(telemetry.ProviderHeartbeat, telemetry.OperationRevoked)
				w.observeAssignmentEnd(*updated)
				w.revoke(assignment)
				return
			}
			w.observeOperation(telemetry.ProviderHeartbeat, telemetry.OperationSuccess)
			if updated.SlotState != dmsclient.SlotStateReady || updated.ProviderLeaseExpiresAt == nil {
				_ = w.failRemote(ctx, *updated, "heartbeat response did not retain a ready typed lease")
				return
			}
			if !assignmentMetadataMatches(*updated, w.metadata) {
				_ = w.failRemote(ctx, *updated, "heartbeat metadata differs from live relay configuration")
				return
			}
			if ctx.Err() != nil || !w.config.Now().UTC().Before(knownAuthorityDeadline(assignment, session)) {
				w.revoke(assignment)
				return
			}
			authority, authorityErr := w.authority(*updated)
			if authorityErr != nil {
				w.revoke(assignment)
				return
			}
			// Refresh every returned authority deadline atomically. InstallStarting
			// preserves the ready bit for the same exact generation.
			key, keyErr = requiredKey(assignment)
			if keyErr != nil {
				w.revoke(assignment)
				return
			}
			w.authorityMu.Lock()
			w.mu.Lock()
			current, currentExists := w.tracked[key]
			if ctx.Err() != nil || !currentExists || current != tracked {
				w.mu.Unlock()
				w.authorityMu.Unlock()
				return
			}
			if err := w.registry.RefreshReady(authority); err != nil {
				w.mu.Unlock()
				w.authorityMu.Unlock()
				w.revoke(assignment)
				return
			}
			tracked.assignment = *updated
			w.mu.Unlock()
			w.authorityMu.Unlock()
			w.signalDeadline()
			w.observeBooking(telemetry.BookingRenewed)
			break
		}
	}
}

func (w *Worker) revoke(assignment dmsclient.Assignment) error {
	key, err := requiredKey(assignment)
	if err != nil {
		return err
	}
	target, err := peer.Decode(assignment.TargetPeerID)
	if err != nil {
		return err
	}
	w.authorityMu.Lock()
	defer w.authorityMu.Unlock()

	w.mu.Lock()
	trackedRemoved := false
	sessionID := w.session.ProviderSessionID
	if assignment.ProviderSessionID != nil {
		sessionID = *assignment.ProviderSessionID
	}
	fence := booking.Fence{ProviderSessionID: sessionID, AssignmentID: key.assignment, ReservationEpoch: key.epoch}
	if tracked, exists := w.tracked[key]; exists {
		if tracked.cancel != nil {
			tracked.cancel()
		}
		delete(w.tracked, key)
		trackedRemoved = true
	}
	w.mu.Unlock()
	var result error
	removed, authorityRemoved := w.registry.RemoveExact(target, fence)
	if authorityRemoved {
		if closeErr := w.closePeer(removed); closeErr != nil {
			w.report(fmt.Errorf("close revoked relay target %s: %w", removed, closeErr))
			result = closeErr
		}
	}
	if trackedRemoved || authorityRemoved {
		w.signal()
		w.signalDeadline()
		w.signalAvailability()
	}
	return result
}

func knownAuthorityDeadline(assignment dmsclient.Assignment, session dmsclient.Session) time.Time {
	deadlines := []time.Time{assignment.RequestedUntil, assignment.AuthorityExpiresAt, session.SessionExpiresAt, session.ProviderNodeJWTExpiresAt}
	if assignment.ProviderLeaseExpiresAt != nil {
		deadlines = append(deadlines, *assignment.ProviderLeaseExpiresAt)
	}
	earliest := deadlines[0]
	for _, deadline := range deadlines[1:] {
		if deadline.Before(earliest) {
			earliest = deadline
		}
	}
	return earliest.UTC()
}

func (w *Worker) removeTrackedFence(fence booking.Fence) {
	key := assignmentKey{assignment: fence.AssignmentID, epoch: fence.ReservationEpoch}
	w.mu.Lock()
	defer w.mu.Unlock()
	tracked, exists := w.tracked[key]
	if !exists || tracked.assignment.ProviderSessionID == nil || *tracked.assignment.ProviderSessionID != fence.ProviderSessionID {
		return
	}
	if tracked.cancel != nil {
		tracked.cancel()
	}
	delete(w.tracked, key)
}

// RelinquishAll asks DMS to clear every currently tracked child before local
// teardown. It uses authoritative Active snapshots rather than only local
// tracking, so a claim/Ready response lost at the drain boundary cannot leave
// an unseen DMS lease behind. A second Active read resolves ambiguous
// Relinquish responses and supplies the exact remaining count.
func (w *Worker) RelinquishAll(ctx context.Context) (int, error) {
	if w == nil {
		return 0, nil
	}
	if err := w.lockClaims(ctx); err != nil {
		return w.ActiveAssignments(), err
	}
	defer w.claimMu.Unlock()
	w.mu.Lock()
	token := w.token
	session := w.session
	w.mu.Unlock()
	assignments, err := w.dms.Active(ctx, token, session.ProviderSessionID)
	if err != nil {
		w.observeOperation(telemetry.ProviderActive, operationErrorOutcome(err))
		return w.ActiveAssignments(), fmt.Errorf("list assignments before relay drain: %w", err)
	}
	w.observeOperation(telemetry.ProviderActive, telemetry.OperationSuccess)
	slices.SortFunc(assignments, func(left, right dmsclient.Assignment) int {
		if left.AssignmentID == nil || right.AssignmentID == nil {
			return 0
		}
		return strings.Compare(left.AssignmentID.String(), right.AssignmentID.String())
	})
	type relinquishResult struct {
		assignment dmsclient.Assignment
		err        error
	}
	results := make(chan relinquishResult, len(assignments))
	semaphore := make(chan struct{}, maximumDrainRelinquishConcurrency)
	var relinquishWait sync.WaitGroup
	for _, assignment := range assignments {
		assignment := assignment
		relinquishWait.Add(1)
		go func() {
			defer relinquishWait.Done()
			key, keyErr := requiredKey(assignment)
			if keyErr != nil {
				results <- relinquishResult{assignment: assignment, err: keyErr}
				return
			}
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				results <- relinquishResult{assignment: assignment, err: ctx.Err()}
				return
			}
			_, relinquishErr := w.dms.Relinquish(ctx, token, session.ProviderSessionID, key.assignment, key.epoch)
			<-semaphore
			results <- relinquishResult{assignment: assignment, err: relinquishErr}
		}()
	}
	go func() {
		relinquishWait.Wait()
		close(results)
	}()
	var mutationErrors error
	var localErrors error
	for result := range results {
		if result.err != nil {
			w.observeOperation(telemetry.ProviderRelinquish, operationErrorOutcome(result.err))
			mutationErrors = errors.Join(mutationErrors, result.err)
			// The request may have committed despite a lost response. The
			// pre-release circuit grace is already over, so fail closed locally
			// for this exact generation before the authoritative Active read.
			localErrors = errors.Join(localErrors, w.revoke(result.assignment))
			continue
		}
		w.observeOperation(telemetry.ProviderRelinquish, telemetry.OperationSuccess)
		w.observeBooking(telemetry.BookingRelinquished)
		localErrors = errors.Join(localErrors, w.revoke(result.assignment))
	}
	remaining, err := w.dms.Active(ctx, token, session.ProviderSessionID)
	if err != nil {
		w.observeOperation(telemetry.ProviderActive, operationErrorOutcome(err))
		return len(assignments), &RelinquishAllError{
			Local:  localErrors,
			Remote: errors.Join(mutationErrors, fmt.Errorf("list assignments after relay drain: %w", err)),
		}
	}
	w.observeOperation(telemetry.ProviderActive, telemetry.OperationSuccess)
	remote := make(map[assignmentKey]struct{}, len(remaining))
	for _, assignment := range remaining {
		key, keyErr := requiredKey(assignment)
		if keyErr != nil {
			mutationErrors = errors.Join(mutationErrors, keyErr)
			continue
		}
		remote[key] = struct{}{}
	}
	w.mu.Lock()
	stale := make([]dmsclient.Assignment, 0)
	for key, tracked := range w.tracked {
		if _, exists := remote[key]; !exists {
			stale = append(stale, tracked.assignment)
		}
	}
	w.mu.Unlock()
	for _, assignment := range stale {
		localErrors = errors.Join(localErrors, w.revoke(assignment))
	}
	if len(remaining) == 0 {
		// The authoritative read proves any earlier transport error committed.
		return 0, localErrors
	}
	if mutationErrors == nil {
		mutationErrors = errors.New("DMS retained relay assignments after relinquish")
	}
	return len(remaining), &RelinquishAllError{Local: localErrors, Remote: mutationErrors}
}

func (w *Worker) Shutdown(ctx context.Context) error {
	return w.shutdown(ctx, true)
}

// ShutdownPreserving stops local authority without calling per-assignment
// relinquish. It is used only after an exact restart_same_identity session
// DELETE has atomically converted ready children to recovery holds.
func (w *Worker) ShutdownPreserving(ctx context.Context) error {
	return w.shutdown(ctx, false)
}

func (w *Worker) shutdown(ctx context.Context, relinquish bool) error {
	if w == nil {
		return nil
	}
	w.close.Do(func() {
		w.beginDrainGates()
		w.mu.Lock()
		if w.cancel != nil {
			w.cancel()
		}
		w.mu.Unlock()
		// Cancel first so any in-flight HTTP request holding claimMu observes
		// shutdown before this join. Production DMS calls are context-bound.
		w.claimMu.Lock()
		w.claimMu.Unlock()
		w.wait.Wait()

		w.mu.Lock()
		assignments := make([]dmsclient.Assignment, 0, len(w.tracked))
		for _, tracked := range w.tracked {
			assignments = append(assignments, tracked.assignment)
		}
		token := w.token
		session := w.session
		w.mu.Unlock()
		var result error
		for _, assignment := range assignments {
			key, err := requiredKey(assignment)
			result = errors.Join(result, w.revoke(assignment))
			if relinquish && err == nil && token != "" && session.ProviderSessionID != uuid.Nil {
				_, relinquishErr := w.dms.Relinquish(ctx, token, session.ProviderSessionID, key.assignment, key.epoch)
				if relinquishErr != nil {
					w.observeOperation(telemetry.ProviderRelinquish, operationErrorOutcome(relinquishErr))
				} else {
					w.observeOperation(telemetry.ProviderRelinquish, telemetry.OperationSuccess)
					w.observeBooking(telemetry.BookingRelinquished)
				}
				result = errors.Join(result, relinquishErr)
			}
		}
		w.authorityMu.Lock()
		for _, target := range w.registry.ClearSession(session.ProviderSessionID) {
			result = errors.Join(result, w.closePeer(target))
		}
		w.authorityMu.Unlock()
		w.closeErr = result
	})
	return w.closeErr
}

func (w *Worker) control() (string, dmsclient.Session, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.token == "" || w.session.ProviderSessionID == uuid.Nil {
		return "", dmsclient.Session{}, errors.New("provider control authority is unavailable")
	}
	return w.token, w.session, nil
}

func (w *Worker) report(err error) {
	if err == nil {
		return
	}
	select {
	case w.errors <- err:
	default:
	}
}

func (w *Worker) observeOperation(operation telemetry.ProviderOperation, outcome telemetry.OperationOutcome) {
	if w != nil && w.observer != nil {
		w.observer.ObserveProviderOperation(operation, outcome)
	}
}

func (w *Worker) observeBooking(event telemetry.BookingEvent) {
	if w != nil && w.observer != nil {
		w.observer.ObserveBookingEvent(event)
	}
}

func (w *Worker) observeAssignmentEnd(assignment dmsclient.Assignment) {
	switch assignment.BookingState {
	case dmsclient.BookingStateCanceled:
		w.observeBooking(telemetry.BookingCanceled)
	case dmsclient.BookingStateExpired:
		w.observeBooking(telemetry.BookingExpired)
	case dmsclient.BookingStateFailed:
		w.observeBooking(telemetry.BookingFailed)
	}
}

func operationErrorOutcome(err error) telemetry.OperationOutcome {
	if isTerminalAssignmentError(err) || isTerminalSessionError(err) {
		return telemetry.OperationTerminal
	}
	return telemetry.OperationRetryable
}

func (w *Worker) signal() {
	select {
	case w.wakeup <- struct{}{}:
	default:
	}
}

func (w *Worker) signalDeadline() {
	select {
	case w.deadlineChanged <- struct{}{}:
	default:
	}
}

func (w *Worker) signalAvailability() {
	select {
	case w.availability <- struct{}{}:
	default:
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func requiredKey(assignment dmsclient.Assignment) (assignmentKey, error) {
	if assignment.AssignmentID == nil || assignment.ReservationEpoch == nil || *assignment.AssignmentID == uuid.Nil || *assignment.ReservationEpoch == uuid.Nil {
		return assignmentKey{}, errors.New("DMS assignment is missing its assignment or reservation-epoch fence")
	}
	return assignmentKey{assignment: *assignment.AssignmentID, epoch: *assignment.ReservationEpoch}, nil
}

func assignmentMetadataMatches(assignment dmsclient.Assignment, expected dmsclient.Metadata) bool {
	return assignment.Metadata != nil && sameMetadata(*assignment.Metadata, expected)
}

func sameMetadata(left, right dmsclient.Metadata) bool {
	return slices.Equal(left.BaseAddresses, right.BaseAddresses) &&
		slices.Equal(left.EndpointKeys, right.EndpointKeys) &&
		left.Limits == right.Limits && left.ConfigFingerprint == right.ConfigFingerprint
}

func cloneMetadata(value dmsclient.Metadata) dmsclient.Metadata {
	value.BaseAddresses = slices.Clone(value.BaseAddresses)
	value.EndpointKeys = slices.Clone(value.EndpointKeys)
	return value
}

func isTerminalSessionError(err error) bool {
	var apiErr *dmsclient.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusGone || apiErr.StatusCode == http.StatusForbidden
}

func isTerminalAssignmentError(err error) bool { return isTerminalSessionError(err) }

func wait(ctx context.Context, delay time.Duration, wake <-chan struct{}) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	case <-wake:
		return nil
	}
}
