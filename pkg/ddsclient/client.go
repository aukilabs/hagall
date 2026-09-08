package ddsclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aukilabs/hagall/pkg/authpkg"
	"github.com/aukilabs/hagall/pkg/models"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	cryptopb "github.com/libp2p/go-libp2p/core/crypto/pb"
	"github.com/libp2p/go-libp2p/core/peer"
	"golang.org/x/mod/semver"
)

const (
	maxResponseBytes = 64 << 10
	siweClockSkew    = time.Minute
	relayCapability  = models.CircuitRelayCapabilityName
)

var ErrNodeNotFound = errors.New("DDS node not found")

// NodeTokenVerifier verifies a DDS Node token signature and profile before the
// client trusts any claims returned by DDS. The relay's verification-key ring
// can implement this interface without coupling this package to its lifecycle.
type NodeTokenVerifier interface {
	VerifyNodeAccessToken(context.Context, string) (*authpkg.NodeAccessClaims, error)
}

type NodeTokenVerifierFunc func(context.Context, string) (*authpkg.NodeAccessClaims, error)

func (f NodeTokenVerifierFunc) VerifyNodeAccessToken(ctx context.Context, token string) (*authpkg.NodeAccessClaims, error) {
	return f(ctx, token)
}

type Options struct {
	BaseURL             string
	HTTPClient          *http.Client
	AllowHTTPForTesting bool
	Verifier            NodeTokenVerifier
	Now                 func() time.Time
}

type Client struct {
	baseURL  *url.URL
	http     *http.Client
	verifier NodeTokenVerifier
	now      func() time.Time
}

type AuthenticateInput struct {
	RegistrationCredentials string
	Version                 string
	MaxConcurrency          *int
	WalletPrivateKey        *ecdsa.PrivateKey
	PeerPrivateKey          libp2pcrypto.PrivKey
}

type Access struct {
	Token              string
	NodeID             string
	OrganizationID     string
	PeerID             peer.ID
	IssuedAt           time.Time
	ExpiresAt          time.Time
	MaxConcurrency     *int
	SchedulingRevision uint64
}

type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("DDS request failed with HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("DDS request failed with HTTP %d: %s", e.StatusCode, e.Message)
}

func (e *HTTPError) Is(target error) bool {
	return target == ErrNodeNotFound && e.StatusCode == http.StatusForbidden && e.Message == "node not found"
}

func New(opts Options) (*Client, error) {
	baseURL, err := parseBaseURL(opts.BaseURL, opts.AllowHTTPForTesting)
	if err != nil {
		return nil, err
	}
	if opts.HTTPClient == nil {
		return nil, errors.New("DDS HTTP client is required")
	}
	if opts.HTTPClient.Timeout <= 0 {
		return nil, errors.New("DDS HTTP client must have a positive timeout")
	}
	if opts.Verifier == nil {
		return nil, errors.New("DDS Node-token verifier is required")
	}
	httpClient := *opts.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{baseURL: baseURL, http: &httpClient, verifier: opts.Verifier, now: now}, nil
}

// Authenticate authenticates an existing Node first. Registration is attempted
// for the DDS 403 response whose complete body is "node not found", or after a
// verified existing token reports a different max_concurrency than the
// configured registration override. Each SIWE proof obtains a new nonce,
// including registration and the post-register authentication retry.
func (c *Client) Authenticate(ctx context.Context, input AuthenticateInput) (*Access, error) {
	expectedNodeID, peerID, err := validateAuthenticateInput(input)
	if err != nil {
		return nil, err
	}

	registered := false
	unbound, err := c.authenticateSIWE(ctx, input.WalletPrivateKey)
	if err != nil {
		if err != ErrNodeNotFound {
			return nil, err
		}
		if err := c.register(ctx, input); err != nil {
			return nil, fmt.Errorf("register relay Node with DDS: %w", err)
		}
		registered = true
		unbound, err = c.authenticateSIWE(ctx, input.WalletPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("authenticate newly registered relay Node: %w", err)
		}
	}

	expectedWallet := strings.ToLower(ethcrypto.PubkeyToAddress(input.WalletPrivateKey.PublicKey).Hex())
	var validationMax *int
	if registered {
		validationMax = input.MaxConcurrency
	}
	unboundClaims, err := c.validateTokenResponse(ctx, unbound, expectedNodeID, expectedWallet, nil, validationMax)
	if err != nil {
		return nil, fmt.Errorf("validate unbound DDS Node token: %w", err)
	}
	if !registered && input.MaxConcurrency != nil && !equalInt(unboundClaims.MaxConcurrency, input.MaxConcurrency) {
		if err := c.register(ctx, input); err != nil {
			return nil, fmt.Errorf("reconcile relay Node registration with DDS: %w", err)
		}
		unbound, err = c.authenticateSIWE(ctx, input.WalletPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("authenticate updated relay Node: %w", err)
		}
		unboundClaims, err = c.validateTokenResponse(ctx, unbound, expectedNodeID, expectedWallet, nil, input.MaxConcurrency)
		if err != nil {
			return nil, fmt.Errorf("validate updated DDS Node token: %w", err)
		}
	}

	bound, err := c.bindPeer(ctx, unbound.AccessToken, input.PeerPrivateKey, peerID)
	if err != nil {
		return nil, err
	}
	if bound.AccessExpiresAt.IsZero() || !bound.AccessExpiresAt.Equal(unbound.AccessExpiresAt) {
		return nil, errors.New("peer binding changed the literal Node-token expiration")
	}
	boundClaims, err := c.validateBoundToken(ctx, bound, expectedNodeID, peerID, input.MaxConcurrency)
	if err != nil {
		return nil, fmt.Errorf("validate peer-bound DDS Node token: %w", err)
	}
	if err := sameMachineProfile(unboundClaims, boundClaims); err != nil {
		return nil, err
	}

	revision, err := schedulingRevision(boundClaims)
	if err != nil {
		return nil, err
	}
	return &Access{
		Token:              bound.AccessToken,
		NodeID:             boundClaims.NodeID,
		OrganizationID:     boundClaims.OrganizationID,
		PeerID:             peerID,
		IssuedAt:           boundClaims.IssuedAt.Time.UTC(),
		ExpiresAt:          boundClaims.ExpiresAt.Time.UTC(),
		MaxConcurrency:     cloneInt(boundClaims.MaxConcurrency),
		SchedulingRevision: revision,
	}, nil
}

// RefreshAt returns the exact 75%-of-literal-lifetime refresh instant.
func RefreshAt(issuedAt, expiresAt time.Time) (time.Time, error) {
	if issuedAt.IsZero() || expiresAt.IsZero() || !expiresAt.After(issuedAt) {
		return time.Time{}, errors.New("Node-token issued_at and expiration must define a positive lifetime")
	}
	lifetime := expiresAt.Sub(issuedAt)
	return issuedAt.Add(lifetime - lifetime/4), nil
}

// ValidateVersion enforces a DDS registration-compatible semantic version.
// The v prefix is optional because DDS accepts both forms, but development
// labels such as "dev" are rejected before any authentication network I/O.
func ValidateVersion(version string) error {
	if version == "" || strings.TrimSpace(version) != version {
		return errors.New("relay version is required without surrounding whitespace")
	}
	canonical := version
	if !strings.HasPrefix(canonical, "v") {
		canonical = "v" + canonical
	}
	if !semver.IsValid(canonical) {
		return errors.New("relay version must be a valid semantic version")
	}
	return nil
}

type siweRequestResponse struct {
	Nonce    string `json:"nonce"`
	Domain   string `json:"domain"`
	URI      string `json:"uri"`
	Version  string `json:"version"`
	ChainID  int64  `json:"chainId"`
	IssuedAt string `json:"issuedAt"`
}

type siweProof struct {
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

type tokenResponse struct {
	NodeID          string    `json:"node_id"`
	Wallet          string    `json:"wallet"`
	AccessToken     string    `json:"access_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
}

func (c *Client) authenticateSIWE(ctx context.Context, wallet *ecdsa.PrivateKey) (*tokenResponse, error) {
	proof, err := c.newSIWEProof(ctx, wallet)
	if err != nil {
		return nil, err
	}
	var response tokenResponse
	if _, err := c.doJSON(ctx, http.MethodPost, "/internal/v1/auth/siwe/verify", "", proof, &response, http.StatusOK); err != nil {
		if errors.Is(err, ErrNodeNotFound) {
			// This sentinel is created only at the authentication endpoint. An
			// identical response from nonce issuance must never authorize a
			// registration attempt.
			return nil, ErrNodeNotFound
		}
		return nil, err
	}
	return &response, nil
}

func (c *Client) register(ctx context.Context, input AuthenticateInput) error {
	proof, err := c.newSIWEProof(ctx, input.WalletPrivateKey)
	if err != nil {
		return err
	}
	request := struct {
		Message                 string   `json:"message"`
		Signature               string   `json:"signature"`
		RegistrationCredentials string   `json:"registration_credentials"`
		Capabilities            []string `json:"capabilities"`
		Version                 string   `json:"version"`
		MaxConcurrency          *int     `json:"max_concurrency,omitempty"`
	}{
		Message:                 proof.Message,
		Signature:               proof.Signature,
		RegistrationCredentials: input.RegistrationCredentials,
		Capabilities:            []string{relayCapability},
		Version:                 input.Version,
		MaxConcurrency:          cloneInt(input.MaxConcurrency),
	}
	_, err = c.doJSON(ctx, http.MethodPost, "/internal/v1/nodes/register-wallet", "", request, nil, http.StatusOK)
	return err
}

func (c *Client) newSIWEProof(ctx context.Context, wallet *ecdsa.PrivateKey) (*siweProof, error) {
	walletAddress := ethcrypto.PubkeyToAddress(wallet.PublicKey).Hex()
	var challenge siweRequestResponse
	request := struct {
		Wallet string `json:"wallet"`
	}{Wallet: walletAddress}
	if _, err := c.doJSON(ctx, http.MethodPost, "/internal/v1/auth/siwe/request", "", request, &challenge, http.StatusOK); err != nil {
		return nil, err
	}
	issuedAt, err := c.validateSIWEChallenge(challenge)
	if err != nil {
		return nil, err
	}
	message := buildSIWEMessage(challenge.Domain, walletAddress, challenge.URI, challenge.ChainID, challenge.Nonce, issuedAt)
	signature, err := ethcrypto.Sign(accounts.TextHash([]byte(message)), wallet)
	if err != nil {
		return nil, fmt.Errorf("sign SIWE message: %w", err)
	}
	return &siweProof{Message: message, Signature: hexutil.Encode(signature)}, nil
}

func (c *Client) validateSIWEChallenge(challenge siweRequestResponse) (time.Time, error) {
	if challenge.Nonce == "" || strings.TrimSpace(challenge.Nonce) != challenge.Nonce || len(challenge.Nonce) < 8 {
		return time.Time{}, errors.New("DDS returned an invalid SIWE nonce")
	}
	if challenge.Domain != c.baseURL.Host {
		return time.Time{}, errors.New("DDS SIWE domain does not match the configured DDS URL")
	}
	challengeURI, err := parseBaseURL(challenge.URI, c.baseURL.Scheme == "http")
	if err != nil || challengeURI.String() != c.baseURL.String() {
		return time.Time{}, errors.New("DDS SIWE URI does not match the configured DDS URL")
	}
	if challenge.Version != "1" || challenge.ChainID <= 0 {
		return time.Time{}, errors.New("DDS returned an unsupported SIWE profile")
	}
	issuedAt, err := time.Parse(time.RFC3339, challenge.IssuedAt)
	if err != nil {
		return time.Time{}, errors.New("DDS returned an invalid SIWE issuedAt")
	}
	now := c.now().UTC()
	if issuedAt.Before(now.Add(-siweClockSkew)) || issuedAt.After(now.Add(siweClockSkew)) {
		return time.Time{}, errors.New("DDS SIWE issuedAt is outside the permitted clock skew")
	}
	return issuedAt.UTC(), nil
}

type peerChallengeRequest struct {
	PeerID    string `json:"peer_id"`
	PublicKey string `json:"public_key"`
}

type peerChallengeResponse struct {
	ChallengeID string    `json:"challenge_id"`
	Challenge   string    `json:"challenge"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type peerVerifyRequest struct {
	ChallengeID string `json:"challenge_id"`
	Signature   string `json:"signature"`
}

type peerVerifyResponse struct {
	PeerID          string    `json:"peer_id"`
	AccessToken     string    `json:"access_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
}

func (c *Client) bindPeer(ctx context.Context, bearer string, privateKey libp2pcrypto.PrivKey, expectedPeerID peer.ID) (*peerVerifyResponse, error) {
	publicKey, err := libp2pcrypto.MarshalPublicKey(privateKey.GetPublic())
	if err != nil {
		return nil, fmt.Errorf("marshal relay public key: %w", err)
	}
	request := peerChallengeRequest{
		PeerID:    expectedPeerID.String(),
		PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
	}
	var challenge peerChallengeResponse
	if _, err := c.doJSON(ctx, http.MethodPost, "/internal/v1/auth/p2p/challenge", bearer, request, &challenge, http.StatusOK); err != nil {
		return nil, err
	}
	if challenge.ChallengeID == "" || strings.TrimSpace(challenge.ChallengeID) != challenge.ChallengeID {
		return nil, errors.New("DDS returned an invalid peer challenge ID")
	}
	challengeBytes, err := base64.RawURLEncoding.DecodeString(challenge.Challenge)
	if err != nil || len(challengeBytes) != 32 {
		return nil, errors.New("DDS returned an invalid peer challenge")
	}
	now := c.now().UTC()
	if !challenge.ExpiresAt.After(now) || challenge.ExpiresAt.After(now.Add(time.Minute)) {
		return nil, errors.New("DDS returned an invalid peer challenge expiration")
	}
	signature, err := privateKey.Sign(challengeBytes)
	if err != nil {
		return nil, fmt.Errorf("sign DDS peer challenge: %w", err)
	}
	verifyRequest := peerVerifyRequest{
		ChallengeID: challenge.ChallengeID,
		Signature:   base64.RawURLEncoding.EncodeToString(signature),
	}
	var response peerVerifyResponse
	if _, err := c.doJSON(ctx, http.MethodPost, "/internal/v1/auth/p2p/verify", bearer, verifyRequest, &response, http.StatusOK); err != nil {
		return nil, err
	}
	if response.PeerID != expectedPeerID.String() {
		return nil, errors.New("DDS peer verification returned another Peer ID")
	}
	return &response, nil
}

func (c *Client) validateTokenResponse(ctx context.Context, response *tokenResponse, expectedNodeID, expectedWallet string, expectedPeerID *peer.ID, configuredMax *int) (*authpkg.NodeAccessClaims, error) {
	if response == nil || response.AccessToken == "" || strings.TrimSpace(response.AccessToken) != response.AccessToken {
		return nil, errors.New("DDS returned an empty Node access token")
	}
	if response.NodeID != expectedNodeID || response.Wallet != expectedWallet {
		return nil, errors.New("DDS Node response does not match the configured identity")
	}
	claims, err := c.verifier.VerifyNodeAccessToken(ctx, response.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("verify DDS Node access token: %w", err)
	}
	if err := validateClaims(claims, expectedNodeID, expectedPeerID, configuredMax, response.AccessExpiresAt, c.now().UTC()); err != nil {
		return nil, err
	}
	return claims, nil
}

func (c *Client) validateBoundToken(ctx context.Context, response *peerVerifyResponse, expectedNodeID string, expectedPeerID peer.ID, configuredMax *int) (*authpkg.NodeAccessClaims, error) {
	if response == nil || response.AccessToken == "" || strings.TrimSpace(response.AccessToken) != response.AccessToken {
		return nil, errors.New("DDS returned an empty peer-bound Node access token")
	}
	claims, err := c.verifier.VerifyNodeAccessToken(ctx, response.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("verify peer-bound DDS Node access token: %w", err)
	}
	if err := validateClaims(claims, expectedNodeID, &expectedPeerID, configuredMax, response.AccessExpiresAt, c.now().UTC()); err != nil {
		return nil, err
	}
	return claims, nil
}

func validateClaims(claims *authpkg.NodeAccessClaims, expectedNodeID string, expectedPeerID *peer.ID, configuredMax *int, responseExpiry, now time.Time) error {
	if claims == nil || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return errors.New("DDS Node token is missing required timestamps")
	}
	if claims.NodeID != expectedNodeID || claims.Subject != expectedNodeID {
		return errors.New("DDS Node token contains another Node identity")
	}
	if len(claims.Capabilities) != 1 || claims.Capabilities[0] != relayCapability {
		return errors.New("DDS Node token is not relay-only")
	}
	if claims.SchedulingRevision == nil || *claims.SchedulingRevision < 0 {
		return errors.New("DDS relay Node token is missing a scheduling revision")
	}
	if claims.MaxConcurrency != nil && *claims.MaxConcurrency <= 0 {
		return errors.New("DDS relay Node token contains invalid max_concurrency")
	}
	if configuredMax != nil && (claims.MaxConcurrency == nil || *claims.MaxConcurrency != *configuredMax) {
		return errors.New("DDS relay Node token max_concurrency does not match configuration")
	}
	if expectedPeerID == nil {
		if claims.PeerID != nil {
			return errors.New("DDS returned an already peer-bound token before the challenge")
		}
	} else if claims.PeerID == nil || *claims.PeerID != expectedPeerID.String() {
		return errors.New("DDS Node token is not bound to the relay Peer ID")
	}
	claimExpiry := claims.ExpiresAt.Time.UTC()
	if responseExpiry.IsZero() || !claimExpiry.Equal(responseExpiry.UTC()) {
		return errors.New("DDS Node-token response expiration does not match its signed exp")
	}
	if !claimExpiry.After(now) || !claimExpiry.After(claims.IssuedAt.Time.UTC()) {
		return errors.New("DDS Node token is not literally current")
	}
	return nil
}

func sameMachineProfile(unbound, bound *authpkg.NodeAccessClaims) error {
	if unbound == nil || bound == nil ||
		unbound.NodeID != bound.NodeID ||
		unbound.OrganizationID != bound.OrganizationID ||
		unbound.NodeMode != bound.NodeMode ||
		!slices.Equal(unbound.Capabilities, bound.Capabilities) ||
		!equalInt(unbound.MaxConcurrency, bound.MaxConcurrency) ||
		!equalInt64(unbound.SchedulingRevision, bound.SchedulingRevision) ||
		unbound.Issuer != bound.Issuer ||
		unbound.Subject != bound.Subject ||
		!slices.Equal(unbound.Audience, bound.Audience) ||
		unbound.IssuedAt == nil || bound.IssuedAt == nil || !unbound.IssuedAt.Time.Equal(bound.IssuedAt.Time) ||
		unbound.ExpiresAt == nil || bound.ExpiresAt == nil || !unbound.ExpiresAt.Time.Equal(bound.ExpiresAt.Time) ||
		!equalNumericDate(unbound.NotBefore, bound.NotBefore) ||
		unbound.ID != bound.ID {
		return errors.New("DDS peer binding changed the Node-token machine profile")
	}
	return nil
}

func schedulingRevision(claims *authpkg.NodeAccessClaims) (uint64, error) {
	if claims == nil || claims.SchedulingRevision == nil || *claims.SchedulingRevision < 0 {
		return 0, errors.New("DDS relay Node token is missing a valid scheduling revision")
	}
	return uint64(*claims.SchedulingRevision), nil
}

func validateAuthenticateInput(input AuthenticateInput) (string, peer.ID, error) {
	credentials, err := models.ValidateRegistrationCredentials(input.RegistrationCredentials)
	if err != nil || credentials.Secret == "" {
		return "", "", errors.New("DDS registration credentials are invalid")
	}
	if _, err := uuid.Parse(credentials.ID); err != nil {
		return "", "", errors.New("DDS registration credentials contain an invalid Node ID")
	}
	if err := ValidateVersion(input.Version); err != nil {
		return "", "", err
	}
	if input.MaxConcurrency != nil && *input.MaxConcurrency <= 0 {
		return "", "", errors.New("max_concurrency must be positive when set")
	}
	if input.WalletPrivateKey == nil {
		return "", "", errors.New("relay wallet private key is required")
	}
	if input.PeerPrivateKey == nil || input.PeerPrivateKey.Type() != cryptopb.KeyType_Ed25519 {
		return "", "", errors.New("relay Ed25519 libp2p private key is required")
	}
	peerID, err := peer.IDFromPrivateKey(input.PeerPrivateKey)
	if err != nil {
		return "", "", fmt.Errorf("derive relay Peer ID: %w", err)
	}
	return credentials.ID, peerID, nil
}

func buildSIWEMessage(domain, address, uri string, chainID int64, nonce string, issuedAt time.Time) string {
	return fmt.Sprintf("%s wants you to sign in with your Ethereum account:\n%s\n\nURI: %s\nVersion: 1\nChain ID: %d\nNonce: %s\nIssued At: %s\n",
		domain, address, uri, chainID, nonce, issuedAt.UTC().Format(time.RFC3339))
}

func (c *Client) doJSON(ctx context.Context, method, endpoint, bearer string, input, output any, expectedStatus int) (int, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return 0, fmt.Errorf("encode DDS request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(endpoint), body)
	if err != nil {
		return 0, fmt.Errorf("create DDS request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("perform DDS request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := readBounded(response.Body)
	if err != nil {
		return response.StatusCode, err
	}
	if response.StatusCode != expectedStatus {
		message := strings.TrimSpace(string(responseBody))
		if bearer != "" {
			message = strings.ReplaceAll(message, bearer, "[redacted]")
		}
		return response.StatusCode, &HTTPError{StatusCode: response.StatusCode, Message: message}
	}
	if output == nil {
		if len(bytes.TrimSpace(responseBody)) != 0 {
			return response.StatusCode, errors.New("DDS returned an unexpected success response body")
		}
		return response.StatusCode, nil
	}
	if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
		return response.StatusCode, err
	}
	if err := decodeStrictJSON(responseBody, output); err != nil {
		return response.StatusCode, fmt.Errorf("decode DDS response: %w", err)
	}
	return response.StatusCode, nil
}

func (c *Client) endpoint(endpoint string) string {
	copyURL := *c.baseURL
	copyURL.Path = strings.TrimRight(copyURL.Path, "/") + endpoint
	return copyURL.String()
}

func parseBaseURL(raw string, allowHTTP bool) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return nil, errors.New("DDS base URL is required without surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("DDS base URL is invalid")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("DDS base URL must use HTTPS")
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
	limited := io.LimitReader(reader, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read DDS response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("DDS response exceeds the 64 KiB limit")
	}
	return data, nil
}

func requireJSON(contentType string) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return errors.New("DDS returned a non-JSON success response")
	}
	return nil
}

func decodeStrictJSON(data []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("DDS response contains trailing JSON")
	}
	return nil
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func equalInt(left, right *int) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalInt64(left, right *int64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalNumericDate(left, right *jwt.NumericDate) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && left.Time.Equal(right.Time))
}
