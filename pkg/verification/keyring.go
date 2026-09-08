package verification

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/aukilabs/hagall/pkg/authpkg"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
)

var (
	ErrNoTrustedKey         = errors.New("no trusted DDS verification key is available")
	ErrRotationTooFast      = errors.New("DDS published verification-key overlap is shorter than required")
	ErrUnknownSignature     = errors.New("P2P token was not signed by a retained DDS key")
	ErrStaleKeySet          = errors.New("DDS verification-key generation moved backwards")
	ErrConflictingKeySet    = errors.New("DDS verification keys changed without advancing their generation")
	ErrInvalidKeySetLineage = errors.New("DDS verification-key set does not continue the installed rotation lineage")
)

// Material is one DDS verification key returned by the trusted key endpoint.
type Material struct {
	PublicKey crypto.PublicKey
	Method    jwt.SigningMethod
}

// KeySet is the bounded current/previous DDS verification-key publication.
// Generation is monotonic and PreviousKeyOverlap is the minimum duration for
// which DDS promises to publish a rotated-out current key.
type KeySet struct {
	Generation         uint64
	PreviousKeyOverlap time.Duration
	Current            Material
	Previous           *Material
}

// Fetcher loads the bounded DDS verification-key set over an authenticated
// configuration path. Implementations must retain normal TLS verification.
type Fetcher interface {
	Fetch(context.Context) (KeySet, error)
}

type retainedKey struct {
	material    Material
	fingerprint string
	lastSeen    time.Time
}

// Ring follows the complete, monotonic current/previous key set published by
// DDS. Higher generations must continue the installed rotation lineage, and
// both published keys fail closed when the last successful fetch becomes stale.
type Ring struct {
	mu sync.RWMutex

	fetcher          Fetcher
	expectedMethod   string
	overlap          time.Duration
	maxStaleness     time.Duration
	unknownRefresh   time.Duration
	now              func() time.Time
	generation       uint64
	current          *retainedKey
	previous         *retainedKey
	lastUnknownFetch time.Time
}

type Options struct {
	ExpectedMethod       string
	PreviousKeyOverlap   time.Duration
	MaxKeyStaleness      time.Duration
	UnknownRefreshPeriod time.Duration
	Now                  func() time.Time
}

// NodeTokenVerifier adapts a Ring to the DDS client's Node-token verifier. It
// pins the expected DDS issuer and audience while preserving an injectable
// clock for deterministic refresh and expiry tests.
type NodeTokenVerifier struct {
	ring      *Ring
	issuer    string
	audiences []string
	clockSkew time.Duration
	now       func() time.Time
}

func NewNodeTokenVerifier(ring *Ring, issuer string, audiences []string, clockSkew time.Duration, now func() time.Time) (*NodeTokenVerifier, error) {
	if ring == nil {
		return nil, errors.New("DDS verification-key ring is required")
	}
	if issuer == "" {
		return nil, errors.New("DDS Node-token issuer is required")
	}
	if len(audiences) == 0 {
		return nil, errors.New("DDS Node-token audience is required")
	}
	for _, audience := range audiences {
		if audience == "" {
			return nil, errors.New("DDS Node-token audience must not contain empty values")
		}
	}
	if clockSkew < 0 {
		return nil, errors.New("DDS Node-token clock skew cannot be negative")
	}
	if now == nil {
		now = time.Now
	}
	return &NodeTokenVerifier{
		ring:      ring,
		issuer:    issuer,
		audiences: slices.Clone(audiences),
		clockSkew: clockSkew,
		now:       now,
	}, nil
}

func (v *NodeTokenVerifier) VerifyNodeAccessToken(ctx context.Context, token string) (*authpkg.NodeAccessClaims, error) {
	if v == nil || v.ring == nil {
		return nil, errors.New("DDS Node-token verifier is unavailable")
	}
	return v.ring.verifyNodeAccessToken(ctx, token, v.issuer, v.audiences, v.now().UTC(), v.clockSkew)
}

func NewRing(fetcher Fetcher, opts Options) (*Ring, error) {
	if fetcher == nil {
		return nil, errors.New("DDS verification-key fetcher is required")
	}
	if jwt.GetSigningMethod(opts.ExpectedMethod) == nil {
		return nil, errors.New("DDS verification signing method is unsupported")
	}
	if opts.PreviousKeyOverlap < authpkg.P2PAccessTokenTTL {
		return nil, fmt.Errorf("previous-key overlap must cover the P2P token lifetime: %w", ErrRotationTooFast)
	}
	if opts.MaxKeyStaleness < opts.PreviousKeyOverlap {
		return nil, errors.New("verification-key staleness must be at least the previous-key overlap")
	}
	if opts.UnknownRefreshPeriod <= 0 {
		return nil, errors.New("unknown-signature refresh period must be positive")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Ring{
		fetcher:        fetcher,
		expectedMethod: opts.ExpectedMethod,
		overlap:        opts.PreviousKeyOverlap,
		maxStaleness:   opts.MaxKeyStaleness,
		unknownRefresh: opts.UnknownRefreshPeriod,
		now:            now,
	}, nil
}

// Preload synchronously establishes the first trusted key. Callers must not
// report data-plane readiness until this succeeds.
func (r *Ring) Preload(ctx context.Context) error {
	return r.refresh(ctx, r.now().UTC())
}

// RunRefreshLoop periodically refreshes the current key until ctx is canceled.
// A failed refresh preserves the bounded retained ring; new verification fails
// closed once every retained key is stale.
func (r *Ring) RunRefreshLoop(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		if onError != nil {
			onError(errors.New("verification-key refresh interval must be positive"))
		}
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.refresh(ctx, r.now().UTC()); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// VerifyP2P verifies the fixed DDS P2P profile against the bounded key ring.
// Clock skew affects request acceptance only; LiteralExpiresAt is always the
// signed exp and must be used when deriving downstream authority.
func (r *Ring) VerifyP2P(ctx context.Context, token string, now time.Time, skew time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
	if token == "" {
		return nil, time.Time{}, errors.New("P2P access token is required")
	}
	if skew < 0 {
		return nil, time.Time{}, errors.New("verification skew cannot be negative")
	}
	keys := r.usableKeys(now.UTC())
	claims, exp, err := verifyWithKeys(token, keys, r.expectedMethod, now.UTC(), skew)
	if err == nil {
		return claims, exp, nil
	}

	if (errors.Is(err, ErrUnknownSignature) || errors.Is(err, ErrNoTrustedKey)) && r.reserveUnknownRefresh(now.UTC()) {
		if refreshErr := r.refresh(ctx, now.UTC()); refreshErr == nil {
			keys = r.usableKeys(now.UTC())
			return verifyWithKeys(token, keys, r.expectedMethod, now.UTC(), skew)
		}
	}
	return nil, time.Time{}, err
}

func (r *Ring) verifyNodeAccessToken(ctx context.Context, token, issuer string, audiences []string, now time.Time, skew time.Duration) (*authpkg.NodeAccessClaims, error) {
	if token == "" {
		return nil, errors.New("DDS Node access token is required")
	}
	keys := r.usableKeys(now)
	claims, err := verifyNodeWithKeys(token, keys, r.expectedMethod, issuer, audiences, now, skew)
	if err == nil {
		return claims, nil
	}
	if (errors.Is(err, ErrUnknownSignature) || errors.Is(err, ErrNoTrustedKey)) && r.reserveUnknownRefresh(now) {
		if refreshErr := r.refresh(ctx, now); refreshErr == nil {
			return verifyNodeWithKeys(token, r.usableKeys(now), r.expectedMethod, issuer, audiences, now, skew)
		}
	}
	return nil, err
}

func (r *Ring) Ready(now time.Time) bool {
	return len(r.usableKeys(now.UTC())) > 0
}

func (r *Ring) refresh(ctx context.Context, now time.Time) error {
	keySet, err := r.fetcher.Fetch(ctx)
	if err != nil {
		return fmt.Errorf("fetch DDS verification keys: %w", err)
	}
	if keySet.Generation == 0 {
		return errors.New("DDS verification-key generation must be positive")
	}
	if keySet.PreviousKeyOverlap < r.overlap {
		return fmt.Errorf("DDS published previous-key overlap %s, expected at least %s: %w", keySet.PreviousKeyOverlap, r.overlap, ErrRotationTooFast)
	}
	current, err := r.validateFetchedKey(keySet.Current, now)
	if err != nil {
		return err
	}
	var previous *retainedKey
	if keySet.Previous != nil {
		value, err := r.validateFetchedKey(*keySet.Previous, now)
		if err != nil {
			return err
		}
		if value.fingerprint == current.fingerprint {
			return errors.New("DDS current and previous verification keys must differ")
		}
		previous = value
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		r.generation = keySet.Generation
		r.current = current
		r.previous = previous
		return nil
	}
	if keySet.Generation < r.generation {
		return ErrStaleKeySet
	}
	if keySet.Generation == r.generation {
		if !samePublishedKeySet(r.current, r.previous, current, previous) {
			return ErrConflictingKeySet
		}
		r.current.material = current.material
		r.current.lastSeen = now
		if r.previous != nil {
			r.previous.material = previous.material
			r.previous.lastSeen = now
		}
		return nil
	}

	if !continuesPublishedKeyLineage(r.current, r.previous, current, previous) {
		return ErrInvalidKeySetLineage
	}
	r.generation = keySet.Generation
	r.current = current
	r.previous = previous
	return nil
}

func (r *Ring) validateFetchedKey(material Material, now time.Time) (*retainedKey, error) {
	if material.PublicKey == nil || material.Method == nil {
		return nil, errors.New("DDS verification endpoint returned an incomplete key")
	}
	if material.Method.Alg() != r.expectedMethod {
		return nil, fmt.Errorf("DDS verification endpoint returned signing method %q, expected %q", material.Method.Alg(), r.expectedMethod)
	}
	fingerprint, err := keyFingerprint(material.PublicKey, material.Method)
	if err != nil {
		return nil, err
	}
	return &retainedKey{material: material, fingerprint: fingerprint, lastSeen: now}, nil
}

func samePublishedKeySet(liveCurrent, livePrevious, nextCurrent, nextPrevious *retainedKey) bool {
	if liveCurrent.fingerprint != nextCurrent.fingerprint {
		return false
	}
	if livePrevious == nil || nextPrevious == nil {
		return livePrevious == nil && nextPrevious == nil
	}
	return livePrevious.fingerprint == nextPrevious.fingerprint
}

func continuesPublishedKeyLineage(liveCurrent, livePrevious, nextCurrent, nextPrevious *retainedKey) bool {
	if liveCurrent.fingerprint != nextCurrent.fingerprint {
		return nextPrevious != nil && nextPrevious.fingerprint == liveCurrent.fingerprint
	}
	if livePrevious == nil {
		return nextPrevious == nil
	}
	return nextPrevious == nil || nextPrevious.fingerprint == livePrevious.fingerprint
}

func (r *Ring) reserveUnknownRefresh(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lastUnknownFetch.IsZero() && now.Sub(r.lastUnknownFetch) < r.unknownRefresh {
		return false
	}
	r.lastUnknownFetch = now
	return true
}

func (r *Ring) usableKeys(now time.Time) []Material {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]Material, 0, 2)
	if r.current != nil && now.Sub(r.current.lastSeen) <= r.maxStaleness {
		keys = append(keys, r.current.material)
	}
	if r.previous != nil && now.Sub(r.previous.lastSeen) <= r.maxStaleness {
		keys = append(keys, r.previous.material)
	}
	return keys
}

func keyFingerprint(publicKey crypto.PublicKey, method jwt.SigningMethod) (string, error) {
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("encode DDS verification key: %w", err)
	}
	digest := sha256.Sum256(append([]byte(method.Alg()+"\x00"), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func verifyWithKeys(token string, keys []Material, expectedMethod string, now time.Time, skew time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error) {
	if len(keys) == 0 {
		return nil, time.Time{}, ErrNoTrustedKey
	}
	var lastErr error
	for _, key := range keys {
		claims := &authpkg.P2PAccessTokenClaims{}
		parser := &jwt.Parser{SkipClaimsValidation: true, ValidMethods: []string{expectedMethod}}
		parsed, err := parser.ParseWithClaims(token, claims, func(parsed *jwt.Token) (interface{}, error) {
			if parsed.Method.Alg() != key.Method.Alg() || parsed.Method.Alg() != expectedMethod {
				return nil, errors.New("unexpected P2P token signing method")
			}
			return key.PublicKey, nil
		})
		if err != nil || parsed == nil || !parsed.Valid {
			lastErr = err
			continue
		}
		if err := validateP2PClaims(claims, now, skew); err != nil {
			return nil, time.Time{}, err
		}
		return claims, claims.ExpiresAt.Time.UTC(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("P2P token signature is invalid")
	}
	return nil, time.Time{}, fmt.Errorf("%w: %v", ErrUnknownSignature, lastErr)
}

func verifyNodeWithKeys(token string, keys []Material, expectedMethod, issuer string, audiences []string, now time.Time, skew time.Duration) (*authpkg.NodeAccessClaims, error) {
	if len(keys) == 0 {
		return nil, ErrNoTrustedKey
	}
	var lastErr error
	for _, key := range keys {
		claims := &authpkg.NodeAccessClaims{}
		parser := &jwt.Parser{SkipClaimsValidation: true, ValidMethods: []string{expectedMethod}}
		parsed, err := parser.ParseWithClaims(token, claims, func(parsed *jwt.Token) (interface{}, error) {
			if parsed.Method.Alg() != key.Method.Alg() || parsed.Method.Alg() != expectedMethod {
				return nil, errors.New("unexpected DDS Node-token signing method")
			}
			return key.PublicKey, nil
		})
		if err != nil || parsed == nil || !parsed.Valid {
			lastErr = err
			continue
		}
		if err := validateNodeClaims(claims, issuer, audiences, now, skew); err != nil {
			return nil, err
		}
		return claims, nil
	}
	if lastErr == nil {
		lastErr = errors.New("DDS Node-token signature is invalid")
	}
	return nil, fmt.Errorf("%w: %v", ErrUnknownSignature, lastErr)
}

func validateNodeClaims(claims *authpkg.NodeAccessClaims, issuer string, audiences []string, now time.Time, skew time.Duration) error {
	if claims == nil || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return errors.New("DDS Node token requires iat and exp")
	}
	if claims.Issuer != issuer || claims.Subject == "" || claims.Subject != claims.NodeID {
		return errors.New("DDS Node token issuer or subject is invalid")
	}
	if !equalClaimStrings(claims.Audience, audiences) {
		return errors.New("DDS Node token audience is invalid")
	}
	if _, err := uuid.Parse(claims.OrganizationID); err != nil {
		return errors.New("DDS Node token organization is invalid")
	}
	if claims.NodeMode != authpkg.NodeModePublic && claims.NodeMode != authpkg.NodeModeDedicated {
		return errors.New("DDS Node token mode is invalid")
	}
	if len(claims.Capabilities) != 1 || claims.Capabilities[0] != "/p2p/circuit-relay/v1" {
		return errors.New("DDS Node token is not relay-only")
	}
	if claims.MaxConcurrency != nil && *claims.MaxConcurrency <= 0 {
		return errors.New("DDS Node token max_concurrency is invalid")
	}
	if claims.SchedulingRevision == nil || *claims.SchedulingRevision < 0 {
		return errors.New("DDS Node token scheduling revision is invalid")
	}
	if claims.PeerID != nil {
		if _, err := peer.Decode(*claims.PeerID); err != nil {
			return errors.New("DDS Node token Peer ID is invalid")
		}
	}
	if claims.IssuedAt.Time.After(now.Add(skew)) || (claims.NotBefore != nil && claims.NotBefore.Time.After(now.Add(skew))) {
		return errors.New("DDS Node token is not yet valid")
	}
	if !now.Before(claims.ExpiresAt.Time.Add(skew)) || !claims.ExpiresAt.Time.After(claims.IssuedAt.Time) {
		return errors.New("DDS Node token is expired or has an invalid lifetime")
	}
	return nil
}

func equalClaimStrings(actual jwt.ClaimStrings, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	counts := make(map[string]int, len(expected))
	for _, value := range expected {
		counts[value]++
	}
	for _, value := range actual {
		if counts[value] == 0 {
			return false
		}
		counts[value]--
	}
	return true
}

func validateP2PClaims(claims *authpkg.P2PAccessTokenClaims, now time.Time, skew time.Duration) error {
	if claims == nil {
		return errors.New("P2P token claims are required")
	}
	if claims.Type != authpkg.P2PAccessTokenType || claims.Issuer != authpkg.P2PAccessTokenIssuer {
		return errors.New("invalid P2P token profile")
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != authpkg.P2PAccessTokenAudience {
		return errors.New("invalid P2P token audience")
	}
	if err := authpkg.ValidateP2PSubject(claims.Subject, claims.PeerType); err != nil {
		return errors.New("invalid P2P token subject")
	}
	if _, err := peer.Decode(claims.PeerID); err != nil {
		return errors.New("invalid P2P token Peer ID")
	}
	if err := validateP2POrganization(claims.OrganizationID); err != nil {
		return err
	}
	if !slices.Equal(claims.Scopes, []string{authpkg.P2PAccessTokenScope}) {
		return errors.New("invalid P2P token scope")
	}
	if claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return errors.New("P2P token requires iat and exp")
	}
	if !claims.ExpiresAt.Time.Equal(claims.IssuedAt.Time.Add(authpkg.P2PAccessTokenTTL)) {
		return errors.New("P2P token lifetime must be exactly 30 minutes")
	}
	if claims.IssuedAt.Time.After(now.Add(skew)) {
		return errors.New("P2P token is not yet valid")
	}
	if claims.NotBefore != nil && claims.NotBefore.Time.After(now.Add(skew)) {
		return errors.New("P2P token is not yet valid")
	}
	if !now.Before(claims.ExpiresAt.Time.Add(skew)) {
		return errors.New("P2P token is expired")
	}
	if err := validateDomains(claims.PeerType, claims.DomainIDs); err != nil {
		return err
	}
	return nil
}

func validateDomains(peerType string, domainIDs []string) error {
	switch peerType {
	case authpkg.P2PPeerTypeRobot,
		authpkg.P2PPeerTypeCompute,
		authpkg.P2PPeerTypeUser,
		authpkg.P2PPeerTypeApp,
		authpkg.P2PPeerTypeDomainServer:
	default:
		return errors.New("P2P token peer_type is invalid")
	}
	// peer_type is signed application metadata, not relay admission policy.
	// Admission checks membership of the requested Domain independently and
	// therefore accepts the same bounded nonempty list for every peer type.
	if len(domainIDs) < 1 || len(domainIDs) > authpkg.P2PDomainServerMaxDomains {
		return errors.New("P2P token Domain count is invalid")
	}
	seen := make(map[uuid.UUID]struct{}, len(domainIDs))
	for _, value := range domainIDs {
		domainID, err := uuid.Parse(value)
		if err != nil || domainID.String() != value {
			return errors.New("P2P token contains an invalid Domain ID")
		}
		if _, exists := seen[domainID]; exists {
			return errors.New("P2P token Domain IDs must be unique")
		}
		seen[domainID] = struct{}{}
	}
	return nil
}

func validateP2POrganization(organizationID string) error {
	if organizationID == "" {
		return nil
	}
	if !isCanonicalUUID(organizationID) {
		return errors.New("P2P token requester organization ID must be canonical when present")
	}
	return nil
}

func isCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}
