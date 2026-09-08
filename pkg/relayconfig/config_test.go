package relayconfig

import (
	"crypto/rand"
	"math"
	"testing"
	"time"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

func TestCanonicalizeMatchesDMSContract(t *testing.T) {
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)

	limits := Limits{DataBytesPerDirection: 10_737_418_240, Duration: 15 * time.Minute}
	canonical, err := Canonicalize([]string{
		"/dns4/Relay-B.Dev.Aukiverse.com./tcp/0443/p2p/" + peerID.String(),
		"/dns4/relay-a.dev.aukiverse.com/tcp/443/wss/p2p/" + peerID.String(),
	}, peerID, limits)
	require.NoError(t, err)
	require.Equal(t, []string{
		"/dns4/relay-a.dev.aukiverse.com/tcp/443/wss/p2p/" + peerID.String(),
		"/dns4/relay-b.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
	}, canonical.Bases())
	require.Equal(t, []string{
		"/dns4/relay-a.dev.aukiverse.com/tcp/443/wss",
		"/dns4/relay-b.dev.aukiverse.com/tcp/443",
	}, canonical.EndpointKeys())
	require.Equal(t,
		"relay-config-v1|"+
			"/dns4/relay-a.dev.aukiverse.com/tcp/443/wss/p2p/"+peerID.String()+","+
			"/dns4/relay-b.dev.aukiverse.com/tcp/443/p2p/"+peerID.String()+"|"+
			"/dns4/relay-a.dev.aukiverse.com/tcp/443/wss,"+
			"/dns4/relay-b.dev.aukiverse.com/tcp/443|10737418240|900",
		canonical.Fingerprint(),
	)
	require.Equal(t, limits, canonical.Limits())
	addrs, err := canonical.Multiaddrs()
	require.NoError(t, err)
	require.Len(t, addrs, 2)

	bases := canonical.Bases()
	bases[0] = "mutated"
	require.NotEqual(t, "mutated", canonical.Bases()[0])
	endpoints := canonical.EndpointKeys()
	endpoints[0] = "mutated"
	require.NotEqual(t, "mutated", canonical.EndpointKeys()[0])
}

func TestCanonicalizeRejectsInvalidAndDuplicateBases(t *testing.T) {
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)
	otherPrivateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	otherPeerID, err := peer.IDFromPrivateKey(otherPrivateKey)
	require.NoError(t, err)
	limits := Limits{DataBytesPerDirection: 1, Duration: MinimumCircuitDuration}

	invalid := []string{
		"/ip4/203.0.113.1/tcp/443/p2p/" + peerID.String(),
		"/dns/relay.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns6/relay.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dnsaddr/relay.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com/udp/443/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com/tcp/0/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com/tcp/65536/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com/tcp/443",
		"/dns4/relay.dev.aukiverse.com/tcp/443/ws/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com/tcp/443/tls/ws/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com/tcp/443/p2p/" + peerID.String() + "/p2p-circuit",
		"/dns4/relay.dev.aukiverse.com/tcp/443/wss/p2p/" + peerID.String() + "/p2p-circuit",
		"/dns4/relay.dev.aukiverse.com/tcp/443/p2p/" + otherPeerID.String(),
		"/dns4/localhost/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.local/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.internal/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.invalid/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.test/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.example/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.home.arpa/tcp/443/p2p/" + peerID.String(),
		"/dns4/singlelabel/tcp/443/p2p/" + peerID.String(),
		"/dns4/-relay.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay_.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			_, err := Canonicalize([]string{raw}, peerID, limits)
			require.Error(t, err)
		})
	}

	_, err = Canonicalize([]string{
		"/dns4/RELAY.dev.aukiverse.com/tcp/0443/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com./tcp/443/p2p/" + peerID.String(),
	}, peerID, limits)
	require.ErrorContains(t, err, "duplicate")
}

func TestCanonicalizeKeepsTCPAndWSSOnOneHostDistinct(t *testing.T) {
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)

	canonical, err := Canonicalize([]string{
		"/dns4/relay.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns4/RELAY.dev.aukiverse.com/tcp/443/wss/p2p/" + peerID.String(),
	}, peerID, Limits{DataBytesPerDirection: 1, Duration: MinimumCircuitDuration})
	require.NoError(t, err)
	require.Equal(t, []string{
		"/dns4/relay.dev.aukiverse.com/tcp/443",
		"/dns4/relay.dev.aukiverse.com/tcp/443/wss",
	}, canonical.EndpointKeys())
}

func TestCanonicalizeRequiresBothStandardTransports(t *testing.T) {
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)
	limits := Limits{DataBytesPerDirection: 1, Duration: MinimumCircuitDuration}

	for _, base := range []string{
		"/dns4/relay.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay.dev.aukiverse.com/tcp/4443/wss/p2p/" + peerID.String(),
	} {
		_, err := Canonicalize([]string{base}, peerID, limits)
		require.ErrorContains(t, err, "at least one direct TCP endpoint and one WSS endpoint")
	}
}

func TestLimitsRejectWireTruncationAndShortAttempts(t *testing.T) {
	maxWireDuration := time.Duration(uint64(math.MaxUint32)) * time.Second
	for _, test := range []struct {
		name   string
		limits Limits
	}{
		{name: "zero data", limits: Limits{Duration: MinimumCircuitDuration}},
		{name: "negative data", limits: Limits{DataBytesPerDirection: -1, Duration: MinimumCircuitDuration}},
		{name: "short duration", limits: Limits{DataBytesPerDirection: 1, Duration: MinimumCircuitDuration - time.Second}},
		{name: "fractional duration", limits: Limits{DataBytesPerDirection: 1, Duration: MinimumCircuitDuration + time.Nanosecond}},
		{name: "duration overflow", limits: Limits{DataBytesPerDirection: 1, Duration: maxWireDuration + time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, test.limits.Validate())
		})
	}
	require.NoError(t, (Limits{DataBytesPerDirection: math.MaxInt64, Duration: maxWireDuration}).Validate())
}
