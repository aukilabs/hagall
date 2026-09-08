// Package booking owns the relay's bounded, in-memory view of DMS booking
// authority. It is the only state consulted by the Circuit Relay ACL; network
// and DMS operations deliberately remain outside its locks.
package booking

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
)

const MaximumProviderBookings = 256

var (
	ErrAuthorityExpired   = errors.New("relay booking authority is expired")
	ErrBookingCapacity    = errors.New("relay booking registry capacity is exhausted")
	ErrAdmissionCapacity  = errors.New("relay admission cache capacity is exhausted")
	ErrSessionConflict    = errors.New("relay provider session conflicts with installed authority")
	ErrTargetConflict     = errors.New("relay target already has another booking generation")
	ErrAssignmentConflict = errors.New("relay assignment already belongs to another target")
	ErrStaleFence         = errors.New("relay booking generation fence is stale")
	ErrAuthorityNotReady  = errors.New("relay booking authority is not ready")
	ErrAuthorityNotFound  = errors.New("relay booking authority was not found")
)

type Config struct {
	MaximumBookings   int
	MaximumAdmissions int
	AdmissionTTL      time.Duration
	Now               func() time.Time
}

type Fence struct {
	ProviderSessionID uuid.UUID
	AssignmentID      uuid.UUID
	ReservationEpoch  uuid.UUID
}

func (f Fence) validate() error {
	if f.ProviderSessionID == uuid.Nil || f.AssignmentID == uuid.Nil || f.ReservationEpoch == uuid.Nil {
		return errors.New("provider session, assignment, and reservation epoch are required")
	}
	return nil
}

type Session struct {
	ProviderSessionID uuid.UUID
	EffectiveCapacity int
	SessionExpiresAt  time.Time
	NodeJWTExpiresAt  time.Time
}

type Authority struct {
	BookingID              uuid.UUID
	SlotID                 uuid.UUID
	DomainID               uuid.UUID
	TargetPeerID           peer.ID
	Fence                  Fence
	RequestedUntil         time.Time
	AuthorityExpiresAt     time.Time
	ProviderLeaseExpiresAt time.Time
}

func (a Authority) validate() error {
	if a.BookingID == uuid.Nil || a.SlotID == uuid.Nil || a.DomainID == uuid.Nil {
		return errors.New("booking, slot, and Domain IDs are required")
	}
	if a.TargetPeerID == "" {
		return errors.New("target Peer ID is required")
	}
	if err := a.Fence.validate(); err != nil {
		return err
	}
	if a.RequestedUntil.IsZero() || a.AuthorityExpiresAt.IsZero() || a.ProviderLeaseExpiresAt.IsZero() {
		return errors.New("requested, booking-authority, and provider-lease deadlines are required")
	}
	return nil
}

type Snapshot struct {
	Authority      Authority
	EffectiveUntil time.Time
}

type ExpiredAuthority struct {
	TargetPeerID peer.ID
	Fence        Fence
}

type AdmissionRequest struct {
	SourcePeerID        peer.ID
	DomainID            uuid.UUID
	TargetPeerID        peer.ID
	ExpectedFence       Fence
	LiteralJWTExpiresAt time.Time
}

type admissionKey struct {
	source peer.ID
	domain uuid.UUID
	target peer.ID
}

type admission struct {
	fence         Fence
	acceptedUntil time.Time
}

type authorityState struct {
	authority Authority
	ready     bool
}

// Registry is safe for concurrent worker, authentication-handler, and ACL use.
// ACL lookups are O(1) and never perform expiry scans or external work.
type Registry struct {
	maximumBookings   int
	maximumAdmissions int
	admissionTTL      time.Duration
	now               func() time.Time

	mu                 sync.RWMutex
	session            Session
	draining           bool
	authorities        map[peer.ID]authorityState
	byAssignment       map[uuid.UUID]peer.ID
	admissions         map[admissionKey]admission
	admissionsByTarget map[peer.ID]map[admissionKey]struct{}
}

func New(config Config) (*Registry, error) {
	if config.MaximumBookings < 1 || config.MaximumBookings > MaximumProviderBookings {
		return nil, fmt.Errorf("maximum relay bookings must be in [1,%d]", MaximumProviderBookings)
	}
	if config.MaximumAdmissions < 1 {
		return nil, errors.New("maximum relay admissions must be positive")
	}
	if config.AdmissionTTL <= 0 || config.AdmissionTTL > 30*time.Second {
		return nil, errors.New("relay admission TTL must be in (0s,30s]")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Registry{
		maximumBookings:    config.MaximumBookings,
		maximumAdmissions:  config.MaximumAdmissions,
		admissionTTL:       config.AdmissionTTL,
		now:                now,
		authorities:        make(map[peer.ID]authorityState, config.MaximumBookings),
		byAssignment:       make(map[uuid.UUID]peer.ID, config.MaximumBookings),
		admissions:         make(map[admissionKey]admission, config.MaximumAdmissions),
		admissionsByTarget: make(map[peer.ID]map[admissionKey]struct{}, config.MaximumBookings),
	}, nil
}

// SetSession installs or refreshes the literal provider-session authority. A
// new session cannot inherit entries from an old process generation.
func (r *Registry) SetSession(session Session) error {
	if r == nil {
		return errors.New("relay booking registry is required")
	}
	if session.ProviderSessionID == uuid.Nil {
		return errors.New("provider session ID is required")
	}
	if session.EffectiveCapacity < 1 || session.EffectiveCapacity > r.maximumBookings {
		return ErrBookingCapacity
	}
	session.SessionExpiresAt = session.SessionExpiresAt.UTC()
	session.NodeJWTExpiresAt = session.NodeJWTExpiresAt.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	if !now.Before(session.SessionExpiresAt) || !now.Before(session.NodeJWTExpiresAt) {
		return ErrAuthorityExpired
	}
	if r.session.ProviderSessionID == session.ProviderSessionID &&
		(!now.Before(r.session.SessionExpiresAt) || !now.Before(r.session.NodeJWTExpiresAt)) {
		return ErrAuthorityExpired
	}
	if r.session.ProviderSessionID != uuid.Nil && r.session.ProviderSessionID != session.ProviderSessionID && len(r.authorities) != 0 {
		return ErrSessionConflict
	}
	if len(r.authorities) > session.EffectiveCapacity {
		return ErrBookingCapacity
	}
	if r.session.ProviderSessionID != session.ProviderSessionID {
		r.clearAdmissionsLocked()
	}
	r.session = session
	return nil
}

// InstallStarting installs reservation authority before the worker reports the
// child ready. Source admission and CONNECT remain disabled until ActivateReady.
func (r *Registry) InstallStarting(authority Authority) error {
	if r == nil {
		return errors.New("relay booking registry is required")
	}
	if err := authority.validate(); err != nil {
		return err
	}
	authority = normalizeAuthority(authority)

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	if r.session.ProviderSessionID == uuid.Nil || authority.Fence.ProviderSessionID != r.session.ProviderSessionID {
		return ErrSessionConflict
	}
	if _, active := r.effectiveDeadlineLocked(authority, now); !active {
		return ErrAuthorityExpired
	}
	if existing, exists := r.authorities[authority.TargetPeerID]; exists {
		if existing.authority.Fence != authority.Fence {
			return ErrTargetConflict
		}
		if !sameImmutableAuthority(existing.authority, authority) {
			return ErrTargetConflict
		}
		if _, active := r.effectiveDeadlineLocked(existing.authority, now); !active {
			return ErrAuthorityExpired
		}
		existing.authority = authority
		r.authorities[authority.TargetPeerID] = existing
		return nil
	}
	if target, exists := r.byAssignment[authority.Fence.AssignmentID]; exists && target != authority.TargetPeerID {
		return ErrAssignmentConflict
	}
	if len(r.authorities) >= r.session.EffectiveCapacity || len(r.authorities) >= r.maximumBookings {
		return ErrBookingCapacity
	}
	r.authorities[authority.TargetPeerID] = authorityState{authority: authority}
	r.byAssignment[authority.Fence.AssignmentID] = authority.TargetPeerID
	return nil
}

func (r *Registry) ActivateReady(target peer.ID, fence Fence) error {
	if r == nil {
		return errors.New("relay booking registry is required")
	}
	if target == "" {
		return errors.New("target Peer ID is required")
	}
	if err := fence.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	state, exists := r.authorities[target]
	if !exists {
		return ErrAuthorityNotFound
	}
	if state.authority.Fence != fence {
		return ErrStaleFence
	}
	if _, active := r.effectiveDeadlineLocked(state.authority, now); !active {
		return ErrAuthorityExpired
	}
	state.ready = true
	r.authorities[target] = state
	return nil
}

// UpdateLease applies only to the exact current generation. It cannot revive
// an expired authority; the worker must remove/reconcile such a generation.
func (r *Registry) UpdateLease(target peer.ID, fence Fence, expiresAt time.Time) error {
	if r == nil {
		return errors.New("relay booking registry is required")
	}
	if target == "" || expiresAt.IsZero() {
		return errors.New("target Peer ID and provider-lease deadline are required")
	}
	expiresAt = expiresAt.UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	if !now.Before(expiresAt) {
		return ErrAuthorityExpired
	}
	state, exists := r.authorities[target]
	if !exists {
		return ErrAuthorityNotFound
	}
	if state.authority.Fence != fence {
		return ErrStaleFence
	}
	if _, active := r.effectiveDeadlineLocked(state.authority, now); !active {
		return ErrAuthorityExpired
	}
	state.authority.ProviderLeaseExpiresAt = expiresAt
	if _, active := r.effectiveDeadlineLocked(state.authority, now); !active {
		return ErrAuthorityExpired
	}
	r.authorities[target] = state
	return nil
}

// RefreshReady atomically renews every returned deadline only while the exact
// ready generation still exists and remains live under its previously known
// deadlines. Unlike InstallStarting it can never recreate an expired/revoked
// generation after a concurrent sweeper or reassignment.
func (r *Registry) RefreshReady(authority Authority) error {
	if r == nil {
		return errors.New("relay booking registry is required")
	}
	if err := authority.validate(); err != nil {
		return err
	}
	authority = normalizeAuthority(authority)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	state, exists := r.authorities[authority.TargetPeerID]
	if !exists {
		return ErrAuthorityNotFound
	}
	if state.authority.Fence != authority.Fence {
		return ErrStaleFence
	}
	if !state.ready {
		return ErrAuthorityNotReady
	}
	if !sameImmutableAuthority(state.authority, authority) {
		return ErrTargetConflict
	}
	if _, active := r.effectiveDeadlineLocked(state.authority, now); !active {
		return ErrAuthorityExpired
	}
	if _, active := r.effectiveDeadlineLocked(authority, now); !active {
		return ErrAuthorityExpired
	}
	state.authority = authority
	r.authorities[authority.TargetPeerID] = state
	return nil
}

// SnapshotForAdmission captures the exact ready generation that a potentially
// slow JWT verification must match again when it commits admission.
func (r *Registry) SnapshotForAdmission(target peer.ID, domain uuid.UUID) (Snapshot, bool) {
	if r == nil || target == "" || domain == uuid.Nil {
		return Snapshot{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.draining {
		return Snapshot{}, false
	}
	now := r.now().UTC()
	state, exists := r.authorities[target]
	if !exists || !state.ready || state.authority.DomainID != domain {
		return Snapshot{}, false
	}
	deadline, active := r.effectiveDeadlineLocked(state.authority, now)
	if !active {
		return Snapshot{}, false
	}
	return Snapshot{Authority: state.authority, EffectiveUntil: deadline}, true
}

// Admit commits short-lived source authority only if the generation captured
// before JWT verification is still current and ready.
func (r *Registry) Admit(request AdmissionRequest) (time.Time, error) {
	if r == nil {
		return time.Time{}, errors.New("relay booking registry is required")
	}
	if request.SourcePeerID == "" || request.TargetPeerID == "" || request.DomainID == uuid.Nil || request.LiteralJWTExpiresAt.IsZero() {
		return time.Time{}, errors.New("source, Domain, target, and literal JWT expiry are required")
	}
	if err := request.ExpectedFence.validate(); err != nil {
		return time.Time{}, err
	}
	tokenDeadline := request.LiteralJWTExpiresAt.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return time.Time{}, ErrAuthorityNotReady
	}
	now := r.now().UTC()
	if !now.Before(tokenDeadline) {
		return time.Time{}, ErrAuthorityExpired
	}
	state, exists := r.authorities[request.TargetPeerID]
	if !exists {
		return time.Time{}, ErrAuthorityNotFound
	}
	if state.authority.Fence != request.ExpectedFence {
		return time.Time{}, ErrStaleFence
	}
	if !state.ready {
		return time.Time{}, ErrAuthorityNotReady
	}
	if state.authority.DomainID != request.DomainID {
		return time.Time{}, ErrAuthorityNotFound
	}
	deadline, active := r.effectiveDeadlineLocked(state.authority, now)
	if !active {
		return time.Time{}, ErrAuthorityExpired
	}
	deadline = earliest(deadline, tokenDeadline, now.Add(r.admissionTTL))
	if !now.Before(deadline) {
		return time.Time{}, ErrAuthorityExpired
	}

	key := admissionKey{source: request.SourcePeerID, domain: request.DomainID, target: request.TargetPeerID}
	if _, exists := r.admissions[key]; !exists && len(r.admissions) >= r.maximumAdmissions {
		r.pruneAdmissionsLocked(now)
		if len(r.admissions) >= r.maximumAdmissions {
			return time.Time{}, ErrAdmissionCapacity
		}
	}
	r.admissions[key] = admission{fence: request.ExpectedFence, acceptedUntil: deadline}
	keys := r.admissionsByTarget[request.TargetPeerID]
	if keys == nil {
		keys = make(map[admissionKey]struct{})
		r.admissionsByTarget[request.TargetPeerID] = keys
	}
	keys[key] = struct{}{}
	return deadline, nil
}

func (r *Registry) AllowReserve(target peer.ID) bool {
	if r == nil || target == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.draining {
		return false
	}
	now := r.now().UTC()
	state, exists := r.authorities[target]
	if !exists {
		return false
	}
	_, active := r.effectiveDeadlineLocked(state.authority, now)
	return active
}

func (r *Registry) AllowConnect(source, target peer.ID) bool {
	if r == nil || source == "" || target == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.draining {
		return false
	}
	now := r.now().UTC()
	state, exists := r.authorities[target]
	if !exists || !state.ready {
		return false
	}
	if _, active := r.effectiveDeadlineLocked(state.authority, now); !active {
		return false
	}
	entry, exists := r.admissions[admissionKey{source: source, domain: state.authority.DomainID, target: target}]
	return exists && entry.fence == state.authority.Fence && now.Before(entry.acceptedUntil)
}

// RemoveExact revokes one generation atomically with all admissions for its
// target. The caller closes the returned target's connections after this method
// returns and therefore never does network work while holding the registry lock.
func (r *Registry) RemoveExact(target peer.ID, fence Fence) (peer.ID, bool) {
	if r == nil || target == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, exists := r.authorities[target]
	if !exists || state.authority.Fence != fence {
		return "", false
	}
	r.removeTargetLocked(target, state)
	return target, true
}

// Expire removes every entry whose literal local authority has reached its
// deadline and returns exact generations whose target peers must be closed.
func (r *Registry) Expire() []ExpiredAuthority {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	removed := make([]ExpiredAuthority, 0)
	for target, state := range r.authorities {
		if _, active := r.effectiveDeadlineLocked(state.authority, now); active {
			continue
		}
		r.removeTargetLocked(target, state)
		removed = append(removed, ExpiredAuthority{TargetPeerID: target, Fence: state.authority.Fence})
	}
	r.pruneAdmissionsLocked(now)
	sort.Slice(removed, func(i, j int) bool { return removed[i].TargetPeerID.String() < removed[j].TargetPeerID.String() })
	return removed
}

// ClearSession fail-closes the exact current process generation and returns all
// targets whose direct relay connections must be closed.
func (r *Registry) ClearSession(expected uuid.UUID) []peer.ID {
	if r == nil || expected == uuid.Nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session.ProviderSessionID != expected {
		return nil
	}
	removed := make([]peer.ID, 0, len(r.authorities))
	for target, state := range r.authorities {
		r.removeTargetLocked(target, state)
		removed = append(removed, target)
	}
	r.session = Session{}
	r.clearAdmissionsLocked()
	sort.Slice(removed, func(i, j int) bool { return removed[i].String() < removed[j].String() })
	return removed
}

func (r *Registry) Counts() (bookings, admissions int) {
	if r == nil {
		return 0, 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.authorities), len(r.admissions)
}

// BeginDrain is sticky for one process incarnation. It rejects new RESERVE,
// source admission, and CONNECT decisions while retaining the exact authority
// records and existing go-libp2p circuits for the bounded drain window.
func (r *Registry) BeginDrain() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.draining = true
	r.clearAdmissionsLocked()
	r.mu.Unlock()
}

func (r *Registry) Draining() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.draining
}

// NextDeadline returns the earliest literal authority deadline that requires
// target teardown. The worker arms an exact timer from this value; ACL reads
// remain independently fail closed at the same boundary.
func (r *Registry) NextDeadline() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := r.now().UTC()
	var result time.Time
	for _, state := range r.authorities {
		deadline, _ := r.effectiveDeadlineLocked(state.authority, now)
		if deadline.IsZero() {
			continue
		}
		if result.IsZero() || deadline.Before(result) {
			result = deadline
		}
	}
	return result.UTC()
}

func (r *Registry) effectiveDeadlineLocked(authority Authority, now time.Time) (time.Time, bool) {
	if r.session.ProviderSessionID == uuid.Nil || authority.Fence.ProviderSessionID != r.session.ProviderSessionID {
		return time.Time{}, false
	}
	deadline := earliest(
		authority.RequestedUntil,
		authority.AuthorityExpiresAt,
		authority.ProviderLeaseExpiresAt,
		r.session.SessionExpiresAt,
		r.session.NodeJWTExpiresAt,
	)
	return deadline, now.Before(deadline)
}

func (r *Registry) admissionActiveLocked(key admissionKey, entry admission, now time.Time) bool {
	state, exists := r.authorities[key.target]
	if !exists || !state.ready || state.authority.DomainID != key.domain || state.authority.Fence != entry.fence {
		return false
	}
	if _, active := r.effectiveDeadlineLocked(state.authority, now); !active {
		return false
	}
	return now.Before(entry.acceptedUntil)
}

func (r *Registry) pruneAdmissionsLocked(now time.Time) {
	for key, entry := range r.admissions {
		if r.admissionActiveLocked(key, entry, now) {
			continue
		}
		r.removeAdmissionLocked(key)
	}
}

func (r *Registry) removeTargetLocked(target peer.ID, state authorityState) {
	delete(r.authorities, target)
	delete(r.byAssignment, state.authority.Fence.AssignmentID)
	for key := range r.admissionsByTarget[target] {
		delete(r.admissions, key)
	}
	delete(r.admissionsByTarget, target)
}

func (r *Registry) removeAdmissionLocked(key admissionKey) {
	delete(r.admissions, key)
	keys := r.admissionsByTarget[key.target]
	delete(keys, key)
	if len(keys) == 0 {
		delete(r.admissionsByTarget, key.target)
	}
}

func (r *Registry) clearAdmissionsLocked() {
	clear(r.admissions)
	clear(r.admissionsByTarget)
}

func normalizeAuthority(authority Authority) Authority {
	authority.RequestedUntil = authority.RequestedUntil.UTC()
	authority.AuthorityExpiresAt = authority.AuthorityExpiresAt.UTC()
	authority.ProviderLeaseExpiresAt = authority.ProviderLeaseExpiresAt.UTC()
	return authority
}

func sameImmutableAuthority(left, right Authority) bool {
	return left.BookingID == right.BookingID &&
		left.SlotID == right.SlotID &&
		left.DomainID == right.DomainID &&
		left.TargetPeerID == right.TargetPeerID &&
		left.Fence == right.Fence
}

func earliest(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		value = value.UTC()
		if result.IsZero() || value.Before(result) {
			result = value
		}
	}
	return result
}
