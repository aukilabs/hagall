package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

func testPeerID(t *testing.T) peer.ID {
	t.Helper()
	privateKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	peerID, err := peer.IDFromPrivateKey(privateKey)
	require.NoError(t, err)
	return peerID
}

func requiredEnvironment(t *testing.T) map[string]string {
	t.Helper()
	peerID := testPeerID(t)
	return map[string]string{
		"RELAY_DDS_URL":                       "https://dds.dev.aukiverse.com",
		"RELAY_DMS_URL":                       "https://dms.dev.aukiverse.com/api",
		"RELAY_DDS_PUBLIC_KEY_URL":            "https://dds.dev.aukiverse.com/internal/v1/auth/public-key",
		"RELAY_REGISTRATION_CREDENTIALS_FILE": "/run/secrets/registration-credentials",
		"RELAY_WALLET_PRIVATE_KEY_FILE":       "/run/secrets/wallet-private-key",
		"RELAY_LIBP2P_PRIVATE_KEY_FILE":       "/run/secrets/libp2p-private-key",
		"RELAY_PUBLIC_BASE_MULTIADDRS": "/dns4/relay.dev.aukiverse.com/tcp/4001/p2p/" + peerID.String() +
			",/dns4/relay.dev.aukiverse.com/tcp/4002/wss/p2p/" + peerID.String(),
	}
}

func lookupMap(values map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		value, present := values[name]
		return value, present
	}
}

func validConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadFrom(lookupMap(requiredEnvironment(t)))
	require.NoError(t, err)
	return cfg
}

func TestLoadUsesExactImplementationDefaults(t *testing.T) {
	values := requiredEnvironment(t)
	cfg, err := LoadFrom(lookupMap(values))
	require.NoError(t, err)

	require.Equal(t, "https://dds.dev.aukiverse.com", cfg.DDSURL)
	require.Equal(t, "https://dms.dev.aukiverse.com/api", cfg.DMSURL)
	require.False(t, cfg.AllowLocalTestHTTP)
	require.Equal(t, "/ip4/0.0.0.0/tcp/4001", cfg.TCPListenMultiaddr)
	require.Equal(t, "/ip4/0.0.0.0/tcp/4002/ws", cfg.WebSocketListenMultiaddr)
	require.Equal(t, 10*time.Second, cfg.HTTP.RequestTimeout)
	require.Equal(t, RetryConfig{
		InitialBackoff:              time.Second,
		MaximumBackoff:              30 * time.Second,
		EmptyClaimMaxJitterFraction: 0.20,
	}, cfg.Retry)
	require.Equal(t, RequiredProviderStatusPeriod, cfg.Timing.StatusInterval)
	require.Equal(t, 10*time.Second, cfg.Timing.StatusMaxBackoff)
	require.Equal(t, RequiredProviderSessionTTL, cfg.Timing.ProviderSessionTTL)
	require.Equal(t, 0.75, cfg.Timing.NodeTokenRefreshFraction)
	require.Equal(t, 0.25, cfg.Timing.LeaseHeartbeatMinFraction)
	require.Equal(t, 0.35, cfg.Timing.LeaseHeartbeatMaxFraction)
	require.Equal(t, "ES256", cfg.VerificationKeys.SigningMethod)
	require.Equal(t, 2*time.Minute, cfg.VerificationKeys.RefreshInterval)
	require.Equal(t, 31*time.Minute, cfg.VerificationKeys.PreviousKeyOverlap)
	require.Equal(t, time.Hour, cfg.VerificationKeys.MaxStaleness)
	require.Equal(t, time.Minute, cfg.VerificationKeys.ClockSkew)
	require.Equal(t, 30*time.Second, cfg.VerificationKeys.UnknownRefreshInterval)
	require.Equal(t, AdmissionConfig{
		TTL:             30 * time.Second,
		MaximumEntries:  4096,
		AuthConcurrency: 64,
		AttemptsPerPeer: 8,
		AttemptsPerIP:   32,
		AttemptWindow:   10 * time.Second,
	}, cfg.Admission)
	require.Equal(t, DefaultCircuitDataBytes, cfg.Relay.Limits.DataBytesPerDirection)
	require.Equal(t, DefaultCircuitDuration, cfg.Relay.Limits.Duration)
	require.Nil(t, cfg.Relay.DDSMaxConcurrency)
	require.Equal(t, DefaultLocalCapacity, cfg.Relay.LocalCapacity)
	require.Equal(t, DefaultLocalCapacity, cfg.Relay.MaxReservations)
	require.Equal(t, DefaultLocalCapacity, cfg.Relay.MaxReservationsPerIP)
	require.Equal(t, DefaultLocalCapacity, cfg.Relay.MaxReservationsPerASN)
	require.Equal(t, 16, cfg.Relay.MaxCircuitsPerPeer)
	require.Equal(t, DefaultReservationTTL, cfg.Relay.ReservationTTL)
	require.Equal(t, 2048, cfg.Relay.BufferSize)
	require.Equal(t, ResourceManagerConfig{
		MemoryBytes:     1 << 30,
		FileDescriptors: 4096,
		Connections:     2048,
		Streams:         8192,
	}, cfg.ResourceManager)
	require.Equal(t, ConnectionManagerConfig{LowWater: 768, HighWater: 1024}, cfg.ConnectionManager)
	require.Equal(t, "127.0.0.1:9090", cfg.AdminAddress)
	require.Equal(t, "127.0.0.1:9091", cfg.MetricsAddress)
	require.False(t, cfg.AcceptBookings)
	require.Equal(t, 15*time.Minute, cfg.ShutdownDrain)
}

func TestLoadParsesEveryEnvironmentMatrixEntry(t *testing.T) {
	peerID := testPeerID(t)
	values := map[string]string{
		"RELAY_ADMIN_ADDR":                            "127.0.0.1:9190",
		"RELAY_ADMISSION_MAX_ENTRIES":                 "5000",
		"RELAY_ADMISSION_TTL":                         "20s",
		"RELAY_AUTH_ATTEMPT_WINDOW":                   "12s",
		"RELAY_AUTH_MAX_ATTEMPTS_PER_IP":              "33",
		"RELAY_AUTH_MAX_ATTEMPTS_PER_PEER":            "9",
		"RELAY_AUTH_MAX_CONCURRENCY":                  "80",
		"RELAY_ACCEPT_BOOKINGS":                       "true",
		"RELAY_BUFFER_SIZE":                           "4096",
		"RELAY_CIRCUIT_DATA_BYTES_PER_DIRECTION":      "21474836480",
		"RELAY_CIRCUIT_DURATION":                      "20m",
		"RELAY_CONNMGR_HIGH_WATER":                    "1500",
		"RELAY_CONNMGR_LOW_WATER":                     "1000",
		"RELAY_DDS_KEY_CLOCK_SKEW":                    "2m",
		"RELAY_DDS_KEY_MAX_STALE":                     "2h",
		"RELAY_DDS_KEY_OVERLAP":                       "32m",
		"RELAY_DDS_KEY_REFRESH_INTERVAL":              "3m",
		"RELAY_DDS_KEY_UNKNOWN_REFRESH_INTERVAL":      "45s",
		"RELAY_DDS_MAX_CONCURRENCY":                   "36",
		"RELAY_DDS_PUBLIC_KEY_URL":                    "https://keys.dev.aukiverse.com/current",
		"RELAY_DDS_SIGNING_METHOD":                    "ES384",
		"RELAY_DDS_URL":                               "https://dds.dev.aukiverse.com/v1",
		"RELAY_DMS_URL":                               "https://dms.dev.aukiverse.com/v1",
		"RELAY_EMPTY_CLAIM_MAX_JITTER_FRACTION":       "0.15",
		"RELAY_HTTP_REQUEST_TIMEOUT":                  "11s",
		"RELAY_LIBP2P_PRIVATE_KEY_FILE":               "/var/run/relay/libp2p",
		"RELAY_LOCAL_CAPACITY":                        "40",
		"RELAY_LOCAL_TEST_ALLOW_HTTP":                 "false",
		"RELAY_MAX_CIRCUITS_PER_PEER":                 "20",
		"RELAY_MAX_RESERVATIONS":                      "60",
		"RELAY_MAX_RESERVATIONS_PER_ASN":              "60",
		"RELAY_MAX_RESERVATIONS_PER_IP":               "50",
		"RELAY_METRICS_ADDR":                          "127.0.0.1:9191",
		"RELAY_NODE_TOKEN_REFRESH_FRACTION":           "0.75",
		"RELAY_PROVIDER_LEASE_HEARTBEAT_MAX_FRACTION": "0.35",
		"RELAY_PROVIDER_LEASE_HEARTBEAT_MIN_FRACTION": "0.25",
		"RELAY_PROVIDER_SESSION_TTL":                  "3m",
		"RELAY_PUBLIC_BASE_MULTIADDRS":                "/dns4/relay-b.dev.aukiverse.com/tcp/443/p2p/" + peerID.String() + ", /dns4/relay-a.dev.aukiverse.com/tcp/4002/wss/p2p/" + peerID.String(),
		"RELAY_REGISTRATION_CREDENTIALS_FILE":         "/var/run/relay/credentials",
		"RELAY_RESERVATION_TTL":                       "10m",
		"RELAY_RETRY_INITIAL_BACKOFF":                 "2s",
		"RELAY_RETRY_MAX_BACKOFF":                     "25s",
		"RELAY_RM_CONNECTIONS":                        "3000",
		"RELAY_RM_FILE_DESCRIPTORS":                   "8000",
		"RELAY_RM_MEMORY_BYTES":                       "2147483648",
		"RELAY_RM_STREAMS":                            "9000",
		"RELAY_SHUTDOWN_DRAIN":                        "10m",
		"RELAY_STATUS_INTERVAL":                       "30s",
		"RELAY_STATUS_MAX_BACKOFF":                    "12s",
		"RELAY_TCP_LISTEN_MULTIADDR":                  "/ip6/::/tcp/4101",
		"RELAY_WALLET_PRIVATE_KEY_FILE":               "/var/run/relay/wallet",
		"RELAY_WS_LISTEN_MULTIADDR":                   "/ip6/::/tcp/4102/ws",
	}

	matrix := EnvironmentMatrix()
	require.Len(t, values, len(matrix), "the test fixture must exercise every documented environment entry")
	for _, spec := range matrix {
		_, covered := values[spec.Name]
		require.Truef(t, covered, "environment matrix entry %s is not parsed by the full fixture", spec.Name)
		require.NotEmpty(t, spec.Constraint)
		if spec.Required {
			require.Empty(t, spec.Default)
		} else {
			require.NotEmpty(t, spec.Default)
		}
	}

	cfg, err := LoadFrom(lookupMap(values))
	require.NoError(t, err)
	require.True(t, cfg.AcceptBookings)
	require.Equal(t, 36, *cfg.Relay.DDSMaxConcurrency)
	require.Equal(t, 40, cfg.Relay.LocalCapacity)
	require.Equal(t, 60, cfg.Relay.MaxReservations)
	require.Equal(t, int64(21_474_836_480), cfg.Relay.Limits.DataBytesPerDirection)
	require.Equal(t, 20*time.Minute, cfg.Relay.Limits.Duration)
	require.Equal(t, "/ip6/::/tcp/4101", cfg.TCPListenMultiaddr)
	require.Equal(t, "/ip6/::/tcp/4102/ws", cfg.WebSocketListenMultiaddr)
	require.Equal(t, []string{
		"/dns4/relay-b.dev.aukiverse.com/tcp/443/p2p/" + peerID.String(),
		"/dns4/relay-a.dev.aukiverse.com/tcp/4002/wss/p2p/" + peerID.String(),
	}, cfg.PublicBaseMultiaddrs)
	require.Equal(t, "ES384", cfg.VerificationKeys.SigningMethod)
	require.Equal(t, 2*time.Minute, cfg.VerificationKeys.ClockSkew)
	require.Equal(t, 12*time.Second, cfg.Timing.StatusMaxBackoff)
	require.Equal(t, 2*time.Second, cfg.Retry.InitialBackoff)
	require.Equal(t, 25*time.Second, cfg.Retry.MaximumBackoff)
	require.Equal(t, 0.15, cfg.Retry.EmptyClaimMaxJitterFraction)

	names := make([]string, 0, len(matrix))
	for _, spec := range matrix {
		names = append(names, spec.Name)
	}
	require.True(t, slices.IsSorted(names), "environment matrix must be deterministic")
	require.Len(t, names, len(unique(names)), "environment names must be unique")
}

func TestLoadRejectsMalformedEnvironmentValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "boolean", key: "RELAY_ACCEPT_BOOKINGS", value: "sometimes"},
		{name: "duration", key: "RELAY_HTTP_REQUEST_TIMEOUT", value: "ten"},
		{name: "float", key: "RELAY_NODE_TOKEN_REFRESH_FRACTION", value: "NaN"},
		{name: "integer", key: "RELAY_LOCAL_CAPACITY", value: "32.5"},
		{name: "optional integer", key: "RELAY_DDS_MAX_CONCURRENCY", value: "thirty-two"},
		{name: "int64", key: "RELAY_CIRCUIT_DATA_BYTES_PER_DIRECTION", value: "18446744073709551615"},
		{name: "blank optional string", key: "RELAY_ADMIN_ADDR", value: " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := requiredEnvironment(t)
			values[test.key] = test.value
			_, err := LoadFrom(lookupMap(values))
			require.ErrorContains(t, err, test.key)
		})
	}
	require.Error(t, func() error {
		_, err := LoadFrom(nil)
		return err
	}())
}

func TestURLPolicyRequiresHTTPSExceptExplicitLoopbackTest(t *testing.T) {
	for _, test := range []struct {
		name      string
		url       string
		allowHTTP bool
		wantError bool
	}{
		{name: "https", url: "https://dds.example.com/path"},
		{name: "localhost enabled", url: "http://localhost:8080", allowHTTP: true},
		{name: "localhost trailing dot", url: "http://localhost.:8080", allowHTTP: true},
		{name: "IPv4 loopback enabled", url: "http://127.0.0.2:8080", allowHTTP: true},
		{name: "IPv6 loopback enabled", url: "http://[::1]:8080", allowHTTP: true},
		{name: "localhost disabled", url: "http://localhost:8080", wantError: true},
		{name: "remote enabled", url: "http://dds.example.com", allowHTTP: true, wantError: true},
		{name: "localhost suffix attack", url: "http://localhost.example.com", allowHTTP: true, wantError: true},
		{name: "userinfo", url: "https://token@dds.example.com", wantError: true},
		{name: "query", url: "https://dds.example.com?secret=x", wantError: true},
		{name: "fragment", url: "https://dds.example.com#fragment", wantError: true},
		{name: "unsupported scheme", url: "file:///tmp/key", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateServiceURL(test.url, test.allowHTTP)
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	values := requiredEnvironment(t)
	values["RELAY_LOCAL_TEST_ALLOW_HTTP"] = "true"
	values["RELAY_DDS_URL"] = "http://localhost:8080"
	values["RELAY_DMS_URL"] = "http://127.0.0.1:8081"
	values["RELAY_DDS_PUBLIC_KEY_URL"] = "http://[::1]:8082/key"
	_, err := LoadFrom(lookupMap(values))
	require.NoError(t, err)
}

func TestValidationRejectsUnsafeCrossFieldConfigurations(t *testing.T) {
	tests := []struct {
		name     string
		contains string
		mutate   func(*Config)
	}{
		{name: "status contract", contains: "status interval", mutate: func(c *Config) { c.Timing.StatusInterval = 29 * time.Second }},
		{name: "retry ordering", contains: "retry initial", mutate: func(c *Config) { c.Retry.InitialBackoff = 31 * time.Second }},
		{name: "retry cap", contains: "retry maximum", mutate: func(c *Config) { c.Retry.MaximumBackoff = 31 * time.Second }},
		{name: "claim jitter", contains: "jitter fraction", mutate: func(c *Config) { c.Retry.EmptyClaimMaxJitterFraction = 0.21 }},
		{name: "session contract", contains: "session TTL", mutate: func(c *Config) { c.Timing.ProviderSessionTTL = 4 * time.Minute }},
		{name: "refresh fraction", contains: "refresh fraction", mutate: func(c *Config) { c.Timing.NodeTokenRefreshFraction = 0.8 }},
		{name: "heartbeat fractions", contains: "heartbeat fractions", mutate: func(c *Config) { c.Timing.LeaseHeartbeatMaxFraction = 0.4 }},
		{name: "symmetric signing method", contains: "unsupported DDS signing", mutate: func(c *Config) { c.VerificationKeys.SigningMethod = "HS256" }},
		{name: "key overlap", contains: "overlap", mutate: func(c *Config) { c.VerificationKeys.PreviousKeyOverlap = 30 * time.Minute }},
		{name: "key staleness", contains: "staleness", mutate: func(c *Config) { c.VerificationKeys.MaxStaleness = 30 * time.Minute }},
		{name: "unknown refresh", contains: "unknown-signature", mutate: func(c *Config) { c.VerificationKeys.UnknownRefreshInterval = 3 * time.Minute }},
		{name: "admission TTL", contains: "admission TTL", mutate: func(c *Config) { c.Admission.TTL = 31 * time.Second }},
		{name: "auth peer concurrency", contains: "must not exceed", mutate: func(c *Config) { c.Admission.AttemptsPerPeer = 65 }},
		{name: "capacity below range", contains: "local relay capacity", mutate: func(c *Config) { c.Relay.LocalCapacity = 0 }},
		{name: "capacity above range", contains: "local relay capacity", mutate: func(c *Config) { c.Relay.LocalCapacity = 257 }},
		{name: "DDS capacity below range", contains: "DDS max_concurrency", mutate: func(c *Config) { value := 0; c.Relay.DDSMaxConcurrency = &value }},
		{name: "DDS capacity above local", contains: "configured DDS/DMS capacity", mutate: func(c *Config) { value := 33; c.Relay.DDSMaxConcurrency = &value }},
		{name: "absent DDS capacity requires local DMS default", contains: "configured DDS/DMS capacity", mutate: func(c *Config) {
			c.Relay.DDSMaxConcurrency = nil
			c.Relay.LocalCapacity = 31
		}},
		{name: "reservation quota", contains: "cover local", mutate: func(c *Config) { c.Relay.MaxReservationsPerIP = 31 }},
		{name: "fixed reservation TTL", contains: "reservation TTL", mutate: func(c *Config) { c.Relay.ReservationTTL = 9 * time.Minute }},
		{name: "fractional circuit duration", contains: "whole seconds", mutate: func(c *Config) { c.Relay.Limits.Duration += time.Nanosecond }},
		{name: "resource file descriptors", contains: "file descriptors", mutate: func(c *Config) { c.ResourceManager.FileDescriptors = 1000 }},
		{name: "resource memory minimum", contains: "at least 128 MiB", mutate: func(c *Config) { c.ResourceManager.MemoryBytes = MinimumResourceManagerMemory - 1 }},
		{name: "resource streams", contains: "streams", mutate: func(c *Config) { c.ResourceManager.Streams = 1000 }},
		{name: "connmgr low capacity", contains: "low water", mutate: func(c *Config) { c.ConnectionManager.LowWater = 31 }},
		{name: "connmgr ordering", contains: "below high", mutate: func(c *Config) { c.ConnectionManager.LowWater = c.ConnectionManager.HighWater }},
		{name: "connmgr resource cap", contains: "resource-manager connections", mutate: func(c *Config) { c.ConnectionManager.HighWater = 2049 }},
		{name: "same admin listeners", contains: "must differ", mutate: func(c *Config) { c.MetricsAddress = c.AdminAddress }},
		{name: "zero shutdown", contains: "shutdown drain", mutate: func(c *Config) { c.ShutdownDrain = 0 }},
		{name: "fractional shutdown", contains: "whole seconds", mutate: func(c *Config) { c.ShutdownDrain = time.Second + time.Nanosecond }},
		{name: "unbounded shutdown", contains: "shutdown drain", mutate: func(c *Config) { c.ShutdownDrain = 16 * time.Minute }},
		{name: "application stream listen", contains: "relay TCP listen multiaddr", mutate: func(c *Config) { c.TCPListenMultiaddr += "/p2p/12D3KooWBad" }},
		{name: "DNS TCP listen", contains: "ip4 or ip6", mutate: func(c *Config) { c.TCPListenMultiaddr = "/dns4/localhost/tcp/4001" }},
		{name: "WebSocket stream listen", contains: "relay WebSocket listen multiaddr", mutate: func(c *Config) { c.WebSocketListenMultiaddr = "/ip4/127.0.0.1/tcp/4002/wss" }},
		{name: "DNS WebSocket listen", contains: "ip4 or ip6", mutate: func(c *Config) { c.WebSocketListenMultiaddr = "/dns4/localhost/tcp/4002/ws" }},
		{name: "missing WebSocket listener", contains: "relay WebSocket listen multiaddr", mutate: func(c *Config) { c.WebSocketListenMultiaddr = "" }},
		{name: "shared transport socket", contains: "different sockets", mutate: func(c *Config) { c.WebSocketListenMultiaddr = c.TCPListenMultiaddr + "/ws" }},
		{name: "empty advertised entry", contains: "empty entries", mutate: func(c *Config) { c.PublicBaseMultiaddrs = []string{""} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig(t)
			test.mutate(&cfg)
			err := cfg.Validate()
			require.ErrorContains(t, err, test.contains)
		})
	}
}

func TestResourceManagerMemoryAcceptsExactMinimum(t *testing.T) {
	cfg := validConfig(t)
	cfg.ResourceManager.MemoryBytes = MinimumResourceManagerMemory
	require.NoError(t, cfg.Validate())
}

func TestValidateEffectiveCapacityChecksEveryCapacityBackstop(t *testing.T) {
	cfg := validConfig(t)
	require.NoError(t, cfg.ValidateEffectiveCapacity(32))
	require.Error(t, cfg.ValidateEffectiveCapacity(0))
	require.Error(t, cfg.ValidateEffectiveCapacity(257))

	for _, mutate := range []func(*Config){
		func(c *Config) { c.Relay.LocalCapacity = 31 },
		func(c *Config) { c.Relay.MaxReservations = 31 },
		func(c *Config) { c.Relay.MaxReservationsPerIP = 31 },
		func(c *Config) { c.Relay.MaxReservationsPerASN = 31 },
		func(c *Config) { c.ResourceManager.Connections = 31 },
		func(c *Config) { c.ResourceManager.Streams = 31 },
		func(c *Config) { c.ConnectionManager.LowWater = 31 },
	} {
		candidate := cfg
		mutate(&candidate)
		require.ErrorContains(t, candidate.ValidateEffectiveCapacity(32), "below effective")
	}
}

func TestEffectiveCapacityForDDSClaimUsesSignedOverrideOrDMSDefault(t *testing.T) {
	cfg := validConfig(t)

	effective, err := cfg.EffectiveCapacityForDDSClaim(nil)
	require.NoError(t, err)
	require.Equal(t, DefaultDMSRelayCapacity, effective)

	signed := 24
	effective, err = cfg.EffectiveCapacityForDDSClaim(&signed)
	require.NoError(t, err)
	require.Equal(t, signed, effective)

	configured := 20
	cfg.Relay.DDSMaxConcurrency = &configured
	_, err = cfg.EffectiveCapacityForDDSClaim(&signed)
	require.ErrorContains(t, err, "does not match")

	tooLarge := 33
	cfg.Relay.DDSMaxConcurrency = nil
	_, err = cfg.EffectiveCapacityForDDSClaim(&tooLarge)
	require.ErrorContains(t, err, "below effective")
}

func TestRedactedViewNeverExposesSecretReferences(t *testing.T) {
	cfg := validConfig(t)
	encoded, err := cfg.MarshalRedactedJSON()
	require.NoError(t, err)
	for _, secretPath := range []string{
		cfg.Secrets.RegistrationCredentials,
		cfg.Secrets.WalletPrivateKey,
		cfg.Secrets.Libp2pPrivateKey,
	} {
		require.NotContains(t, string(encoded), secretPath)
	}
	require.Equal(t, 3, strings.Count(string(encoded), redacted))
	require.Contains(t, string(encoded), cfg.DDSURL)
	require.Contains(t, string(encoded), Libp2pInboundUpgradeTimeout.String())

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	public := cfg.Redacted()["public_base_multiaddrs"].([]string)
	public[0] = "mutated"
	require.NotEqual(t, "mutated", cfg.PublicBaseMultiaddrs[0])
}

func TestMissingRequiredConfigurationFailsClosed(t *testing.T) {
	values := requiredEnvironment(t)
	for _, key := range []string{
		"RELAY_DDS_URL",
		"RELAY_DMS_URL",
		"RELAY_DDS_PUBLIC_KEY_URL",
		"RELAY_REGISTRATION_CREDENTIALS_FILE",
		"RELAY_WALLET_PRIVATE_KEY_FILE",
		"RELAY_LIBP2P_PRIVATE_KEY_FILE",
		"RELAY_PUBLIC_BASE_MULTIADDRS",
	} {
		t.Run(key, func(t *testing.T) {
			candidate := make(map[string]string, len(values))
			for name, value := range values {
				candidate[name] = value
			}
			delete(candidate, key)
			_, err := LoadFrom(lookupMap(candidate))
			require.Error(t, err)
		})
	}
}

func unique(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func ExampleEnvironmentMatrix() {
	for _, entry := range EnvironmentMatrix()[:2] {
		fmt.Printf("%s=%s\n", entry.Name, entry.Default)
	}
	// Output:
	// RELAY_ACCEPT_BOOKINGS=false
	// RELAY_ADMIN_ADDR=127.0.0.1:9090
}
