package authpkg

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/service-lib/pkg/tokenclaims"
	"github.com/aukilabs/service-lib/pkg/tokens"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	P2PAccessTokenType     = "p2p-access"
	P2PAccessTokenIssuer   = "dds"
	P2PAccessTokenAudience = "auki-p2p"
	P2PAccessTokenScope    = "domain-data:r"

	P2PPeerTypeRobot        = "robot"
	P2PPeerTypeCompute      = "compute"
	P2PPeerTypeUser         = "user"
	P2PPeerTypeApp          = "app"
	P2PPeerTypeDomainServer = "domain_server"

	P2PAccessTokenTTL                 = 30 * time.Minute
	P2PDomainServerMaxDomains         = 25
	P2PAccessTokenMaximumCompactBytes = 64 * 1_024
	P2PPeerIDMaximumBytes             = 255
	P2PSubjectMaximumBytes            = 255
)

// P2PAccessTokenClaims is the common DDS-signed authorization profile used by
// authenticated peer sessions. It is intentionally independent of HTTP Domain
// access tokens and contains no task- or dataset-specific grant.
// OrganizationID identifies the requester organization when the issuer knows
// it. It remains optional while previously issued credentials drain.
type P2PAccessTokenClaims struct {
	PeerType       string   `json:"peer_type"`
	PeerID         string   `json:"peer_id"`
	OrganizationID string   `json:"organization_id,omitempty"`
	DomainIDs      []string `json:"domain_ids"`
	Scopes         []string `json:"scopes"`
	tokenclaims.TypedClaims
}

func (c P2PAccessTokenClaims) VerifyAudience(cmp string, req bool) bool {
	return c.RegisteredClaims.VerifyAudience(cmp, req)
}

func (c P2PAccessTokenClaims) VerifyIssuer(cmp string, req bool) bool {
	return c.RegisteredClaims.VerifyIssuer(cmp, req)
}

func (c P2PAccessTokenClaims) Valid() error {
	if c.Type != P2PAccessTokenType {
		return p2pClaimsError("invalid token type")
	}
	if c.Issuer != P2PAccessTokenIssuer {
		return p2pClaimsError("invalid issuer")
	}
	if len(c.Audience) != 1 || c.Audience[0] != P2PAccessTokenAudience {
		return p2pClaimsError("invalid audience profile")
	}
	if err := ValidateP2PSubject(c.Subject, c.PeerType); err != nil {
		return p2pClaimsError(err.Error())
	}
	if err := validateP2PPeerID(c.PeerID); err != nil {
		return p2pClaimsError(err.Error())
	}
	if err := validateP2POrganization(c.OrganizationID); err != nil {
		return p2pClaimsError(err.Error())
	}
	if err := validateP2PDomains(c.PeerType, c.DomainIDs); err != nil {
		return p2pClaimsError(err.Error())
	}
	if len(c.Scopes) != 1 || c.Scopes[0] != P2PAccessTokenScope {
		return p2pClaimsError("invalid scope profile")
	}
	if c.IssuedAt == nil {
		return p2pClaimsError("issued_at is required")
	}
	if c.ExpiresAt == nil {
		return p2pClaimsError("expires_at is required")
	}
	if !c.ExpiresAt.Time.Equal(c.IssuedAt.Time.Add(P2PAccessTokenTTL)) {
		return p2pClaimsError("expiration must be exactly 30 minutes after issued_at")
	}

	return c.RegisteredClaims.Valid()
}

// GenerateP2PAccessToken issues the fixed DDS P2P token profile. The token TTL,
// issuer, audience, and scope are constants rather than caller-controlled
// values.
func GenerateP2PAccessToken(
	cfg *tokens.Config,
	subject string,
	organizationID string,
	peerType string,
	peerID string,
	domainIDs []string,
) (string, time.Time, error) {
	issuedAt := jwt.NewNumericDate(time.Now().UTC()).Time
	return GenerateP2PAccessTokenAt(cfg, subject, organizationID, peerType, peerID, domainIDs, issuedAt)
}

// GenerateP2PAccessTokenAt is the deterministic form used by DDS issuance
// handlers and contract tests. Expiration is always derived from issuedAt.
func GenerateP2PAccessTokenAt(
	cfg *tokens.Config,
	subject string,
	organizationID string,
	peerType string,
	peerID string,
	domainIDs []string,
	issuedAt time.Time,
) (string, time.Time, error) {
	if cfg == nil {
		return "", time.Time{}, errors.New("token config is required for p2p access token")
	}
	if cfg.SigningMethod == nil {
		return "", time.Time{}, errors.New("signing method is required for p2p access token")
	}
	if cfg.PrivateKey == nil {
		return "", time.Time{}, errors.New("private key is required for p2p access token")
	}
	if issuedAt.IsZero() {
		return "", time.Time{}, errors.New("issued_at is required for p2p access token")
	}
	if err := ValidateP2PSubject(subject, peerType); err != nil {
		return "", time.Time{}, err
	}
	if err := validateP2PPeerID(peerID); err != nil {
		return "", time.Time{}, err
	}
	if err := validateP2POrganization(organizationID); err != nil {
		return "", time.Time{}, err
	}
	if err := validateP2PDomains(peerType, domainIDs); err != nil {
		return "", time.Time{}, err
	}

	issuedAtClaim := jwt.NewNumericDate(issuedAt.UTC())
	expiresAtClaim := jwt.NewNumericDate(issuedAtClaim.Time.Add(P2PAccessTokenTTL))
	claims := &P2PAccessTokenClaims{
		PeerType:       peerType,
		PeerID:         peerID,
		OrganizationID: organizationID,
		DomainIDs:      slices.Clone(domainIDs),
		Scopes:         []string{P2PAccessTokenScope},
		TypedClaims: tokenclaims.TypedClaims{
			Type: P2PAccessTokenType,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    P2PAccessTokenIssuer,
				Subject:   subject,
				Audience:  jwt.ClaimStrings{P2PAccessTokenAudience},
				IssuedAt:  issuedAtClaim,
				ExpiresAt: expiresAtClaim,
			},
		},
	}

	signed, err := jwt.NewWithClaims(cfg.SigningMethod, claims).SignedString(cfg.PrivateKey)
	if err != nil {
		return "", time.Time{}, errors.New("failed to sign p2p access token").Wrap(err)
	}
	return signed, expiresAtClaim.Time, nil
}

// VerifyP2PAccessToken verifies the signature and complete fixed P2P claim
// profile, including the role-specific Domain count and exact 30-minute TTL.
func VerifyP2PAccessToken(cfg *tokens.Config, tokenStr string) (*P2PAccessTokenClaims, error) {
	if cfg == nil || cfg.SigningMethod == nil || cfg.PublicKey == nil {
		return nil, errors.New("verification config is required for p2p access token")
	}
	if len(tokenStr) == 0 || len(tokenStr) > P2PAccessTokenMaximumCompactBytes ||
		strings.TrimSpace(tokenStr) != tokenStr {
		return nil, errors.New("P2P access token is invalid")
	}

	var claims P2PAccessTokenClaims
	_, err := parseTokenWithClaims(tokenStr, &claims, func(token *jwt.Token) (interface{}, error) {
		if token.Method.Alg() != cfg.SigningMethod.Alg() {
			return nil, errors.New("invalid signing method for p2p access token")
		}
		return cfg.PublicKey, nil
	})
	if err != nil {
		return nil, errors.New("failed to parse p2p access token").Wrap(err)
	}
	return &claims, nil
}

// P2PAccessTokenVerifier accepts the current DDS P2P signing key and, while it
// is configured, the previous signing key retained for the published rotation
// overlap. The verifier contains public material only.
type P2PAccessTokenVerifier struct {
	configs []*tokens.Config
}

// NewP2PAccessTokenVerifier freezes the bounded current/previous verification
// set used by DDS P2P bearer middleware.
func NewP2PAccessTokenVerifier(
	current *tokens.Config,
	previousPublicKeyPEM string,
) (*P2PAccessTokenVerifier, error) {
	if err := validateP2PVerificationConfig(current); err != nil {
		return nil, err
	}

	configs := []*tokens.Config{{
		SigningMethod: current.SigningMethod,
		PublicKey:     current.PublicKey,
	}}
	if strings.TrimSpace(previousPublicKeyPEM) != "" {
		previous, err := jwt.ParseECPublicKeyFromPEM([]byte(previousPublicKeyPEM))
		if err != nil {
			return nil, errors.New("previous P2P verification key must be a valid ES256 public key PEM").Wrap(err)
		}
		if previous.Curve != elliptic.P256() {
			return nil, errors.New("previous P2P verification key must use the P-256 curve")
		}
		if publicKeysEqual(current.PublicKey.(*ecdsa.PublicKey), previous) {
			return nil, errors.New("previous P2P verification key must differ from current")
		}
		configs = append(configs, &tokens.Config{
			SigningMethod: jwt.SigningMethodES256,
			PublicKey:     previous,
		})
	}

	return &P2PAccessTokenVerifier{configs: configs}, nil
}

// Verify validates the signature against the bounded rotation set and then
// validates the complete fixed P2P claim profile.
func (v *P2PAccessTokenVerifier) Verify(tokenStr string) (*P2PAccessTokenClaims, error) {
	if v == nil || len(v.configs) == 0 {
		return nil, errors.New("P2P access token verifier is required")
	}
	if len(tokenStr) == 0 || len(tokenStr) > P2PAccessTokenMaximumCompactBytes ||
		strings.TrimSpace(tokenStr) != tokenStr {
		return nil, errors.New("P2P access token is invalid")
	}

	for _, cfg := range v.configs {
		claims, err := VerifyP2PAccessToken(cfg, tokenStr)
		if err == nil {
			return claims, nil
		}
	}
	return nil, errors.New("P2P access token is invalid")
}

func validateP2PVerificationConfig(cfg *tokens.Config) error {
	if cfg == nil || cfg.SigningMethod == nil || cfg.PublicKey == nil {
		return errors.New("current P2P verification config is required")
	}
	if cfg.SigningMethod.Alg() != jwt.SigningMethodES256.Alg() {
		return errors.New("P2P token signing method must be ES256")
	}
	publicKey, ok := cfg.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return errors.New("current P2P verification key must be an ES256 public key")
	}
	return nil
}

func publicKeysEqual(left, right *ecdsa.PublicKey) bool {
	return left != nil && right != nil &&
		left.Curve == right.Curve && left.X.Cmp(right.X) == 0 && left.Y.Cmp(right.Y) == 0
}

func validateP2PDomains(peerType string, domainIDs []string) error {
	switch peerType {
	case P2PPeerTypeRobot, P2PPeerTypeCompute, P2PPeerTypeUser, P2PPeerTypeApp:
		if len(domainIDs) != 1 {
			return errors.New("robot, compute, user, and app p2p access tokens require exactly one domain_id")
		}
	case P2PPeerTypeDomainServer:
		if len(domainIDs) < 1 || len(domainIDs) > P2PDomainServerMaxDomains {
			return errors.New("domain_server p2p access tokens require between 1 and 25 domain_ids")
		}
	default:
		return errors.New("invalid peer_type for p2p access token")
	}

	seen := make(map[uuid.UUID]struct{}, len(domainIDs))
	for _, domainID := range domainIDs {
		parsed, err := uuid.Parse(domainID)
		if err != nil || parsed.String() != domainID {
			return errors.New("domain_ids must contain only canonical uuids for p2p access token")
		}
		if _, exists := seen[parsed]; exists {
			return errors.New("domain_ids must be unique for p2p access token")
		}
		seen[parsed] = struct{}{}
	}
	return nil
}

func validateP2POrganization(organizationID string) error {
	if organizationID == "" {
		return nil
	}
	return validateCanonicalP2PUUID(organizationID, "organization_id")
}

// ValidateP2PSubject preserves opaque human subjects without normalization.
// App/machine issuance and service boundaries retain canonical UUID subjects.
func ValidateP2PSubject(subject, peerType string) error {
	if len(subject) == 0 || len(subject) > P2PSubjectMaximumBytes || !utf8.ValidString(subject) {
		return errors.New("subject must contain 1..=255 UTF-8 bytes")
	}
	if peerType != P2PPeerTypeUser {
		return validateCanonicalP2PUUID(subject, "subject")
	}
	return nil
}

func validateCanonicalP2PUUID(value, claim string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value {
		return errors.New(claim + " must be a canonical uuid for p2p access token")
	}
	return nil
}

func validateP2PPeerID(peerID string) error {
	if peerID == "" || len(peerID) > P2PPeerIDMaximumBytes || strings.TrimSpace(peerID) != peerID {
		return errors.New("peer_id is required for p2p access token")
	}
	parsed, err := peer.Decode(peerID)
	if err != nil {
		return errors.New("peer_id must be a valid libp2p Peer ID for p2p access token").Wrap(err)
	}
	if parsed.String() != peerID {
		return errors.New("peer_id must be a canonical libp2p Peer ID for p2p access token")
	}
	return nil
}

func p2pClaimsError(message string) error {
	return jwt.NewValidationError(message, jwt.ValidationErrorClaimsInvalid)
}
