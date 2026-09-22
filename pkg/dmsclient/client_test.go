package dmsclient

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

type dmsHarness struct {
	t        *testing.T
	now      time.Time
	token    string
	expected Expectations
	session  uuid.UUID

	mu          sync.Mutex
	openCount   int
	statusCount int
	deleteCount int
	mutate      func(*sessionResponse)
}

func (h *dmsHarness) handler(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, "Bearer "+h.token, r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/relay-provider/session":
		h.handleOpen(w, r)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/relay-provider/status":
		h.handleStatus(w, r)
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/relay-provider/session/"+h.session.String():
		h.handleDelete(w, r)
	default:
		h.t.Errorf("unexpected DMS request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (h *dmsHarness) handleOpen(w http.ResponseWriter, r *http.Request) {
	var request openSessionRequest
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&request))
	require.NotEqual(h.t, uuid.Nil, request.BootNonce)
	require.Equal(h.t, h.expected.Metadata.BaseAddresses, request.BaseAddresses)
	require.Equal(h.t, h.expected.Metadata.EndpointKeys, request.EndpointKeys)
	require.Equal(h.t, h.expected.Metadata.Limits, request.Limits)
	require.Equal(h.t, h.expected.Metadata.ConfigFingerprint, request.ConfigFingerprint)
	h.mu.Lock()
	h.openCount++
	count := h.openCount
	h.mu.Unlock()
	status := http.StatusCreated
	if count > 1 {
		status = http.StatusOK
	}
	h.writeSession(w, status)
}

func (h *dmsHarness) handleStatus(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, h.session.String(), r.Header.Get(providerSessionHeader))
	var raw map[string]json.RawMessage
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&raw))
	require.NotContains(h.t, raw, "shutdown_intent")
	var accepting, draining bool
	require.NoError(h.t, json.Unmarshal(raw["accepting_bookings"], &accepting))
	require.NoError(h.t, json.Unmarshal(raw["draining"], &draining))
	require.False(h.t, accepting)
	require.False(h.t, draining)
	var request statusRequest
	encoded, err := json.Marshal(raw)
	require.NoError(h.t, err)
	require.NoError(h.t, json.Unmarshal(encoded, &request))
	require.Equal(h.t, h.expected.Metadata.BaseAddresses, request.BaseAddresses)
	require.Equal(h.t, h.expected.Metadata.EndpointKeys, request.EndpointKeys)
	require.Equal(h.t, h.expected.Metadata.Limits, request.Limits)
	require.Equal(h.t, h.expected.Metadata.ConfigFingerprint, request.ConfigFingerprint)
	h.mu.Lock()
	h.statusCount++
	h.mu.Unlock()
	h.writeSession(w, http.StatusOK)
}

func (h *dmsHarness) handleDelete(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, h.session.String(), r.Header.Get(providerSessionHeader))
	h.mu.Lock()
	h.deleteCount++
	h.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *dmsHarness) writeSession(w http.ResponseWriter, status int) {
	response := validSessionResponse(h.now, h.session, h.expected)
	if h.mutate != nil {
		h.mutate(&response)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	require.NoError(h.t, json.NewEncoder(w).Encode(response))
}

func TestProviderSessionOpenReplayFalseStatusAndExactRelease(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	expected := validExpectations(now)
	harness := &dmsHarness{t: t, now: now, token: "peer-bound-node-token", expected: expected, session: uuid.New()}
	server := httptest.NewServer(http.HandlerFunc(harness.handler))
	defer server.Close()
	client := newDMSClient(t, server.URL+"/v1", now, 30*time.Second, 10*time.Second, 10*time.Second)
	bootNonce := uuid.New()

	created, err := client.OpenSession(context.Background(), OpenInput{
		AccessToken: harness.token, BootNonce: bootNonce, Expected: expected,
	})
	require.NoError(t, err)
	require.Equal(t, OpenCreated, created.Outcome)
	require.Equal(t, harness.session, created.Session.ProviderSessionID)
	require.Equal(t, uint32(32), created.Session.EffectiveCapacity)
	require.Equal(t, 180*time.Second, created.Session.StatusTTL)
	require.Equal(t, 180*time.Second, created.Session.ProviderLeaseTTL)
	require.Equal(t, 5*time.Minute, created.Session.RecoveryGrace)
	require.Equal(t, expected.Metadata, created.Session.Metadata)

	replayed, err := client.OpenSession(context.Background(), OpenInput{
		AccessToken: harness.token, BootNonce: bootNonce, Expected: expected,
	})
	require.NoError(t, err)
	require.Equal(t, OpenReplayed, replayed.Outcome)
	require.Equal(t, created.Session.ProviderSessionID, replayed.Session.ProviderSessionID)

	status, err := client.ReportNotAccepting(context.Background(), harness.token, harness.session, expected)
	require.NoError(t, err)
	require.Equal(t, harness.session, status.ProviderSessionID)

	require.NoError(t, client.ReleaseSession(context.Background(), harness.token, harness.session))
	require.Equal(t, 2, harness.openCount)
	require.Equal(t, 1, harness.statusCount)
	require.Equal(t, 1, harness.deleteCount)
}

func TestProviderSessionResponseValidationFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*sessionResponse)
	}{
		{name: "accepting before ACL", mutate: func(r *sessionResponse) { r.AcceptingBookings = true }},
		{name: "draining", mutate: func(r *sessionResponse) { r.Draining = true }},
		{name: "shutdown intent", mutate: func(r *sessionResponse) { value := "reassign"; r.ShutdownIntent = &value }},
		{name: "zero session ID", mutate: func(r *sessionResponse) { r.ProviderSessionID = uuid.Nil }},
		{name: "capacity over total", mutate: func(r *sessionResponse) { r.EffectiveCapacity = 33 }},
		{name: "wrong revision", mutate: func(r *sessionResponse) { r.SchedulingRevision++ }},
		{name: "wrong status TTL", mutate: func(r *sessionResponse) { r.StatusTTLSeconds = 179 }},
		{name: "short lease TTL", mutate: func(r *sessionResponse) { r.ProviderLeaseTTLSeconds = 59 }},
		{name: "long lease TTL", mutate: func(r *sessionResponse) { r.ProviderLeaseTTLSeconds = 301 }},
		{name: "short recovery grace", mutate: func(r *sessionResponse) { r.RecoveryGraceSeconds = 299 }},
		{name: "wrong JWT expiry", mutate: func(r *sessionResponse) { r.ProviderNodeJWTExpiresAt = r.ProviderNodeJWTExpiresAt.Add(time.Second) }},
		{name: "expired session", mutate: func(r *sessionResponse) { r.SessionExpiresAt = now }},
		{name: "session beyond rolling TTL", mutate: func(r *sessionResponse) { r.SessionExpiresAt = now.Add(181 * time.Second) }},
		{name: "session beyond JWT", mutate: func(r *sessionResponse) { r.SessionExpiresAt = r.ProviderNodeJWTExpiresAt.Add(time.Second) }},
		{name: "base mismatch", mutate: func(r *sessionResponse) { r.BaseAddresses[0] += "/p2p-circuit" }},
		{name: "endpoint mismatch", mutate: func(r *sessionResponse) { r.EndpointKeys[0] += "-other" }},
		{name: "limit mismatch", mutate: func(r *sessionResponse) { r.Limits.DurationSeconds++ }},
		{name: "fingerprint mismatch", mutate: func(r *sessionResponse) { r.ConfigFingerprint += "-other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected := validExpectations(now)
			harness := &dmsHarness{
				t: t, now: now, token: "peer-bound-node-token", expected: expected, session: uuid.New(), mutate: test.mutate,
			}
			server := httptest.NewServer(http.HandlerFunc(harness.handler))
			defer server.Close()
			client := newDMSClient(t, server.URL+"/v1", now, 30*time.Second, 10*time.Second, 10*time.Second)

			_, err := client.OpenSession(context.Background(), OpenInput{
				AccessToken: harness.token, BootNonce: uuid.New(), Expected: expected,
			})
			require.Error(t, err)
		})
	}
}

func TestStrictRelayResponsesRejectDuplicateAndNonCanonicalFields(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		message string
	}{
		{
			name: "exact duplicate fence field", payload: `{"reservation_epoch":"00000000-0000-0000-0000-000000000001","reservation_epoch":"00000000-0000-0000-0000-000000000002"}`,
			message: "duplicate field",
		},
		{
			name: "case variant cannot override revoke", payload: `{"revoke":true,"Revoke":false}`,
			message: "non-canonical field",
		},
		{
			name: "nested duplicate limit", payload: `{"limits":{"duration_seconds":900,"duration_seconds":901,"data_bytes_per_direction":1}}`,
			message: "duplicate field",
		},
		{
			name: "required field cannot be null", payload: `{"revoke":null}`,
			message: "null field",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response assignmentResponse
			err := decodeStrictJSON([]byte(test.payload), &response)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestStrictActiveResponseRequiresFieldsOnEveryNestedAssignment(t *testing.T) {
	now := time.Date(2026, 8, 20, 11, 30, 0, 0, time.UTC)
	response := validAssignmentResponse(now, uuid.New(), uuid.New(), uuid.New(), SlotStateReady, validExpectations(now).Metadata)
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(encoded, &raw))

	for _, missing := range []string{"revoke", "scheduling_revision"} {
		t.Run(missing, func(t *testing.T) {
			item := make(map[string]any, len(raw))
			for key, value := range raw {
				item[key] = value
			}
			delete(item, missing)
			payload, marshalErr := json.Marshal(map[string]any{"assignments": []any{item}})
			require.NoError(t, marshalErr)
			var active assignmentsResponse
			decodeErr := decodeStrictJSON(payload, &active)
			require.ErrorContains(t, decodeErr, `missing required field "`+missing+`"`)
		})
	}

	item := make(map[string]any, len(raw))
	for key, value := range raw {
		item[key] = value
	}
	item["revoke"] = nil
	payload, err := json.Marshal(map[string]any{"assignments": []any{item}})
	require.NoError(t, err)
	var active assignmentsResponse
	require.ErrorContains(t, decodeStrictJSON(payload, &active), "null field")
}

func TestProviderCapacityChecksEveryLocalReservationScope(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for _, field := range []string{"total", "per-IP", "per-ASN"} {
		t.Run(field, func(t *testing.T) {
			expected := validExpectations(now)
			switch field {
			case "total":
				expected.LocalCapacity.Total = 31
			case "per-IP":
				expected.LocalCapacity.PerIP = 31
			case "per-ASN":
				expected.LocalCapacity.PerASN = 31
			}
			harness := &dmsHarness{t: t, now: now, token: "token", expected: expected, session: uuid.New()}
			server := httptest.NewServer(http.HandlerFunc(harness.handler))
			defer server.Close()
			client := newDMSClient(t, server.URL+"/v1", now, 30*time.Second, 10*time.Second, 10*time.Second)
			_, err := client.OpenSession(context.Background(), OpenInput{AccessToken: "token", BootNonce: uuid.New(), Expected: expected})
			require.ErrorContains(t, err, "total/IP/ASN")
		})
	}
}

func TestProviderSessionRejectsTimingInequality(t *testing.T) {
	now := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	expected := validExpectations(now)
	harness := &dmsHarness{t: t, now: now, token: "token", expected: expected, session: uuid.New()}
	server := httptest.NewServer(http.HandlerFunc(harness.handler))
	defer server.Close()
	client := newDMSClient(t, server.URL+"/v1", now, 160*time.Second, 10*time.Second, 10*time.Second)
	_, err := client.OpenSession(context.Background(), OpenInput{AccessToken: "token", BootNonce: uuid.New(), Expected: expected})
	require.ErrorContains(t, err, "do not fit")
}

func TestProviderSessionReturnsTypedBoundedErrorsWithoutToken(t *testing.T) {
	secretToken := "secret-peer-bound-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+secretToken, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusConflict)
		require.NoError(t, json.NewEncoder(w).Encode(relayErrorResponse{
			Code: secretToken, Error: "reflected " + secretToken,
		}))
	}))
	defer server.Close()
	now := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
	_, err := client.OpenSession(context.Background(), OpenInput{
		AccessToken: secretToken, BootNonce: uuid.New(), Expected: validExpectations(now),
	})
	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, http.StatusConflict, apiErr.StatusCode)
	require.Equal(t, "[redacted]", apiErr.Code)
	require.NotContains(t, err.Error(), secretToken)

	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
	}))
	defer oversized.Close()
	client = newDMSClient(t, oversized.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
	_, err = client.OpenSession(context.Background(), OpenInput{
		AccessToken: secretToken, BootNonce: uuid.New(), Expected: validExpectations(now),
	})
	require.ErrorContains(t, err, "64 KiB")
	require.NotContains(t, err.Error(), secretToken)
}

func TestProviderSessionRequiresCanonicalImmutableInputAndFiniteHTTPTimeout(t *testing.T) {
	_, err := New(Options{
		Capacity:            32,
		BaseURL:             "http://dms.example.com",
		HTTPClient:          &http.Client{Timeout: 10 * time.Second},
		AllowHTTPForTesting: true,
		StatusInterval:      30 * time.Second,
		MaxStatusBackoff:    10 * time.Second,
	})
	require.ErrorContains(t, err, "HTTPS")

	_, err = New(Options{
		Capacity: 32,
		BaseURL:  "https://dms.example.com", HTTPClient: &http.Client{},
		StatusInterval: 30 * time.Second, MaxStatusBackoff: 10 * time.Second,
	})
	require.ErrorContains(t, err, "positive timeout")

	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	client, err := New(Options{
		Capacity: 32,
		BaseURL:  "https://dms.example.com", HTTPClient: &http.Client{Timeout: 10 * time.Second},
		StatusInterval: 30 * time.Second, MaxStatusBackoff: 10 * time.Second, Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	expected := validExpectations(now)
	expected.Metadata.BaseAddresses = append(expected.Metadata.BaseAddresses, expected.Metadata.BaseAddresses[0])
	_, err = client.OpenSession(context.Background(), OpenInput{AccessToken: "token", BootNonce: uuid.New(), Expected: expected})
	require.ErrorContains(t, err, "sorted and unique")
}

func TestOpenReplayPreservesAlreadyAcceptingStatus(t *testing.T) {
	now := time.Date(2026, 8, 20, 16, 0, 0, 0, time.UTC)
	expected := validExpectations(now)
	sessionID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		response := validSessionResponse(now, sessionID, expected)
		response.AcceptingBookings = true
		writeDMSJSON(t, w, http.StatusOK, response)
	}))
	defer server.Close()
	client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)

	result, err := client.OpenSession(context.Background(), OpenInput{
		AccessToken: "token", BootNonce: uuid.New(), Expected: expected,
	})
	require.NoError(t, err)
	require.Equal(t, OpenReplayed, result.Outcome)
	require.Equal(t, ProviderStatus{AcceptingBookings: true}, result.Session.Status)
}

func TestReportStatusSupportsAcceptingAndDraining(t *testing.T) {
	now := time.Date(2026, 8, 20, 16, 30, 0, 0, time.UTC)
	expected := validExpectations(now)
	for _, test := range []struct {
		name   string
		status ProviderStatus
	}{
		{name: "accepting", status: ProviderStatus{AcceptingBookings: true}},
		{name: "draining", status: ProviderStatus{Draining: true}},
		{name: "restart intent", status: ProviderStatus{Draining: true, ShutdownIntent: stringPointer(ShutdownIntentRestartSameIdentity)}},
		{name: "reassign intent", status: ProviderStatus{Draining: true, ShutdownIntent: stringPointer(ShutdownIntentReassign)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessionID := uuid.New()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPut, r.Method)
				require.Equal(t, "/relay-provider/status", r.URL.Path)
				require.Equal(t, sessionID.String(), r.Header.Get(providerSessionHeader))
				var request statusRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Equal(t, test.status.AcceptingBookings, request.AcceptingBookings)
				require.Equal(t, test.status.Draining, request.Draining)
				require.Equal(t, test.status.ShutdownIntent, request.ShutdownIntent)
				response := validSessionResponse(now, sessionID, expected)
				response.AcceptingBookings = test.status.AcceptingBookings
				response.Draining = test.status.Draining
				response.ShutdownIntent = cloneStringPointer(test.status.ShutdownIntent)
				writeDMSJSON(t, w, http.StatusOK, response)
			}))
			defer server.Close()
			client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)

			result, err := client.ReportStatus(context.Background(), "token", sessionID, expected, test.status)
			require.NoError(t, err)
			require.True(t, equalProviderStatus(test.status, result.Status))
		})
	}

	invalid := ProviderStatus{AcceptingBookings: true, Draining: true}
	client := newDMSClient(t, "https://dms.example.com", now, 30*time.Second, 10*time.Second, 10*time.Second)
	_, err := client.ReportStatus(context.Background(), "token", uuid.New(), expected, invalid)
	require.ErrorContains(t, err, "accepting and draining")
}

func TestClaimCreatedAndEmptyRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 20, 17, 0, 0, 0, time.UTC)
	sessionID := uuid.New()
	claim := validClaimResponse(now, sessionID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/relay-provider/bookings/claim", r.URL.Path)
		require.Equal(t, sessionID.String(), r.Header.Get(providerSessionHeader))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Empty(t, body)
		writeDMSJSON(t, w, http.StatusCreated, claim)
	}))
	defer server.Close()
	client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)

	result, err := client.Claim(context.Background(), "token", sessionID)
	require.NoError(t, err)
	require.NotNil(t, result.Assignment)
	require.Zero(t, result.RetryAfter)
	require.Equal(t, claim.AssignmentID, *result.Assignment.AssignmentID)
	require.Nil(t, result.Assignment.Metadata)

	emptyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer emptyServer.Close()
	client = newDMSClient(t, emptyServer.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
	result, err = client.Claim(context.Background(), "token", sessionID)
	require.NoError(t, err)
	require.Nil(t, result.Assignment)
	require.Equal(t, 2*time.Second, result.RetryAfter)
}

func TestClaimRejectsInvalidRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 20, 17, 30, 0, 0, time.UTC)
	for _, retryAfter := range []string{"", "0", "31", "+2", "Wed, 21 Oct 2015 07:28:00 GMT"} {
		t.Run(retryAfter, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				if retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
			_, err := client.Claim(context.Background(), "token", uuid.New())
			require.ErrorContains(t, err, "Retry-After")
		})
	}
}

func TestActiveAndFencedAssignmentMethodsUseExactWireContract(t *testing.T) {
	now := time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)
	sessionID := uuid.New()
	assignmentID := uuid.New()
	epoch := uuid.New()
	metadata := validExpectations(now).Metadata
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, sessionID.String(), r.Header.Get(providerSessionHeader))
		suffix := strings.TrimPrefix(r.URL.Path, "/relay-provider/assignments/"+assignmentID.String()+"/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/relay-provider/bookings/active":
			ready := validAssignmentResponse(now, sessionID, assignmentID, epoch, SlotStateReady, metadata)
			writeDMSJSON(t, w, http.StatusOK, assignmentsResponse{Assignments: []assignmentResponse{ready}})
		case r.Method == http.MethodPost && suffix == "recover":
			requireFenceRequest(t, r, epoch)
			writeDMSJSON(t, w, http.StatusOK, validAssignmentResponse(now, sessionID, assignmentID, epoch, SlotStateStarting, metadata))
		case r.Method == http.MethodPost && suffix == "heartbeat":
			requireFenceRequest(t, r, epoch)
			writeDMSJSON(t, w, http.StatusOK, validAssignmentResponse(now, sessionID, assignmentID, epoch, SlotStateReady, metadata))
		case r.Method == http.MethodPut && suffix == "ready":
			var request readyRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, epoch, request.ReservationEpoch)
			require.Equal(t, metadata, Metadata{BaseAddresses: request.BaseAddresses, EndpointKeys: request.EndpointKeys, Limits: request.Limits, ConfigFingerprint: request.ConfigFingerprint})
			writeDMSJSON(t, w, http.StatusOK, validAssignmentResponse(now, sessionID, assignmentID, epoch, SlotStateReady, metadata))
		case r.Method == http.MethodPost && suffix == "fail":
			var request failRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, epoch, request.ReservationEpoch)
			require.Equal(t, "retryable", request.Failure)
			writeDMSJSON(t, w, http.StatusOK, revokedAssignmentResponse(now))
		case r.Method == http.MethodPost && suffix == "relinquish":
			requireFenceRequest(t, r, epoch)
			writeDMSJSON(t, w, http.StatusOK, revokedAssignmentResponse(now))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)

	active, err := client.Active(context.Background(), "token", sessionID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, SlotStateReady, active[0].SlotState)
	require.Equal(t, metadata, *active[0].Metadata)

	recovered, err := client.Recover(context.Background(), "token", sessionID, assignmentID, epoch)
	require.NoError(t, err)
	require.Equal(t, SlotStateStarting, recovered.SlotState)
	heartbeat, err := client.Heartbeat(context.Background(), "token", sessionID, assignmentID, epoch)
	require.NoError(t, err)
	require.Equal(t, SlotStateReady, heartbeat.SlotState)
	ready, err := client.Ready(context.Background(), "token", sessionID, assignmentID, epoch, metadata)
	require.NoError(t, err)
	require.Equal(t, SlotStateReady, ready.SlotState)
	failed, err := client.Fail(context.Background(), "token", sessionID, assignmentID, epoch)
	require.NoError(t, err)
	require.True(t, failed.Revoke)
	relinquished, err := client.Relinquish(context.Background(), "token", sessionID, assignmentID, epoch)
	require.NoError(t, err)
	require.True(t, relinquished.Revoke)
}

func TestAssignmentGoneDecodesSnapshotOrStableError(t *testing.T) {
	now := time.Date(2026, 8, 20, 19, 0, 0, 0, time.UTC)
	sessionID := uuid.New()
	assignmentID := uuid.New()
	epoch := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/recover") {
			writeDMSJSON(t, w, http.StatusGone, revokedAssignmentResponse(now))
			return
		}
		if r.Header.Get("Authorization") == "Bearer secret-410-bearer" {
			writeDMSJSON(t, w, http.StatusGone, relayErrorResponse{
				Code: "secret-410-bearer", Error: "reflected secret-410-bearer",
			})
			return
		}
		writeDMSJSON(t, w, http.StatusGone, relayErrorResponse{Code: "session_ended", Error: "provider session ended"})
	}))
	defer server.Close()
	client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)

	revoked, err := client.Recover(context.Background(), "token", sessionID, assignmentID, epoch)
	require.NoError(t, err)
	require.True(t, revoked.Revoke)

	_, err = client.Heartbeat(context.Background(), "token", sessionID, assignmentID, epoch)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusGone, apiErr.StatusCode)
	require.Equal(t, "session_ended", apiErr.Code)

	_, err = client.Heartbeat(context.Background(), "secret-410-bearer", sessionID, assignmentID, epoch)
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "[redacted]", apiErr.Code)
	require.NotContains(t, err.Error(), "secret-410-bearer")
}

func TestAssignmentResponsesRejectMissingFieldsBadDeadlinesAndFences(t *testing.T) {
	now := time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC)
	sessionID := uuid.New()
	assignmentID := uuid.New()
	epoch := uuid.New()

	t.Run("claim missing required field", func(t *testing.T) {
		claim := validClaimResponse(now, sessionID)
		raw, err := json.Marshal(claim)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		delete(body, "scheduling_revision")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeDMSJSON(t, w, http.StatusCreated, body)
		}))
		defer server.Close()
		client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
		_, err = client.Claim(context.Background(), "token", sessionID)
		require.ErrorContains(t, err, "scheduling_revision")
	})

	t.Run("claim lease beyond authority", func(t *testing.T) {
		claim := validClaimResponse(now, sessionID)
		claim.ProviderLeaseExpiresAt = claim.AuthorityExpiresAt.Add(time.Second)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeDMSJSON(t, w, http.StatusCreated, claim)
		}))
		defer server.Close()
		client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
		_, err := client.Claim(context.Background(), "token", sessionID)
		require.ErrorContains(t, err, "parent authority")
	})

	t.Run("active partial metadata", func(t *testing.T) {
		response := validAssignmentResponse(now, sessionID, assignmentID, epoch, SlotStateReady, validExpectations(now).Metadata)
		response.EndpointKeys = nil
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeDMSJSON(t, w, http.StatusOK, assignmentsResponse{Assignments: []assignmentResponse{response}})
		}))
		defer server.Close()
		client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
		_, err := client.Active(context.Background(), "token", sessionID)
		require.ErrorContains(t, err, "partial provider metadata")
	})

	t.Run("mutation changed fence", func(t *testing.T) {
		response := validAssignmentResponse(now, sessionID, uuid.New(), epoch, SlotStateReady, validExpectations(now).Metadata)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeDMSJSON(t, w, http.StatusOK, response)
		}))
		defer server.Close()
		client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second)
		_, err := client.Heartbeat(context.Background(), "token", sessionID, assignmentID, epoch)
		require.ErrorContains(t, err, "changed the requested fence")
	})
}

func validExpectations(now time.Time) Expectations {
	return Expectations{
		Metadata: Metadata{
			BaseAddresses: []string{
				"/dns4/relay-a.dev.aukiverse.com/tcp/443/p2p/12D3KooWQ4fS8W4KJmGcgCjeH75qZKJ9xV6WGUzEJpZdC9bmbiD9",
			},
			EndpointKeys:      []string{"/dns4/relay-a.dev.aukiverse.com/tcp/443"},
			Limits:            Limits{DurationSeconds: 900, DataBytesPerDirection: 9_449_976_628},
			ConfigFingerprint: "relay-config-v1|base|endpoint|9449976628|900",
		},
		LocalCapacity:      LocalCapacity{Total: 32, PerIP: 32, PerASN: 32},
		SchedulingRevision: 7,
		NodeTokenExpiresAt: now.Add(5 * time.Minute),
	}
}

func validSessionResponse(now time.Time, sessionID uuid.UUID, expected Expectations) sessionResponse {
	return sessionResponse{
		ProviderSessionID:        sessionID,
		EffectiveCapacity:        32,
		SchedulingRevision:       expected.SchedulingRevision,
		SessionExpiresAt:         now.Add(180 * time.Second),
		ProviderNodeJWTExpiresAt: expected.NodeTokenExpiresAt,
		StatusTTLSeconds:         180,
		ProviderLeaseTTLSeconds:  180,
		RecoveryGraceSeconds:     300,
		BaseAddresses:            append([]string(nil), expected.Metadata.BaseAddresses...),
		EndpointKeys:             append([]string(nil), expected.Metadata.EndpointKeys...),
		Limits:                   expected.Metadata.Limits,
		ConfigFingerprint:        expected.Metadata.ConfigFingerprint,
	}
}

func validClaimResponse(now time.Time, sessionID uuid.UUID) claimResponse {
	return claimResponse{
		BookingID:              uuid.New(),
		SlotID:                 uuid.New(),
		SlotIndex:              0,
		RelayCount:             1,
		DomainID:               uuid.New(),
		TargetPeerID:           testPeerID,
		BookingState:           BookingStateActive,
		SlotState:              SlotStateStarting,
		ProviderSessionID:      sessionID,
		AssignmentID:           uuid.New(),
		ReservationEpoch:       uuid.New(),
		RequestedUntil:         now.Add(time.Hour),
		AuthorityExpiresAt:     now.Add(5 * time.Minute),
		ProviderLeaseExpiresAt: now.Add(2 * time.Minute),
		EffectiveCapacity:      32,
		SchedulingRevision:     7,
		Revoke:                 false,
	}
}

func validAssignmentResponse(now time.Time, sessionID, assignmentID, epoch uuid.UUID, state SlotState, metadata Metadata) assignmentResponse {
	lease := now.Add(2 * time.Minute)
	return assignmentResponse{
		BookingID:              uuid.New(),
		SlotID:                 uuid.New(),
		SlotIndex:              0,
		RelayCount:             1,
		DomainID:               uuid.New(),
		TargetPeerID:           testPeerID,
		BookingState:           BookingStateActive,
		SlotState:              state,
		ProviderSessionID:      &sessionID,
		AssignmentID:           &assignmentID,
		ReservationEpoch:       &epoch,
		RequestedUntil:         now.Add(time.Hour),
		AuthorityExpiresAt:     now.Add(5 * time.Minute),
		ProviderLeaseExpiresAt: &lease,
		EffectiveCapacity:      32,
		SchedulingRevision:     7,
		ProviderBaseAddresses:  append([]string(nil), metadata.BaseAddresses...),
		EndpointKeys:           append([]string(nil), metadata.EndpointKeys...),
		Limits:                 &metadata.Limits,
		ConfigFingerprint:      stringPointer(metadata.ConfigFingerprint),
		Revoke:                 false,
	}
}

func revokedAssignmentResponse(now time.Time) assignmentResponse {
	return assignmentResponse{
		BookingID:          uuid.New(),
		SlotID:             uuid.New(),
		SlotIndex:          0,
		RelayCount:         1,
		DomainID:           uuid.New(),
		TargetPeerID:       testPeerID,
		BookingState:       BookingStateActive,
		SlotState:          SlotStateQueued,
		RequestedUntil:     now.Add(time.Hour),
		AuthorityExpiresAt: now.Add(5 * time.Minute),
		EffectiveCapacity:  32,
		SchedulingRevision: 7,
		Revoke:             true,
	}
}

func requireFenceRequest(t *testing.T, request *http.Request, epoch uuid.UUID) {
	t.Helper()
	var body assignmentFenceRequest
	require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
	require.Equal(t, epoch, body.ReservationEpoch)
}

func writeDMSJSON(t *testing.T, writer http.ResponseWriter, status int, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	require.NoError(t, json.NewEncoder(writer).Encode(value))
}

func stringPointer(value string) *string { return &value }

const testPeerID = "12D3KooWQ4fS8W4KJmGcgCjeH75qZKJ9xV6WGUzEJpZdC9bmbiD9"

func newDMSClient(t *testing.T, baseURL string, now time.Time, statusInterval, requestTimeout, maxBackoff time.Duration, capacities ...int) *Client {
	t.Helper()
	capacity := 32
	if len(capacities) != 0 {
		capacity = capacities[0]
	}
	client, err := New(Options{
		Capacity:            capacity,
		BaseURL:             baseURL,
		HTTPClient:          &http.Client{Timeout: requestTimeout},
		AllowHTTPForTesting: strings.HasPrefix(baseURL, "http://"),
		StatusInterval:      statusInterval,
		MaxStatusBackoff:    maxBackoff,
		Now:                 func() time.Time { return now },
	})
	require.NoError(t, err)
	return client
}

func TestProviderSessionAndActiveRecoverySupport10000Slots(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	sessionID := uuid.New()
	expected := validExpectations(now)
	expected.LocalCapacity = LocalCapacity{Total: 10000, PerIP: 10000, PerASN: 10000}
	rows := make([]assignmentResponse, 10000)
	for i := range rows {
		_, public, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
		require.NoError(t, err)
		target, err := peer.IDFromPublicKey(public)
		require.NoError(t, err)
		rows[i] = validAssignmentResponse(now, sessionID, uuid.New(), uuid.New(), SlotStateReady, expected.Metadata)
		rows[i].TargetPeerID = target.String()
		rows[i].EffectiveCapacity = 10000
	}
	for _, overflow := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				response := validSessionResponse(now, sessionID, expected)
				response.EffectiveCapacity = 10000
				if overflow {
					response.EffectiveCapacity++
				}
				writeDMSJSON(t, w, http.StatusCreated, response)
				return
			}
			response := assignmentsResponse{Assignments: rows}
			if overflow {
				response.Assignments = append(response.Assignments, rows[0])
			}
			writeDMSJSON(t, w, http.StatusOK, response)
		}))
		client := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second, 10000)
		_, openErr := client.OpenSession(context.Background(), OpenInput{AccessToken: "token", BootNonce: uuid.New(), Expected: expected})
		active, activeErr := client.Active(context.Background(), "token", sessionID)
		if !overflow {
			smaller := newDMSClient(t, server.URL, now, 30*time.Second, 10*time.Second, 10*time.Second, 800)
			_, err := smaller.Active(context.Background(), "token", sessionID)
			require.ErrorContains(t, err, "more than 800")
		}
		server.Close()
		if overflow {
			require.ErrorContains(t, openErr, "effective capacity")
			require.ErrorContains(t, activeErr, "more than 10000")
		} else {
			require.NoError(t, openErr)
			require.NoError(t, activeErr)
			require.Len(t, active, 10000)
		}
	}
}
