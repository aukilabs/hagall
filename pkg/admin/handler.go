package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	maxDrainBodyBytes       = 1 << 10
	maxDrainDeadlineSeconds = 900
)

// ErrDrainConflict lets another lifecycle trigger (for example SIGTERM)
// report that this process incarnation already chose an immutable drain.
var ErrDrainConflict = errors.New("relay drain already started")

type ShutdownIntent string

const (
	ShutdownIntentRestartSameIdentity ShutdownIntent = "restart_same_identity"
	ShutdownIntentReassign            ShutdownIntent = "reassign"
)

type DrainRequest struct {
	ShutdownIntent  ShutdownIntent `json:"shutdown_intent"`
	DeadlineSeconds uint16         `json:"deadline_seconds"`
}

type DrainOutcome string

const (
	DrainOutcomeCompleted        DrainOutcome = "completed"
	DrainOutcomeDeadlineExceeded DrainOutcome = "deadline_exceeded"
	DrainOutcomeControlError     DrainOutcome = "control_error"
)

// DrainResult is intentionally aggregate-only. Booking, assignment, peer, and
// session identities must never be added to this wire type.
type DrainResult struct {
	ShutdownIntent  ShutdownIntent `json:"shutdown_intent"`
	Result          DrainOutcome   `json:"result"`
	LeasesTotal     uint32         `json:"leases_total"`
	LeasesReleased  uint32         `json:"leases_released"`
	LeasesRemaining uint32         `json:"leases_remaining"`
}

type RelayLimits struct {
	DurationSeconds       uint32 `json:"duration_seconds"`
	DataBytesPerDirection int64  `json:"data_bytes_per_direction"`
}

// DMSInfo exposes only rollout-relevant provider state. In particular,
// it excludes the provider session ID, Node identity, credentials, and every
// booking or assignment identifier.
type DMSInfo struct {
	SessionPresent           bool            `json:"session_present"`
	AcceptingBookings        bool            `json:"accepting_bookings"`
	Draining                 bool            `json:"draining"`
	ShutdownIntent           *ShutdownIntent `json:"shutdown_intent"`
	SessionExpiresAt         *time.Time      `json:"session_expires_at"`
	ProviderNodeJWTExpiresAt *time.Time      `json:"provider_node_jwt_expires_at"`
	EffectiveCapacity        uint32          `json:"effective_capacity"`
	SchedulingRevision       uint64          `json:"scheduling_revision"`
	StatusTTLSeconds         uint32          `json:"status_ttl_seconds"`
	ProviderLeaseTTLSeconds  uint32          `json:"provider_lease_ttl_seconds"`
	RecoveryGraceSeconds     uint32          `json:"recovery_grace_seconds"`
}

// TimerInfo contains exactly the locally configured values used by the
// rollout inequalities in TODO_relay.md.
type TimerInfo struct {
	HTTPRequestTimeout                    string  `json:"http_request_timeout"`
	RetryInitialBackoff                   string  `json:"retry_initial_backoff"`
	RetryMaximumBackoff                   string  `json:"retry_maximum_backoff"`
	ProviderStatusInterval                string  `json:"provider_status_interval"`
	ProviderStatusMaximumBackoff          string  `json:"provider_status_maximum_backoff"`
	ConfiguredProviderSessionTTL          string  `json:"configured_provider_session_ttl"`
	NodeTokenRefreshFraction              float64 `json:"node_token_refresh_fraction"`
	ProviderLeaseHeartbeatMinimumFraction float64 `json:"provider_lease_heartbeat_minimum_fraction"`
	ProviderLeaseHeartbeatMaximumFraction float64 `json:"provider_lease_heartbeat_maximum_fraction"`
	ReservationTTL                        string  `json:"reservation_ttl"`
	ShutdownDrain                         string  `json:"shutdown_drain"`
}

// RelayInfo is the exact redacted /relay-info response DTO.
type RelayInfo struct {
	PeerID    string      `json:"peer_id"`
	Addresses []string    `json:"addresses"`
	Limits    RelayLimits `json:"limits"`
	DMS       DMSInfo     `json:"dms"`
	Timers    TimerInfo   `json:"timers"`
}

type RelayInfoFunc func(context.Context) (RelayInfo, error)
type DrainFunc func(context.Context, DrainRequest) (DrainResult, error)

func AdminHandler(options Options) (http.Handler, error) {
	if options.Context == nil {
		return nil, errors.New("admin context is required")
	}
	if options.State == nil || options.Registry == nil {
		return nil, errors.New("admin state and Prometheus registry are required")
	}
	if options.RelayInfo == nil || options.Drain == nil {
		return nil, errors.New("admin relay-info and drain callbacks are required")
	}
	drains := &drainCoordinator{
		context: options.Context,
		drain:   options.Drain,
		state:   options.State,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", probe(options.State.Alive, "not live\n"))
	mux.HandleFunc("GET /readyz", probe(options.State.Ready, "not ready\n"))
	mux.HandleFunc("GET /accepting", accepting(options.State))
	mux.Handle("GET /metrics", promhttp.HandlerFor(options.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /relay-info", relayInfo(options.RelayInfo))
	mux.HandleFunc("POST /drain", drain(drains))
	return noStore(mux), nil
}

func MetricsHandler(registry *prometheus.Registry) (http.Handler, error) {
	if registry == nil {
		return nil, errors.New("Prometheus registry is required")
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	return noStore(mux), nil
}

func probe(ok func() bool, unavailable string) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ok() {
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte(unavailable))
			return
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
	}
}

func accepting(state *State) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		snapshot := state.Snapshot().Scheduling
		status := http.StatusOK
		if !snapshot.AcceptingBookings {
			status = http.StatusServiceUnavailable
		}
		writeJSON(response, status, snapshot)
	}
}

func relayInfo(snapshot RelayInfoFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		info, err := snapshot(request.Context())
		if err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, "relay info unavailable")
			return
		}
		info, err = normalizeRelayInfo(info)
		if err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, "relay info unavailable")
			return
		}
		writeJSON(response, http.StatusOK, info)
	}
}

func normalizeRelayInfo(info RelayInfo) (RelayInfo, error) {
	if strings.TrimSpace(info.PeerID) != info.PeerID || info.PeerID == "" {
		return RelayInfo{}, errors.New("relay peer ID is required")
	}
	decodedPeerID, err := peer.Decode(info.PeerID)
	if err != nil || decodedPeerID.String() != info.PeerID {
		return RelayInfo{}, errors.New("relay peer ID is invalid")
	}
	if len(info.Addresses) == 0 {
		return RelayInfo{}, errors.New("at least one relay address is required")
	}
	info.Addresses = append([]string(nil), info.Addresses...)
	sort.Strings(info.Addresses)
	for index, address := range info.Addresses {
		parsed, err := ma.NewMultiaddr(address)
		if err != nil || parsed.String() != address {
			return RelayInfo{}, errors.New("relay addresses must be canonical multiaddrs")
		}
		if index > 0 && address == info.Addresses[index-1] {
			return RelayInfo{}, errors.New("relay addresses must be unique")
		}
	}
	if info.Limits.DurationSeconds == 0 || info.Limits.DataBytesPerDirection <= 0 {
		return RelayInfo{}, errors.New("relay limits must be positive")
	}
	if err := validateAndNormalizeDMSInfo(&info.DMS); err != nil {
		return RelayInfo{}, err
	}
	if err := validateTimerInfo(info.Timers); err != nil {
		return RelayInfo{}, err
	}
	return info, nil
}

func validateAndNormalizeDMSInfo(info *DMSInfo) error {
	if info == nil {
		return errors.New("DMS relay info is required")
	}
	var err error
	info.SessionExpiresAt, err = normalizedTimestamp(info.SessionExpiresAt)
	if err != nil {
		return errors.New("DMS session expiry is invalid")
	}
	info.ProviderNodeJWTExpiresAt, err = normalizedTimestamp(info.ProviderNodeJWTExpiresAt)
	if err != nil {
		return errors.New("provider Node JWT expiry is invalid")
	}
	if info.ShutdownIntent != nil {
		intent := *info.ShutdownIntent
		if intent != ShutdownIntentRestartSameIdentity && intent != ShutdownIntentReassign {
			return errors.New("DMS shutdown intent is invalid")
		}
		info.ShutdownIntent = &intent
	}
	if info.AcceptingBookings && info.Draining {
		return errors.New("provider cannot accept bookings while draining")
	}
	if info.ShutdownIntent != nil && (!info.Draining || info.AcceptingBookings) {
		return errors.New("DMS shutdown intent requires draining state")
	}
	if info.SessionPresent {
		if info.SessionExpiresAt == nil || info.ProviderNodeJWTExpiresAt == nil ||
			info.StatusTTLSeconds == 0 || info.ProviderLeaseTTLSeconds == 0 ||
			info.EffectiveCapacity == 0 || info.RecoveryGraceSeconds == 0 {
			return errors.New("DMS session metadata is incomplete")
		}
		return nil
	}
	if info.AcceptingBookings || info.Draining || info.ShutdownIntent != nil {
		return errors.New("DMS provider status requires a session")
	}
	return nil
}

func normalizedTimestamp(value *time.Time) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	if value.IsZero() {
		return nil, errors.New("timestamp is zero")
	}
	normalized := value.UTC()
	return &normalized, nil
}

func validateTimerInfo(info TimerInfo) error {
	requestTimeout, err := parseStableDuration(info.HTTPRequestTimeout)
	if err != nil {
		return errors.New("HTTP request timeout is invalid")
	}
	initialBackoff, err := parseStableDuration(info.RetryInitialBackoff)
	if err != nil {
		return errors.New("retry initial backoff is invalid")
	}
	maximumBackoff, err := parseStableDuration(info.RetryMaximumBackoff)
	if err != nil || initialBackoff > maximumBackoff {
		return errors.New("retry maximum backoff is invalid")
	}
	statusInterval, err := parseStableDuration(info.ProviderStatusInterval)
	if err != nil {
		return errors.New("provider status interval is invalid")
	}
	statusMaximumBackoff, err := parseStableDuration(info.ProviderStatusMaximumBackoff)
	if err != nil || statusMaximumBackoff > statusInterval {
		return errors.New("provider status maximum backoff is invalid")
	}
	configuredSessionTTL, err := parseStableDuration(info.ConfiguredProviderSessionTTL)
	if err != nil || !durationsFitBefore(configuredSessionTTL, statusInterval, requestTimeout, statusMaximumBackoff) {
		return errors.New("configured provider session TTL is invalid")
	}
	if math.IsNaN(info.NodeTokenRefreshFraction) || math.IsInf(info.NodeTokenRefreshFraction, 0) ||
		info.NodeTokenRefreshFraction <= 0 || info.NodeTokenRefreshFraction >= 1 {
		return errors.New("Node token refresh fraction is invalid")
	}
	minimumHeartbeat := info.ProviderLeaseHeartbeatMinimumFraction
	maximumHeartbeat := info.ProviderLeaseHeartbeatMaximumFraction
	if math.IsNaN(minimumHeartbeat) || math.IsInf(minimumHeartbeat, 0) ||
		math.IsNaN(maximumHeartbeat) || math.IsInf(maximumHeartbeat, 0) ||
		minimumHeartbeat <= 0 || maximumHeartbeat >= 1 || minimumHeartbeat > maximumHeartbeat {
		return errors.New("provider lease heartbeat fractions are invalid")
	}
	if _, err := parseStableDuration(info.ReservationTTL); err != nil {
		return errors.New("reservation TTL is invalid")
	}
	shutdownDrain, err := parseStableDuration(info.ShutdownDrain)
	if err != nil || shutdownDrain > maxDrainDeadlineSeconds*time.Second {
		return errors.New("shutdown drain is invalid")
	}
	return nil
}

func parseStableDuration(raw string) (time.Duration, error) {
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 || duration.String() != raw {
		return 0, errors.New("duration is not a stable positive Go duration")
	}
	return duration, nil
}

func durationsFitBefore(limit time.Duration, values ...time.Duration) bool {
	remaining := limit
	for _, value := range values {
		if value <= 0 || value >= remaining {
			return false
		}
		remaining -= value
	}
	return true
}

func drain(coordinator *drainCoordinator) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if !remoteIP(request.RemoteAddr).IsLoopback() {
			writeAPIError(response, http.StatusForbidden, "forbidden")
			return
		}
		drainRequest, err := decodeDrainRequest(response, request)
		if err != nil {
			writeAPIError(response, http.StatusBadRequest, "invalid request")
			return
		}
		operation, conflict := coordinator.start(drainRequest)
		if conflict {
			writeAPIError(response, http.StatusConflict, "drain already started")
			return
		}
		select {
		case <-operation.done:
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(operation.status)
			_, _ = response.Write(operation.body)
		case <-request.Context().Done():
			return
		}
	}
}

func decodeDrainRequest(response http.ResponseWriter, request *http.Request) (DrainRequest, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return DrainRequest{}, errors.New("content type must be application/json")
	}
	if request.ContentLength > maxDrainBodyBytes {
		return DrainRequest{}, errors.New("request body is too large")
	}
	request.Body = http.MaxBytesReader(response, request.Body, maxDrainBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.UseNumber()

	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return DrainRequest{}, errors.New("request body must be an object")
	}
	var result DrainRequest
	seen := make(map[string]struct{}, 2)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return DrainRequest{}, errors.New("invalid request field")
		}
		field, ok := fieldToken.(string)
		if !ok {
			return DrainRequest{}, errors.New("request field name must be a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return DrainRequest{}, errors.New("duplicate request field")
		}
		seen[field] = struct{}{}
		switch field {
		case "shutdown_intent":
			var intent string
			if err := decoder.Decode(&intent); err != nil {
				return DrainRequest{}, errors.New("invalid shutdown intent")
			}
			result.ShutdownIntent = ShutdownIntent(intent)
		case "deadline_seconds":
			value, err := decoder.Token()
			if err != nil {
				return DrainRequest{}, errors.New("invalid drain deadline")
			}
			number, ok := value.(json.Number)
			if !ok {
				return DrainRequest{}, errors.New("invalid drain deadline")
			}
			deadline, err := strconv.ParseUint(number.String(), 10, 16)
			if err != nil {
				return DrainRequest{}, errors.New("invalid drain deadline")
			}
			result.DeadlineSeconds = uint16(deadline)
		default:
			return DrainRequest{}, errors.New("unknown request field")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return DrainRequest{}, errors.New("request body is not a complete object")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return DrainRequest{}, err
	}
	if len(seen) != 2 {
		return DrainRequest{}, errors.New("request fields are required")
	}
	if result.ShutdownIntent != ShutdownIntentRestartSameIdentity && result.ShutdownIntent != ShutdownIntentReassign {
		return DrainRequest{}, errors.New("invalid shutdown intent")
	}
	if result.DeadlineSeconds < 1 || result.DeadlineSeconds > maxDrainDeadlineSeconds {
		return DrainRequest{}, errors.New("invalid drain deadline")
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("request body has trailing content")
}

func remoteIP(remoteAddress string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

type drainCoordinator struct {
	context context.Context
	drain   DrainFunc
	state   *State

	mu        sync.Mutex
	operation *drainOperation
}

type drainOperation struct {
	request       DrainRequest
	leasesAtStart uint32
	done          chan struct{}
	status        int
	body          []byte
}

func (c *drainCoordinator) start(request DrainRequest) (*drainOperation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.operation != nil {
		if c.operation.request != request {
			return nil, true
		}
		return c.operation, false
	}
	operation := &drainOperation{
		request:       request,
		leasesAtStart: c.state.Snapshot().Scheduling.UsedBookingSlots,
		done:          make(chan struct{}),
	}
	c.operation = operation
	c.state.BeginDrain()
	go c.run(operation)
	return operation, false
}

func (c *drainCoordinator) run(operation *drainOperation) {
	defer func() {
		if recover() != nil {
			result := controlErrorDrainResult(operation.request)
			operation.status = http.StatusServiceUnavailable
			operation.body = mustEncodeJSON(result)
		}
		close(operation.done)
	}()

	base := context.WithoutCancel(c.context)
	ctx, cancel := context.WithTimeout(base, time.Duration(operation.request.DeadlineSeconds)*time.Second)
	defer cancel()
	callbackResult := make(chan drainCallbackResult, 1)
	go func() {
		callbackResult <- callDrain(c.drain, ctx, operation.request)
	}()
	var result DrainResult
	var err error
	select {
	case callback := <-callbackResult:
		result, err = callback.result, callback.err
	case <-ctx.Done():
		result = DrainResult{
			LeasesTotal:     operation.leasesAtStart,
			LeasesRemaining: operation.leasesAtStart,
		}
		err = context.DeadlineExceeded
	}
	if errors.Is(err, ErrDrainConflict) {
		operation.status = http.StatusConflict
		operation.body = mustEncodeJSON(apiError{Error: "drain already started"})
		// A typed conflict means another trigger selected the process-wide drain
		// before this HTTP request reached the application coordinator. Do not
		// retain the rejected request: a later HTTP request that exactly matches
		// the selected drain must still be able to join and replay it.
		c.forgetRejected(operation)
		return
	}
	result, operation.status = normalizeDrainResult(operation.request, result, err)
	operation.body = mustEncodeJSON(result)
}

func (c *drainCoordinator) forgetRejected(operation *drainOperation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.operation == operation {
		c.operation = nil
	}
}

type drainCallbackResult struct {
	result DrainResult
	err    error
}

func callDrain(callback DrainFunc, ctx context.Context, request DrainRequest) (result drainCallbackResult) {
	defer func() {
		if recover() != nil {
			result = drainCallbackResult{err: errors.New("drain callback panicked")}
		}
	}()
	result.result, result.err = callback(ctx, request)
	return result
}

func normalizeDrainResult(request DrainRequest, result DrainResult, callbackErr error) (DrainResult, int) {
	result.ShutdownIntent = request.ShutdownIntent
	if errors.Is(callbackErr, context.DeadlineExceeded) {
		result.Result = DrainOutcomeDeadlineExceeded
	} else if callbackErr != nil {
		result.Result = DrainOutcomeControlError
	}
	if !validDrainCounts(result) {
		return controlErrorDrainResult(request), http.StatusServiceUnavailable
	}
	switch result.Result {
	case DrainOutcomeCompleted:
		if result.LeasesRemaining != 0 || result.LeasesReleased != result.LeasesTotal {
			return controlErrorDrainResult(request), http.StatusServiceUnavailable
		}
		return result, http.StatusOK
	case DrainOutcomeDeadlineExceeded:
		return result, http.StatusGatewayTimeout
	case DrainOutcomeControlError:
		return result, http.StatusServiceUnavailable
	default:
		return controlErrorDrainResult(request), http.StatusServiceUnavailable
	}
}

func validDrainCounts(result DrainResult) bool {
	return uint64(result.LeasesReleased)+uint64(result.LeasesRemaining) == uint64(result.LeasesTotal)
}

func controlErrorDrainResult(request DrainRequest) DrainResult {
	return DrainResult{
		ShutdownIntent: request.ShutdownIntent,
		Result:         DrainOutcomeControlError,
	}
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(response, request)
	})
}

type apiError struct {
	Error string `json:"error"`
}

func writeAPIError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, apiError{Error: message})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = response.Write(mustEncodeJSON(value))
}

func mustEncodeJSON(value any) []byte {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		panic(fmt.Sprintf("encode static admin response: %v", err))
	}
	return body.Bytes()
}
