package admin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestStateTransitionsAndLowCardinalityMetrics(t *testing.T) {
	state := NewState()
	require.Equal(t, StateSnapshot{
		Alive:      true,
		Scheduling: SchedulingSnapshot{State: AcceptingStateNotReady},
	}, state.Snapshot())

	require.NoError(t, state.SetScheduling(AcceptingStateAccepting, 4, 1))
	require.False(t, state.Accepting(), "an unready relay must fail closed")
	state.SetReady(true)
	require.NoError(t, state.SetScheduling(AcceptingStateAccepting, 4, 1))
	require.Equal(t, StateSnapshot{
		Alive: true,
		Ready: true,
		Scheduling: SchedulingSnapshot{
			State:             AcceptingStateAccepting,
			AcceptingBookings: true,
			EffectiveCapacity: 4,
			UsedBookingSlots:  1,
			FreeBookingSlots:  3,
		},
	}, state.Snapshot())

	registry := prometheus.NewRegistry()
	require.NoError(t, state.RegisterMetrics(registry))
	handler := newAdminHandler(t, state, registry, nil)
	response := serve(handler, http.MethodGet, "/metrics", "", "127.0.0.1:1234", "")
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), "auki_relay_node_data_plane_ready 1")
	require.Contains(t, response.Body.String(), "auki_relay_node_accepting_bookings 1")
	require.Contains(t, response.Body.String(), "auki_relay_node_draining 0")

	state.BeginDrain()
	require.Equal(t, StateSnapshot{
		Alive: true,
		Ready: true,
		Scheduling: SchedulingSnapshot{
			State:             AcceptingStateDraining,
			Draining:          true,
			EffectiveCapacity: 4,
			UsedBookingSlots:  1,
			FreeBookingSlots:  3,
		},
	}, state.Snapshot())
	response = serve(handler, http.MethodGet, "/accepting", "", "127.0.0.1:1234", "")
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.JSONEq(t, `{
		"state":"draining",
		"accepting_bookings":false,
		"draining":true,
		"effective_capacity":4,
		"used_booking_slots":1,
		"free_booking_slots":3
	}`, response.Body.String())

	require.NoError(t, state.SetScheduling(AcceptingStateAccepting, 4, 1))
	require.Equal(t, AcceptingStateDraining, state.Snapshot().Scheduling.State, "a stale scheduling update must not reopen a draining process")
	require.NoError(t, state.SetScheduling(AcceptingStateControlStale, 4, 1))
	require.Equal(t, AcceptingStateDraining, state.Snapshot().Scheduling.State, "drain is immutable for the process incarnation")
	require.NoError(t, state.SetScheduling(AcceptingStateAccepting, 4, 1))
	state.SetReady(false)
	require.False(t, state.Accepting())
	state.SetAlive(false)
	require.Equal(t, AcceptingStateTerminal, state.Snapshot().Scheduling.State)

	require.Error(t, state.SetScheduling(AcceptingState("other"), 4, 0))
	require.Error(t, state.SetScheduling(AcceptingStateAccepting, 4, 4))
	require.Error(t, state.SetScheduling(AcceptingStateSaturated, 4, 3))
	require.Error(t, state.SetScheduling(AcceptingStateControlStale, 3, 4))
}

func TestAcceptingEndpointReportsEveryBoundedReason(t *testing.T) {
	tests := []struct {
		state      AcceptingState
		effective  uint32
		used       uint32
		httpStatus int
	}{
		{state: AcceptingStateNotReady, effective: 4, used: 1, httpStatus: http.StatusServiceUnavailable},
		{state: AcceptingStateBootstrapClosed, effective: 4, used: 1, httpStatus: http.StatusServiceUnavailable},
		{state: AcceptingStateControlStale, effective: 4, used: 1, httpStatus: http.StatusServiceUnavailable},
		{state: AcceptingStateSaturated, effective: 4, used: 4, httpStatus: http.StatusServiceUnavailable},
		{state: AcceptingStateDraining, effective: 4, used: 2, httpStatus: http.StatusServiceUnavailable},
		{state: AcceptingStateAccepting, effective: 4, used: 1, httpStatus: http.StatusOK},
		{state: AcceptingStateTerminal, effective: 4, used: 1, httpStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			state := NewState()
			state.SetReady(true)
			require.NoError(t, state.SetScheduling(test.state, test.effective, test.used))
			handler := newAdminHandler(t, state, prometheus.NewRegistry(), nil)
			response := serve(handler, http.MethodGet, "/accepting", "", "127.0.0.1:1", "")
			require.Equal(t, test.httpStatus, response.Code)
			var snapshot SchedulingSnapshot
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
			require.Equal(t, test.state, snapshot.State)
			require.Equal(t, test.state == AcceptingStateAccepting, snapshot.AcceptingBookings)
			require.Equal(t, test.state == AcceptingStateDraining, snapshot.Draining)
			require.Equal(t, test.effective, snapshot.EffectiveCapacity)
			require.Equal(t, test.used, snapshot.UsedBookingSlots)
			require.Equal(t, test.effective-test.used, snapshot.FreeBookingSlots)
		})
	}
}

func TestEveryAdminResponseIsNoStore(t *testing.T) {
	state := NewState()
	registry := prometheus.NewRegistry()
	handler := newAdminHandler(t, state, registry, nil)

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/livez"},
		{method: http.MethodGet, path: "/readyz"},
		{method: http.MethodGet, path: "/accepting"},
		{method: http.MethodGet, path: "/metrics"},
		{method: http.MethodGet, path: "/relay-info"},
		{method: http.MethodGet, path: "/missing"},
		{method: http.MethodPut, path: "/drain"},
	} {
		response := serve(handler, test.method, test.path, test.body, "127.0.0.1:1234", "")
		require.Equal(t, "no-store", response.Header().Get("Cache-Control"), "%s %s", test.method, test.path)
	}
}

func TestRelayInfoIsExactSortedUTCAndRedacted(t *testing.T) {
	state := NewState()
	registry := prometheus.NewRegistry()
	options := testOptions(state, registry)
	localTime := time.Date(2026, time.August, 20, 19, 34, 56, 0, time.FixedZone("test", 7*60*60))
	jwtLocalTime := localTime.Add(time.Hour)
	options.RelayInfo = func(context.Context) (RelayInfo, error) {
		return RelayInfo{
			PeerID: "12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP",
			Addresses: []string{
				"/ip6/::1/tcp/4001",
				"/ip4/127.0.0.1/tcp/4001",
			},
			Limits: RelayLimits{DurationSeconds: 900, DataBytesPerDirection: 1048576},
			DMS: DMSInfo{
				SessionPresent:           true,
				AcceptingBookings:        true,
				SessionExpiresAt:         &localTime,
				ProviderNodeJWTExpiresAt: &jwtLocalTime,
				EffectiveCapacity:        16,
				SchedulingRevision:       42,
				StatusTTLSeconds:         180,
				ProviderLeaseTTLSeconds:  120,
				RecoveryGraceSeconds:     1200,
			},
			Timers: TimerInfo{
				HTTPRequestTimeout:                    "10s",
				RetryInitialBackoff:                   "1s",
				RetryMaximumBackoff:                   "30s",
				ProviderStatusInterval:                "30s",
				ProviderStatusMaximumBackoff:          "10s",
				ConfiguredProviderSessionTTL:          "3m0s",
				NodeTokenRefreshFraction:              0.75,
				ProviderLeaseHeartbeatMinimumFraction: 0.25,
				ProviderLeaseHeartbeatMaximumFraction: 0.35,
				ReservationTTL:                        "10m0s",
				ShutdownDrain:                         "15m0s",
			},
		}, nil
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	response := serve(handler, http.MethodGet, "/relay-info", "", "127.0.0.1:1234", "")
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, `{"peer_id":"12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP","addresses":["/ip4/127.0.0.1/tcp/4001","/ip6/::1/tcp/4001"],"limits":{"duration_seconds":900,"data_bytes_per_direction":1048576},"dms":{"session_present":true,"accepting_bookings":true,"draining":false,"shutdown_intent":null,"session_expires_at":"2026-08-20T12:34:56Z","provider_node_jwt_expires_at":"2026-08-20T13:34:56Z","effective_capacity":16,"scheduling_revision":42,"status_ttl_seconds":180,"provider_lease_ttl_seconds":120,"recovery_grace_seconds":1200},"timers":{"http_request_timeout":"10s","retry_initial_backoff":"1s","retry_maximum_backoff":"30s","provider_status_interval":"30s","provider_status_maximum_backoff":"10s","configured_provider_session_ttl":"3m0s","node_token_refresh_fraction":0.75,"provider_lease_heartbeat_minimum_fraction":0.25,"provider_lease_heartbeat_maximum_fraction":0.35,"reservation_ttl":"10m0s","shutdown_drain":"15m0s"}}`+"\n", response.Body.String())
	for _, forbidden := range []string{"provider_session_id", "node_id", "booking_id", "assignment_id", "organization", "domain_id", "jwt_value", "token_value", "credential", "wallet", "private_key"} {
		require.NotContains(t, strings.ToLower(response.Body.String()), forbidden)
	}
}

func TestRelayInfoFailuresAreGeneric(t *testing.T) {
	state := NewState()
	registry := prometheus.NewRegistry()
	options := testOptions(state, registry)
	options.RelayInfo = func(context.Context) (RelayInfo, error) {
		return RelayInfo{}, errors.New("secret-jwt-and-provider-session")
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	response := serve(handler, http.MethodGet, "/relay-info", "", "127.0.0.1:1234", "")
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.JSONEq(t, `{"error":"relay info unavailable"}`, response.Body.String())
	require.NotContains(t, response.Body.String(), "secret")

	options = testOptions(state, prometheus.NewRegistry())
	options.RelayInfo = func(context.Context) (RelayInfo, error) {
		return RelayInfo{PeerID: "peer", Addresses: []string{""}}, nil
	}
	handler, err = AdminHandler(options)
	require.NoError(t, err)
	response = serve(handler, http.MethodGet, "/relay-info", "", "127.0.0.1:1234", "")
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.JSONEq(t, `{"error":"relay info unavailable"}`, response.Body.String())
}

func TestRelayInfoValidationRejectsNonCanonicalOrUnsafeSnapshots(t *testing.T) {
	base, err := testOptions(NewState(), prometheus.NewRegistry()).RelayInfo(context.Background())
	require.NoError(t, err)
	tests := []struct {
		name   string
		mutate func(*RelayInfo)
	}{
		{name: "invalid peer", mutate: func(info *RelayInfo) { info.PeerID = "peer" }},
		{name: "invalid address", mutate: func(info *RelayInfo) { info.Addresses = []string{"not-a-multiaddr"} }},
		{name: "duplicate address", mutate: func(info *RelayInfo) { info.Addresses = append(info.Addresses, info.Addresses[0]) }},
		{name: "zero duration limit", mutate: func(info *RelayInfo) { info.Limits.DurationSeconds = 0 }},
		{name: "missing session expiry", mutate: func(info *RelayInfo) { info.DMS.SessionExpiresAt = nil }},
		{name: "missing JWT expiry", mutate: func(info *RelayInfo) { info.DMS.ProviderNodeJWTExpiresAt = nil }},
		{name: "accepting and draining", mutate: func(info *RelayInfo) { info.DMS.AcceptingBookings = true; info.DMS.Draining = true }},
		{name: "status without session", mutate: func(info *RelayInfo) { info.DMS.SessionPresent = false; info.DMS.Draining = true }},
		{name: "unknown shutdown intent", mutate: func(info *RelayInfo) { intent := ShutdownIntent("other"); info.DMS.ShutdownIntent = &intent }},
		{name: "intent without draining", mutate: func(info *RelayInfo) { intent := ShutdownIntentReassign; info.DMS.ShutdownIntent = &intent }},
		{name: "noncanonical duration", mutate: func(info *RelayInfo) { info.Timers.ProviderStatusInterval = "30.0s" }},
		{name: "timing inequality", mutate: func(info *RelayInfo) { info.Timers.ConfiguredProviderSessionTTL = "50s" }},
		{name: "retry inversion", mutate: func(info *RelayInfo) { info.Timers.RetryInitialBackoff = "31s" }},
		{name: "NaN refresh fraction", mutate: func(info *RelayInfo) { info.Timers.NodeTokenRefreshFraction = math.NaN() }},
		{name: "heartbeat inversion", mutate: func(info *RelayInfo) {
			info.Timers.ProviderLeaseHeartbeatMinimumFraction = 0.5
			info.Timers.ProviderLeaseHeartbeatMaximumFraction = 0.4
		}},
		{name: "unbounded shutdown", mutate: func(info *RelayInfo) { info.Timers.ShutdownDrain = "15m1s" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := base
			info.Addresses = append([]string(nil), base.Addresses...)
			test.mutate(&info)
			_, err := normalizeRelayInfo(info)
			require.Error(t, err)
		})
	}

	validDrain := base
	validDrain.DMS.AcceptingBookings = false
	validDrain.DMS.Draining = true
	intent := ShutdownIntentRestartSameIdentity
	validDrain.DMS.ShutdownIntent = &intent
	normalized, err := normalizeRelayInfo(validDrain)
	require.NoError(t, err)
	require.Equal(t, ShutdownIntentRestartSameIdentity, *normalized.DMS.ShutdownIntent)
}

func TestDrainAuthorizationUsesOnlyKernelObservedLoopback(t *testing.T) {
	state := NewState()
	state.SetReady(true)
	registry := prometheus.NewRegistry()
	var calls atomic.Int32
	options := testOptions(state, registry)
	options.Drain = func(context.Context, DrainRequest) (DrainResult, error) {
		calls.Add(1)
		return DrainResult{Result: DrainOutcomeCompleted, LeasesTotal: 2, LeasesReleased: 2}, nil
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	body := `{"shutdown_intent":"reassign","deadline_seconds":30}`

	for _, authorization := range []string{"", "Bearer wrong", "Bearer test-operations-bearer"} {
		response := serve(handler, http.MethodPost, "/drain", body, "203.0.113.8:4321", authorization)
		require.Equal(t, http.StatusForbidden, response.Code)
		require.JSONEq(t, `{"error":"forbidden"}`, response.Body.String())
	}
	require.Zero(t, calls.Load())

	request := httptest.NewRequest(http.MethodPost, "/drain", strings.NewReader(body))
	request.RemoteAddr = "203.0.113.8:4321"
	request.Host = "127.0.0.1"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Forwarded-For", "127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code, "forwarded headers never establish loopback authority")

	response = serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:4321", "")
	require.Equal(t, http.StatusOK, response.Code)
	require.JSONEq(t, `{
		"shutdown_intent":"reassign",
		"result":"completed",
		"leases_total":2,
		"leases_released":2,
		"leases_remaining":0
	}`, response.Body.String())
	require.Equal(t, int32(1), calls.Load())
	require.True(t, state.Draining())
	require.False(t, state.Accepting())

	loopbackState := NewState()
	loopbackState.SetReady(true)
	loopbackOptions := testOptions(loopbackState, prometheus.NewRegistry())
	loopbackHandler, err := AdminHandler(loopbackOptions)
	require.NoError(t, err)
	for _, remote := range []string{"127.0.0.1:9", "[::1]:9"} {
		response = serve(loopbackHandler, http.MethodPost, "/drain", `{"shutdown_intent":"restart_same_identity","deadline_seconds":1}`, remote, "")
		require.Equal(t, http.StatusOK, response.Code, remote)
	}
}

func TestDrainRejectsEveryNonExactRequest(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "missing content type", body: `{"shutdown_intent":"reassign","deadline_seconds":1}`},
		{name: "wrong content type", contentType: "text/plain", body: `{"shutdown_intent":"reassign","deadline_seconds":1}`},
		{name: "array", contentType: "application/json", body: `[]`},
		{name: "empty", contentType: "application/json", body: ``},
		{name: "missing intent", contentType: "application/json", body: `{"deadline_seconds":1}`},
		{name: "missing deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign"}`},
		{name: "unknown", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":1,"extra":true}`},
		{name: "case variant", contentType: "application/json", body: `{"Shutdown_Intent":"reassign","deadline_seconds":1}`},
		{name: "duplicate", contentType: "application/json", body: `{"shutdown_intent":"reassign","shutdown_intent":"reassign","deadline_seconds":1}`},
		{name: "null intent", contentType: "application/json", body: `{"shutdown_intent":null,"deadline_seconds":1}`},
		{name: "null deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":null}`},
		{name: "string deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":"1"}`},
		{name: "fractional deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":1.5}`},
		{name: "exponent deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":1e0}`},
		{name: "zero deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":0}`},
		{name: "negative deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":-1}`},
		{name: "large deadline", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":901}`},
		{name: "invalid intent", contentType: "application/json", body: `{"shutdown_intent":"restart","deadline_seconds":1}`},
		{name: "trailing JSON", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":1}{}`},
		{name: "trailing token", contentType: "application/json", body: `{"shutdown_intent":"reassign","deadline_seconds":1} true`},
		{name: "oversize", contentType: "application/json", body: strings.Repeat(" ", maxDrainBodyBytes) + `{"shutdown_intent":"reassign","deadline_seconds":1}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			state := NewState()
			options := testOptions(state, prometheus.NewRegistry())
			options.Drain = func(context.Context, DrainRequest) (DrainResult, error) {
				calls.Add(1)
				return DrainResult{Result: DrainOutcomeCompleted}, nil
			}
			handler, err := AdminHandler(options)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/drain", strings.NewReader(test.body))
			request.RemoteAddr = "127.0.0.1:1234"
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.JSONEq(t, `{"error":"invalid request"}`, response.Body.String())
			require.Zero(t, calls.Load())
			require.False(t, state.Draining())
		})
	}
}

func TestDrainAcceptsExactDeadlineBounds(t *testing.T) {
	for _, deadline := range []string{"1", "900"} {
		t.Run(deadline, func(t *testing.T) {
			state := NewState()
			options := testOptions(state, prometheus.NewRegistry())
			handler, err := AdminHandler(options)
			require.NoError(t, err)
			body := `{"shutdown_intent":"restart_same_identity","deadline_seconds":` + deadline + `}`
			response := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:1", "")
			require.Equal(t, http.StatusOK, response.Code)
		})
	}
}

func TestConcurrentIdenticalDrainWaitsOnceAndReplays(t *testing.T) {
	state := NewState()
	state.SetReady(true)
	require.NoError(t, state.SetScheduling(AcceptingStateAccepting, 4, 1))
	options := testOptions(state, prometheus.NewRegistry())
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	options.Drain = func(ctx context.Context, request DrainRequest) (DrainResult, error) {
		require.Equal(t, ShutdownIntentRestartSameIdentity, request.ShutdownIntent)
		require.Equal(t, uint16(30), request.DeadlineSeconds)
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return DrainResult{Result: DrainOutcomeCompleted, LeasesTotal: 3, LeasesReleased: 3}, nil
		case <-ctx.Done():
			return DrainResult{}, ctx.Err()
		}
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	body := `{"shutdown_intent":"restart_same_identity","deadline_seconds":30}`

	responses := make(chan *httptest.ResponseRecorder, 2)
	invoke := func() {
		responses <- serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:2", "")
	}
	go invoke()
	<-started
	require.Equal(t, AcceptingStateDraining, state.Snapshot().Scheduling.State)
	require.Equal(t, uint32(4), state.Snapshot().Scheduling.EffectiveCapacity)
	require.Equal(t, uint32(1), state.Snapshot().Scheduling.UsedBookingSlots)
	go invoke()

	conflict := serve(handler, http.MethodPost, "/drain", `{"shutdown_intent":"reassign","deadline_seconds":30}`, "127.0.0.1:2", "")
	require.Equal(t, http.StatusConflict, conflict.Code)
	require.JSONEq(t, `{"error":"drain already started"}`, conflict.Body.String())
	conflict = serve(handler, http.MethodPost, "/drain", `{"shutdown_intent":"restart_same_identity","deadline_seconds":31}`, "127.0.0.1:2", "")
	require.Equal(t, http.StatusConflict, conflict.Code)

	close(release)
	first := <-responses
	second := <-responses
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, first.Code, second.Code)
	require.Equal(t, first.Body.String(), second.Body.String())
	require.Equal(t, int32(1), calls.Load())

	replayed := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:2", "")
	require.Equal(t, first.Code, replayed.Code)
	require.Equal(t, first.Body.String(), replayed.Body.String())
	require.Equal(t, int32(1), calls.Load())
}

func TestDrainMapsCrossTriggerConflictToHTTPConflict(t *testing.T) {
	options := testOptions(NewState(), prometheus.NewRegistry())
	options.Drain = func(context.Context, DrainRequest) (DrainResult, error) {
		return DrainResult{}, ErrDrainConflict
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)

	response := serve(handler, http.MethodPost, "/drain", `{"shutdown_intent":"reassign","deadline_seconds":30}`, "127.0.0.1:2", "")
	require.Equal(t, http.StatusConflict, response.Code)
	require.JSONEq(t, `{"error":"drain already started"}`, response.Body.String())
}

func TestRejectedCrossTriggerDoesNotPoisonMatchingDrainReplay(t *testing.T) {
	state := NewState()
	state.SetReady(true)
	require.NoError(t, state.SetScheduling(AcceptingStateAccepting, 4, 2))
	options := testOptions(state, prometheus.NewRegistry())
	selectedBySignal := DrainRequest{
		ShutdownIntent:  ShutdownIntentRestartSameIdentity,
		DeadlineSeconds: 30,
	}
	selectedDone := make(chan struct{})
	matchingJoined := make(chan struct{})
	var joinedOnce sync.Once
	var calls atomic.Int32
	options.Drain = func(ctx context.Context, request DrainRequest) (DrainResult, error) {
		calls.Add(1)
		if request != selectedBySignal {
			return DrainResult{}, ErrDrainConflict
		}
		joinedOnce.Do(func() { close(matchingJoined) })
		select {
		case <-selectedDone:
			return DrainResult{Result: DrainOutcomeCompleted, LeasesTotal: 2, LeasesReleased: 2}, nil
		case <-ctx.Done():
			return DrainResult{}, ctx.Err()
		}
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)

	conflict := serve(handler, http.MethodPost, "/drain", `{"shutdown_intent":"reassign","deadline_seconds":30}`, "127.0.0.1:2", "")
	require.Equal(t, http.StatusConflict, conflict.Code)
	require.JSONEq(t, `{"error":"drain already started"}`, conflict.Body.String())
	require.Equal(t, int32(1), calls.Load())

	body := `{"shutdown_intent":"restart_same_identity","deadline_seconds":30}`
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:2", "")
	}()
	<-matchingJoined
	select {
	case <-responses:
		t.Fatal("matching HTTP request returned before the signal-selected drain completed")
	default:
	}
	close(selectedDone)
	matching := <-responses
	require.Equal(t, http.StatusOK, matching.Code)
	require.JSONEq(t, `{
		"shutdown_intent":"restart_same_identity",
		"result":"completed",
		"leases_total":2,
		"leases_released":2,
		"leases_remaining":0
	}`, matching.Body.String())
	require.Equal(t, int32(2), calls.Load())

	replayed := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:2", "")
	require.Equal(t, matching.Code, replayed.Code)
	require.Equal(t, matching.Body.String(), replayed.Body.String())
	require.Equal(t, int32(2), calls.Load())
}

func TestCanceledDrainCallerDoesNotCancelOperation(t *testing.T) {
	state := NewState()
	options := testOptions(state, prometheus.NewRegistry())
	started := make(chan struct{})
	release := make(chan struct{})
	canceled := make(chan struct{})
	options.Drain = func(ctx context.Context, _ DrainRequest) (DrainResult, error) {
		close(started)
		select {
		case <-release:
			return DrainResult{Result: DrainOutcomeCompleted}, nil
		case <-ctx.Done():
			close(canceled)
			return DrainResult{}, ctx.Err()
		}
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	body := `{"shutdown_intent":"reassign","deadline_seconds":30}`
	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/drain", strings.NewReader(body)).WithContext(requestContext)
	request.RemoteAddr = "127.0.0.1:3"
	request.Header.Set("Content-Type", "application/json")
	returned := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(returned)
	}()
	<-started
	cancel()
	<-returned
	select {
	case <-canceled:
		t.Fatal("client cancellation canceled the drain operation")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)

	replayed := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:3", "")
	require.Equal(t, http.StatusOK, replayed.Code)
}

func TestDrainCallbackDeadlineAndErrorsAreBoundedAndRedacted(t *testing.T) {
	state := NewState()
	options := testOptions(state, prometheus.NewRegistry())
	options.Drain = func(ctx context.Context, _ DrainRequest) (DrainResult, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		remaining := time.Until(deadline)
		require.Positive(t, remaining)
		require.LessOrEqual(t, remaining, time.Second)
		return DrainResult{}, errors.New("secret provider session and booking IDs")
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	body := `{"shutdown_intent":"reassign","deadline_seconds":1}`
	first := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:4", "")
	require.Equal(t, http.StatusServiceUnavailable, first.Code)
	require.JSONEq(t, `{
		"shutdown_intent":"reassign",
		"result":"control_error",
		"leases_total":0,
		"leases_released":0,
		"leases_remaining":0
	}`, first.Body.String())
	require.NotContains(t, first.Body.String(), "secret")
	replayed := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:4", "")
	require.Equal(t, first.Code, replayed.Code)
	require.Equal(t, first.Body.String(), replayed.Body.String())
}

func TestDrainOutcomesMapToExactStatusesAndRedactedCounts(t *testing.T) {
	tests := []struct {
		name           string
		result         DrainResult
		err            error
		expectedStatus int
		expected       DrainResult
	}{
		{
			name:           "completed",
			result:         DrainResult{ShutdownIntent: ShutdownIntentReassign, Result: DrainOutcomeCompleted, LeasesTotal: 3, LeasesReleased: 3},
			expectedStatus: http.StatusOK,
			expected:       DrainResult{ShutdownIntent: ShutdownIntentRestartSameIdentity, Result: DrainOutcomeCompleted, LeasesTotal: 3, LeasesReleased: 3},
		},
		{
			name:           "deadline exceeded",
			result:         DrainResult{Result: DrainOutcomeDeadlineExceeded, LeasesTotal: 3, LeasesReleased: 2, LeasesRemaining: 1},
			expectedStatus: http.StatusGatewayTimeout,
			expected:       DrainResult{ShutdownIntent: ShutdownIntentRestartSameIdentity, Result: DrainOutcomeDeadlineExceeded, LeasesTotal: 3, LeasesReleased: 2, LeasesRemaining: 1},
		},
		{
			name:           "control error",
			result:         DrainResult{Result: DrainOutcomeControlError, LeasesTotal: 3, LeasesReleased: 1, LeasesRemaining: 2},
			expectedStatus: http.StatusServiceUnavailable,
			expected:       DrainResult{ShutdownIntent: ShutdownIntentRestartSameIdentity, Result: DrainOutcomeControlError, LeasesTotal: 3, LeasesReleased: 1, LeasesRemaining: 2},
		},
		{
			name:           "callback error",
			result:         DrainResult{LeasesTotal: 3, LeasesReleased: 1, LeasesRemaining: 2},
			err:            errors.New("secret booking and session IDs"),
			expectedStatus: http.StatusServiceUnavailable,
			expected:       DrainResult{ShutdownIntent: ShutdownIntentRestartSameIdentity, Result: DrainOutcomeControlError, LeasesTotal: 3, LeasesReleased: 1, LeasesRemaining: 2},
		},
		{
			name:           "context deadline",
			result:         DrainResult{LeasesTotal: 3, LeasesReleased: 1, LeasesRemaining: 2},
			err:            context.DeadlineExceeded,
			expectedStatus: http.StatusGatewayTimeout,
			expected:       DrainResult{ShutdownIntent: ShutdownIntentRestartSameIdentity, Result: DrainOutcomeDeadlineExceeded, LeasesTotal: 3, LeasesReleased: 1, LeasesRemaining: 2},
		},
		{
			name:           "completed with remaining leases fails closed",
			result:         DrainResult{Result: DrainOutcomeCompleted, LeasesTotal: 3, LeasesReleased: 2, LeasesRemaining: 1},
			expectedStatus: http.StatusServiceUnavailable,
			expected:       DrainResult{ShutdownIntent: ShutdownIntentRestartSameIdentity, Result: DrainOutcomeControlError},
		},
		{
			name:           "inconsistent counts fail closed",
			result:         DrainResult{Result: DrainOutcomeDeadlineExceeded, LeasesTotal: 3, LeasesReleased: 2, LeasesRemaining: 2},
			expectedStatus: http.StatusServiceUnavailable,
			expected:       DrainResult{ShutdownIntent: ShutdownIntentRestartSameIdentity, Result: DrainOutcomeControlError},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions(NewState(), prometheus.NewRegistry())
			options.Drain = func(context.Context, DrainRequest) (DrainResult, error) {
				return test.result, test.err
			}
			handler, err := AdminHandler(options)
			require.NoError(t, err)
			response := serve(handler, http.MethodPost, "/drain", `{"shutdown_intent":"restart_same_identity","deadline_seconds":30}`, "127.0.0.1:5", "")
			require.Equal(t, test.expectedStatus, response.Code)
			var result DrainResult
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.Equal(t, test.expected, result)
			require.NotContains(t, response.Body.String(), "secret")
		})
	}
}

func TestDrainForcesItsDeadlineWhenCallbackIgnoresCancellation(t *testing.T) {
	state := NewState()
	state.SetReady(true)
	require.NoError(t, state.SetScheduling(AcceptingStateControlStale, 4, 2))
	options := testOptions(state, prometheus.NewRegistry())
	release := make(chan struct{})
	options.Drain = func(context.Context, DrainRequest) (DrainResult, error) {
		<-release
		return DrainResult{Result: DrainOutcomeCompleted}, nil
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	started := time.Now()
	response := serve(handler, http.MethodPost, "/drain", `{"shutdown_intent":"reassign","deadline_seconds":1}`, "127.0.0.1:6", "")
	elapsed := time.Since(started)
	close(release)
	require.Equal(t, http.StatusGatewayTimeout, response.Code)
	require.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	require.Less(t, elapsed, 2*time.Second)
	require.JSONEq(t, `{
		"shutdown_intent":"reassign",
		"result":"deadline_exceeded",
		"leases_total":2,
		"leases_released":0,
		"leases_remaining":2
	}`, response.Body.String())
}

func TestDrainCallbackPanicIsRedactedAndReplayed(t *testing.T) {
	options := testOptions(NewState(), prometheus.NewRegistry())
	var calls atomic.Int32
	options.Drain = func(context.Context, DrainRequest) (DrainResult, error) {
		calls.Add(1)
		panic("secret provider state")
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	body := `{"shutdown_intent":"reassign","deadline_seconds":1}`
	first := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:7", "")
	require.Equal(t, http.StatusServiceUnavailable, first.Code)
	require.NotContains(t, first.Body.String(), "secret")
	second := serve(handler, http.MethodPost, "/drain", body, "127.0.0.1:7", "")
	require.Equal(t, first.Body.String(), second.Body.String())
	require.Equal(t, int32(1), calls.Load())
}

func TestAdminHandlerRejectsMissingOrUnsafeDependencies(t *testing.T) {
	valid := testOptions(NewState(), prometheus.NewRegistry())
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "context", mutate: func(options *Options) { options.Context = nil }},
		{name: "state", mutate: func(options *Options) { options.State = nil }},
		{name: "registry", mutate: func(options *Options) { options.Registry = nil }},
		{name: "relay info", mutate: func(options *Options) { options.RelayInfo = nil }},
		{name: "drain", mutate: func(options *Options) { options.Drain = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			_, err := AdminHandler(options)
			require.Error(t, err)
		})
	}
}

func TestAdminStateIsRaceSafe(t *testing.T) {
	state := NewState()
	state.SetReady(true)
	var workers sync.WaitGroup
	for index := 0; index < 16; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				reason := AcceptingStateControlStale
				switch (index + iteration) % 3 {
				case 0:
					reason = AcceptingStateAccepting
				case 1:
					reason = AcceptingStateDraining
				}
				if err := state.SetScheduling(reason, 4, 1); err != nil {
					t.Errorf("set scheduling state: %v", err)
					return
				}
				_ = state.Snapshot()
				_ = state.Accepting()
			}
		}(index)
	}
	workers.Wait()
	snapshot := state.Snapshot().Scheduling
	require.False(t, snapshot.AcceptingBookings && snapshot.Draining)
}

func newAdminHandler(t *testing.T, state *State, registry *prometheus.Registry, mutate func(*Options)) http.Handler {
	t.Helper()
	options := testOptions(state, registry)
	if mutate != nil {
		mutate(&options)
	}
	handler, err := AdminHandler(options)
	require.NoError(t, err)
	return handler
}

func serve(handler http.Handler, method, path, body, remoteAddress, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = remoteAddress
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
