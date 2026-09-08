package ddsclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/authpkg"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

type mapVerifier struct {
	claims map[string]*authpkg.NodeAccessClaims
}

func (v mapVerifier) VerifyNodeAccessToken(_ context.Context, token string) (*authpkg.NodeAccessClaims, error) {
	claims, ok := v.claims[token]
	if !ok {
		return nil, fmt.Errorf("unknown test token")
	}
	copyClaims := *claims
	return &copyClaims, nil
}

type ddsHarness struct {
	t                 *testing.T
	now               time.Time
	nodeID            string
	organizationID    string
	walletAddress     string
	peerID            peer.ID
	expiry            time.Time
	unboundToken      string
	boundToken        string
	initialVerifyCode int
	initialVerifyBody string

	mu                  sync.Mutex
	serverURL           string
	nonceCount          int
	verifyCount         int
	registerCount       int
	peerChallengeCount  int
	peerVerifyCount     int
	issuedNonces        map[string]bool
	usedNonces          map[string]bool
	challengePublicKey  libp2pcrypto.PubKey
	challenge           []byte
	challengeBearer     string
	registrationMax     *int
	registrationVersion string
	registrationCaps    []string
	registrationCreds   string
}

func newDDSHarness(t *testing.T, now time.Time, nodeID, organizationID, walletAddress string, peerID peer.ID) *ddsHarness {
	return &ddsHarness{
		t:              t,
		now:            now,
		nodeID:         nodeID,
		organizationID: organizationID,
		walletAddress:  strings.ToLower(walletAddress),
		peerID:         peerID,
		expiry:         now.Add(5 * time.Minute),
		unboundToken:   "unbound-node-token",
		boundToken:     "peer-bound-node-token",
		issuedNonces:   make(map[string]bool),
		usedNonces:     make(map[string]bool),
		challenge:      []byte("0123456789abcdef0123456789abcdef"),
	}
}

func (h *ddsHarness) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/internal/v1/auth/siwe/request":
		h.handleSIWERequest(w, r)
	case "/internal/v1/auth/siwe/verify":
		h.handleSIWEVerify(w, r)
	case "/internal/v1/nodes/register-wallet":
		h.handleRegister(w, r)
	case "/internal/v1/auth/p2p/challenge":
		h.handlePeerChallenge(w, r)
	case "/internal/v1/auth/p2p/verify":
		h.handlePeerVerify(w, r)
	default:
		h.t.Errorf("unexpected DDS path %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func (h *ddsHarness) handleSIWERequest(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, http.MethodPost, r.Method)
	var request struct {
		Wallet string `json:"wallet"`
	}
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&request))
	require.Equal(h.t, h.walletAddress, strings.ToLower(request.Wallet))
	h.mu.Lock()
	h.nonceCount++
	nonce := fmt.Sprintf("nonce-%08d", h.nonceCount)
	h.issuedNonces[nonce] = true
	h.mu.Unlock()
	writeJSON(h.t, w, http.StatusOK, map[string]any{
		"nonce": nonce, "domain": r.Host, "uri": h.serverURL,
		"version": "1", "chainId": int64(8453), "issuedAt": h.now.Format(time.RFC3339),
	})
}

func (h *ddsHarness) handleSIWEVerify(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, http.MethodPost, r.Method)
	var proof siweProof
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&proof))
	h.verifyProof(proof)
	h.mu.Lock()
	h.verifyCount++
	verifyCount := h.verifyCount
	h.mu.Unlock()
	if verifyCount == 1 && h.initialVerifyCode != 0 {
		w.WriteHeader(h.initialVerifyCode)
		_, _ = w.Write([]byte(h.initialVerifyBody))
		return
	}
	writeJSON(h.t, w, http.StatusOK, tokenResponse{
		NodeID:          h.nodeID,
		Wallet:          h.walletAddress,
		AccessToken:     h.unboundToken,
		AccessExpiresAt: h.expiry,
	})
}

func (h *ddsHarness) handleRegister(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, http.MethodPost, r.Method)
	var request struct {
		Message                 string   `json:"message"`
		Signature               string   `json:"signature"`
		RegistrationCredentials string   `json:"registration_credentials"`
		Capabilities            []string `json:"capabilities"`
		Version                 string   `json:"version"`
		MaxConcurrency          *int     `json:"max_concurrency"`
	}
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&request))
	h.verifyProof(siweProof{Message: request.Message, Signature: request.Signature})
	h.mu.Lock()
	h.registerCount++
	h.registrationMax = cloneInt(request.MaxConcurrency)
	h.registrationVersion = request.Version
	h.registrationCaps = append([]string(nil), request.Capabilities...)
	h.registrationCreds = request.RegistrationCredentials
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (h *ddsHarness) handlePeerChallenge(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, http.MethodPost, r.Method)
	bearer := r.Header.Get("Authorization")
	require.Equal(h.t, "Bearer "+h.unboundToken, bearer)
	var request peerChallengeRequest
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&request))
	require.Equal(h.t, h.peerID.String(), request.PeerID)
	encodedPublicKey, err := base64.RawURLEncoding.DecodeString(request.PublicKey)
	require.NoError(h.t, err)
	publicKey, err := libp2pcrypto.UnmarshalPublicKey(encodedPublicKey)
	require.NoError(h.t, err)
	derived, err := peer.IDFromPublicKey(publicKey)
	require.NoError(h.t, err)
	require.Equal(h.t, h.peerID, derived)
	h.mu.Lock()
	h.peerChallengeCount++
	h.challengePublicKey = publicKey
	h.challengeBearer = bearer
	h.mu.Unlock()
	writeJSON(h.t, w, http.StatusOK, peerChallengeResponse{
		ChallengeID: "challenge-id",
		Challenge:   base64.RawURLEncoding.EncodeToString(h.challenge),
		ExpiresAt:   h.now.Add(30 * time.Second),
	})
}

func (h *ddsHarness) handlePeerVerify(w http.ResponseWriter, r *http.Request) {
	require.Equal(h.t, http.MethodPost, r.Method)
	require.Equal(h.t, h.challengeBearer, r.Header.Get("Authorization"), "challenge and verify must use the same unbound Bearer")
	var request peerVerifyRequest
	require.NoError(h.t, json.NewDecoder(r.Body).Decode(&request))
	require.Equal(h.t, "challenge-id", request.ChallengeID)
	signature, err := base64.RawURLEncoding.DecodeString(request.Signature)
	require.NoError(h.t, err)
	valid, err := h.challengePublicKey.Verify(h.challenge, signature)
	require.NoError(h.t, err)
	require.True(h.t, valid)
	h.mu.Lock()
	h.peerVerifyCount++
	h.mu.Unlock()
	writeJSON(h.t, w, http.StatusOK, peerVerifyResponse{
		PeerID:          h.peerID.String(),
		AccessToken:     h.boundToken,
		AccessExpiresAt: h.expiry,
	})
}

func (h *ddsHarness) verifyProof(proof siweProof) {
	parsed, err := authpkg.ParseSIWEMessage(proof.Message)
	require.NoError(h.t, err)
	require.Equal(h.t, h.serverURL, parsed.URI)
	require.Equal(h.t, int64(8453), parsed.ChainID)
	require.Equal(h.t, h.now, parsed.IssuedAt)
	h.mu.Lock()
	require.True(h.t, h.issuedNonces[parsed.Nonce], "proof must use a server-issued nonce")
	require.False(h.t, h.usedNonces[parsed.Nonce], "every SIWE flow must use a fresh nonce")
	h.usedNonces[parsed.Nonce] = true
	h.mu.Unlock()
	signature, err := hexutil.Decode(proof.Signature)
	require.NoError(h.t, err)
	publicKey, err := ethcrypto.SigToPub(accounts.TextHash([]byte(proof.Message)), signature)
	require.NoError(h.t, err)
	require.Equal(h.t, h.walletAddress, strings.ToLower(ethcrypto.PubkeyToAddress(*publicKey).Hex()))
}

func TestAuthenticateExistingNodeAndBindPeer(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	input, nodeID, organizationID, walletAddress, peerID := testAuthenticateInput(t)
	harness := newDDSHarness(t, now, nodeID, organizationID, walletAddress, peerID)
	server := httptest.NewServer(http.HandlerFunc(harness.handler))
	defer server.Close()
	harness.serverURL = server.URL

	client := newTestClient(t, server.URL, now, harness, input.MaxConcurrency)
	access, err := client.Authenticate(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, harness.boundToken, access.Token)
	require.Equal(t, nodeID, access.NodeID)
	require.Equal(t, organizationID, access.OrganizationID)
	require.Equal(t, peerID, access.PeerID)
	require.Equal(t, now, access.IssuedAt)
	require.Equal(t, harness.expiry, access.ExpiresAt)
	require.Equal(t, uint64(7), access.SchedulingRevision)
	require.Equal(t, 32, *access.MaxConcurrency)
	require.Equal(t, 1, harness.nonceCount)
	require.Equal(t, 1, harness.verifyCount)
	require.Zero(t, harness.registerCount)
	require.Equal(t, 1, harness.peerChallengeCount)
	require.Equal(t, 1, harness.peerVerifyCount)
}

func TestAuthenticateReconcilesExistingNodeMaxConcurrency(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 15, 0, 0, time.UTC)
	input, nodeID, organizationID, walletAddress, peerID := testAuthenticateInput(t)
	desiredMax := 128
	input.MaxConcurrency = &desiredMax
	currentMax := 32
	harness := newDDSHarness(t, now, nodeID, organizationID, walletAddress, peerID)
	server := httptest.NewServer(http.HandlerFunc(harness.handler))
	defer server.Close()
	harness.serverURL = server.URL

	verifier := NodeTokenVerifierFunc(func(_ context.Context, token string) (*authpkg.NodeAccessClaims, error) {
		harness.mu.Lock()
		registerCount := harness.registerCount
		registeredMax := cloneInt(harness.registrationMax)
		harness.mu.Unlock()

		maxConcurrency := &currentMax
		revision := int64(7)
		if registerCount > 0 {
			maxConcurrency = registeredMax
			revision++
		}
		var boundPeer *string
		switch token {
		case harness.unboundToken:
		case harness.boundToken:
			value := peerID.String()
			boundPeer = &value
		default:
			return nil, fmt.Errorf("unknown test token")
		}
		claims := nodeClaims(now, harness.expiry, nodeID, organizationID, maxConcurrency, boundPeer)
		claims.SchedulingRevision = &revision
		return claims, nil
	})
	client, err := New(Options{
		BaseURL:             server.URL,
		HTTPClient:          &http.Client{Timeout: time.Second},
		AllowHTTPForTesting: true,
		Verifier:            verifier,
		Now:                 func() time.Time { return now },
	})
	require.NoError(t, err)

	access, err := client.Authenticate(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, desiredMax, *access.MaxConcurrency)
	require.Equal(t, uint64(8), access.SchedulingRevision)
	require.Equal(t, 3, harness.nonceCount, "initial auth, registration, and updated auth each need a nonce")
	require.Equal(t, 2, harness.verifyCount)
	require.Equal(t, 1, harness.registerCount)
	require.Equal(t, desiredMax, *harness.registrationMax)
	require.Equal(t, 1, harness.peerChallengeCount)
	require.Equal(t, 1, harness.peerVerifyCount)
}

func TestAuthenticateRejectsStaleClaimAfterMaxConcurrencyReconciliation(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 20, 0, 0, time.UTC)
	input, nodeID, organizationID, walletAddress, peerID := testAuthenticateInput(t)
	desiredMax := 128
	input.MaxConcurrency = &desiredMax
	currentMax := 32
	harness := newDDSHarness(t, now, nodeID, organizationID, walletAddress, peerID)
	server := httptest.NewServer(http.HandlerFunc(harness.handler))
	defer server.Close()
	harness.serverURL = server.URL
	client := newTestClient(t, server.URL, now, harness, &currentMax)

	_, err := client.Authenticate(context.Background(), input)
	require.ErrorContains(t, err, "validate updated DDS Node token")
	require.ErrorContains(t, err, "max_concurrency does not match configuration")
	require.Equal(t, 1, harness.registerCount)
	require.Zero(t, harness.peerChallengeCount)
}

func TestAuthenticateSupportsAbsentDDSMaxConcurrency(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 30, 0, 0, time.UTC)
	input, nodeID, organizationID, walletAddress, peerID := testAuthenticateInput(t)
	input.MaxConcurrency = nil
	harness := newDDSHarness(t, now, nodeID, organizationID, walletAddress, peerID)
	server := httptest.NewServer(http.HandlerFunc(harness.handler))
	defer server.Close()
	harness.serverURL = server.URL

	signedMax := 32
	client := newTestClient(t, server.URL, now, harness, &signedMax)
	access, err := client.Authenticate(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, signedMax, *access.MaxConcurrency)
	require.Zero(t, harness.registerCount)
}

func TestAuthenticateRejectsInvalidVersionBeforeNetworkIO(t *testing.T) {
	input, _, _, _, _ := testAuthenticateInput(t)
	input.Version = "dev"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	client, err := New(Options{
		BaseURL:             server.URL,
		HTTPClient:          &http.Client{Timeout: time.Second},
		AllowHTTPForTesting: true,
		Verifier:            mapVerifier{claims: map[string]*authpkg.NodeAccessClaims{}},
	})
	require.NoError(t, err)

	_, err = client.Authenticate(context.Background(), input)
	require.ErrorContains(t, err, "semantic version")
	require.Zero(t, requests)
}

func TestValidateVersionAcceptsDDSRegistrationVersions(t *testing.T) {
	for _, version := range []string{"v0.0.0", "1.2.3", "v1.2.3-rc.1+sha"} {
		require.NoError(t, ValidateVersion(version), version)
	}
	for _, version := range []string{"", "dev", "v1.2.3.4", " v1.2.3"} {
		require.Error(t, ValidateVersion(version), version)
	}
}

func TestAuthenticateRegistersOnlyAfterExactNodeNotFoundAndUsesFreshNonces(t *testing.T) {
	now := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
	input, nodeID, organizationID, walletAddress, peerID := testAuthenticateInput(t)
	harness := newDDSHarness(t, now, nodeID, organizationID, walletAddress, peerID)
	harness.initialVerifyCode = http.StatusForbidden
	harness.initialVerifyBody = "node not found"
	server := httptest.NewServer(http.HandlerFunc(harness.handler))
	defer server.Close()
	harness.serverURL = server.URL

	client := newTestClient(t, server.URL, now, harness, input.MaxConcurrency)
	_, err := client.Authenticate(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, 3, harness.nonceCount, "failed auth, registration, and post-registration auth each need a nonce")
	require.Equal(t, 3, len(harness.usedNonces))
	require.Equal(t, 2, harness.verifyCount)
	require.Equal(t, 1, harness.registerCount)
	require.Equal(t, []string{relayCapability}, harness.registrationCaps)
	require.Equal(t, "v1.2.3", harness.registrationVersion)
	require.Equal(t, input.RegistrationCredentials, harness.registrationCreds)
	require.Equal(t, 32, *harness.registrationMax)
}

func TestAuthenticateDoesNotRegisterForOtherFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "other forbidden", status: http.StatusForbidden, body: "wallet does not match node"},
		{name: "unauthorized", status: http.StatusUnauthorized, body: "invalid nonce"},
		{name: "server failure", status: http.StatusInternalServerError, body: "internal server error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
			input, nodeID, organizationID, walletAddress, peerID := testAuthenticateInput(t)
			harness := newDDSHarness(t, now, nodeID, organizationID, walletAddress, peerID)
			harness.initialVerifyCode = test.status
			harness.initialVerifyBody = test.body
			server := httptest.NewServer(http.HandlerFunc(harness.handler))
			defer server.Close()
			harness.serverURL = server.URL
			client := newTestClient(t, server.URL, now, harness, input.MaxConcurrency)

			_, err := client.Authenticate(context.Background(), input)
			require.Error(t, err)
			require.Zero(t, harness.registerCount)
			require.Equal(t, 1, harness.nonceCount)
			require.Zero(t, harness.peerChallengeCount)
		})
	}
}

func TestAuthenticateDoesNotTreatNonceEndpointAsNodeNotFound(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	input, _, _, _, _ := testAuthenticateInput(t)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("node not found"))
	}))
	defer server.Close()
	client, err := New(Options{
		BaseURL:             server.URL,
		HTTPClient:          &http.Client{Timeout: time.Second},
		AllowHTTPForTesting: true,
		Verifier:            mapVerifier{claims: map[string]*authpkg.NodeAccessClaims{}},
		Now:                 func() time.Time { return now },
	})
	require.NoError(t, err)

	_, err = client.Authenticate(context.Background(), input)
	require.Error(t, err)
	require.Equal(t, []string{"/internal/v1/auth/siwe/request"}, paths)
}

func TestValidateClaimsFailsClosedOnRelayProfileChanges(t *testing.T) {
	now := time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	nodeID := uuid.NewString()
	organizationID := uuid.NewString()
	_, publicKey, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPublicKey(publicKey)
	require.NoError(t, err)
	maxConcurrency := 32
	base := nodeClaims(now, now.Add(5*time.Minute), nodeID, organizationID, &maxConcurrency, nil)
	tests := []struct {
		name   string
		mutate func(*authpkg.NodeAccessClaims)
		peer   *peer.ID
	}{
		{name: "extra capability", mutate: func(c *authpkg.NodeAccessClaims) { c.Capabilities = append(c.Capabilities, "/other") }},
		{name: "missing revision", mutate: func(c *authpkg.NodeAccessClaims) { c.SchedulingRevision = nil }},
		{name: "wrong node", mutate: func(c *authpkg.NodeAccessClaims) { c.NodeID = uuid.NewString() }},
		{name: "unexpected unbound peer", mutate: func(c *authpkg.NodeAccessClaims) { value := peerID.String(); c.PeerID = &value }},
		{name: "wrong bound peer", peer: &peerID, mutate: func(c *authpkg.NodeAccessClaims) {
			value := "12D3KooWQ4fS8W4KJmGcgCjeH75qZKJ9xV6WGUzEJpZdC9bmbiD9"
			c.PeerID = &value
		}},
		{name: "wrong capacity", mutate: func(c *authpkg.NodeAccessClaims) { value := 31; c.MaxConcurrency = &value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := *base
			test.mutate(&claims)
			err := validateClaims(&claims, nodeID, test.peer, &maxConcurrency, base.ExpiresAt.Time, now)
			require.Error(t, err)
		})
	}
}

func TestRefreshAtSupportsDDSFiveMinuteAndOneHourProfiles(t *testing.T) {
	issuedAt := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		lifetime time.Duration
		expected time.Duration
	}{
		{lifetime: 5 * time.Minute, expected: 3*time.Minute + 45*time.Second},
		{lifetime: time.Hour, expected: 45 * time.Minute},
	}
	for _, test := range tests {
		actual, err := RefreshAt(issuedAt, issuedAt.Add(test.lifetime))
		require.NoError(t, err)
		require.Equal(t, issuedAt.Add(test.expected), actual)
	}
	_, err := RefreshAt(issuedAt, issuedAt)
	require.Error(t, err)
}

func TestClientRejectsUnboundedHTTPAndOversizedResponse(t *testing.T) {
	_, err := New(Options{
		BaseURL:             "http://dds.example.com",
		HTTPClient:          &http.Client{Timeout: time.Second},
		AllowHTTPForTesting: true,
		Verifier: NodeTokenVerifierFunc(func(context.Context, string) (*authpkg.NodeAccessClaims, error) {
			return nil, errors.New("unused")
		}),
	})
	require.ErrorContains(t, err, "HTTPS")

	_, err = New(Options{
		BaseURL:    "https://dds.example.com",
		HTTPClient: &http.Client{},
		Verifier:   mapVerifier{claims: map[string]*authpkg.NodeAccessClaims{}},
	})
	require.ErrorContains(t, err, "positive timeout")

	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	input, nodeID, organizationID, walletAddress, peerID := testAuthenticateInput(t)
	_ = nodeID
	_ = organizationID
	_ = walletAddress
	_ = peerID
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
	}))
	defer server.Close()
	client, err := New(Options{
		BaseURL: server.URL, HTTPClient: &http.Client{Timeout: time.Second}, AllowHTTPForTesting: true,
		Verifier: mapVerifier{claims: map[string]*authpkg.NodeAccessClaims{}}, Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	_, err = client.Authenticate(context.Background(), input)
	require.ErrorContains(t, err, "64 KiB")
}

func testAuthenticateInput(t *testing.T) (AuthenticateInput, string, string, string, peer.ID) {
	t.Helper()
	wallet, err := ethcrypto.GenerateKey()
	require.NoError(t, err)
	peerPrivate, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(peerPrivate)
	require.NoError(t, err)
	nodeID := uuid.NewString()
	organizationID := uuid.NewString()
	maxConcurrency := 32
	credentials := base64.StdEncoding.EncodeToString([]byte(nodeID + ":registration-secret"))
	return AuthenticateInput{
		RegistrationCredentials: credentials,
		Version:                 "v1.2.3",
		MaxConcurrency:          &maxConcurrency,
		WalletPrivateKey:        wallet,
		PeerPrivateKey:          peerPrivate,
	}, nodeID, organizationID, ethcrypto.PubkeyToAddress(wallet.PublicKey).Hex(), peerID
}

func newTestClient(t *testing.T, baseURL string, now time.Time, harness *ddsHarness, configuredMax *int) *Client {
	t.Helper()
	unbound := nodeClaims(now, harness.expiry, harness.nodeID, harness.organizationID, configuredMax, nil)
	boundPeer := harness.peerID.String()
	bound := nodeClaims(now, harness.expiry, harness.nodeID, harness.organizationID, configuredMax, &boundPeer)
	client, err := New(Options{
		BaseURL:             baseURL,
		HTTPClient:          &http.Client{Timeout: time.Second},
		AllowHTTPForTesting: true,
		Verifier: mapVerifier{claims: map[string]*authpkg.NodeAccessClaims{
			harness.unboundToken: unbound,
			harness.boundToken:   bound,
		}},
		Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	return client
}

func nodeClaims(issuedAt, expiresAt time.Time, nodeID, organizationID string, maxConcurrency *int, peerID *string) *authpkg.NodeAccessClaims {
	revision := int64(7)
	return &authpkg.NodeAccessClaims{
		NodeID:             nodeID,
		OrganizationID:     organizationID,
		NodeMode:           authpkg.NodeModePublic,
		Capabilities:       []string{relayCapability},
		MaxConcurrency:     cloneInt(maxConcurrency),
		SchedulingRevision: &revision,
		PeerID:             peerID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "dds", Subject: nodeID, Audience: jwt.ClaimStrings{"dms"},
			IssuedAt: jwt.NewNumericDate(issuedAt), ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(value))
}
