package authpkg

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aukilabs/service-lib/pkg/tokenclaims"
	"github.com/aukilabs/service-lib/pkg/tokens"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

const testP2PPeerID = "12D3KooWJ5dZVvy5TE9GdMjj4X9f7P8S4gN1fqLwQhUNhB8Fr9uR"

func p2pTokenTestConfig(t *testing.T) *tokens.Config {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &tokens.Config{
		SigningMethod: jwt.SigningMethodES256,
		PrivateKey:    privateKey,
		PublicKey:     &privateKey.PublicKey,
	}
}

func TestP2PAccessTokenRoleContracts(t *testing.T) {
	cfg := p2pTokenTestConfig(t)
	subject := uuid.NewString()
	organizationID := uuid.NewString()
	issuedAt := jwt.NewNumericDate(time.Now().UTC()).Time

	tests := []struct {
		name           string
		peerType       string
		organizationID string
		domainIDs      []string
	}{
		{name: "robot", peerType: P2PPeerTypeRobot, organizationID: organizationID, domainIDs: p2pDomainIDs(1)},
		{name: "legacy robot without organization", peerType: P2PPeerTypeRobot, domainIDs: p2pDomainIDs(1)},
		{name: "compute", peerType: P2PPeerTypeCompute, organizationID: organizationID, domainIDs: p2pDomainIDs(1)},
		{name: "user", peerType: P2PPeerTypeUser, organizationID: organizationID, domainIDs: p2pDomainIDs(1)},
		{name: "app", peerType: P2PPeerTypeApp, organizationID: organizationID, domainIDs: p2pDomainIDs(1)},
		{name: "domain server one domain", peerType: P2PPeerTypeDomainServer, domainIDs: p2pDomainIDs(1)},
		{name: "domain server with organization metadata", peerType: P2PPeerTypeDomainServer, organizationID: organizationID, domainIDs: p2pDomainIDs(1)},
		{name: "domain server twenty-five domains", peerType: P2PPeerTypeDomainServer, domainIDs: p2pDomainIDs(P2PDomainServerMaxDomains)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokenStr, expiresAt, err := GenerateP2PAccessTokenAt(
				cfg,
				subject,
				tt.organizationID,
				tt.peerType,
				testP2PPeerID,
				tt.domainIDs,
				issuedAt,
			)
			require.NoError(t, err)
			require.Equal(t, issuedAt.Add(P2PAccessTokenTTL), expiresAt)

			claims, err := VerifyP2PAccessToken(cfg, tokenStr)
			require.NoError(t, err)
			require.Equal(t, P2PAccessTokenType, claims.Type)
			require.Equal(t, P2PAccessTokenIssuer, claims.Issuer)
			require.Equal(t, jwt.ClaimStrings{P2PAccessTokenAudience}, claims.Audience)
			require.Equal(t, subject, claims.Subject)
			require.Equal(t, tt.peerType, claims.PeerType)
			require.Equal(t, testP2PPeerID, claims.PeerID)
			require.Equal(t, tt.organizationID, claims.OrganizationID)
			require.Equal(t, tt.domainIDs, claims.DomainIDs)
			require.Equal(t, []string{P2PAccessTokenScope}, claims.Scopes)
			require.Equal(t, issuedAt.Unix(), claims.IssuedAt.Unix())
			require.Equal(t, expiresAt.Unix(), claims.ExpiresAt.Unix())
		})
	}
}

func TestP2PAccessTokenLanguageNeutralJSONProfile(t *testing.T) {
	cfg := p2pTokenTestConfig(t)
	subject := uuid.NewString()
	organizationID := uuid.NewString()
	domainID := uuid.NewString()
	tokenStr, _, err := GenerateP2PAccessTokenAt(
		cfg,
		subject,
		organizationID,
		P2PPeerTypeRobot,
		testP2PPeerID,
		[]string{domainID},
		time.Now().UTC(),
	)
	require.NoError(t, err)

	payload, err := jwt.DecodeSegment(strings.Split(tokenStr, ".")[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(payload, &claims))
	require.Equal(t, P2PAccessTokenType, claims["type"])
	require.Equal(t, P2PAccessTokenIssuer, claims["iss"])
	require.Equal(t, []any{P2PAccessTokenAudience}, claims["aud"])
	require.Equal(t, subject, claims["sub"])
	require.Equal(t, P2PPeerTypeRobot, claims["peer_type"])
	require.Equal(t, testP2PPeerID, claims["peer_id"])
	require.Equal(t, organizationID, claims["organization_id"])
	require.Equal(t, []any{domainID}, claims["domain_ids"])
	require.Equal(t, []any{P2PAccessTokenScope}, claims["scopes"])
	require.Contains(t, claims, "iat")
	require.Contains(t, claims, "exp")
	require.NotContains(t, claims, "task_id")
	require.NotContains(t, claims, "dataset_id")
}

func TestGenerateP2PAccessTokenRejectsInvalidInput(t *testing.T) {
	cfg := p2pTokenTestConfig(t)
	subject := uuid.NewString()
	organizationID := uuid.NewString()
	domainIDs := p2pDomainIDs(1)
	issuedAt := time.Now().UTC()

	tests := []struct {
		name           string
		cfg            *tokens.Config
		subject        string
		organizationID string
		peerType       string
		peerID         string
		domainIDs      []string
		issuedAt       time.Time
	}{
		{name: "missing config", cfg: nil, subject: subject, peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "missing signing method", cfg: &tokens.Config{PrivateKey: cfg.PrivateKey}, subject: subject, peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "missing private key", cfg: &tokens.Config{SigningMethod: cfg.SigningMethod}, subject: subject, peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "missing issued at", cfg: cfg, subject: subject, peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs},
		{name: "invalid subject", cfg: cfg, subject: "not-a-uuid", peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "noncanonical subject", cfg: cfg, subject: strings.ToUpper(subject), peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "invalid organization", cfg: cfg, subject: subject, organizationID: "not-a-uuid", peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "noncanonical organization", cfg: cfg, subject: subject, organizationID: strings.ToUpper(organizationID), peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "missing peer id", cfg: cfg, subject: subject, peerType: P2PPeerTypeRobot, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "whitespace peer id", cfg: cfg, subject: subject, peerType: P2PPeerTypeRobot, peerID: " " + testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "malformed peer id", cfg: cfg, subject: subject, peerType: P2PPeerTypeRobot, peerID: "not-a-peer-id", domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "unknown peer type", cfg: cfg, subject: subject, peerType: "unknown", peerID: testP2PPeerID, domainIDs: domainIDs, issuedAt: issuedAt},
		{name: "robot missing domain", cfg: cfg, subject: subject, peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, issuedAt: issuedAt},
		{name: "robot multiple domains", cfg: cfg, subject: subject, peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: p2pDomainIDs(2), issuedAt: issuedAt},
		{name: "compute multiple domains", cfg: cfg, subject: subject, peerType: P2PPeerTypeCompute, peerID: testP2PPeerID, domainIDs: p2pDomainIDs(2), issuedAt: issuedAt},
		{name: "user multiple domains", cfg: cfg, subject: subject, peerType: P2PPeerTypeUser, peerID: testP2PPeerID, domainIDs: p2pDomainIDs(2), issuedAt: issuedAt},
		{name: "app multiple domains", cfg: cfg, subject: subject, peerType: P2PPeerTypeApp, peerID: testP2PPeerID, domainIDs: p2pDomainIDs(2), issuedAt: issuedAt},
		{name: "domain server missing domain", cfg: cfg, subject: subject, peerType: P2PPeerTypeDomainServer, peerID: testP2PPeerID, issuedAt: issuedAt},
		{name: "domain server too many domains", cfg: cfg, subject: subject, peerType: P2PPeerTypeDomainServer, peerID: testP2PPeerID, domainIDs: p2pDomainIDs(P2PDomainServerMaxDomains + 1), issuedAt: issuedAt},
		{name: "invalid domain", cfg: cfg, subject: subject, peerType: P2PPeerTypeRobot, peerID: testP2PPeerID, domainIDs: []string{"not-a-uuid"}, issuedAt: issuedAt},
		{name: "duplicate domain", cfg: cfg, subject: subject, peerType: P2PPeerTypeDomainServer, peerID: testP2PPeerID, domainIDs: []string{domainIDs[0], domainIDs[0]}, issuedAt: issuedAt},
		{name: "equivalent duplicate domain", cfg: cfg, subject: subject, peerType: P2PPeerTypeDomainServer, peerID: testP2PPeerID, domainIDs: []string{domainIDs[0], strings.ToUpper(domainIDs[0])}, issuedAt: issuedAt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidateOrganizationID := tt.organizationID
			if candidateOrganizationID == "" && tt.peerType != P2PPeerTypeDomainServer {
				candidateOrganizationID = organizationID
			}
			_, _, err := GenerateP2PAccessTokenAt(
				tt.cfg,
				tt.subject,
				candidateOrganizationID,
				tt.peerType,
				tt.peerID,
				tt.domainIDs,
				tt.issuedAt,
			)
			require.Error(t, err)
		})
	}
}

func TestVerifyP2PAccessTokenRejectsInvalidClaims(t *testing.T) {
	cfg := p2pTokenTestConfig(t)
	now := jwt.NewNumericDate(time.Now().UTC()).Time

	tests := []struct {
		name   string
		mutate func(*P2PAccessTokenClaims)
	}{
		{name: "wrong type", mutate: func(c *P2PAccessTokenClaims) { c.Type = "domain-access" }},
		{name: "wrong issuer", mutate: func(c *P2PAccessTokenClaims) { c.Issuer = "api" }},
		{name: "missing audience", mutate: func(c *P2PAccessTokenClaims) { c.Audience = nil }},
		{name: "wrong audience", mutate: func(c *P2PAccessTokenClaims) { c.Audience = jwt.ClaimStrings{"other"} }},
		{name: "extra audience", mutate: func(c *P2PAccessTokenClaims) { c.Audience = jwt.ClaimStrings{P2PAccessTokenAudience, "other"} }},
		{name: "duplicate audience", mutate: func(c *P2PAccessTokenClaims) {
			c.Audience = jwt.ClaimStrings{P2PAccessTokenAudience, P2PAccessTokenAudience}
		}},
		{name: "invalid subject", mutate: func(c *P2PAccessTokenClaims) { c.Subject = "not-a-uuid" }},
		{name: "noncanonical subject", mutate: func(c *P2PAccessTokenClaims) { c.Subject = strings.ToUpper(c.Subject) }},
		{name: "invalid organization", mutate: func(c *P2PAccessTokenClaims) { c.OrganizationID = "not-a-uuid" }},
		{name: "noncanonical organization", mutate: func(c *P2PAccessTokenClaims) { c.OrganizationID = strings.ToUpper(c.OrganizationID) }},
		{name: "missing peer id", mutate: func(c *P2PAccessTokenClaims) { c.PeerID = "" }},
		{name: "malformed peer id", mutate: func(c *P2PAccessTokenClaims) { c.PeerID = "not-a-peer-id" }},
		{name: "unknown peer type", mutate: func(c *P2PAccessTokenClaims) { c.PeerType = "unknown" }},
		{name: "robot multiple domains", mutate: func(c *P2PAccessTokenClaims) { c.DomainIDs = p2pDomainIDs(2) }},
		{name: "domain server too many domains", mutate: func(c *P2PAccessTokenClaims) {
			c.PeerType = P2PPeerTypeDomainServer
			c.OrganizationID = ""
			c.DomainIDs = p2pDomainIDs(P2PDomainServerMaxDomains + 1)
		}},
		{name: "invalid domain", mutate: func(c *P2PAccessTokenClaims) { c.DomainIDs = []string{"invalid"} }},
		{name: "duplicate domain", mutate: func(c *P2PAccessTokenClaims) {
			domainID := c.DomainIDs[0]
			c.PeerType = P2PPeerTypeDomainServer
			c.OrganizationID = ""
			c.DomainIDs = []string{domainID, domainID}
		}},
		{name: "missing scopes", mutate: func(c *P2PAccessTokenClaims) { c.Scopes = nil }},
		{name: "unknown scope", mutate: func(c *P2PAccessTokenClaims) { c.Scopes = []string{"domain-data:w"} }},
		{name: "extra scope", mutate: func(c *P2PAccessTokenClaims) { c.Scopes = []string{P2PAccessTokenScope, "domain:r"} }},
		{name: "duplicate scope", mutate: func(c *P2PAccessTokenClaims) { c.Scopes = []string{P2PAccessTokenScope, P2PAccessTokenScope} }},
		{name: "missing issued at", mutate: func(c *P2PAccessTokenClaims) { c.IssuedAt = nil }},
		{name: "missing expiration", mutate: func(c *P2PAccessTokenClaims) { c.ExpiresAt = nil }},
		{name: "short ttl", mutate: func(c *P2PAccessTokenClaims) {
			c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(P2PAccessTokenTTL - time.Second))
		}},
		{name: "long ttl", mutate: func(c *P2PAccessTokenClaims) {
			c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(P2PAccessTokenTTL + time.Second))
		}},
		{name: "expired", mutate: func(c *P2PAccessTokenClaims) {
			c.IssuedAt = jwt.NewNumericDate(now.Add(-P2PAccessTokenTTL - 2*time.Minute))
			c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(P2PAccessTokenTTL))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := validP2PClaims(now)
			tt.mutate(claims)
			tokenStr := signP2PClaims(t, cfg, claims)

			_, err := VerifyP2PAccessToken(cfg, tokenStr)
			require.Error(t, err)
		})
	}
}

func TestVerifyP2PAccessTokenRejectsInvalidSignatureAndAlgorithm(t *testing.T) {
	cfg := p2pTokenTestConfig(t)
	otherCfg := p2pTokenTestConfig(t)
	claims := validP2PClaims(time.Now().UTC())
	tokenStr := signP2PClaims(t, otherCfg, claims)

	_, err := VerifyP2PAccessToken(cfg, tokenStr)
	require.Error(t, err)

	hmacCfg := &tokens.Config{
		SigningMethod: jwt.SigningMethodHS256,
		PrivateKey:    []byte("synthetic-p2p-secret"),
	}
	hmacToken := signP2PClaims(t, hmacCfg, claims)
	_, err = VerifyP2PAccessToken(cfg, hmacToken)
	require.ErrorContains(t, err, "invalid signing method")
}

func TestP2PAccessTokenVerifierAcceptsCurrentAndPreviousRotationKeys(t *testing.T) {
	current := p2pTokenTestConfig(t)
	previous := p2pTokenTestConfig(t)
	previousPEM, err := tokens.CreatePublicKeyPEM(previous.PublicKey)
	require.NoError(t, err)
	verifier, err := NewP2PAccessTokenVerifier(current, previousPEM)
	require.NoError(t, err)

	claims := validP2PClaims(time.Now().UTC())
	currentToken := signP2PClaims(t, current, claims)
	previousToken := signP2PClaims(t, previous, claims)

	verifiedCurrent, err := verifier.Verify(currentToken)
	require.NoError(t, err)
	require.Equal(t, claims.PeerID, verifiedCurrent.PeerID)
	verifiedPrevious, err := verifier.Verify(previousToken)
	require.NoError(t, err)
	require.Equal(t, claims.PeerID, verifiedPrevious.PeerID)

	unknown := p2pTokenTestConfig(t)
	_, err = verifier.Verify(signP2PClaims(t, unknown, claims))
	require.Error(t, err)
}

func TestP2PAccessTokenVerifierRejectsUnsafeKeySets(t *testing.T) {
	current := p2pTokenTestConfig(t)
	currentPEM, err := tokens.CreatePublicKeyPEM(current.PublicKey)
	require.NoError(t, err)

	_, err = NewP2PAccessTokenVerifier(nil, "")
	require.Error(t, err)
	_, err = NewP2PAccessTokenVerifier(
		&tokens.Config{SigningMethod: jwt.SigningMethodHS256, PublicKey: []byte("secret")},
		"",
	)
	require.Error(t, err)
	_, err = NewP2PAccessTokenVerifier(current, "not PEM")
	require.Error(t, err)
	_, err = NewP2PAccessTokenVerifier(current, currentPEM)
	require.Error(t, err)
}

func TestP2PAccessTokenVerifierRejectsOversizedBearerAndNonCanonicalPeerID(t *testing.T) {
	config := p2pTokenTestConfig(t)
	verifier, err := NewP2PAccessTokenVerifier(config, "")
	require.NoError(t, err)

	_, err = verifier.Verify(strings.Repeat("a", P2PAccessTokenMaximumCompactBytes+1))
	require.Error(t, err)

	canonicalPeerID, err := peer.Decode(testP2PPeerID)
	require.NoError(t, err)
	nonCanonicalPeerID := peer.ToCid(canonicalPeerID).String()
	require.NotEqual(t, testP2PPeerID, nonCanonicalPeerID)
	claims := validP2PClaims(time.Now().UTC())
	claims.PeerID = nonCanonicalPeerID
	_, err = verifier.Verify(signP2PClaims(t, config, claims))
	require.Error(t, err)

	_, _, err = GenerateP2PAccessToken(
		config,
		uuid.NewString(),
		uuid.NewString(),
		P2PPeerTypeUser,
		nonCanonicalPeerID,
		p2pDomainIDs(1),
	)
	require.Error(t, err)
}

func validP2PClaims(issuedAt time.Time) *P2PAccessTokenClaims {
	issuedAtClaim := jwt.NewNumericDate(issuedAt.UTC())
	return &P2PAccessTokenClaims{
		PeerType:       P2PPeerTypeRobot,
		PeerID:         testP2PPeerID,
		OrganizationID: uuid.NewString(),
		DomainIDs:      p2pDomainIDs(1),
		Scopes:         []string{P2PAccessTokenScope},
		TypedClaims: tokenclaims.TypedClaims{
			Type: P2PAccessTokenType,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    P2PAccessTokenIssuer,
				Subject:   uuid.NewString(),
				Audience:  jwt.ClaimStrings{P2PAccessTokenAudience},
				IssuedAt:  issuedAtClaim,
				ExpiresAt: jwt.NewNumericDate(issuedAtClaim.Add(P2PAccessTokenTTL)),
			},
		},
	}
}

func signP2PClaims(t *testing.T, cfg *tokens.Config, claims *P2PAccessTokenClaims) string {
	t.Helper()
	tokenStr, err := jwt.NewWithClaims(cfg.SigningMethod, claims).SignedString(cfg.PrivateKey)
	require.NoError(t, err)
	return tokenStr
}

func p2pDomainIDs(count int) []string {
	domainIDs := make([]string, count)
	for i := range count {
		domainIDs[i] = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("p2p-domain-%d", i))).String()
	}
	return domainIDs
}
