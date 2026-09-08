package authpkg

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestP2POpaqueUserSubjectsRoundTripAndRemainBounded(t *testing.T) {
	cfg := p2pTokenTestConfig(t)
	organizationID, domainID := uuid.NewString(), uuid.NewString()
	for _, subject := range []string{
		"2938475629384756", " User|Case-敏感 ",
		"550E8400-E29B-41D4-A716-446655440000", uuid.NewString(),
		strings.Repeat("x", 255), strings.Repeat("界", 85),
	} {
		token, _, err := GenerateP2PAccessToken(cfg, subject, organizationID, P2PPeerTypeUser, testP2PPeerID, []string{domainID})
		require.NoError(t, err)
		claims, err := VerifyP2PAccessToken(cfg, token)
		require.NoError(t, err)
		require.Equal(t, subject, claims.Subject)
	}
	for _, subject := range []string{"", strings.Repeat("x", 256), strings.Repeat("é", 128), string([]byte{0xff})} {
		_, _, err := GenerateP2PAccessToken(cfg, subject, organizationID, P2PPeerTypeUser, testP2PPeerID, []string{domainID})
		require.Error(t, err)
	}
	for _, peerType := range []string{P2PPeerTypeApp, P2PPeerTypeRobot, P2PPeerTypeCompute, P2PPeerTypeDomainServer} {
		_, _, err := GenerateP2PAccessTokenAt(cfg, "opaque-user", organizationID, peerType, testP2PPeerID, []string{domainID}, time.Now())
		require.Error(t, err, "app/machine subjects must still be canonical UUIDs")
	}
	valid, _, err := GenerateP2PAccessToken(cfg, "opaque-user", organizationID, P2PPeerTypeUser, testP2PPeerID, []string{domainID})
	require.NoError(t, err)
	claims, err := VerifyP2PAccessToken(cfg, valid)
	require.NoError(t, err)
	for _, subject := range []string{"", strings.Repeat("x", 256), strings.Repeat("é", 128)} {
		claims.Subject = subject
		_, err := VerifyP2PAccessToken(cfg, signP2PClaims(t, cfg, claims))
		require.Error(t, err, "a valid signature cannot bypass subject bounds")
	}
}
