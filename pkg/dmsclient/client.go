package dmsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	maxResponseBytes       = 64 << 10
	maxActiveResponseBytes = (maxEffectiveCapacity + 1) * maxResponseBytes
	providerSessionHeader  = "Provider-Session-Id"
	statusTTLSeconds       = 180
	minLeaseTTLSeconds     = 60
	maxLeaseTTLSeconds     = 300
	minRecoverySeconds     = 300
	maxEffectiveCapacity   = 256
)

type Options struct {
	BaseURL             string
	HTTPClient          *http.Client
	AllowHTTPForTesting bool
	StatusInterval      time.Duration
	MaxStatusBackoff    time.Duration
	Now                 func() time.Time
}

type Client struct {
	baseURL          *url.URL
	http             *http.Client
	statusInterval   time.Duration
	maxStatusBackoff time.Duration
	now              func() time.Time
}

type Limits struct {
	DurationSeconds       uint32 `json:"duration_seconds"`
	DataBytesPerDirection int64  `json:"data_bytes_per_direction"`
}

type Metadata struct {
	BaseAddresses     []string
	EndpointKeys      []string
	Limits            Limits
	ConfigFingerprint string
}

type LocalCapacity struct {
	Total  int
	PerIP  int
	PerASN int
}

type Expectations struct {
	Metadata           Metadata
	LocalCapacity      LocalCapacity
	SchedulingRevision uint64
	NodeTokenExpiresAt time.Time
}

type OpenInput struct {
	AccessToken string
	BootNonce   uuid.UUID
	Expected    Expectations
}

type OpenOutcome string

const (
	OpenCreated  OpenOutcome = "created"
	OpenReplayed OpenOutcome = "replayed"
)

type OpenResult struct {
	Outcome OpenOutcome
	Session Session
}

type Session struct {
	ProviderSessionID        uuid.UUID
	Status                   ProviderStatus
	EffectiveCapacity        uint32
	SchedulingRevision       uint64
	SessionExpiresAt         time.Time
	ProviderNodeJWTExpiresAt time.Time
	StatusTTL                time.Duration
	ProviderLeaseTTL         time.Duration
	RecoveryGrace            time.Duration
	Metadata                 Metadata
}

// ProviderStatus is the exact provider selection/drain state sent to DMS.
// ShutdownIntent is nil or one of the exported ShutdownIntent* constants.
type ProviderStatus struct {
	AcceptingBookings bool
	Draining          bool
	ShutdownIntent    *string
}

const (
	ShutdownIntentRestartSameIdentity = "restart_same_identity"
	ShutdownIntentReassign            = "reassign"
)

// BookingState and SlotState mirror the stable DMS relay wire enums.
type BookingState string

const (
	BookingStateActive   BookingState = "active"
	BookingStateCanceled BookingState = "canceled"
	BookingStateExpired  BookingState = "expired"
	BookingStateFailed   BookingState = "failed"
)

type SlotState string

const (
	SlotStateQueued      SlotState = "queued"
	SlotStateStarting    SlotState = "starting"
	SlotStateReady       SlotState = "ready"
	SlotStateRecovering  SlotState = "recovering"
	SlotStateReassigning SlotState = "reassigning"
	SlotStateEnded       SlotState = "ended"
)

// Assignment is the common validated view of claim, active-recovery, and
// fenced-mutation responses. Claim responses deliberately have nil Metadata;
// DMS pins the provider configuration internally and the worker uses its
// already-validated Session.Metadata when sending Ready.
type Assignment struct {
	BookingID              uuid.UUID
	SlotID                 uuid.UUID
	SlotIndex              uint8
	RelayCount             uint8
	DomainID               uuid.UUID
	TargetPeerID           string
	BookingState           BookingState
	SlotState              SlotState
	ProviderSessionID      *uuid.UUID
	AssignmentID           *uuid.UUID
	ReservationEpoch       *uuid.UUID
	RequestedUntil         time.Time
	AuthorityExpiresAt     time.Time
	ProviderLeaseExpiresAt *time.Time
	RecoveryExpiresAt      *time.Time
	EffectiveCapacity      uint32
	SchedulingRevision     uint64
	Metadata               *Metadata
	Revoke                 bool
}

// ClaimResult contains either one assignment or an authoritative empty-claim
// delay. Exactly one of Assignment and RetryAfter is populated.
type ClaimResult struct {
	Assignment *Assignment
	RetryAfter time.Duration
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("DMS relay request failed with HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("DMS relay request failed with HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

func New(opts Options) (*Client, error) {
	baseURL, err := parseBaseURL(opts.BaseURL, opts.AllowHTTPForTesting)
	if err != nil {
		return nil, err
	}
	if opts.HTTPClient == nil {
		return nil, errors.New("DMS HTTP client is required")
	}
	if opts.HTTPClient.Timeout <= 0 {
		return nil, errors.New("DMS HTTP client must have a positive timeout")
	}
	if opts.StatusInterval <= 0 {
		return nil, errors.New("DMS provider-status interval must be positive")
	}
	if opts.MaxStatusBackoff <= 0 {
		return nil, errors.New("DMS provider-status maximum backoff must be positive")
	}
	httpClient := *opts.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		baseURL:          baseURL,
		http:             &httpClient,
		statusInterval:   opts.StatusInterval,
		maxStatusBackoff: opts.MaxStatusBackoff,
		now:              now,
	}, nil
}

type openSessionRequest struct {
	BootNonce         uuid.UUID `json:"boot_nonce"`
	BaseAddresses     []string  `json:"base_addresses"`
	EndpointKeys      []string  `json:"endpoint_keys"`
	Limits            Limits    `json:"limits"`
	ConfigFingerprint string    `json:"config_fingerprint"`
}

// OpenSession opens a new provider session or replays a lost response for the
// same boot nonce. Both outcomes are validated against the immutable local
// metadata and literal Node-token authority.
func (c *Client) OpenSession(ctx context.Context, input OpenInput) (*OpenResult, error) {
	if input.BootNonce == uuid.Nil {
		return nil, errors.New("provider boot nonce is required")
	}
	if err := validateAccessToken(input.AccessToken); err != nil {
		return nil, err
	}
	if err := validateExpectations(input.Expected); err != nil {
		return nil, err
	}
	request := openSessionRequest{
		BootNonce:         input.BootNonce,
		BaseAddresses:     slices.Clone(input.Expected.Metadata.BaseAddresses),
		EndpointKeys:      slices.Clone(input.Expected.Metadata.EndpointKeys),
		Limits:            input.Expected.Metadata.Limits,
		ConfigFingerprint: input.Expected.Metadata.ConfigFingerprint,
	}
	var response sessionResponse
	status, err := c.doJSON(ctx, http.MethodPost, "/relay-provider/session", input.AccessToken, uuid.Nil, request, &response, http.StatusCreated, http.StatusOK)
	if err != nil {
		return nil, err
	}
	session, err := c.validateSession(response, input.Expected)
	if err != nil {
		return nil, err
	}
	outcome := OpenReplayed
	if status == http.StatusCreated {
		outcome = OpenCreated
		if !equalProviderStatus(session.Status, ProviderStatus{}) {
			return nil, errors.New("DMS created provider session is not false/false non-draining")
		}
	}
	return &OpenResult{Outcome: outcome, Session: *session}, nil
}

type statusRequest struct {
	AcceptingBookings bool     `json:"accepting_bookings"`
	Draining          bool     `json:"draining"`
	ShutdownIntent    *string  `json:"shutdown_intent,omitempty"`
	BaseAddresses     []string `json:"base_addresses"`
	EndpointKeys      []string `json:"endpoint_keys"`
	Limits            Limits   `json:"limits"`
	ConfigFingerprint string   `json:"config_fingerprint"`
}

// ReportStatus updates the exact provider status and validates that DMS echoed
// the requested state together with every immutable session pin.
func (c *Client) ReportStatus(ctx context.Context, accessToken string, sessionID uuid.UUID, expected Expectations, status ProviderStatus) (*Session, error) {
	if sessionID == uuid.Nil {
		return nil, errors.New("provider session ID is required")
	}
	if err := validateAccessToken(accessToken); err != nil {
		return nil, err
	}
	if err := validateExpectations(expected); err != nil {
		return nil, err
	}
	if err := validateProviderStatus(status); err != nil {
		return nil, err
	}
	request := statusRequest{
		AcceptingBookings: status.AcceptingBookings,
		Draining:          status.Draining,
		ShutdownIntent:    cloneStringPointer(status.ShutdownIntent),
		BaseAddresses:     slices.Clone(expected.Metadata.BaseAddresses),
		EndpointKeys:      slices.Clone(expected.Metadata.EndpointKeys),
		Limits:            expected.Metadata.Limits,
		ConfigFingerprint: expected.Metadata.ConfigFingerprint,
	}
	var response sessionResponse
	if _, err := c.doJSON(ctx, http.MethodPut, "/relay-provider/status", accessToken, sessionID, request, &response, http.StatusOK); err != nil {
		return nil, err
	}
	session, err := c.validateSession(response, expected)
	if err != nil {
		return nil, err
	}
	if session.ProviderSessionID != sessionID {
		return nil, errors.New("DMS provider-status response changed the session ID")
	}
	if !equalProviderStatus(session.Status, status) {
		return nil, errors.New("DMS provider-status response changed the requested status")
	}
	return session, nil
}

// ReportNotAccepting preserves the Prompt-4 call surface as a fail-closed
// wrapper around the typed status API.
func (c *Client) ReportNotAccepting(ctx context.Context, accessToken string, sessionID uuid.UUID, expected Expectations) (*Session, error) {
	return c.ReportStatus(ctx, accessToken, sessionID, expected, ProviderStatus{})
}

// ReleaseSession performs the exact fenced DELETE: the UUID in the path and
// Provider-Session-Id header are always the same caller-supplied value.
func (c *Client) ReleaseSession(ctx context.Context, accessToken string, sessionID uuid.UUID) error {
	if sessionID == uuid.Nil {
		return errors.New("provider session ID is required")
	}
	if err := validateAccessToken(accessToken); err != nil {
		return err
	}
	endpoint := "/relay-provider/session/" + url.PathEscape(sessionID.String())
	_, err := c.doJSON(ctx, http.MethodDelete, endpoint, accessToken, sessionID, nil, nil, http.StatusNoContent)
	return err
}

type claimResponse struct {
	BookingID              uuid.UUID    `json:"booking_id"`
	SlotID                 uuid.UUID    `json:"slot_id"`
	SlotIndex              uint8        `json:"slot_index"`
	RelayCount             uint8        `json:"relay_count"`
	DomainID               uuid.UUID    `json:"domain_id"`
	TargetPeerID           string       `json:"target_peer_id"`
	BookingState           BookingState `json:"booking_state"`
	SlotState              SlotState    `json:"slot_state"`
	ProviderSessionID      uuid.UUID    `json:"provider_session_id"`
	AssignmentID           uuid.UUID    `json:"assignment_id"`
	ReservationEpoch       uuid.UUID    `json:"reservation_epoch"`
	RequestedUntil         time.Time    `json:"requested_until"`
	AuthorityExpiresAt     time.Time    `json:"authority_expires_at"`
	ProviderLeaseExpiresAt time.Time    `json:"provider_lease_expires_at"`
	EffectiveCapacity      uint32       `json:"effective_capacity"`
	SchedulingRevision     uint64       `json:"scheduling_revision"`
	Revoke                 bool         `json:"revoke"`
}

type assignmentResponse struct {
	BookingID              uuid.UUID    `json:"booking_id"`
	SlotID                 uuid.UUID    `json:"slot_id"`
	SlotIndex              uint8        `json:"slot_index"`
	RelayCount             uint8        `json:"relay_count"`
	DomainID               uuid.UUID    `json:"domain_id"`
	TargetPeerID           string       `json:"target_peer_id"`
	BookingState           BookingState `json:"booking_state"`
	SlotState              SlotState    `json:"slot_state"`
	ProviderSessionID      *uuid.UUID   `json:"provider_session_id,omitempty"`
	AssignmentID           *uuid.UUID   `json:"assignment_id,omitempty"`
	ReservationEpoch       *uuid.UUID   `json:"reservation_epoch,omitempty"`
	RequestedUntil         time.Time    `json:"requested_until"`
	AuthorityExpiresAt     time.Time    `json:"authority_expires_at"`
	ProviderLeaseExpiresAt *time.Time   `json:"provider_lease_expires_at,omitempty"`
	RecoveryExpiresAt      *time.Time   `json:"recovery_expires_at,omitempty"`
	EffectiveCapacity      uint32       `json:"effective_capacity"`
	SchedulingRevision     uint64       `json:"scheduling_revision"`
	ProviderBaseAddresses  []string     `json:"provider_base_addresses,omitempty"`
	EndpointKeys           []string     `json:"endpoint_keys,omitempty"`
	Limits                 *Limits      `json:"limits,omitempty"`
	ConfigFingerprint      *string      `json:"config_fingerprint,omitempty"`
	Revoke                 bool         `json:"revoke"`
}

type assignmentsResponse struct {
	Assignments []assignmentResponse `json:"assignments"`
}

type assignmentFenceRequest struct {
	ReservationEpoch uuid.UUID `json:"reservation_epoch"`
}

type readyRequest struct {
	ReservationEpoch  uuid.UUID `json:"reservation_epoch"`
	BaseAddresses     []string  `json:"base_addresses"`
	EndpointKeys      []string  `json:"endpoint_keys"`
	Limits            Limits    `json:"limits"`
	ConfigFingerprint string    `json:"config_fingerprint"`
}

type failRequest struct {
	ReservationEpoch uuid.UUID `json:"reservation_epoch"`
	Failure          string    `json:"failure"`
}

// Claim claims at most one assignment. An empty response returns a nil
// Assignment and the exact validated Retry-After duration.
func (c *Client) Claim(ctx context.Context, accessToken string, sessionID uuid.UUID) (*ClaimResult, error) {
	if err := validateProviderCall(accessToken, sessionID); err != nil {
		return nil, err
	}
	response, err := c.do(ctx, http.MethodPost, "/relay-provider/bookings/claim", accessToken, sessionID, nil, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusCreated:
		if len(response.Header.Values("Retry-After")) != 0 {
			return nil, errors.New("DMS returned Retry-After with a non-empty claim")
		}
		var decoded claimResponse
		if err := decodeWireJSON(response, &decoded); err != nil {
			return nil, fmt.Errorf("decode DMS relay claim response: %w", err)
		}
		assignment, err := c.assignmentFromClaim(decoded, sessionID)
		if err != nil {
			return nil, err
		}
		return &ClaimResult{Assignment: assignment}, nil
	case http.StatusNoContent:
		if len(response.Body) != 0 {
			return nil, errors.New("DMS returned an unexpected empty-claim response body")
		}
		retryAfter, err := parseRetryAfter(response.Header)
		if err != nil {
			return nil, err
		}
		return &ClaimResult{RetryAfter: retryAfter}, nil
	default:
		return nil, decodeAPIError(response.StatusCode, response.Header.Get("Content-Type"), response.Body, accessToken)
	}
}

// Active returns every current assignment for the exact provider session.
func (c *Client) Active(ctx context.Context, accessToken string, sessionID uuid.UUID) ([]Assignment, error) {
	if err := validateProviderCall(accessToken, sessionID); err != nil {
		return nil, err
	}
	var response assignmentsResponse
	if _, err := c.doJSONWithLimit(ctx, http.MethodGet, "/relay-provider/bookings/active", accessToken, sessionID, nil, &response, maxActiveResponseBytes, http.StatusOK); err != nil {
		return nil, err
	}
	if len(response.Assignments) > maxEffectiveCapacity {
		return nil, errors.New("DMS returned more than 256 active relay assignments")
	}
	assignments := make([]Assignment, 0, len(response.Assignments))
	seenAssignments := make(map[uuid.UUID]struct{}, len(response.Assignments))
	seenTargets := make(map[string]struct{}, len(response.Assignments))
	for index, raw := range response.Assignments {
		assignment, err := c.assignmentFromResponse(raw, sessionID)
		if err != nil {
			return nil, fmt.Errorf("validate DMS active relay assignment %d: %w", index, err)
		}
		if assignment.Revoke {
			return nil, errors.New("DMS active-assignment response contains revoked authority")
		}
		if _, exists := seenAssignments[*assignment.AssignmentID]; exists {
			return nil, errors.New("DMS active-assignment response contains a duplicate assignment ID")
		}
		if _, exists := seenTargets[assignment.TargetPeerID]; exists {
			return nil, errors.New("DMS active-assignment response contains a duplicate target Peer ID")
		}
		seenAssignments[*assignment.AssignmentID] = struct{}{}
		seenTargets[assignment.TargetPeerID] = struct{}{}
		assignments = append(assignments, *assignment)
	}
	return assignments, nil
}

func (c *Client) Recover(ctx context.Context, accessToken string, sessionID, assignmentID, reservationEpoch uuid.UUID) (*Assignment, error) {
	assignment, err := c.mutateAssignment(ctx, http.MethodPost, "recover", accessToken, sessionID, assignmentID, reservationEpoch, assignmentFenceRequest{ReservationEpoch: reservationEpoch})
	if err != nil || assignment.Revoke {
		return assignment, err
	}
	if assignment.SlotState != SlotStateStarting || assignment.ProviderLeaseExpiresAt == nil || assignment.RecoveryExpiresAt != nil {
		return nil, errors.New("DMS recover response did not return a starting typed lease")
	}
	return assignment, nil
}

func (c *Client) Heartbeat(ctx context.Context, accessToken string, sessionID, assignmentID, reservationEpoch uuid.UUID) (*Assignment, error) {
	assignment, err := c.mutateAssignment(ctx, http.MethodPost, "heartbeat", accessToken, sessionID, assignmentID, reservationEpoch, assignmentFenceRequest{ReservationEpoch: reservationEpoch})
	if err != nil || assignment.Revoke {
		return assignment, err
	}
	if assignment.SlotState != SlotStateStarting && assignment.SlotState != SlotStateReady {
		return nil, errors.New("DMS heartbeat response did not retain a typed lease")
	}
	return assignment, nil
}

func (c *Client) Ready(ctx context.Context, accessToken string, sessionID, assignmentID, reservationEpoch uuid.UUID, metadata Metadata) (*Assignment, error) {
	if err := validateMetadata(metadata); err != nil {
		return nil, err
	}
	assignment, err := c.mutateAssignment(ctx, http.MethodPut, "ready", accessToken, sessionID, assignmentID, reservationEpoch, readyRequest{
		ReservationEpoch:  reservationEpoch,
		BaseAddresses:     slices.Clone(metadata.BaseAddresses),
		EndpointKeys:      slices.Clone(metadata.EndpointKeys),
		Limits:            metadata.Limits,
		ConfigFingerprint: metadata.ConfigFingerprint,
	})
	if err != nil || assignment.Revoke {
		return assignment, err
	}
	if assignment.SlotState != SlotStateReady || assignment.Metadata == nil || !equalMetadata(*assignment.Metadata, metadata) {
		return nil, errors.New("DMS ready response did not return the exact ready provider configuration")
	}
	return assignment, nil
}

func (c *Client) Fail(ctx context.Context, accessToken string, sessionID, assignmentID, reservationEpoch uuid.UUID) (*Assignment, error) {
	assignment, err := c.mutateAssignment(ctx, http.MethodPost, "fail", accessToken, sessionID, assignmentID, reservationEpoch, failRequest{
		ReservationEpoch: reservationEpoch,
		Failure:          "retryable",
	})
	if err != nil {
		return nil, err
	}
	if !assignment.Revoke {
		return nil, errors.New("DMS fail response retained assignment authority")
	}
	return assignment, nil
}

func (c *Client) Relinquish(ctx context.Context, accessToken string, sessionID, assignmentID, reservationEpoch uuid.UUID) (*Assignment, error) {
	assignment, err := c.mutateAssignment(ctx, http.MethodPost, "relinquish", accessToken, sessionID, assignmentID, reservationEpoch, assignmentFenceRequest{ReservationEpoch: reservationEpoch})
	if err != nil {
		return nil, err
	}
	if !assignment.Revoke {
		return nil, errors.New("DMS relinquish response retained assignment authority")
	}
	return assignment, nil
}

type sessionResponse struct {
	ProviderSessionID        uuid.UUID `json:"provider_session_id"`
	AcceptingBookings        bool      `json:"accepting_bookings"`
	Draining                 bool      `json:"draining"`
	ShutdownIntent           *string   `json:"shutdown_intent,omitempty"`
	EffectiveCapacity        uint32    `json:"effective_capacity"`
	SchedulingRevision       uint64    `json:"scheduling_revision"`
	SessionExpiresAt         time.Time `json:"session_expires_at"`
	ProviderNodeJWTExpiresAt time.Time `json:"provider_node_jwt_expires_at"`
	StatusTTLSeconds         uint32    `json:"status_ttl_seconds"`
	ProviderLeaseTTLSeconds  uint32    `json:"provider_lease_ttl_seconds"`
	RecoveryGraceSeconds     uint32    `json:"recovery_grace_seconds"`
	BaseAddresses            []string  `json:"base_addresses"`
	EndpointKeys             []string  `json:"endpoint_keys"`
	Limits                   Limits    `json:"limits"`
	ConfigFingerprint        string    `json:"config_fingerprint"`
}

func (c *Client) validateSession(response sessionResponse, expected Expectations) (*Session, error) {
	if response.ProviderSessionID == uuid.Nil {
		return nil, errors.New("DMS returned an empty provider session ID")
	}
	status := ProviderStatus{
		AcceptingBookings: response.AcceptingBookings,
		Draining:          response.Draining,
		ShutdownIntent:    cloneStringPointer(response.ShutdownIntent),
	}
	if err := validateProviderStatus(status); err != nil {
		return nil, fmt.Errorf("DMS returned an invalid provider status: %w", err)
	}
	if response.EffectiveCapacity == 0 || response.EffectiveCapacity > maxEffectiveCapacity {
		return nil, errors.New("DMS returned an invalid effective provider capacity")
	}
	capacity := int(response.EffectiveCapacity)
	if capacity > expected.LocalCapacity.Total || capacity > expected.LocalCapacity.PerIP || capacity > expected.LocalCapacity.PerASN {
		return nil, errors.New("DMS effective capacity exceeds local total/IP/ASN reservation capacity")
	}
	if response.SchedulingRevision != expected.SchedulingRevision {
		return nil, errors.New("DMS scheduling revision does not match the peer-bound Node token")
	}
	if response.StatusTTLSeconds != statusTTLSeconds {
		return nil, errors.New("DMS returned an unsupported provider status TTL")
	}
	if response.ProviderLeaseTTLSeconds < minLeaseTTLSeconds || response.ProviderLeaseTTLSeconds > maxLeaseTTLSeconds {
		return nil, errors.New("DMS returned a provider lease TTL outside 60..300 seconds")
	}
	if response.RecoveryGraceSeconds < minRecoverySeconds {
		return nil, errors.New("DMS returned a provider recovery grace below five minutes")
	}
	if err := validateWireTime("session_expires_at", response.SessionExpiresAt); err != nil {
		return nil, err
	}
	if err := validateWireTime("provider_node_jwt_expires_at", response.ProviderNodeJWTExpiresAt); err != nil {
		return nil, err
	}
	statusTTL := time.Duration(response.StatusTTLSeconds) * time.Second
	if c.statusInterval+c.http.Timeout+c.maxStatusBackoff >= statusTTL {
		return nil, errors.New("provider status cadence, timeout, and backoff do not fit inside the DMS status TTL")
	}
	now := c.now().UTC()
	expectedJWTExpiry := expected.NodeTokenExpiresAt.UTC()
	if !response.ProviderNodeJWTExpiresAt.UTC().Equal(expectedJWTExpiry) {
		return nil, errors.New("DMS provider Node-JWT expiry does not match the literal signed expiration")
	}
	if !response.SessionExpiresAt.After(now) || response.SessionExpiresAt.After(now.Add(statusTTL)) {
		return nil, errors.New("DMS provider session deadline is outside its rolling status TTL")
	}
	if response.SessionExpiresAt.After(response.ProviderNodeJWTExpiresAt) {
		return nil, errors.New("DMS provider session deadline exceeds Node-token authority")
	}
	if !slices.Equal(response.BaseAddresses, expected.Metadata.BaseAddresses) ||
		!slices.Equal(response.EndpointKeys, expected.Metadata.EndpointKeys) ||
		response.Limits != expected.Metadata.Limits ||
		response.ConfigFingerprint != expected.Metadata.ConfigFingerprint {
		return nil, errors.New("DMS provider session metadata does not match the immutable local relay configuration")
	}
	return &Session{
		ProviderSessionID:        response.ProviderSessionID,
		Status:                   status,
		EffectiveCapacity:        response.EffectiveCapacity,
		SchedulingRevision:       response.SchedulingRevision,
		SessionExpiresAt:         response.SessionExpiresAt.UTC(),
		ProviderNodeJWTExpiresAt: response.ProviderNodeJWTExpiresAt.UTC(),
		StatusTTL:                statusTTL,
		ProviderLeaseTTL:         time.Duration(response.ProviderLeaseTTLSeconds) * time.Second,
		RecoveryGrace:            time.Duration(response.RecoveryGraceSeconds) * time.Second,
		Metadata: Metadata{
			BaseAddresses:     slices.Clone(response.BaseAddresses),
			EndpointKeys:      slices.Clone(response.EndpointKeys),
			Limits:            response.Limits,
			ConfigFingerprint: response.ConfigFingerprint,
		},
	}, nil
}

func validateProviderStatus(status ProviderStatus) error {
	if status.AcceptingBookings && status.Draining {
		return errors.New("provider cannot be accepting and draining")
	}
	if status.ShutdownIntent != nil {
		if !status.Draining || status.AcceptingBookings {
			return errors.New("provider shutdown intent requires draining=true and accepting_bookings=false")
		}
		switch *status.ShutdownIntent {
		case ShutdownIntentRestartSameIdentity, ShutdownIntentReassign:
		default:
			return errors.New("provider shutdown intent is invalid")
		}
	}
	return nil
}

func equalProviderStatus(left, right ProviderStatus) bool {
	if left.AcceptingBookings != right.AcceptingBookings || left.Draining != right.Draining {
		return false
	}
	if left.ShutdownIntent == nil || right.ShutdownIntent == nil {
		return left.ShutdownIntent == nil && right.ShutdownIntent == nil
	}
	return *left.ShutdownIntent == *right.ShutdownIntent
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func (c *Client) mutateAssignment(
	ctx context.Context,
	method, suffix, accessToken string,
	sessionID, assignmentID, reservationEpoch uuid.UUID,
	request any,
) (*Assignment, error) {
	if err := validateProviderCall(accessToken, sessionID); err != nil {
		return nil, err
	}
	if assignmentID == uuid.Nil || reservationEpoch == uuid.Nil {
		return nil, errors.New("assignment ID and reservation epoch are required")
	}
	endpoint := "/relay-provider/assignments/" + url.PathEscape(assignmentID.String()) + "/" + suffix
	response, err := c.do(ctx, method, endpoint, accessToken, sessionID, request, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusGone {
		return nil, decodeAPIError(response.StatusCode, response.Header.Get("Content-Type"), response.Body, accessToken)
	}

	var decoded assignmentResponse
	decodeAssignmentErr := decodeWireJSON(response, &decoded)
	if decodeAssignmentErr == nil {
		assignment, err := c.assignmentFromResponse(decoded, sessionID)
		if err != nil {
			return nil, err
		}
		if err := validateReturnedFence(assignment, sessionID, assignmentID, reservationEpoch); err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusGone && !assignment.Revoke {
			return nil, errors.New("DMS returned a non-revoked assignment snapshot with HTTP 410")
		}
		return assignment, nil
	}
	if response.StatusCode == http.StatusGone {
		if apiErr, ok := decodeStrictAPIError(response, accessToken); ok {
			return nil, apiErr
		}
		return nil, fmt.Errorf("decode DMS assignment-or-error 410 response: %w", decodeAssignmentErr)
	}
	return nil, fmt.Errorf("decode DMS relay assignment response: %w", decodeAssignmentErr)
}

func (c *Client) assignmentFromClaim(response claimResponse, sessionID uuid.UUID) (*Assignment, error) {
	if err := validateWireTime("requested_until", response.RequestedUntil); err != nil {
		return nil, err
	}
	if err := validateWireTime("authority_expires_at", response.AuthorityExpiresAt); err != nil {
		return nil, err
	}
	if err := validateWireTime("provider_lease_expires_at", response.ProviderLeaseExpiresAt); err != nil {
		return nil, err
	}
	providerSessionID := response.ProviderSessionID
	assignmentID := response.AssignmentID
	reservationEpoch := response.ReservationEpoch
	lease := response.ProviderLeaseExpiresAt.UTC()
	assignment := &Assignment{
		BookingID:              response.BookingID,
		SlotID:                 response.SlotID,
		SlotIndex:              response.SlotIndex,
		RelayCount:             response.RelayCount,
		DomainID:               response.DomainID,
		TargetPeerID:           response.TargetPeerID,
		BookingState:           response.BookingState,
		SlotState:              response.SlotState,
		ProviderSessionID:      &providerSessionID,
		AssignmentID:           &assignmentID,
		ReservationEpoch:       &reservationEpoch,
		RequestedUntil:         response.RequestedUntil.UTC(),
		AuthorityExpiresAt:     response.AuthorityExpiresAt.UTC(),
		ProviderLeaseExpiresAt: &lease,
		EffectiveCapacity:      response.EffectiveCapacity,
		SchedulingRevision:     response.SchedulingRevision,
		Revoke:                 response.Revoke,
	}
	if err := validateAssignmentCommon(assignment); err != nil {
		return nil, fmt.Errorf("validate DMS relay claim response: %w", err)
	}
	if assignment.ProviderSessionID == nil || *assignment.ProviderSessionID != sessionID {
		return nil, errors.New("DMS relay claim response changed the provider session fence")
	}
	if assignment.Revoke || assignment.BookingState != BookingStateActive || assignment.SlotState != SlotStateStarting {
		return nil, errors.New("DMS relay claim response did not return active starting authority")
	}
	if assignment.ProviderLeaseExpiresAt == nil || assignment.RecoveryExpiresAt != nil {
		return nil, errors.New("DMS relay claim response did not return exactly one typed lease deadline")
	}
	if err := validateCurrentDeadline("provider lease", *assignment.ProviderLeaseExpiresAt, c.now().UTC(), assignment.RequestedUntil, assignment.AuthorityExpiresAt); err != nil {
		return nil, err
	}
	return assignment, nil
}

func (c *Client) assignmentFromResponse(response assignmentResponse, sessionID uuid.UUID) (*Assignment, error) {
	if err := validateWireTime("requested_until", response.RequestedUntil); err != nil {
		return nil, err
	}
	if err := validateWireTime("authority_expires_at", response.AuthorityExpiresAt); err != nil {
		return nil, err
	}
	if response.ProviderLeaseExpiresAt != nil {
		if err := validateWireTime("provider_lease_expires_at", *response.ProviderLeaseExpiresAt); err != nil {
			return nil, err
		}
	}
	if response.RecoveryExpiresAt != nil {
		if err := validateWireTime("recovery_expires_at", *response.RecoveryExpiresAt); err != nil {
			return nil, err
		}
	}
	metadata, err := metadataFromAssignmentResponse(response)
	if err != nil {
		return nil, err
	}
	assignment := &Assignment{
		BookingID:              response.BookingID,
		SlotID:                 response.SlotID,
		SlotIndex:              response.SlotIndex,
		RelayCount:             response.RelayCount,
		DomainID:               response.DomainID,
		TargetPeerID:           response.TargetPeerID,
		BookingState:           response.BookingState,
		SlotState:              response.SlotState,
		ProviderSessionID:      cloneUUIDPointer(response.ProviderSessionID),
		AssignmentID:           cloneUUIDPointer(response.AssignmentID),
		ReservationEpoch:       cloneUUIDPointer(response.ReservationEpoch),
		RequestedUntil:         response.RequestedUntil.UTC(),
		AuthorityExpiresAt:     response.AuthorityExpiresAt.UTC(),
		ProviderLeaseExpiresAt: cloneTimePointer(response.ProviderLeaseExpiresAt),
		RecoveryExpiresAt:      cloneTimePointer(response.RecoveryExpiresAt),
		EffectiveCapacity:      response.EffectiveCapacity,
		SchedulingRevision:     response.SchedulingRevision,
		Metadata:               metadata,
		Revoke:                 response.Revoke,
	}
	if err := validateAssignmentCommon(assignment); err != nil {
		return nil, fmt.Errorf("validate DMS relay assignment response: %w", err)
	}
	if assignment.Revoke {
		return assignment, nil
	}
	if assignment.BookingState != BookingStateActive || assignment.ProviderSessionID == nil || *assignment.ProviderSessionID != sessionID ||
		assignment.AssignmentID == nil || assignment.ReservationEpoch == nil || assignment.Metadata == nil {
		return nil, errors.New("DMS relay assignment response omitted current authority fields")
	}
	now := c.now().UTC()
	switch assignment.SlotState {
	case SlotStateStarting, SlotStateReady:
		if assignment.ProviderLeaseExpiresAt == nil || assignment.RecoveryExpiresAt != nil {
			return nil, errors.New("DMS typed-lease assignment has invalid lease/recovery deadlines")
		}
		if err := validateCurrentDeadline("provider lease", *assignment.ProviderLeaseExpiresAt, now, assignment.RequestedUntil, assignment.AuthorityExpiresAt); err != nil {
			return nil, err
		}
	case SlotStateRecovering:
		if assignment.ProviderLeaseExpiresAt != nil || assignment.RecoveryExpiresAt == nil {
			return nil, errors.New("DMS recovering assignment has invalid lease/recovery deadlines")
		}
		if err := validateCurrentDeadline("recovery", *assignment.RecoveryExpiresAt, now, assignment.RequestedUntil, assignment.AuthorityExpiresAt); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("DMS non-revoked assignment is not starting, ready, or recovering")
	}
	return assignment, nil
}

func validateAssignmentCommon(assignment *Assignment) error {
	if assignment == nil {
		return errors.New("assignment is required")
	}
	if assignment.BookingID == uuid.Nil || assignment.SlotID == uuid.Nil || assignment.DomainID == uuid.Nil {
		return errors.New("booking, slot, and Domain IDs must be nonzero UUIDs")
	}
	if assignment.RelayCount < 1 || assignment.RelayCount > 3 || assignment.SlotIndex >= assignment.RelayCount {
		return errors.New("relay count or slot index is outside the v1 range")
	}
	if _, err := peer.Decode(assignment.TargetPeerID); err != nil {
		return errors.New("target Peer ID is invalid")
	}
	switch assignment.BookingState {
	case BookingStateActive, BookingStateCanceled, BookingStateExpired, BookingStateFailed:
	default:
		return errors.New("booking state is invalid")
	}
	switch assignment.SlotState {
	case SlotStateQueued, SlotStateStarting, SlotStateReady, SlotStateRecovering, SlotStateReassigning, SlotStateEnded:
	default:
		return errors.New("slot state is invalid")
	}
	if assignment.ProviderSessionID != nil && *assignment.ProviderSessionID == uuid.Nil ||
		assignment.AssignmentID != nil && *assignment.AssignmentID == uuid.Nil ||
		assignment.ReservationEpoch != nil && *assignment.ReservationEpoch == uuid.Nil {
		return errors.New("assignment fence contains a zero UUID")
	}
	if assignment.EffectiveCapacity < 1 || assignment.EffectiveCapacity > maxEffectiveCapacity {
		return errors.New("effective provider capacity is outside 1..256")
	}
	if err := validateWireTime("requested_until", assignment.RequestedUntil); err != nil {
		return err
	}
	if err := validateWireTime("authority_expires_at", assignment.AuthorityExpiresAt); err != nil {
		return err
	}
	if assignment.AuthorityExpiresAt.After(assignment.RequestedUntil) {
		return errors.New("assignment authority deadline exceeds its requested horizon")
	}
	if assignment.ProviderLeaseExpiresAt != nil {
		if err := validateWireTime("provider_lease_expires_at", *assignment.ProviderLeaseExpiresAt); err != nil {
			return err
		}
		if assignment.ProviderLeaseExpiresAt.After(assignment.RequestedUntil) || assignment.ProviderLeaseExpiresAt.After(assignment.AuthorityExpiresAt) {
			return errors.New("provider lease deadline exceeds parent authority")
		}
	}
	if assignment.RecoveryExpiresAt != nil {
		if err := validateWireTime("recovery_expires_at", *assignment.RecoveryExpiresAt); err != nil {
			return err
		}
		if assignment.RecoveryExpiresAt.After(assignment.RequestedUntil) || assignment.RecoveryExpiresAt.After(assignment.AuthorityExpiresAt) {
			return errors.New("recovery deadline exceeds parent authority")
		}
	}
	return nil
}

func metadataFromAssignmentResponse(response assignmentResponse) (*Metadata, error) {
	present := 0
	if response.ProviderBaseAddresses != nil {
		present++
	}
	if response.EndpointKeys != nil {
		present++
	}
	if response.Limits != nil {
		present++
	}
	if response.ConfigFingerprint != nil {
		present++
	}
	if present == 0 {
		return nil, nil
	}
	if present != 4 {
		return nil, errors.New("DMS relay assignment response contains partial provider metadata")
	}
	metadata := Metadata{
		BaseAddresses:     slices.Clone(response.ProviderBaseAddresses),
		EndpointKeys:      slices.Clone(response.EndpointKeys),
		Limits:            *response.Limits,
		ConfigFingerprint: *response.ConfigFingerprint,
	}
	if err := validateMetadata(metadata); err != nil {
		return nil, fmt.Errorf("DMS relay assignment metadata is invalid: %w", err)
	}
	return &metadata, nil
}

func validateReturnedFence(assignment *Assignment, sessionID, assignmentID, reservationEpoch uuid.UUID) error {
	if assignment.ProviderSessionID != nil && *assignment.ProviderSessionID != sessionID ||
		assignment.AssignmentID != nil && *assignment.AssignmentID != assignmentID ||
		assignment.ReservationEpoch != nil && *assignment.ReservationEpoch != reservationEpoch {
		return errors.New("DMS relay assignment response changed the requested fence")
	}
	if !assignment.Revoke && (assignment.ProviderSessionID == nil || assignment.AssignmentID == nil || assignment.ReservationEpoch == nil) {
		return errors.New("DMS relay assignment response omitted the requested fence")
	}
	return nil
}

func validateCurrentDeadline(name string, deadline, now time.Time, ceilings ...time.Time) error {
	if !deadline.After(now) {
		return fmt.Errorf("DMS returned an expired %s deadline", name)
	}
	for _, ceiling := range ceilings {
		if deadline.After(ceiling) {
			return fmt.Errorf("DMS returned a %s deadline beyond parent authority", name)
		}
	}
	return nil
}

func validateWireTime(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("DMS returned an empty %s timestamp", name)
	}
	_, offset := value.Zone()
	if offset != 0 {
		return fmt.Errorf("DMS returned a non-UTC %s timestamp", name)
	}
	return nil
}

func validateProviderCall(accessToken string, sessionID uuid.UUID) error {
	if sessionID == uuid.Nil {
		return errors.New("provider session ID is required")
	}
	return validateAccessToken(accessToken)
}

func parseRetryAfter(headers http.Header) (time.Duration, error) {
	values := headers.Values("Retry-After")
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] {
		return 0, errors.New("DMS empty claim requires exactly one integer Retry-After header")
	}
	seconds, err := strconv.ParseUint(values[0], 10, 32)
	if err != nil || seconds < 1 || seconds > 30 {
		return 0, errors.New("DMS empty-claim Retry-After must be integer delta-seconds in 1..30")
	}
	return time.Duration(seconds) * time.Second, nil
}

func cloneUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := value.UTC()
	return &copyValue
}

func validateExpectations(expected Expectations) error {
	if expected.NodeTokenExpiresAt.IsZero() {
		return errors.New("literal Node-token expiration is required")
	}
	if expected.LocalCapacity.Total <= 0 || expected.LocalCapacity.Total > maxEffectiveCapacity ||
		expected.LocalCapacity.PerIP <= 0 || expected.LocalCapacity.PerIP > maxEffectiveCapacity ||
		expected.LocalCapacity.PerASN <= 0 || expected.LocalCapacity.PerASN > maxEffectiveCapacity {
		return errors.New("local total/IP/ASN reservation capacities must each be in 1..256")
	}
	return validateMetadata(expected.Metadata)
}

func validateMetadata(metadata Metadata) error {
	if err := validateSortedUnique(metadata.BaseAddresses, "base addresses"); err != nil {
		return err
	}
	if err := validateSortedUnique(metadata.EndpointKeys, "endpoint keys"); err != nil {
		return err
	}
	if metadata.Limits.DurationSeconds == 0 || metadata.Limits.DataBytesPerDirection <= 0 {
		return errors.New("relay limits must be positive and finite")
	}
	if metadata.ConfigFingerprint == "" || strings.TrimSpace(metadata.ConfigFingerprint) != metadata.ConfigFingerprint {
		return errors.New("relay config fingerprint is required without surrounding whitespace")
	}
	return nil
}

func equalMetadata(left, right Metadata) bool {
	return slices.Equal(left.BaseAddresses, right.BaseAddresses) &&
		slices.Equal(left.EndpointKeys, right.EndpointKeys) &&
		left.Limits == right.Limits && left.ConfigFingerprint == right.ConfigFingerprint
}

func validateSortedUnique(values []string, name string) error {
	if len(values) == 0 {
		return fmt.Errorf("relay %s must be non-empty", name)
	}
	for index, value := range values {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("relay %s contain an invalid value", name)
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("relay %s must be sorted and unique", name)
		}
	}
	return nil
}

func validateAccessToken(token string) error {
	if token == "" || strings.TrimSpace(token) != token {
		return errors.New("peer-bound DDS Node access token is required")
	}
	return nil
}

type relayErrorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type wireResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (c *Client) doJSON(ctx context.Context, method, endpoint, bearer string, sessionID uuid.UUID, input, output any, expectedStatuses ...int) (int, error) {
	return c.doJSONWithLimit(ctx, method, endpoint, bearer, sessionID, input, output, maxResponseBytes, expectedStatuses...)
}

func (c *Client) doJSONWithLimit(ctx context.Context, method, endpoint, bearer string, sessionID uuid.UUID, input, output any, responseLimit int, expectedStatuses ...int) (int, error) {
	response, err := c.do(ctx, method, endpoint, bearer, sessionID, input, responseLimit)
	if err != nil {
		return 0, err
	}
	if !containsStatus(expectedStatuses, response.StatusCode) {
		return response.StatusCode, decodeAPIError(response.StatusCode, response.Header.Get("Content-Type"), response.Body, bearer)
	}
	if output == nil {
		if len(response.Body) != 0 {
			return response.StatusCode, errors.New("DMS returned an unexpected success response body")
		}
		return response.StatusCode, nil
	}
	if err := decodeWireJSON(response, output); err != nil {
		return response.StatusCode, fmt.Errorf("decode DMS relay response: %w", err)
	}
	return response.StatusCode, nil
}

func (c *Client) do(ctx context.Context, method, endpoint, bearer string, sessionID uuid.UUID, input any, responseLimit int) (*wireResponse, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode DMS relay request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(endpoint), body)
	if err != nil {
		return nil, fmt.Errorf("create DMS relay request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if sessionID != uuid.Nil {
		request.Header.Set(providerSessionHeader, sessionID.String())
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("perform DMS relay request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := readBoundedLimit(response.Body, responseLimit)
	if err != nil {
		return nil, err
	}
	cacheControl := response.Header.Values("Cache-Control")
	if len(cacheControl) != 1 || cacheControl[0] != "no-store" {
		return nil, errors.New("DMS relay response is missing Cache-Control: no-store")
	}
	return &wireResponse{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: responseBody}, nil
}

func decodeWireJSON(response *wireResponse, output any) error {
	if response == nil {
		return errors.New("DMS relay response is required")
	}
	if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
		return err
	}
	return decodeStrictJSON(response.Body, output)
}

func decodeAPIError(status int, contentType string, body []byte, bearer string) error {
	var response relayErrorResponse
	if requireJSON(contentType) == nil && decodeStrictJSON(body, &response) == nil && response.Code != "" {
		if bearer != "" {
			response.Code = strings.ReplaceAll(response.Code, bearer, "[redacted]")
			response.Error = strings.ReplaceAll(response.Error, bearer, "[redacted]")
		}
		return &APIError{StatusCode: status, Code: response.Code, Message: response.Error}
	}
	return &APIError{StatusCode: status}
}

func decodeStrictAPIError(response *wireResponse, bearer string) (*APIError, bool) {
	if response == nil || requireJSON(response.Header.Get("Content-Type")) != nil {
		return nil, false
	}
	var decoded relayErrorResponse
	if decodeStrictJSON(response.Body, &decoded) != nil || decoded.Code == "" {
		return nil, false
	}
	if bearer != "" {
		decoded.Code = strings.ReplaceAll(decoded.Code, bearer, "[redacted]")
		decoded.Error = strings.ReplaceAll(decoded.Error, bearer, "[redacted]")
	}
	return &APIError{StatusCode: response.StatusCode, Code: decoded.Code, Message: decoded.Error}, true
}

func (c *Client) endpoint(endpoint string) string {
	copyURL := *c.baseURL
	copyURL.Path = strings.TrimRight(copyURL.Path, "/") + endpoint
	return copyURL.String()
}

func parseBaseURL(raw string, allowHTTP bool) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return nil, errors.New("DMS base URL is required without surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("DMS base URL is invalid")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("DMS base URL must use HTTPS")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readBounded(reader io.Reader) ([]byte, error) {
	return readBoundedLimit(reader, maxResponseBytes)
}

func readBoundedLimit(reader io.Reader, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("DMS relay response limit must be positive")
	}
	limited := io.LimitReader(reader, int64(limit)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read DMS relay response: %w", err)
	}
	if len(data) > limit {
		if limit == maxResponseBytes {
			return nil, errors.New("DMS relay response exceeds the 64 KiB limit")
		}
		return nil, fmt.Errorf("DMS relay response exceeds the %d-byte limit", limit)
	}
	return data, nil
}

func requireJSON(contentType string) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return errors.New("DMS returned a non-JSON response")
	}
	return nil
}

func decodeStrictJSON(data []byte, output any) error {
	if err := validateStrictJSONShape(data, reflect.TypeOf(output)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("DMS relay response contains trailing JSON")
	}
	return nil
}

func validateStrictJSONShape(data []byte, outputType reflect.Type) error {
	if outputType == nil {
		return errors.New("DMS relay response destination is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanStrictJSONValue(decoder, outputType); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("DMS relay response contains trailing JSON")
	}
	return nil
}

func scanStrictJSONValue(decoder *json.Decoder, valueType reflect.Type) error {
	for valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("DMS relay response contains a null field")
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		if valueType.Kind() != reflect.Struct {
			return errors.New("DMS relay response contains an unexpected object")
		}
		fields, required := strictJSONFields(valueType)
		seen := make(map[string]struct{}, len(fields))
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("DMS relay response contains an invalid object field")
			}
			fieldType, exists := fields[key]
			if !exists {
				return fmt.Errorf("DMS relay response contains unknown or non-canonical field %q", key)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("DMS relay response contains duplicate field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanStrictJSONValue(decoder, fieldType); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim('}') {
			return errors.New("DMS relay response contains an unterminated object")
		}
		for field := range required {
			if _, exists := seen[field]; !exists {
				return fmt.Errorf("DMS relay response is missing required field %q", field)
			}
		}
		return nil
	case '[':
		if valueType.Kind() != reflect.Slice && valueType.Kind() != reflect.Array {
			return errors.New("DMS relay response contains an unexpected array")
		}
		for decoder.More() {
			if err := scanStrictJSONValue(decoder, valueType.Elem()); err != nil {
				return err
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim(']') {
			return errors.New("DMS relay response contains an unterminated array")
		}
		return nil
	default:
		return errors.New("DMS relay response contains an unexpected delimiter")
	}
}

func strictJSONFields(valueType reflect.Type) (map[string]reflect.Type, map[string]struct{}) {
	fields := make(map[string]reflect.Type, valueType.NumField())
	required := make(map[string]struct{}, valueType.NumField())
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
		if !slices.Contains(parts[1:], "omitempty") {
			required[name] = struct{}{}
		}
	}
	return fields, required
}

func containsStatus(statuses []int, actual int) bool {
	for _, status := range statuses {
		if status == actual {
			return true
		}
	}
	return false
}
