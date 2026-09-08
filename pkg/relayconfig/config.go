package relayconfig

import (
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const MinimumCircuitDuration = 15 * time.Minute

// Limits is the single authoritative Circuit Relay v2 limit profile. Data is
// the maximum number of bytes in each direction, not a shared bidirectional
// budget.
type Limits struct {
	DataBytesPerDirection int64
	Duration              time.Duration
}

// Validate rejects every value that go-libp2p v0.41.1 would otherwise
// truncate or wrap while encoding the Circuit Relay v2 wire limit.
func (l Limits) Validate() error {
	if l.DataBytesPerDirection <= 0 {
		return fmt.Errorf("relay data limit must be a positive int64")
	}
	if l.Duration < MinimumCircuitDuration {
		return fmt.Errorf("relay circuit duration must be at least %s", MinimumCircuitDuration)
	}
	if l.Duration%time.Second != 0 {
		return fmt.Errorf("relay circuit duration must contain whole seconds")
	}
	seconds := uint64(l.Duration / time.Second)
	if seconds == 0 || seconds > math.MaxUint32 {
		return fmt.Errorf("relay circuit duration seconds must fit uint32")
	}
	return nil
}

func (l Limits) DurationSeconds() uint32 {
	return uint32(l.Duration / time.Second)
}

// Canonical is an immutable provider configuration snapshot suitable for DMS
// session pins and relay-ready metadata.
type Canonical struct {
	bases        []string
	endpointKeys []string
	limits       Limits
	fingerprint  string
}

func (c Canonical) Bases() []string {
	return append([]string(nil), c.bases...)
}

func (c Canonical) EndpointKeys() []string {
	return append([]string(nil), c.endpointKeys...)
}

func (c Canonical) Limits() Limits {
	return c.limits
}

func (c Canonical) Fingerprint() string {
	return c.fingerprint
}

func (c Canonical) Multiaddrs() ([]ma.Multiaddr, error) {
	result := make([]ma.Multiaddr, 0, len(c.bases))
	for _, raw := range c.bases {
		addr, err := ma.NewMultiaddr(raw)
		if err != nil {
			return nil, fmt.Errorf("parse canonical relay base %q: %w", raw, err)
		}
		result = append(result, addr)
	}
	return result, nil
}

// Canonicalize implements the exact DMS v1 grammar and fingerprint:
//
//	/dns4/<lowercase-fqdn>/tcp/<1-65535>/p2p/<relay-peer-id>
//	/dns4/<lowercase-fqdn>/tcp/<1-65535>/wss/p2p/<relay-peer-id>
//
// The returned bases and endpoint keys are sorted. A duplicate is rejected
// after normalization rather than silently deduplicated.
func Canonicalize(rawBases []string, relayPeerID peer.ID, limits Limits) (Canonical, error) {
	if err := limits.Validate(); err != nil {
		return Canonical{}, err
	}
	if len(rawBases) == 0 {
		return Canonical{}, fmt.Errorf("at least one relay base address is required")
	}
	if relayPeerID == "" {
		return Canonical{}, fmt.Errorf("relay peer ID is required")
	}
	decodedPeerID, err := peer.Decode(relayPeerID.String())
	if err != nil || decodedPeerID != relayPeerID {
		return Canonical{}, fmt.Errorf("relay peer ID is invalid")
	}

	bases := make([]string, 0, len(rawBases))
	uniqueBases := make(map[string]struct{}, len(rawBases))
	uniqueEndpoints := make(map[string]struct{}, len(rawBases))
	hasDirectTCP := false
	hasWSS := false
	for _, raw := range rawBases {
		base, endpoint, err := canonicalizeBase(raw, relayPeerID.String())
		if err != nil {
			return Canonical{}, err
		}
		if _, duplicate := uniqueBases[base]; duplicate {
			return Canonical{}, fmt.Errorf("duplicate relay base address after normalization: %s", base)
		}
		uniqueBases[base] = struct{}{}
		uniqueEndpoints[endpoint] = struct{}{}
		bases = append(bases, base)
		if strings.HasSuffix(endpoint, "/wss") {
			hasWSS = true
		} else {
			hasDirectTCP = true
		}
	}
	if !hasDirectTCP || !hasWSS {
		return Canonical{}, fmt.Errorf("relay base addresses must include at least one direct TCP endpoint and one WSS endpoint")
	}

	sort.Strings(bases)
	endpointKeys := make([]string, 0, len(uniqueEndpoints))
	for endpoint := range uniqueEndpoints {
		endpointKeys = append(endpointKeys, endpoint)
	}
	sort.Strings(endpointKeys)

	fingerprint := fmt.Sprintf(
		"relay-config-v1|%s|%s|%d|%d",
		strings.Join(bases, ","),
		strings.Join(endpointKeys, ","),
		limits.DataBytesPerDirection,
		limits.DurationSeconds(),
	)
	return Canonical{
		bases:        bases,
		endpointKeys: endpointKeys,
		limits:       limits,
		fingerprint:  fingerprint,
	}, nil
}

func canonicalizeBase(raw, expectedPeerID string) (string, string, error) {
	parts := strings.Split(raw, "/")
	transportSuffix := ""
	peerIDIndex := 6
	switch {
	case len(parts) == 7 && parts[5] == "p2p":
	case len(parts) == 8 && parts[5] == "wss" && parts[6] == "p2p":
		transportSuffix = "/wss"
		peerIDIndex = 7
	default:
		return "", "", fmt.Errorf("invalid relay base address: %s", raw)
	}
	if parts[0] != "" || parts[1] != "dns4" || parts[3] != "tcp" {
		return "", "", fmt.Errorf("invalid relay base address: %s", raw)
	}

	host, ok := canonicalizeFQDN(parts[2])
	if !ok {
		return "", "", fmt.Errorf("invalid relay base address: %s", raw)
	}
	port, err := strconv.ParseUint(parts[4], 10, 16)
	if err != nil || port == 0 {
		return "", "", fmt.Errorf("invalid relay base address: %s", raw)
	}
	if parts[peerIDIndex] != expectedPeerID {
		return "", "", fmt.Errorf("relay base address peer ID does not match loaded identity")
	}

	endpoint := fmt.Sprintf("/dns4/%s/tcp/%d%s", host, port, transportSuffix)
	base := fmt.Sprintf("%s/p2p/%s", endpoint, expectedPeerID)
	if _, err := ma.NewMultiaddr(base); err != nil {
		return "", "", fmt.Errorf("invalid relay base address: %s: %w", raw, err)
	}
	return base, endpoint, nil
}

func canonicalizeFQDN(raw string) (string, bool) {
	host := strings.ToLower(strings.TrimRight(raw, "."))
	if len(host) == 0 || len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return "", false
	}
	if host == "localhost" {
		return "", false
	}
	for _, suffix := range []string{
		".localhost",
		".local",
		".internal",
		".invalid",
		".test",
		".example",
		".home.arpa",
	} {
		if strings.HasSuffix(host, suffix) {
			return "", false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, char := range []byte(label) {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", false
			}
		}
	}
	return host, true
}
