// Package config owns the standalone relay's operator configuration. Defaults
// here are implementation defaults only; they do not assert that any rollout
// is sized or ready for production traffic.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aukilabs/hagall/pkg/relayconfig"
	"github.com/golang-jwt/jwt/v4"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	DefaultCircuitDuration       = 15 * time.Minute
	DefaultCircuitDataBytes      = int64(10 * 1024 * 1024 * 1024)
	DefaultLocalCapacity         = 32
	DefaultDMSRelayCapacity      = 32
	MaximumLocalCapacity         = relayconfig.MaxCapacityValue
	MinimumResourceManagerMemory = int64(128 << 20)
	DefaultReservationTTL        = 10 * time.Minute
	RequiredProviderSessionTTL   = 3 * time.Minute
	RequiredProviderStatusPeriod = 30 * time.Second

	// These timeout values are fixed by go-libp2p v0.41.1. They are exposed for
	// diagnostics, not represented as environment knobs that the process cannot
	// honor.
	Libp2pInboundUpgradeTimeout      = 15 * time.Second
	Libp2pProtocolNegotiationTimeout = 10 * time.Second
	Libp2pRelayStreamTimeout         = time.Minute
	Libp2pRelayConnectTimeout        = 30 * time.Second
	Libp2pRelayHandshakeTimeout      = time.Minute
)

const redacted = "[redacted:file]"

type LookupEnv func(string) (string, bool)

type SecretFiles struct {
	RegistrationCredentials string
	WalletPrivateKey        string
	Libp2pPrivateKey        string
}

type HTTPConfig struct {
	RequestTimeout time.Duration
}

type RetryConfig struct {
	InitialBackoff              time.Duration
	MaximumBackoff              time.Duration
	EmptyClaimMaxJitterFraction float64
}

type TimingConfig struct {
	StatusInterval            time.Duration
	StatusMaxBackoff          time.Duration
	ProviderSessionTTL        time.Duration
	NodeTokenRefreshFraction  float64
	LeaseHeartbeatMinFraction float64
	LeaseHeartbeatMaxFraction float64
}

type VerificationKeyConfig struct {
	SigningMethod          string
	RefreshInterval        time.Duration
	PreviousKeyOverlap     time.Duration
	MaxStaleness           time.Duration
	ClockSkew              time.Duration
	UnknownRefreshInterval time.Duration
}

type AdmissionConfig struct {
	TTL             time.Duration
	MaximumEntries  int
	AuthConcurrency int
	AttemptsPerPeer int
	AttemptsPerIP   int
	AttemptWindow   time.Duration
}

type RelayConfig struct {
	Limits                relayconfig.Limits
	DDSMaxConcurrency     *int
	LocalCapacity         int
	MaxReservations       int
	MaxReservationsPerIP  int
	MaxReservationsPerASN int
	MaxCircuitsPerPeer    int
	ReservationTTL        time.Duration
	BufferSize            int
}

type ResourceManagerConfig struct {
	MemoryBytes     int64
	FileDescriptors int
	Connections     int
	Streams         int
}

type ConnectionManagerConfig struct {
	LowWater  int
	HighWater int
}

// Config is the validated input to the standalone relay process. It contains
// secret file references, never secret values.
type Config struct {
	DDSURL                   string
	DMSURL                   string
	DDSPublicKeyURL          string
	AllowLocalTestHTTP       bool
	Secrets                  SecretFiles
	TCPListenMultiaddr       string
	WebSocketListenMultiaddr string
	PublicBaseMultiaddrs     []string
	HTTP                     HTTPConfig
	Retry                    RetryConfig
	Timing                   TimingConfig
	VerificationKeys         VerificationKeyConfig
	Admission                AdmissionConfig
	Relay                    RelayConfig
	ResourceManager          ResourceManagerConfig
	ConnectionManager        ConnectionManagerConfig
	AdminAddress             string
	MetricsAddress           string
	AcceptBookings           bool
	ShutdownDrain            time.Duration
}

// Defaults returns operational defaults. Required deployment-specific values
// (URLs, public bases, and secret file references) intentionally remain empty.
func Defaults() Config {
	return Config{
		TCPListenMultiaddr:       "/ip4/0.0.0.0/tcp/4001",
		WebSocketListenMultiaddr: "/ip4/0.0.0.0/tcp/4002/ws",
		HTTP:                     HTTPConfig{RequestTimeout: 10 * time.Second},
		Retry: RetryConfig{
			InitialBackoff:              time.Second,
			MaximumBackoff:              30 * time.Second,
			EmptyClaimMaxJitterFraction: 0.20,
		},
		Timing: TimingConfig{
			StatusInterval:            RequiredProviderStatusPeriod,
			StatusMaxBackoff:          10 * time.Second,
			ProviderSessionTTL:        RequiredProviderSessionTTL,
			NodeTokenRefreshFraction:  0.75,
			LeaseHeartbeatMinFraction: 0.25,
			LeaseHeartbeatMaxFraction: 0.35,
		},
		VerificationKeys: VerificationKeyConfig{
			SigningMethod:          "ES256",
			RefreshInterval:        2 * time.Minute,
			PreviousKeyOverlap:     31 * time.Minute,
			MaxStaleness:           time.Hour,
			ClockSkew:              time.Minute,
			UnknownRefreshInterval: 30 * time.Second,
		},
		Admission: AdmissionConfig{
			TTL:             30 * time.Second,
			MaximumEntries:  4096,
			AuthConcurrency: 64,
			AttemptsPerPeer: 8,
			AttemptsPerIP:   32,
			AttemptWindow:   10 * time.Second,
		},
		Relay: RelayConfig{
			Limits: relayconfig.Limits{
				DataBytesPerDirection: DefaultCircuitDataBytes,
				Duration:              DefaultCircuitDuration,
			},
			LocalCapacity:         DefaultLocalCapacity,
			MaxReservations:       DefaultLocalCapacity,
			MaxReservationsPerIP:  DefaultLocalCapacity,
			MaxReservationsPerASN: DefaultLocalCapacity,
			MaxCircuitsPerPeer:    16,
			ReservationTTL:        DefaultReservationTTL,
			BufferSize:            2048,
		},
		ResourceManager: ResourceManagerConfig{
			MemoryBytes:     1 << 30,
			FileDescriptors: 4096,
			Connections:     2048,
			Streams:         8192,
		},
		ConnectionManager: ConnectionManagerConfig{LowWater: 768, HighWater: 1024},
		AdminAddress:      "127.0.0.1:9090",
		MetricsAddress:    "127.0.0.1:9091",
		AcceptBookings:    false,
		ShutdownDrain:     15 * time.Minute,
	}
}

func Load() (Config, error) {
	return LoadFrom(os.LookupEnv)
}

// LoadFrom parses the complete relay environment and validates all cross-field
// safety constraints before returning.
func LoadFrom(lookup LookupEnv) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("environment lookup is required")
	}
	cfg := Defaults()

	requiredString(lookup, "RELAY_DDS_URL", &cfg.DDSURL)
	requiredString(lookup, "RELAY_DMS_URL", &cfg.DMSURL)
	requiredString(lookup, "RELAY_DDS_PUBLIC_KEY_URL", &cfg.DDSPublicKeyURL)
	requiredString(lookup, "RELAY_REGISTRATION_CREDENTIALS_FILE", &cfg.Secrets.RegistrationCredentials)
	requiredString(lookup, "RELAY_WALLET_PRIVATE_KEY_FILE", &cfg.Secrets.WalletPrivateKey)
	requiredString(lookup, "RELAY_LIBP2P_PRIVATE_KEY_FILE", &cfg.Secrets.Libp2pPrivateKey)
	if raw, present := lookup("RELAY_PUBLIC_BASE_MULTIADDRS"); present {
		cfg.PublicBaseMultiaddrs = splitCommaSeparated(raw)
	}

	var parseErrors []error
	parseErrors = append(parseErrors,
		optionalBool(lookup, "RELAY_LOCAL_TEST_ALLOW_HTTP", &cfg.AllowLocalTestHTTP),
		optionalString(lookup, "RELAY_TCP_LISTEN_MULTIADDR", &cfg.TCPListenMultiaddr),
		optionalString(lookup, "RELAY_WS_LISTEN_MULTIADDR", &cfg.WebSocketListenMultiaddr),
		optionalDuration(lookup, "RELAY_HTTP_REQUEST_TIMEOUT", &cfg.HTTP.RequestTimeout),
		optionalDuration(lookup, "RELAY_RETRY_INITIAL_BACKOFF", &cfg.Retry.InitialBackoff),
		optionalDuration(lookup, "RELAY_RETRY_MAX_BACKOFF", &cfg.Retry.MaximumBackoff),
		optionalFloat(lookup, "RELAY_EMPTY_CLAIM_MAX_JITTER_FRACTION", &cfg.Retry.EmptyClaimMaxJitterFraction),
		optionalDuration(lookup, "RELAY_STATUS_INTERVAL", &cfg.Timing.StatusInterval),
		optionalDuration(lookup, "RELAY_STATUS_MAX_BACKOFF", &cfg.Timing.StatusMaxBackoff),
		optionalDuration(lookup, "RELAY_PROVIDER_SESSION_TTL", &cfg.Timing.ProviderSessionTTL),
		optionalFloat(lookup, "RELAY_NODE_TOKEN_REFRESH_FRACTION", &cfg.Timing.NodeTokenRefreshFraction),
		optionalFloat(lookup, "RELAY_PROVIDER_LEASE_HEARTBEAT_MIN_FRACTION", &cfg.Timing.LeaseHeartbeatMinFraction),
		optionalFloat(lookup, "RELAY_PROVIDER_LEASE_HEARTBEAT_MAX_FRACTION", &cfg.Timing.LeaseHeartbeatMaxFraction),
		optionalString(lookup, "RELAY_DDS_SIGNING_METHOD", &cfg.VerificationKeys.SigningMethod),
		optionalDuration(lookup, "RELAY_DDS_KEY_REFRESH_INTERVAL", &cfg.VerificationKeys.RefreshInterval),
		optionalDuration(lookup, "RELAY_DDS_KEY_OVERLAP", &cfg.VerificationKeys.PreviousKeyOverlap),
		optionalDuration(lookup, "RELAY_DDS_KEY_MAX_STALE", &cfg.VerificationKeys.MaxStaleness),
		optionalDuration(lookup, "RELAY_DDS_KEY_CLOCK_SKEW", &cfg.VerificationKeys.ClockSkew),
		optionalDuration(lookup, "RELAY_DDS_KEY_UNKNOWN_REFRESH_INTERVAL", &cfg.VerificationKeys.UnknownRefreshInterval),
		optionalDuration(lookup, "RELAY_ADMISSION_TTL", &cfg.Admission.TTL),
		optionalInt(lookup, "RELAY_ADMISSION_MAX_ENTRIES", &cfg.Admission.MaximumEntries),
		optionalInt(lookup, "RELAY_AUTH_MAX_CONCURRENCY", &cfg.Admission.AuthConcurrency),
		optionalInt(lookup, "RELAY_AUTH_MAX_ATTEMPTS_PER_PEER", &cfg.Admission.AttemptsPerPeer),
		optionalInt(lookup, "RELAY_AUTH_MAX_ATTEMPTS_PER_IP", &cfg.Admission.AttemptsPerIP),
		optionalDuration(lookup, "RELAY_AUTH_ATTEMPT_WINDOW", &cfg.Admission.AttemptWindow),
		optionalDuration(lookup, "RELAY_CIRCUIT_DURATION", &cfg.Relay.Limits.Duration),
		optionalInt64(lookup, "RELAY_CIRCUIT_DATA_BYTES_PER_DIRECTION", &cfg.Relay.Limits.DataBytesPerDirection),
		optionalIntPointer(lookup, "RELAY_DDS_MAX_CONCURRENCY", &cfg.Relay.DDSMaxConcurrency),
		optionalInt(lookup, "RELAY_LOCAL_CAPACITY", &cfg.Relay.LocalCapacity),
		optionalInt(lookup, "RELAY_MAX_RESERVATIONS", &cfg.Relay.MaxReservations),
		optionalInt(lookup, "RELAY_MAX_RESERVATIONS_PER_IP", &cfg.Relay.MaxReservationsPerIP),
		optionalInt(lookup, "RELAY_MAX_RESERVATIONS_PER_ASN", &cfg.Relay.MaxReservationsPerASN),
		optionalInt(lookup, "RELAY_MAX_CIRCUITS_PER_PEER", &cfg.Relay.MaxCircuitsPerPeer),
		optionalDuration(lookup, "RELAY_RESERVATION_TTL", &cfg.Relay.ReservationTTL),
		optionalInt(lookup, "RELAY_BUFFER_SIZE", &cfg.Relay.BufferSize),
		optionalInt64(lookup, "RELAY_RM_MEMORY_BYTES", &cfg.ResourceManager.MemoryBytes),
		optionalInt(lookup, "RELAY_RM_FILE_DESCRIPTORS", &cfg.ResourceManager.FileDescriptors),
		optionalInt(lookup, "RELAY_RM_CONNECTIONS", &cfg.ResourceManager.Connections),
		optionalInt(lookup, "RELAY_RM_STREAMS", &cfg.ResourceManager.Streams),
		optionalInt(lookup, "RELAY_CONNMGR_LOW_WATER", &cfg.ConnectionManager.LowWater),
		optionalInt(lookup, "RELAY_CONNMGR_HIGH_WATER", &cfg.ConnectionManager.HighWater),
		optionalString(lookup, "RELAY_ADMIN_ADDR", &cfg.AdminAddress),
		optionalString(lookup, "RELAY_METRICS_ADDR", &cfg.MetricsAddress),
		optionalBool(lookup, "RELAY_ACCEPT_BOOKINGS", &cfg.AcceptBookings),
		optionalDuration(lookup, "RELAY_SHUTDOWN_DRAIN", &cfg.ShutdownDrain),
	)
	if err := errors.Join(parseErrors...); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	for name, raw := range map[string]string{
		"DDS URL":            c.DDSURL,
		"DMS URL":            c.DMSURL,
		"DDS public-key URL": c.DDSPublicKeyURL,
	} {
		if err := validateServiceURL(raw, c.AllowLocalTestHTTP); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	for name, path := range map[string]string{
		"registration credentials": c.Secrets.RegistrationCredentials,
		"wallet private key":       c.Secrets.WalletPrivateKey,
		"libp2p private key":       c.Secrets.Libp2pPrivateKey,
	} {
		if strings.TrimSpace(path) == "" {
			errs = append(errs, fmt.Errorf("%s secret file path is required", name))
		}
	}
	if err := validateListenMultiaddr(c.TCPListenMultiaddr); err != nil {
		errs = append(errs, err)
	}
	if err := validateWebSocketListenMultiaddr(c.WebSocketListenMultiaddr); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSuffix(c.WebSocketListenMultiaddr, "/ws") == c.TCPListenMultiaddr {
		errs = append(errs, errors.New("relay TCP and WebSocket listeners must use different sockets"))
	}
	if len(c.PublicBaseMultiaddrs) == 0 {
		errs = append(errs, errors.New("at least one public relay base multiaddr is required"))
	} else {
		seen := make(map[string]struct{}, len(c.PublicBaseMultiaddrs))
		for _, base := range c.PublicBaseMultiaddrs {
			if strings.TrimSpace(base) == "" {
				errs = append(errs, errors.New("public relay base multiaddrs must not contain empty entries"))
				continue
			}
			if _, duplicate := seen[base]; duplicate {
				errs = append(errs, fmt.Errorf("duplicate configured public relay base multiaddr: %s", base))
			}
			seen[base] = struct{}{}
		}
	}

	if c.HTTP.RequestTimeout <= 0 || c.HTTP.RequestTimeout > 30*time.Second {
		errs = append(errs, errors.New("HTTP request timeout must be in (0s,30s]"))
	}
	if c.Retry.InitialBackoff <= 0 || c.Retry.InitialBackoff > c.Retry.MaximumBackoff {
		errs = append(errs, errors.New("retry initial backoff must be positive and no greater than maximum backoff"))
	}
	if c.Retry.MaximumBackoff <= 0 || c.Retry.MaximumBackoff > 30*time.Second {
		errs = append(errs, errors.New("retry maximum backoff must be in (0s,30s]"))
	}
	if c.Retry.EmptyClaimMaxJitterFraction < 0 || c.Retry.EmptyClaimMaxJitterFraction > 0.20 {
		errs = append(errs, errors.New("empty-claim maximum jitter fraction must be in [0,0.20]"))
	}
	if c.Timing.ProviderSessionTTL != RequiredProviderSessionTTL {
		errs = append(errs, fmt.Errorf("provider session TTL must equal the DMS contract %s", RequiredProviderSessionTTL))
	}
	if c.Timing.StatusInterval != RequiredProviderStatusPeriod {
		errs = append(errs, fmt.Errorf("provider status interval must equal the DMS contract %s", RequiredProviderStatusPeriod))
	}
	if c.Timing.StatusMaxBackoff <= 0 || c.Timing.StatusMaxBackoff > c.Timing.StatusInterval {
		errs = append(errs, errors.New("provider status max backoff must be positive and no greater than the status interval"))
	}
	if c.HTTP.RequestTimeout+c.Timing.StatusMaxBackoff+c.Timing.StatusInterval >= c.Timing.ProviderSessionTTL {
		errs = append(errs, errors.New("request timeout plus status interval and backoff must remain below the provider session TTL"))
	}
	if c.Timing.NodeTokenRefreshFraction != 0.75 {
		errs = append(errs, errors.New("Node-token refresh fraction must equal 0.75"))
	}
	if c.Timing.LeaseHeartbeatMinFraction != 0.25 || c.Timing.LeaseHeartbeatMaxFraction != 0.35 {
		errs = append(errs, errors.New("provider lease heartbeat fractions must equal 0.25 and 0.35"))
	}

	keys := c.VerificationKeys
	if !supportedPublicKeySigningMethod(keys.SigningMethod) {
		errs = append(errs, fmt.Errorf("unsupported DDS signing method %q", keys.SigningMethod))
	}
	if keys.RefreshInterval <= 0 || keys.UnknownRefreshInterval <= 0 {
		errs = append(errs, errors.New("DDS key refresh intervals must be positive"))
	}
	if keys.ClockSkew < 0 || keys.ClockSkew > 5*time.Minute {
		errs = append(errs, errors.New("DDS verification clock skew must be in [0s,5m]"))
	}
	if keys.PreviousKeyOverlap < 30*time.Minute+keys.ClockSkew {
		errs = append(errs, errors.New("DDS previous-key overlap must cover the 30-minute P2P token lifetime plus clock skew"))
	}
	if keys.MaxStaleness < keys.PreviousKeyOverlap {
		errs = append(errs, errors.New("DDS key max staleness must be at least the previous-key overlap"))
	}
	if keys.UnknownRefreshInterval > keys.RefreshInterval {
		errs = append(errs, errors.New("unknown-signature refresh interval must not exceed periodic key refresh"))
	}

	admission := c.Admission
	if admission.TTL <= 0 || admission.TTL > 30*time.Second {
		errs = append(errs, errors.New("admission TTL must be in (0s,30s]"))
	}
	if admission.MaximumEntries <= 0 || admission.AuthConcurrency <= 0 || admission.AttemptsPerPeer <= 0 || admission.AttemptsPerIP <= 0 {
		errs = append(errs, errors.New("admission cardinality, auth concurrency, and attempt limits must be positive"))
	}
	if admission.AttemptWindow <= 0 || admission.AttemptWindow > time.Minute {
		errs = append(errs, errors.New("auth attempt window must be in (0s,1m]"))
	}

	if err := c.Relay.Limits.Validate(); err != nil {
		errs = append(errs, err)
	}
	if !inCapacityRange(c.Relay.LocalCapacity) {
		errs = append(errs, fmt.Errorf("local relay capacity must be in [1,%d]", MaximumLocalCapacity))
	}
	configuredCapacity := DefaultDMSRelayCapacity
	if c.Relay.DDSMaxConcurrency != nil {
		configuredCapacity = *c.Relay.DDSMaxConcurrency
		if !inCapacityRange(configuredCapacity) {
			errs = append(errs, fmt.Errorf("DDS max_concurrency must be in [1,%d] when set", MaximumLocalCapacity))
		}
	}
	for name, value := range map[string]int{
		"maximum reservations":         c.Relay.MaxReservations,
		"maximum reservations per IP":  c.Relay.MaxReservationsPerIP,
		"maximum reservations per ASN": c.Relay.MaxReservationsPerASN,
	} {
		if !inCapacityRange(value) {
			errs = append(errs, fmt.Errorf("%s must be in [1,%d]", name, MaximumLocalCapacity))
		} else if value < c.Relay.LocalCapacity {
			errs = append(errs, fmt.Errorf("%s must cover local relay capacity", name))
		}
	}
	if c.Relay.MaxCircuitsPerPeer <= 0 || c.Relay.MaxCircuitsPerPeer > relayconfig.MaximumCircuitsPerPeer {
		errs = append(errs, fmt.Errorf("maximum circuits per peer must be in [1,%d]", relayconfig.MaximumCircuitsPerPeer))
	}
	if c.Relay.ReservationTTL != DefaultReservationTTL {
		errs = append(errs, fmt.Errorf("reservation TTL must equal the fixed %s", DefaultReservationTTL))
	}
	if c.Relay.BufferSize <= 0 || c.Relay.BufferSize > 1<<20 {
		errs = append(errs, errors.New("relay buffer size must be in [1,1048576] bytes"))
	}

	rm := c.ResourceManager
	if rm.MemoryBytes < MinimumResourceManagerMemory {
		errs = append(errs, errors.New("resource-manager memory must be at least 128 MiB"))
	}
	if rm.FileDescriptors <= 0 || rm.Connections <= 0 || rm.Streams <= 0 {
		errs = append(errs, errors.New("resource-manager FD, connection, and stream limits must be positive and finite"))
	}
	if rm.Connections > rm.FileDescriptors {
		errs = append(errs, errors.New("resource-manager connections must not exceed file descriptors"))
	}
	if rm.Streams < rm.Connections {
		errs = append(errs, errors.New("resource-manager streams must be at least its connections"))
	}
	if rm.Connections < c.Relay.LocalCapacity {
		errs = append(errs, errors.New("resource-manager connections must cover local relay capacity"))
	}
	if c.Relay.BufferSize > 0 && c.Relay.BufferSize <= 1<<20 && inCapacityRange(c.Relay.LocalCapacity) {
		minimumBufferMemory := int64(c.Relay.BufferSize) * int64(c.Relay.LocalCapacity) * 2
		if rm.MemoryBytes < minimumBufferMemory {
			errs = append(errs, errors.New("resource-manager memory must cover both relay buffers for local capacity"))
		}
	}
	cm := c.ConnectionManager
	if cm.LowWater < c.Relay.LocalCapacity {
		errs = append(errs, errors.New("connection-manager low water must cover local relay capacity"))
	}
	if cm.LowWater >= cm.HighWater {
		errs = append(errs, errors.New("connection-manager low water must be below high water"))
	}
	if cm.HighWater > rm.Connections {
		errs = append(errs, errors.New("connection-manager high water must not exceed resource-manager connections"))
	}
	if inCapacityRange(configuredCapacity) {
		if err := c.ValidateEffectiveCapacity(configuredCapacity); err != nil {
			errs = append(errs, fmt.Errorf("configured DDS/DMS capacity is not supported locally: %w", err))
		}
	}

	if err := validateBindAddress(c.AdminAddress); err != nil {
		errs = append(errs, fmt.Errorf("admin address: %w", err))
	}
	if err := validateBindAddress(c.MetricsAddress); err != nil {
		errs = append(errs, fmt.Errorf("metrics address: %w", err))
	}
	if c.AdminAddress == c.MetricsAddress {
		errs = append(errs, errors.New("admin and metrics addresses must differ"))
	}
	if c.ShutdownDrain < time.Second || c.ShutdownDrain > DefaultCircuitDuration || c.ShutdownDrain%time.Second != 0 {
		errs = append(errs, fmt.Errorf("shutdown drain must be whole seconds in [1s,%s]", DefaultCircuitDuration))
	}
	return errors.Join(errs...)
}

// ValidateEffectiveCapacity applies the later DMS-returned capacity to the
// already validated local, reservation, quota, connection, and stream limits.
func (c Config) ValidateEffectiveCapacity(effective int) error {
	if !inCapacityRange(effective) {
		return fmt.Errorf("effective relay capacity must be in [1,%d]", MaximumLocalCapacity)
	}
	var errs []error
	for name, value := range map[string]int{
		"local relay capacity":         c.Relay.LocalCapacity,
		"maximum reservations":         c.Relay.MaxReservations,
		"maximum reservations per IP":  c.Relay.MaxReservationsPerIP,
		"maximum reservations per ASN": c.Relay.MaxReservationsPerASN,
		"resource-manager connections": c.ResourceManager.Connections,
		"resource-manager streams":     c.ResourceManager.Streams,
		"connection-manager low water": c.ConnectionManager.LowWater,
	} {
		if value < effective {
			errs = append(errs, fmt.Errorf("%s %d is below effective relay capacity %d", name, value, effective))
		}
	}
	return errors.Join(errs...)
}

// EffectiveCapacityForDDSClaim derives the DMS capacity from verified DDS
// authority. A signed override wins; otherwise the v1 DMS relay default is 32.
// The derived value must fit every local relay and host-resource backstop.
func (c Config) EffectiveCapacityForDDSClaim(signedMaxConcurrency *int) (int, error) {
	if c.Relay.DDSMaxConcurrency != nil {
		if signedMaxConcurrency == nil || *signedMaxConcurrency != *c.Relay.DDSMaxConcurrency {
			return 0, errors.New("DDS-signed max_concurrency does not match the configured registration override")
		}
	}
	effective := DefaultDMSRelayCapacity
	if signedMaxConcurrency != nil {
		effective = *signedMaxConcurrency
	}
	if err := c.ValidateEffectiveCapacity(effective); err != nil {
		return 0, fmt.Errorf("DDS-derived effective relay capacity: %w", err)
	}
	return effective, nil
}

// Redacted returns a JSON-safe view that never exposes secret file paths.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"dds_url":                    c.DDSURL,
		"dms_url":                    c.DMSURL,
		"dds_public_key_url":         c.DDSPublicKeyURL,
		"allow_local_test_http":      c.AllowLocalTestHTTP,
		"registration_credentials":   redacted,
		"wallet_private_key":         redacted,
		"libp2p_private_key":         redacted,
		"tcp_listen_multiaddr":       c.TCPListenMultiaddr,
		"websocket_listen_multiaddr": c.WebSocketListenMultiaddr,
		"public_base_multiaddrs":     append([]string(nil), c.PublicBaseMultiaddrs...),
		"http":                       c.HTTP,
		"retry":                      c.Retry,
		"timing":                     c.Timing,
		"verification_keys":          c.VerificationKeys,
		"admission":                  c.Admission,
		"relay":                      c.Relay,
		"resource_manager":           c.ResourceManager,
		"connection_manager":         c.ConnectionManager,
		"admin_address":              c.AdminAddress,
		"metrics_address":            c.MetricsAddress,
		"accept_bookings":            c.AcceptBookings,
		"shutdown_drain":             c.ShutdownDrain,
		"libp2p_fixed_timeouts": map[string]string{
			"inbound_upgrade":      Libp2pInboundUpgradeTimeout.String(),
			"protocol_negotiation": Libp2pProtocolNegotiationTimeout.String(),
			"relay_stream":         Libp2pRelayStreamTimeout.String(),
			"relay_connect":        Libp2pRelayConnectTimeout.String(),
			"relay_handshake":      Libp2pRelayHandshakeTimeout.String(),
		},
	}
}

func (c Config) MarshalRedactedJSON() ([]byte, error) {
	return json.Marshal(c.Redacted())
}

type EnvSpec struct {
	Name            string
	Default         string
	Constraint      string
	Required        bool
	SecretReference bool
}

// EnvironmentMatrix documents every environment variable consumed by
// LoadFrom. The returned slice is a copy and is sorted by name.
func EnvironmentMatrix() []EnvSpec {
	matrix := append([]EnvSpec(nil), environmentMatrix...)
	sort.Slice(matrix, func(i, j int) bool { return matrix[i].Name < matrix[j].Name })
	return matrix
}

var environmentMatrix = []EnvSpec{
	{Name: "RELAY_ADMIN_ADDR", Default: "127.0.0.1:9090", Constraint: "valid TCP host:port, different from metrics"},
	{Name: "RELAY_ADMISSION_MAX_ENTRIES", Default: "4096", Constraint: ">0"},
	{Name: "RELAY_ADMISSION_TTL", Default: "30s", Constraint: "(0s,30s]"},
	{Name: "RELAY_AUTH_ATTEMPT_WINDOW", Default: "10s", Constraint: "(0s,1m]"},
	{Name: "RELAY_AUTH_MAX_ATTEMPTS_PER_IP", Default: "32", Constraint: ">0; attempts per window, independent of concurrency"},
	{Name: "RELAY_AUTH_MAX_ATTEMPTS_PER_PEER", Default: "8", Constraint: ">0; attempts per window, independent of concurrency"},
	{Name: "RELAY_AUTH_MAX_CONCURRENCY", Default: "64", Constraint: ">0"},
	{Name: "RELAY_ACCEPT_BOOKINGS", Default: "false", Constraint: "boolean; static operator gate for one process incarnation"},
	{Name: "RELAY_BUFFER_SIZE", Default: "2048", Constraint: "1..1048576 bytes"},
	{Name: "RELAY_CIRCUIT_DATA_BYTES_PER_DIRECTION", Default: "10737418240", Constraint: "positive int64; per direction"},
	{Name: "RELAY_CIRCUIT_DURATION", Default: "15m0s", Constraint: "whole seconds, >=15m, <=uint32 seconds"},
	{Name: "RELAY_CONNMGR_HIGH_WATER", Default: "1024", Constraint: "low < high <= RM connections"},
	{Name: "RELAY_CONNMGR_LOW_WATER", Default: "768", Constraint: "capacity <= low < high"},
	{Name: "RELAY_DDS_KEY_CLOCK_SKEW", Default: "1m0s", Constraint: "[0s,5m]"},
	{Name: "RELAY_DDS_KEY_MAX_STALE", Default: "1h0m0s", Constraint: ">= previous-key overlap"},
	{Name: "RELAY_DDS_KEY_OVERLAP", Default: "31m0s", Constraint: ">=30m plus clock skew"},
	{Name: "RELAY_DDS_KEY_REFRESH_INTERVAL", Default: "2m0s", Constraint: ">0"},
	{Name: "RELAY_DDS_KEY_UNKNOWN_REFRESH_INTERVAL", Default: "30s", Constraint: "(0s,periodic refresh]"},
	{Name: "RELAY_DDS_MAX_CONCURRENCY", Default: "unset (DMS relay default 32)", Constraint: "optional positive PostgreSQL INTEGER DDS-signed override; must fit every local backstop"},
	{Name: "RELAY_DDS_PUBLIC_KEY_URL", Constraint: "HTTPS, or loopback HTTP with local-test switch", Required: true},
	{Name: "RELAY_DDS_SIGNING_METHOD", Default: "ES256", Constraint: "registered JWT signing method"},
	{Name: "RELAY_DDS_URL", Constraint: "HTTPS, or loopback HTTP with local-test switch", Required: true},
	{Name: "RELAY_DMS_URL", Constraint: "HTTPS, or loopback HTTP with local-test switch", Required: true},
	{Name: "RELAY_EMPTY_CLAIM_MAX_JITTER_FRACTION", Default: "0.2", Constraint: "[0,0.2] positive-jitter ceiling"},
	{Name: "RELAY_HTTP_REQUEST_TIMEOUT", Default: "10s", Constraint: "(0s,30s]"},
	{Name: "RELAY_LIBP2P_PRIVATE_KEY_FILE", Constraint: "0600-style non-empty regular binary key file", Required: true, SecretReference: true},
	{Name: "RELAY_LOCAL_CAPACITY", Default: "32", Constraint: "positive PostgreSQL INTEGER and >= effective DMS capacity"},
	{Name: "RELAY_LOCAL_TEST_ALLOW_HTTP", Default: "false", Constraint: "boolean; HTTP remains loopback-only"},
	{Name: "RELAY_MAX_CIRCUITS_PER_PEER", Default: "16", Constraint: "1..256"},
	{Name: "RELAY_MAX_RESERVATIONS", Default: "32", Constraint: "local/effective capacity..2147483647"},
	{Name: "RELAY_MAX_RESERVATIONS_PER_ASN", Default: "32", Constraint: "local/effective capacity..2147483647"},
	{Name: "RELAY_MAX_RESERVATIONS_PER_IP", Default: "32", Constraint: "local/effective capacity..2147483647"},
	{Name: "RELAY_METRICS_ADDR", Default: "127.0.0.1:9091", Constraint: "valid TCP host:port, different from admin"},
	{Name: "RELAY_NODE_TOKEN_REFRESH_FRACTION", Default: "0.75", Constraint: "fixed 0.75"},
	{Name: "RELAY_PROVIDER_LEASE_HEARTBEAT_MAX_FRACTION", Default: "0.35", Constraint: "fixed 0.35"},
	{Name: "RELAY_PROVIDER_LEASE_HEARTBEAT_MIN_FRACTION", Default: "0.25", Constraint: "fixed 0.25"},
	{Name: "RELAY_PROVIDER_SESSION_TTL", Default: "3m0s", Constraint: "fixed DMS contract"},
	{Name: "RELAY_PUBLIC_BASE_MULTIADDRS", Constraint: "comma-separated production dns4/tcp/p2p bases", Required: true},
	{Name: "RELAY_REGISTRATION_CREDENTIALS_FILE", Constraint: "0600-style non-empty regular text file", Required: true, SecretReference: true},
	{Name: "RELAY_RESERVATION_TTL", Default: "10m0s", Constraint: "fixed go-libp2p reservation TTL"},
	{Name: "RELAY_RETRY_INITIAL_BACKOFF", Default: "1s", Constraint: "(0s,retry max]"},
	{Name: "RELAY_RETRY_MAX_BACKOFF", Default: "30s", Constraint: "(0s,30s]"},
	{Name: "RELAY_RM_CONNECTIONS", Default: "2048", Constraint: ">= capacity and connmgr high"},
	{Name: "RELAY_RM_FILE_DESCRIPTORS", Default: "4096", Constraint: ">= RM connections"},
	{Name: "RELAY_RM_MEMORY_BYTES", Default: "1073741824", Constraint: ">=128 MiB; covers buffers"},
	{Name: "RELAY_RM_STREAMS", Default: "8192", Constraint: ">= RM connections"},
	{Name: "RELAY_SHUTDOWN_DRAIN", Default: "15m0s", Constraint: "whole seconds in [1s,15m]"},
	{Name: "RELAY_STATUS_INTERVAL", Default: "30s", Constraint: "fixed DMS contract"},
	{Name: "RELAY_STATUS_MAX_BACKOFF", Default: "10s", Constraint: "(0s,status interval]"},
	{Name: "RELAY_TCP_LISTEN_MULTIADDR", Default: "/ip4/0.0.0.0/tcp/4001", Constraint: "one ip4/ip6 plus TCP socket, no p2p"},
	{Name: "RELAY_WALLET_PRIVATE_KEY_FILE", Constraint: "0600-style non-empty regular trimmed-hex file", Required: true, SecretReference: true},
	{Name: "RELAY_WS_LISTEN_MULTIADDR", Default: "/ip4/0.0.0.0/tcp/4002/ws", Constraint: "one ip4/ip6 plus TCP socket and ws; different from TCP listener"},
}

func validateServiceURL(raw string, allowLocalHTTP bool) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Opaque != "" {
		return errors.New("URL must be absolute with a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("URL must not contain userinfo, query, or fragment")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if !allowLocalHTTP || !isLoopbackHostname(parsed.Hostname()) {
			return errors.New("plain HTTP requires the local-test switch and a loopback host")
		}
		return nil
	default:
		return errors.New("URL scheme must be HTTPS")
	}
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func supportedPublicKeySigningMethod(name string) bool {
	method := jwt.GetSigningMethod(name)
	switch method {
	case jwt.SigningMethodRS256, jwt.SigningMethodRS384, jwt.SigningMethodRS512,
		jwt.SigningMethodES256, jwt.SigningMethodES384, jwt.SigningMethodES512,
		jwt.SigningMethodEdDSA:
		return true
	default:
		return false
	}
}

func validateListenMultiaddr(raw string) error {
	addr, err := ma.NewMultiaddr(raw)
	if err != nil {
		return fmt.Errorf("invalid relay TCP listen multiaddr: %w", err)
	}
	protocols := addr.Protocols()
	if len(protocols) != 2 || (protocols[0].Code != ma.P_IP4 && protocols[0].Code != ma.P_IP6) || protocols[1].Code != ma.P_TCP {
		return errors.New("relay TCP listen multiaddr must contain exactly one ip4 or ip6 component followed by tcp")
	}
	port, err := addr.ValueForProtocol(ma.P_TCP)
	if err != nil {
		return errors.New("relay TCP listen multiaddr requires a TCP port")
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return errors.New("relay TCP listen port must be in [1,65535]")
	}
	return nil
}

func validateWebSocketListenMultiaddr(raw string) error {
	addr, err := ma.NewMultiaddr(raw)
	if err != nil {
		return fmt.Errorf("invalid relay WebSocket listen multiaddr: %w", err)
	}
	protocols := addr.Protocols()
	if len(protocols) != 3 || (protocols[0].Code != ma.P_IP4 && protocols[0].Code != ma.P_IP6) || protocols[1].Code != ma.P_TCP || protocols[2].Code != ma.P_WS {
		return errors.New("relay WebSocket listen multiaddr must contain exactly one ip4 or ip6 component followed by tcp and ws")
	}
	port, err := addr.ValueForProtocol(ma.P_TCP)
	if err != nil {
		return errors.New("relay WebSocket listen multiaddr requires a TCP port")
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return errors.New("relay WebSocket listen port must be in [1,65535]")
	}
	return nil
}

func validateBindAddress(raw string) error {
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return errors.New("must be a TCP host:port")
	}
	if strings.TrimSpace(host) == "" {
		return errors.New("host must not be empty")
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return errors.New("port must be in [1,65535]")
	}
	return nil
}

func inCapacityRange(value int) bool {
	return value >= 1 && value <= MaximumLocalCapacity
}

func requiredString(lookup LookupEnv, name string, destination *string) {
	if value, present := lookup(name); present {
		*destination = value
	}
}

func optionalString(lookup LookupEnv, name string, destination *string) error {
	if value, present := lookup(name); present {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
		*destination = value
	}
	return nil
}

func optionalBool(lookup LookupEnv, name string, destination *bool) error {
	value, present := lookup(name)
	if !present {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("parse %s as boolean: %w", name, err)
	}
	*destination = parsed
	return nil
}

func optionalDuration(lookup LookupEnv, name string, destination *time.Duration) error {
	value, present := lookup(name)
	if !present {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("parse %s as duration: %w", name, err)
	}
	*destination = parsed
	return nil
}

func optionalFloat(lookup LookupEnv, name string, destination *float64) error {
	value, present := lookup(name)
	if !present {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fmt.Errorf("parse %s as finite decimal", name)
	}
	*destination = parsed
	return nil
}

func optionalInt(lookup LookupEnv, name string, destination *int) error {
	value, present := lookup(name)
	if !present {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 0)
	if err != nil {
		return fmt.Errorf("parse %s as integer: %w", name, err)
	}
	*destination = int(parsed)
	return nil
}

func optionalIntPointer(lookup LookupEnv, name string, destination **int) error {
	value, present := lookup(name)
	if !present {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 0)
	if err != nil {
		return fmt.Errorf("parse %s as integer: %w", name, err)
	}
	parsedInt := int(parsed)
	*destination = &parsedInt
	return nil
}

func optionalInt64(lookup LookupEnv, name string, destination *int64) error {
	value, present := lookup(name)
	if !present {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("parse %s as int64: %w", name, err)
	}
	*destination = parsed
	return nil
}

func splitCommaSeparated(raw string) []string {
	parts := strings.Split(raw, ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	return parts
}
